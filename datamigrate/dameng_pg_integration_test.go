package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// Requires precreated, empty DBGOLD_* schemas in isolated DM and PG instances.
// Optionally supply DAMENG_PG_SOURCE_SHADOW_SCHEMA to verify same-name tables
// across two DM schemas; mixed-case schema names also exercise identifier quoting.
func TestDamengPGIntegration(t *testing.T) {
	srcDSN, dstDSN := os.Getenv("DAMENG_PG_SOURCE_DSN"), os.Getenv("DAMENG_PG_TARGET_DSN")
	srcSchema, dstSchema := os.Getenv("DAMENG_PG_SOURCE_SCHEMA"), os.Getenv("DAMENG_PG_TARGET_SCHEMA")
	if os.Getenv("DAMENG_PG_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" || srcSchema == "" || dstSchema == "" {
		t.Skip("需要隔离达梦/PostgreSQL、DAMENG_PG_SOURCE_DSN / TARGET_DSN / SOURCE_SCHEMA / TARGET_SCHEMA 及 DAMENG_PG_ALLOW_DDL=1")
	}
	shadow := os.Getenv("DAMENG_PG_SOURCE_SHADOW_SCHEMA")
	for _, schema := range []string{srcSchema, dstSchema, shadow} {
		if schema != "" {
			require.True(t, strings.HasPrefix(strings.ToUpper(schema), "DBGOLD_"), "仅使用预建的独立 DBGOLD_* 测试 schema")
		}
	}
	require.NotEqual(t, srcSchema, shadow)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	src, err := sql.Open("dm", srcDSN)
	require.NoError(t, err)
	defer src.Close()
	dst, err := sql.Open("postgres", dstDSN)
	require.NoError(t, err)
	defer dst.Close()
	var count int
	for _, schema := range []string{srcSchema, shadow} {
		if schema != "" {
			require.NoError(t, src.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER = ?`, schema).Scan(&count))
			require.Zero(t, count, "源测试 schema 必须为空")
		}
	}
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_namespace WHERE nspname = $1`, dstSchema).Scan(&count))
	require.Equal(t, 1, count, "目标测试 schema 必须预先创建")
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = $1`, dstSchema).Scan(&count))
	require.Zero(t, count, "目标测试 schema 必须为空")

	name := fmt.Sprintf("DmPgCase%x", time.Now().UnixNano())
	srcTable := damengnative.Qualified(srcSchema, name)
	dstTable := damengnative.Qualified(dstSchema, strings.ToLower(name))
	var sourceCreated, shadowCreated bool
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_, _ = dst.ExecContext(cleanup, "DROP TABLE IF EXISTS "+dstTable+" CASCADE")
		if sourceCreated {
			_, _ = src.ExecContext(cleanup, "DROP TABLE "+srcTable)
		}
		if shadowCreated {
			_, _ = src.ExecContext(cleanup, "DROP TABLE "+damengnative.Qualified(shadow, name))
		}
	}()
	_, err = src.ExecContext(ctx, "CREATE TABLE "+srcTable+` (
 "Id" INT IDENTITY(5, 1) PRIMARY KEY,
 "todispingrid" INT DEFAULT FALSE,
 "text_value" VARCHAR(40) DEFAULT 'FALSE',
 "literal_null" VARCHAR(40) DEFAULT 'NULL',
 "amount" DECIMAL(38,10) DEFAULT -12345678901234567890.1234567890)`)
	require.NoError(t, err)
	sourceCreated = true
	if shadow != "" {
		_, err = src.ExecContext(ctx, "CREATE TABLE "+damengnative.Qualified(shadow, name)+` ("ShadowId" INT IDENTITY(100, 1))`)
		require.NoError(t, err)
		shadowCreated = true
	}
	_, err = src.ExecContext(ctx, "CREATE INDEX "+damengnative.Qualified(srcSchema, "Idx"+name)+" ON "+srcTable+` ("todispingrid")`)
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "INSERT INTO "+srcTable+` ("text_value") VALUES ('first')`)
	require.NoError(t, err)

	r, err := source.NewDaMeng(srcDSN, srcSchema, source.ConnPoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	defer r.Close()
	seqs, err := r.GetSequences(ctx)
	require.NoError(t, err)
	require.Len(t, seqs, 1, "only the configured owner contributes identity columns")
	require.Equal(t, name, seqs[0].TableName)
	require.Equal(t, "Id", seqs[0].ColumnName)
	require.Empty(t, seqs[0].Unsupported)
	require.EqualValues(t, 6, seqs[0].StartValue)
	w, err := target.NewPostgres(dstDSN, dstSchema, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	job := &Job{LogCh: make(chan string, 2048)}
	report := NewMigrator(r, w, job, Config{
		TargetDBType: "postgres", TargetSchema: dstSchema, LowerCaseNames: true,
		Mode: "include", Filter: name, Content: "both", PageSize: 10, MaxParallel: 1, CaptureRepair: true,
	}).Run(ctx)
	require.False(t, report.HasFailures(), "%+v", report)
	for _, category := range []CategoryReport{report.Tables, report.Data, report.PrimaryKeys, report.Indexes, report.Sequences} {
		require.Equal(t, 1, category.Success, "%+v", category)
	}
	var flag int
	var textValue, literalNull, amount string
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT todispingrid,text_value,literal_null,amount::text FROM "+dstTable+" WHERE id=5").Scan(&flag, &textValue, &literalNull, &amount))
	require.Zero(t, flag)
	require.Equal(t, "first", textValue)
	require.Equal(t, "NULL", literalNull)
	require.Equal(t, "-12345678901234567890.1234567890", amount)
	var next int64
	require.NoError(t, dst.QueryRowContext(ctx, "INSERT INTO "+dstTable+" DEFAULT VALUES RETURNING id,todispingrid,text_value,literal_null,amount::text").Scan(&next, &flag, &textValue, &literalNull, &amount))
	require.EqualValues(t, 6, next)
	require.Zero(t, flag)
	require.Equal(t, "FALSE", textValue)
	require.Equal(t, "NULL", literalNull)
	require.Equal(t, "-12345678901234567890.1234567890", amount)
}
