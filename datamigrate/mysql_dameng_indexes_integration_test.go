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

// Opt-in, dedicated databases only. All DDL is confined to one unique fixture
// table; the test never changes DM's duplicate-index configuration.
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
			report := NewMigrator(reader, writer, job, config).Run(ctx)
			require.False(t, report.HasFailures(), "%+v; logs=%v", report, drainLogs(job))
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

			rows, err := dst.QueryContext(ctx, `SELECT i.INDEX_NAME,i.UNIQUENESS,c.COLUMN_NAME
FROM ALL_INDEXES i JOIN ALL_IND_COLUMNS c ON c.INDEX_OWNER=i.OWNER AND c.INDEX_NAME=i.INDEX_NAME
WHERE i.TABLE_OWNER=? AND i.TABLE_NAME=? ORDER BY i.INDEX_NAME,c.COLUMN_POSITION`, strings.ToUpper(schema), strings.ToUpper(table))
			require.NoError(t, err)
			defer rows.Close()
			keys, unique := map[string][]string{}, map[string]bool{}
			for rows.Next() {
				var name, uniqueness, column string
				require.NoError(t, rows.Scan(&name, &uniqueness, &column))
				keys[name] = append(keys[name], column)
				unique[name] = uniqueness == "UNIQUE"
			}
			require.NoError(t, rows.Err())
			ordinaryCount, uniqueCode, compositeAB, compositeBA := 0, false, false, false
			for name, columns := range keys {
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
