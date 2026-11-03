package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Opt-in, dedicated databases only. Fixture tables and newly installed routines
// are cleaned up; the test never changes DM's duplicate-index configuration.
func TestMySQLDamengDuplicateIndexesIntegration(t *testing.T) {
	srcDSN := os.Getenv("MYSQL_DM_TEST_SOURCE_DSN")
	dstDSN := os.Getenv("MYSQL_DM_TEST_TARGET_DSN")
	schema := os.Getenv("MYSQL_DM_TEST_SCHEMA")
	if srcDSN == "" || dstDSN == "" || schema == "" {
		t.Skip("需要 MYSQL_DM_TEST_SOURCE_DSN / MYSQL_DM_TEST_TARGET_DSN / MYSQL_DM_TEST_SCHEMA")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg, err := mysql.ParseDSN(srcDSN)
	require.NoError(t, err)
	require.NotEmpty(t, cfg.DBName)
	src, err := sql.Open("mysql", srcDSN)
	require.NoError(t, err)
	t.Cleanup(func() { src.Close() })
	dst, err := sql.Open("dm", dstDSN)
	require.NoError(t, err)
	t.Cleanup(func() { dst.Close() })
	table := fmt.Sprintf("DbgDuplicateIdx_%d", time.Now().UnixNano())
	sourceTable := "`" + table + "`"
	targetTable := dialect.NewDaMeng().QualifyTable(schema, table)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, _ = src.ExecContext(cleanup, "DROP TABLE "+sourceTable)
		_, _ = dst.ExecContext(cleanup, "DROP TABLE "+targetTable+" CASCADE CONSTRAINTS")
	})
	_, err = src.ExecContext(ctx, "CREATE TABLE "+sourceTable+` (
 ROW_ID INT NOT NULL, Code INT, Ordinary INT, A INT, B INT,
 PRIMARY KEY (ROW_ID),
 KEY ByRowID (ROW_ID),
 KEY A_CodeNormal (Code), UNIQUE KEY Z_CodeUnique (Code),
 KEY OrdinaryFirst (Ordinary), KEY OrdinarySecond (Ordinary),
 KEY CompositeAB (A, B), KEY CompositeBA (B, A)
)`)
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "INSERT INTO "+sourceTable+" VALUES (1, 10, 20, 30, 40), (2, 11, 21, 31, 41)")
	require.NoError(t, err)

	for _, mode := range []string{"schema_only", "both", "indexes_only"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "indexes_only" {
				_, err := dst.ExecContext(ctx, "DROP TABLE "+targetTable+" CASCADE CONSTRAINTS")
				require.NoError(t, err)
				_, err = dst.ExecContext(ctx, "CREATE TABLE "+targetTable+` ("ROW_ID" INT NOT NULL PRIMARY KEY, "CODE" INT, "ORDINARY" INT, "A" INT, "B" INT)`)
				require.NoError(t, err)
			}
			reader, err := source.NewMySQLForDameng(srcDSN, cfg.DBName, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer reader.Close()
			writer, err := target.NewDaMeng(dstDSN, schema, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer writer.Close()
			config := Config{
				Mode: "include", Filter: table, Content: mode,
				TargetSchema: schema, TargetDBType: "dameng",
				PageSize: 10, MaxParallel: 1, CaptureRepair: true,
			}
			if mode == "indexes_only" {
				config.Content, config.Objects = "schema_only", []string{"indexes"}
			}
			job := &Job{LogCh: make(chan string, 1024)}
			migrationStarted := time.Now()
			report := NewMigrator(reader, writer, job, config).Run(ctx)
			defer cleanupMySQLDMFixtureFunctions(t, dst, report)
			t.Logf("%s migration including index DDL/coverage: %s", mode, time.Since(migrationStarted))
			t.Logf("migration logs: %v", drainLogs(job))
			requireMySQLDMFixtureReport(t, report, mode != "indexes_only")
			require.Equal(t, 7, report.Indexes.Total)
			require.Equal(t, 7, report.Indexes.Success)
			require.Zero(t, report.Indexes.Failed)
			for _, object := range report.RepairManifest.Objects {
				if object.Kind == "index" {
					require.Equal(t, "succeeded", object.Status)
					require.Len(t, object.Units, 1)
					require.Equal(t, "succeeded", object.Units[0].Status)
				}
			}

			catalogStarted := time.Now()
			rows, err := dst.QueryContext(ctx, `SELECT INDEX_NAME,UNIQUENESS FROM ALL_INDEXES
WHERE TABLE_OWNER=? AND TABLE_NAME=?`, strings.ToUpper(schema), strings.ToUpper(table))
			require.NoError(t, err)
			defer rows.Close()
			unique := map[string]bool{}
			for rows.Next() {
				var name, uniqueness string
				require.NoError(t, rows.Scan(&name, &uniqueness))
				unique[name] = uniqueness == "UNIQUE"
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			t.Logf("ALL_INDEXES verification read: %s", time.Since(catalogStarted))

			catalogStarted = time.Now()
			rows, err = dst.QueryContext(ctx, `SELECT INDEX_NAME,COLUMN_NAME,COLUMN_POSITION FROM ALL_IND_COLUMNS
WHERE TABLE_OWNER=? AND TABLE_NAME=?`, strings.ToUpper(schema), strings.ToUpper(table))
			require.NoError(t, err)
			defer rows.Close()
			keyColumns := map[string]map[int]string{}
			for rows.Next() {
				var name, column string
				var position int
				require.NoError(t, rows.Scan(&name, &column, &position))
				if keyColumns[name] == nil {
					keyColumns[name] = map[int]string{}
				}
				keyColumns[name][position] = column
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			t.Logf("ALL_IND_COLUMNS verification read: %s", time.Since(catalogStarted))
			ordinaryCount, uniqueCode, compositeAB, compositeBA := 0, false, false, false
			for name, byPosition := range keyColumns {
				columns := make([]string, len(byPosition))
				for position := 1; position <= len(columns); position++ {
					column, ok := byPosition[position]
					require.True(t, ok, "missing index key position")
					columns[position-1] = column
				}
				if slices.Equal(columns, []string{"ORDINARY"}) {
					ordinaryCount++
				}
				uniqueCode = uniqueCode || unique[name] && slices.Equal(columns, []string{"CODE"})
				compositeAB = compositeAB || slices.Equal(columns, []string{"A", "B"})
				compositeBA = compositeBA || slices.Equal(columns, []string{"B", "A"})
			}
			require.Equal(t, 1, ordinaryCount, "same-column ordinary indexes should reuse one physical index")
			require.True(t, uniqueCode, "the unique index must not be replaced by an ordinary index")
			require.True(t, compositeAB && compositeBA, "different key orders must remain distinct")
		})
	}
}
