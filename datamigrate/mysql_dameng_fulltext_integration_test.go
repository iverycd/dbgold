package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Opt-in: use dedicated test databases. Only this fixture's table and context
// index are created/dropped; the index-only migration preserves existing rows.
func TestMySQLDamengFulltextIntegration(t *testing.T) {
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
	table := fmt.Sprintf("DbgFulltext_%d", time.Now().UnixNano())
	sourceTable := "`" + table + "`"
	dia := dialect.NewDaMeng()
	targetTable := dia.QualifyTable(schema, table)
	index := source.IndexInfo{TableName: table, IndexName: "ByBody", Columns: []string{"Body"}, Method: "FULLTEXT"}
	targetIndex := dia.QuoteIdent(dia.IndexName(index))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, _ = dst.ExecContext(cleanup, "DROP CONTEXT INDEX "+targetIndex+" ON "+targetTable)
		_, _ = dst.ExecContext(cleanup, "DROP TABLE "+targetTable+" CASCADE CONSTRAINTS")
		_, _ = src.ExecContext(cleanup, "DROP TABLE "+sourceTable)
	})
	_, err = src.ExecContext(ctx, "CREATE TABLE "+sourceTable+" (`Id` INT PRIMARY KEY, `Body` TEXT, FULLTEXT KEY `ByBody` (`Body`))")
	require.NoError(t, err)
	_, err = dst.ExecContext(ctx, "CREATE TABLE "+targetTable+` ("ID" INT PRIMARY KEY, "BODY" CLOB)`)
	require.NoError(t, err)
	_, err = dst.ExecContext(ctx, "INSERT INTO "+targetTable+` ("ID", "BODY") VALUES (1, 'initialtoken')`)
	require.NoError(t, err)

	reader, err := source.NewMySQLForDameng(srcDSN, cfg.DBName, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer reader.Close()
	writer, err := target.NewDaMeng(dstDSN, schema, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer writer.Close()
	job := &Job{LogCh: make(chan string, 1024)}
	report := NewMigrator(reader, writer, job, Config{
		Mode: "include", Filter: table, Content: "schema_only", Objects: []string{"indexes"},
		TargetSchema: schema, TargetDBType: "dameng", PageSize: 10, MaxParallel: 1, CaptureRepair: true,
	}).Run(ctx)
	require.False(t, report.HasFailures(), "%+v; logs=%v", report, drainLogs(job))
	require.Equal(t, 1, report.Indexes.Total)
	require.Equal(t, 1, report.Indexes.Success)
	require.NotNil(t, report.RepairManifest)
	found := false
	for _, obj := range report.RepairManifest.Objects {
		if obj.Kind != "index" {
			continue
		}
		found = true
		require.Equal(t, "FULLTEXT", obj.Index.Method)
		require.Equal(t, "succeeded", obj.Status)
		require.Len(t, obj.Units, 1)
		require.Contains(t, obj.Units[0].SQL, "CREATE CONTEXT INDEX")
		require.Contains(t, obj.Units[0].SQL, "SYNC TRANSACTION")
		require.Equal(t, "succeeded", obj.Units[0].Status)
	}
	require.True(t, found)

	assertMatches := func(word string, want int) {
		t.Helper()
		var count int
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+targetTable+` WHERE CONTAINS("BODY", ?)`, word).Scan(&count))
		require.Equal(t, want, count, word)
	}
	assertMatches("initialtoken", 1)
	tx, err := dst.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "INSERT INTO "+targetTable+` ("ID", "BODY") VALUES (2, 'committedtoken')`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "UPDATE "+targetTable+` SET "BODY"='replacementtoken' WHERE "ID"=1`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	assertMatches("committedtoken", 1)
	assertMatches("replacementtoken", 1)
	assertMatches("initialtoken", 0)
}
