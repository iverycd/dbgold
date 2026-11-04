//go:build darwin || linux

package datamigrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// TestMySQLScanComparison uses dedicated instances, generates one immutable
// source fixture, and creates a new target namespace and Go process per trial.
func TestMySQLScanComparison(t *testing.T) {
	runScanComparison(t, "gaussdb", "DBGOLD_MYSQL_SCAN_GAUSS_DSN", [][]string{
		{"offset", "stream", "range"}, {"stream", "range", "offset"}, {"range", "offset", "stream"},
	})
}

// A=OFFSET/row, B=stream/row, C=OFFSET/batch, D=stream/batch.
// In these four orders each treatment occupies each position once and every
// ordered pair of different treatments occurs once at adjacent positions.
func TestMySQLDamengScanComparison(t *testing.T) {
	runScanComparison(t, "dameng", "DBGOLD_MYSQL_SCAN_DM_DSN", [][]string{
		{"A", "B", "D", "C"}, {"B", "C", "A", "D"}, {"C", "D", "B", "A"}, {"D", "A", "C", "B"},
	})
}

func runScanComparison(t *testing.T, product, targetEnv string, orders [][]string) {
	myDSN, dstDSN := os.Getenv("DBGOLD_MYSQL_SCAN_SOURCE_DSN"), os.Getenv(targetEnv)
	if myDSN == "" || dstDSN == "" {
		t.Skipf("set DBGOLD_MYSQL_SCAN_SOURCE_DSN and %s to dedicated test instances", targetEnv)
	}
	if ns := os.Getenv("DBGOLD_SCAN_CHILD_SOURCE"); ns != "" {
		runMySQLScanTrial(t, myDSN, dstDSN, ns, product)
		return
	}
	if product == "dameng" && os.Getenv("DBGOLD_MYSQL_SCAN_SMOKE") == "1" {
		orders = orders[:1]
	}
	rows := 1_000_000
	if raw := os.Getenv("DBGOLD_MYSQL_SCAN_ROWS"); raw != "" {
		var err error
		rows, err = strconv.Atoi(raw)
		require.NoError(t, err)
		require.Positive(t, rows)
	}
	admin, err := sql.Open("mysql", myDSN)
	require.NoError(t, err)
	defer admin.Close()
	ns := fmt.Sprintf("dbgold_scan_%d", time.Now().UnixNano())
	_, err = admin.Exec("CREATE DATABASE `" + ns + "` CHARACTER SET utf8mb4")
	require.NoError(t, err)
	defer func() {
		_, err := admin.Exec("DROP DATABASE `" + ns + "`")
		require.NoError(t, err)
	}()
	cfg, err := mysql.ParseDSN(myDSN)
	require.NoError(t, err)
	cfg.DBName = ns
	fixture, err := sql.Open("mysql", cfg.FormatDSN())
	require.NoError(t, err)
	defer fixture.Close()
	tables := seedMySQLScanFixtures(t, fixture, rows)
	exe, err := os.Executable()
	require.NoError(t, err)
	var trials []scanTrialResult
	file := filepath.Join(t.TempDir(), "trial.json")
	for round, order := range orders {
		for _, strategy := range order {
			t.Logf("round %d starting %s", round+1, strategy)
			cmd := exec.CommandContext(t.Context(), exe, "-test.run=^"+t.Name()+"$", "-test.timeout=1h")
			cmd.Env = append(os.Environ(), "DBGOLD_SCAN_CHILD_SOURCE="+ns, "DBGOLD_SCAN_CHILD_STRATEGY="+strategy,
				"DBGOLD_SCAN_CHILD_RESULT="+file, fmt.Sprintf("DBGOLD_SCAN_CHILD_TABLES=%d", len(tables)))
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
			buf, err := os.ReadFile(file)
			require.NoError(t, err)
			var trial scanTrialResult
			require.NoError(t, json.Unmarshal(buf, &trial))
			trial.Round = round + 1
			rss := float64(cmd.ProcessState.SysUsage().(*syscall.Rusage).Maxrss)
			if runtime.GOOS == "darwin" {
				rss /= 1024 // Darwin returns bytes, Linux returns KiB.
			}
			trial.PeakRSSMiB = rss / 1024
			trials = append(trials, trial)
			b, err := json.Marshal(trial)
			require.NoError(t, err)
			t.Logf("SCAN_TRIAL %s", b)
		}
	}
	if path := os.Getenv("DBGOLD_MYSQL_SCAN_RESULTS"); path != "" {
		b, err := json.MarshalIndent(trials, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, b, 0600))
	}
}

