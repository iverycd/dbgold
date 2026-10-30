package datamigrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// These fixtures require explicit permission and dedicated test databases.
// Only unique schemas, tables, functions and schemes created here are removed.
func TestMySQLSQLServerPartitionIntegration(t *testing.T) {
	myDSN, msDSN := os.Getenv("DBGOLD_PARTITION_MYSQL_DSN"), os.Getenv("DBGOLD_PARTITION_SQLSERVER_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || myDSN == "" || msDSN == "" {
		t.Skip("requires isolated MySQL/SQL Server fixtures and DBGOLD_PARTITION_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	my, err := sql.Open("mysql", myDSN)
	require.NoError(t, err)
	defer my.Close()
	ms, err := sql.Open("sqlserver", msDSN)
	require.NoError(t, err)
	defer ms.Close()
	var database, mysqlVersion, sqlServerVersion string
	require.NoError(t, my.QueryRowContext(ctx, `SELECT DATABASE(),VERSION()`).Scan(&database, &mysqlVersion))
	require.NoError(t, ms.QueryRowContext(ctx, `SELECT CONVERT(nvarchar(max),@@VERSION)`).Scan(&sqlServerVersion))
	t.Logf("MySQL %s → %s", mysqlVersion, sqlServerVersion)
	for _, content := range []string{"both", "schema_only"} {
		t.Run(content, func(t *testing.T) {
			name := "dbgold_ms_" + strconv.FormatInt(time.Now().UnixNano(), 36)
			schema := name
			fn, scheme := partition.SQLServerHelperNames(schema, name)
			_, err := ms.ExecContext(ctx, "CREATE SCHEMA "+partitionSQLServerQuote(schema))
			require.NoError(t, err)
			defer partitionSQLServerCleanup(t, ms, schema, name, fn, scheme)
			parts := []string{"Negative", "Middle", "High", "Empty"}
			_, err = my.ExecContext(ctx, "CREATE TABLE `"+name+"` (`K` INT NOT NULL,`Payload` VARCHAR(40)) PARTITION BY RANGE COLUMNS(`K`) (PARTITION `Negative` VALUES LESS THAN(0),PARTITION `Middle` VALUES LESS THAN(10),PARTITION `High` VALUES LESS THAN(20),PARTITION `Empty` VALUES LESS THAN(MAXVALUE))")
			require.NoError(t, err)
			defer my.ExecContext(context.Background(), "DROP TABLE IF EXISTS `"+name+"`")
			_, err = my.ExecContext(ctx, "INSERT INTO `"+name+"` VALUES(-1,'negative'),(0,'boundary'),(9,'nine'),(10,'ten'),(11,NULL)")
			require.NoError(t, err)
			r, err := source.NewMySQLForSQLServer(myDSN, database, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			w, err := target.NewSQLServer(msDSN, schema, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer w.Close()
			job := &Job{LogCh: make(chan string, 16384)}
			cfg := Config{Mode: "include", Filter: name, Content: content, TargetDBType: "sqlserver", TargetSchema: schema, PageSize: 2, MaxParallel: 1, ValidateData: true, CaptureRepair: true}
			preview := NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
			require.True(t, preview.Ready, "issues=%+v fatal=%s", preview.Issues, preview.FatalError)
			report := NewMigrator(r, w, job, cfg).Run(ctx)
			if report.HasFailures() && report.PartitionMigration != nil {
				for _, entry := range report.PartitionMigration.Tables {
					if entry.Plan == nil {
						continue
					}
					actual, inspectErr := w.InspectPartitionTable(ctx, entry.TargetTable)
					expectedCanonical, expectedErr := partition.CanonicalSpec(entry.Plan.Target)
					actualCanonical, actualErr := partition.CanonicalSpec(actual)
					expectedJSON, _ := json.Marshal(expectedCanonical)
					actualJSON, _ := json.Marshal(actualCanonical)
					t.Logf("partition %s expected=%s actual=%s inspect_error=%v expected_error=%v actual_error=%v", entry.TargetTable, expectedJSON, actualJSON, inspectErr, expectedErr, actualErr)
				}
			}
			partitionRequireSuccessfulReport(t, report)
			entry := report.PartitionMigration.Tables[0]
			require.Equal(t, "succeeded", entry.Status)
			require.Len(t, entry.Plan.Mapping, 4)
			actual, err := w.InspectPartitionTable(ctx, name)
			require.NoError(t, err)
			require.NoError(t, partition.Equivalent(entry.Plan.Target, actual))
			root := partitionSQLServerQuote(schema) + "." + partitionSQLServerQuote(name)
			compareLeaves := func(multiplier int) {
				for i, leaf := range parts {
					sourceRows := partitionIntegrationRows(t, ctx, my, "SELECT `K`,`Payload` FROM `"+name+"` PARTITION (`"+leaf+"`) ORDER BY `K`,`Payload`")
					var expected []string
					for _, row := range sourceRows {
						for n := 0; n < multiplier; n++ {
							expected = append(expected, row)
						}
					}
					targetRows := partitionIntegrationRows(t, ctx, ms, "SELECT [K],[Payload] FROM "+root+" WHERE $PARTITION."+partitionSQLServerQuote(fn)+"([K])="+strconv.Itoa(i+1)+" ORDER BY [K],[Payload]")
					require.Equal(t, expected, targetRows, "source leaf %s must map to ordinal %d", leaf, i+1)
				}
			}
			if content == "schema_only" {
				compareLeaves(0)
				cfg.Content, cfg.CaptureRepair = "data_only", false
				partitionRequireSuccessfulReport(t, NewMigrator(r, w, job, cfg).Run(ctx))
			} else {
				require.NotNil(t, report.DataValidation)
				require.Len(t, report.DataValidation.Tables, 5)
			}
			compareLeaves(1)
			_, err = my.ExecContext(ctx, "UPDATE `"+name+"` SET `K`=10 WHERE `Payload`='negative'")
			require.NoError(t, err)
			_, err = ms.ExecContext(ctx, "UPDATE "+root+" SET [K]=10 WHERE [Payload]='negative'")
			require.NoError(t, err)
			compareLeaves(1)
			cfg.Content, cfg.CaptureRepair = "data_only", false
			partitionRequireSuccessfulReport(t, NewMigrator(r, w, job, cfg).Run(ctx))
			compareLeaves(2)
		})
	}
}

func TestSQLServerRangeLeftPostgresPartitionIntegration(t *testing.T) {
	msDSN, pgDSN := os.Getenv("DBGOLD_PARTITION_SQLSERVER_DSN"), os.Getenv("DBGOLD_PARTITION_PG_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || msDSN == "" || pgDSN == "" {
		t.Skip("requires isolated SQL Server/PostgreSQL fixtures and DBGOLD_PARTITION_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ms, err := sql.Open("sqlserver", msDSN)
	require.NoError(t, err)
	defer ms.Close()
	pg, err := sql.Open("postgres", pgDSN)
	require.NoError(t, err)
	defer pg.Close()
	var database, filegroup, sqlServerVersion, pgVersion string
	require.NoError(t, ms.QueryRowContext(ctx, `SELECT DB_NAME(),CONVERT(nvarchar(max),@@VERSION)`).Scan(&database, &sqlServerVersion))
	require.NoError(t, ms.QueryRowContext(ctx, `SELECT name FROM sys.filegroups WHERE is_default=1`).Scan(&filegroup))
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT version()`).Scan(&pgVersion))
	t.Logf("%s → %s", sqlServerVersion, pgVersion)
	name := "dbgold_left_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	schema, fn, scheme := name, name+"_function", name+"_scheme"
	_, err = ms.ExecContext(ctx, "CREATE SCHEMA "+partitionSQLServerQuote(schema))
	require.NoError(t, err)
	defer partitionSQLServerCleanup(t, ms, schema, name, fn, scheme)
	_, err = ms.ExecContext(ctx, "CREATE PARTITION FUNCTION "+partitionSQLServerQuote(fn)+" (BIGINT) AS RANGE LEFT FOR VALUES (-9223372036854775808,-1,0,1,2,9223372036854775806)")
	require.NoError(t, err)
	_, err = ms.ExecContext(ctx, "CREATE PARTITION SCHEME "+partitionSQLServerQuote(scheme)+" AS PARTITION "+partitionSQLServerQuote(fn)+" ALL TO ("+partitionSQLServerQuote(filegroup)+")")
	require.NoError(t, err)
	root := partitionSQLServerQuote(schema) + "." + partitionSQLServerQuote(name)
	_, err = ms.ExecContext(ctx, "CREATE TABLE "+root+" ([K] BIGINT NOT NULL,[Payload] NVARCHAR(40)) ON "+partitionSQLServerQuote(scheme)+"([K])")
	require.NoError(t, err)
	for i, value := range []int64{math.MinInt64, math.MinInt64 + 1, -2, -1, 0, 1, math.MaxInt64 - 1, math.MaxInt64} {
		_, err = ms.ExecContext(ctx, "INSERT INTO "+root+" ([K],[Payload]) VALUES(@p1,@p2)", value, fmt.Sprintf("Value_%d", i))
		require.NoError(t, err)
	}
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	_, err = pg.ExecContext(ctx, "CREATE SCHEMA "+quote(schema))
	require.NoError(t, err)
	defer pg.ExecContext(context.Background(), "DROP SCHEMA "+quote(schema)+" CASCADE")
	r, err := source.NewSQLServer(msDSN, database, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	w, err := target.NewPostgres(pgDSN, schema, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	job := &Job{LogCh: make(chan string, 16384)}
	cfg := Config{Mode: "include", Filter: name, Content: "both", TargetDBType: "postgres", TargetSchema: schema, PageSize: 2, MaxParallel: 1, ValidateData: true, CaptureRepair: true}
	preview := NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
	require.True(t, preview.Ready, "issues=%+v fatal=%s", preview.Issues, preview.FatalError)
	report := NewMigrator(r, w, job, cfg).Run(ctx)
	partitionRequireSuccessfulReport(t, report)
	entry := report.PartitionMigration.Tables[0]
	require.Contains(t, entry.Plan.Rule, "sqlserver-integer-range-left-to-half-open")
	require.Len(t, entry.Plan.Mapping, 7)
	require.NotNil(t, report.DataValidation)
	require.Len(t, report.DataValidation.Tables, 8)
	actual, err := w.InspectPartitionTable(ctx, name)
	require.NoError(t, err)
	require.NoError(t, partition.Equivalent(entry.Plan.Target, actual))
	compareLeaves := func() {
		for i, mapping := range entry.Plan.Mapping {
			sourceRows := partitionIntegrationRows(t, ctx, ms, "SELECT [K],[Payload] FROM "+root+" WHERE $PARTITION."+partitionSQLServerQuote(fn)+"([K])="+strconv.Itoa(i+1)+" ORDER BY [K],[Payload]")
			targetRows := partitionIntegrationRows(t, ctx, pg, "SELECT \"K\",\"Payload\" FROM "+quote(schema)+"."+quote(mapping.TargetName)+" ORDER BY \"K\",\"Payload\"")
			require.Equal(t, sourceRows, targetRows, "SQL Server native ordinal %d must keep every boundary row", i+1)
			if i == 4 {
				require.Empty(t, targetRows, "the pre-existing empty (1,2] leaf must be preserved")
			}
		}
	}
	compareLeaves()
	_, err = ms.ExecContext(ctx, "UPDATE "+root+" SET [K]=@p1 WHERE [Payload]='Value_1'", int64(math.MaxInt64))
	require.NoError(t, err)
	_, err = pg.ExecContext(ctx, "UPDATE "+quote(schema)+"."+quote(name)+" SET \"K\"=$1 WHERE \"Payload\"='Value_1'", int64(math.MaxInt64))
	require.NoError(t, err)
	compareLeaves()
}

func partitionSQLServerQuote(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }

func partitionSQLServerCleanup(t *testing.T, db *sql.DB, schema, table, function, scheme string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, object := range []struct{ query, name string }{
		{"IF OBJECT_ID(@p1,N'U') IS NOT NULL DROP TABLE " + partitionSQLServerQuote(schema) + "." + partitionSQLServerQuote(table), schema + "." + table},
		{"IF EXISTS(SELECT 1 FROM sys.partition_schemes WHERE name=@p1) DROP PARTITION SCHEME " + partitionSQLServerQuote(scheme), scheme},
		{"IF EXISTS(SELECT 1 FROM sys.partition_functions WHERE name=@p1) DROP PARTITION FUNCTION " + partitionSQLServerQuote(function), function},
		{"IF EXISTS(SELECT 1 FROM sys.schemas WHERE name=@p1) DROP SCHEMA " + partitionSQLServerQuote(schema), schema},
	} {
		_, err := db.ExecContext(ctx, object.query, object.name)
		require.NoError(t, err, "clean up fixture object %s", object.name)
	}
}

func partitionIntegrationRows(t *testing.T, ctx context.Context, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	require.NoError(t, err)
	defer rows.Close()
	var result []string
	for rows.Next() {
		var key int64
		var label sql.NullString
		require.NoError(t, rows.Scan(&key, &label))
		result = append(result, fmt.Sprintf("%d|%t|%s", key, label.Valid, label.String))
	}
	require.NoError(t, rows.Err())
	return result
}

func partitionRequireSuccessfulReport(t *testing.T, report MigrationReport) {
	t.Helper()
	if report.HasFailures() {
		var issues []string
		if report.PartitionMigration != nil {
			for _, entry := range report.PartitionMigration.Tables {
				issues = append(issues, entry.SourceTable+": "+entry.Status+" "+entry.Error)
			}
		}
		t.Fatalf("partition migration failed: %s; tables=%+v data=%+v counts=%+v validation=%+v", strings.Join(issues, "; "), report.Tables, report.Data, report.RowCounts, report.DataValidation)
	}
	require.NotNil(t, report.PartitionMigration)
}