func runMySQLScanTrial(t *testing.T, myDSN, dstDSN, ns, product string) {
	require.Regexp(t, `^dbgold_scan_[0-9]+$`, ns)
	strategy := os.Getenv("DBGOLD_SCAN_CHILD_STRATEGY")
	group, writeStrategy := "", ""
	if product == "dameng" {
		group = strategy
		require.Contains(t, []string{"A", "B", "C", "D"}, group)
		strategy, writeStrategy = "offset", "row"
		if group == "B" || group == "D" {
			strategy = "stream"
		}
		if group == "C" || group == "D" {
			writeStrategy = "batch"
		}
	} else {
		require.Contains(t, []string{"range", "offset", "stream"}, strategy)
	}
	tableCount, err := strconv.Atoi(os.Getenv("DBGOLD_SCAN_CHILD_TABLES"))
	require.NoError(t, err)
	ctx := t.Context()
	cfg, err := mysql.ParseDSN(myDSN)
	require.NoError(t, err)
	cfg.DBName, cfg.ParseTime = ns, true
	makeReader := source.NewMySQL
	driverName := "opengauss"
	if product == "dameng" {
		makeReader = source.NewMySQLForDameng
		driverName = "dm"
	}
	r, err := makeReader(cfg.FormatDSN(), ns, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	admin, err := sql.Open(driverName, dstDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	targetSchema := fmt.Sprintf("dbgold_trial_%d", time.Now().UnixNano())
	tables := make(map[string]*scanTrialTable)
	result := scanTrialResult{Strategy: strategy, Target: product, Group: group, WriteStrategy: writeStrategy, Tables: tables}
	metrics := &scanTrialMetrics{tables: tables}
	var writer target.Writer
	var poolStats func() sql.DBStats
	var batchStats func() (int64, int64)
	if product == "dameng" {
		targetSchema = strings.ToUpper(targetSchema)
		createDMScanSchema(t, admin, targetSchema)
		w, err := target.NewDaMeng(dstDSN, targetSchema, target.ConnPoolConfig{})
		require.NoError(t, err)
		writer, poolStats, batchStats = &scanTrialDMTarget{w, metrics}, w.PoolStats, w.BatchWriteStats
	} else {
		_, err = admin.ExecContext(ctx, "CREATE SCHEMA "+targetSchema)
		require.NoError(t, err)
		defer func() {
			_, err := admin.Exec("DROP SCHEMA " + targetSchema + " CASCADE")
			require.NoError(t, err)
		}()
		w, err := target.NewGaussDB(dstDSN, targetSchema, target.ConnPoolConfig{})
		require.NoError(t, err)
		writer, poolStats = &scanTrialTarget{w, metrics}, w.PoolStats
	}
	defer writer.Close()
	m := NewMigrator(&scanTrialSource{r, metrics}, writer, &Job{LogCh: make(chan string, 1024)}, Config{
		Mode: "all", Content: "both", PageSize: 20000, MaxParallel: 10, IntraTableParallel: 8,
		TargetSchema: targetSchema, TargetDBType: product, ValidateData: true, MySQLReadStrategy: strategy, DamengWriteStrategy: writeStrategy,
	})
	// Query using a separate pool. Performance Schema attributes migration
	// statements to the unique source schema while this observer uses no schema.
	cfg.DBName = ""
	statsDB, err := sql.Open("mysql", cfg.FormatDSN())
	require.NoError(t, err)
	defer statsDB.Close()
	var enabled bool
	require.NoError(t, statsDB.QueryRow("SELECT @@performance_schema").Scan(&enabled))
	require.True(t, enabled, "enable performance_schema for Rows_examined")
	var sourceStart, targetStart sql.DBStats
	var examinedStart, fetchStart int64
	m.observeDataCopy = func(table string, start bool) {
		if table != "" {
			metrics.tableBoundary(table, start)
			return
		}
		if start {
			examinedStart, fetchStart = scanSourceWork(t, ctx, statsDB, ns)
			sourceStart, targetStart = r.PoolStats(), poolStats()
			metrics.start = time.Now()
		} else {
			result.CopyMS = scanMS(time.Since(metrics.start))
			result.SourcePool = poolDelta(sourceStart, r.PoolStats())
			result.TargetPool = poolDelta(targetStart, poolStats())
			examined, fetches := scanSourceWork(t, ctx, statsDB, ns)
			result.SourceRowsExamined = examined - examinedStart
			result.SourceTableFetches = fetches - fetchStart
		}
	}
	start := time.Now()
	report := m.Run(ctx)
	result.JobMS = scanMS(time.Since(start))
	result.JobHasFailures = report.HasFailures()
	require.False(t, result.JobHasFailures, "report=%+v validation=%+v preparation=%+v", report, report.DataValidation, report.CompatibilityFunctions)
	require.Len(t, report.RowCounts, tableCount)
	require.NotNil(t, report.DataValidation)
	result.ContentValidation = report.DataValidation.Status
	if product == "dameng" {
		require.NotNil(t, report.CompatibilityFunctions)
		require.Equal(t, 9, report.CompatibilityFunctions.Success)
		result.BatchExecs, result.ScalarRows = batchStats()
		if writeStrategy == "batch" && result.Tables["scan_dense"].RowsWritten > 1 {
			require.Positive(t, result.BatchExecs)
		}
	}
	require.Equal(t, validation.StatusPassed, report.DataValidation.Status, "%+v", report.DataValidation)
	require.Len(t, report.DataValidation.Tables, tableCount)
	for _, row := range report.RowCounts {
		require.True(t, row.Match, "%+v", row)
		require.Contains(t, tables, row.Table)
		require.Equal(t, row.Src, tables[row.Table].RowsWritten)
	}
	if product == "dameng" && writeStrategy == "row" {
		var written int64
		for _, table := range tables {
			written += table.RowsWritten
		}
		require.Zero(t, result.BatchExecs)
		require.Equal(t, written, result.ScalarRows)
	}
	require.Positive(t, result.SourceRowsExamined, "enable statements_digest for Rows_examined")
	require.Positive(t, result.SourceTableFetches, "enable wait/io/table/sql/handler for COUNT_FETCH")
	buf, err := json.Marshal(result)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(os.Getenv("DBGOLD_SCAN_CHILD_RESULT"), buf, 0600))
}

func scanSourceWork(t *testing.T, ctx context.Context, db *sql.DB, schema string) (int64, int64) {
	t.Helper()
	var rows, fetches int64
	err := db.QueryRowContext(ctx, `SELECT
		(SELECT COALESCE(SUM(SUM_ROWS_EXAMINED),0) FROM performance_schema.events_statements_summary_by_digest WHERE SCHEMA_NAME=?),
		(SELECT COALESCE(SUM(COUNT_FETCH),0) FROM performance_schema.table_io_waits_summary_by_table WHERE OBJECT_SCHEMA=?)`, schema, schema).Scan(&rows, &fetches)
	require.NoError(t, err, "require read access to Performance Schema statement digests and table I/O counters")
	return rows, fetches
}
