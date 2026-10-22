package target

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

// This gated suite creates one unique table only in an explicitly supplied
// empty test schema. Merely having a DSN never authorizes fixture DDL.
func TestDamengNativeIntegrationExactCopy(t *testing.T) {
	dsn, schema := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("实库未验收：需要 DAMENG_DM_TEST_TARGET_DSN / TARGET_SCHEMA 与 DAMENG_DM_TEST_ALLOW_DDL=1，仅用于独立空测试 schema")
	}
	w, err := NewDaMengNative(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var count int
	require.NoError(t, w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=?`, w.schema).Scan(&count))
	require.Zero(t, count, "显式指定的测试 schema 必须最初为空")
	name := fmt.Sprintf("DNATIVE%X", time.Now().UnixNano())
	qualified := damengnative.Qualified(w.schema, name)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_ = w.ExecuteDamengNative(cleanup, "DROP TABLE "+qualified+" CASCADE CONSTRAINTS")
	})
	require.NoError(t, w.ExecuteDamengNative(ctx, "CREATE TABLE "+qualified+` ("Id" BIGINT IDENTITY(1,1),"Amount" DECIMAL(38,19),"Mixed" VARCHAR(100),"Body" CLOB,"Payload" BLOB,"At" TIMESTAMP(9))`))
	table := damengnative.Table{Name: name, Schema: w.schema, Columns: []damengnative.Column{
		{Name: "Id", DataType: "BIGINT", SQLType: "BIGINT", Writable: true, Identity: true},
		{Name: "Amount", DataType: "DECIMAL", SQLType: "DECIMAL(38,19)", Precision: 38, Scale: 19, Writable: true},
		{Name: "Mixed", DataType: "VARCHAR", SQLType: "VARCHAR(100)", Writable: true},
		{Name: "Body", DataType: "CLOB", SQLType: "CLOB", Writable: true},
		{Name: "Payload", DataType: "BLOB", SQLType: "BLOB", Writable: true},
		{Name: "At", DataType: "TIMESTAMP", SQLType: "TIMESTAMP(9)", Writable: true},
	}}
	decimal := "-1234567890123456789.1234567890123456789"
	text := "AbC\x00中文😀"
	stamp := time.Date(2024, 2, 29, 12, 34, 56, 123456789, time.FixedZone("", 8*3600))
	require.NoError(t, w.CopyDamengNativeBatch(ctx, table, [][]any{{int64(42), decimal, text, "", []byte{}, stamp}, {int64(43), nil, nil, nil, nil, nil}}))
	var gotDecimal, gotText string
	var at time.Time
	var bodyNull, blobNull int
	require.NoError(t, w.db.QueryRowContext(ctx, `SELECT CAST("Amount" AS VARCHAR(256)),"Mixed",CASE WHEN "Body" IS NULL THEN 1 ELSE 0 END,CASE WHEN "Payload" IS NULL THEN 1 ELSE 0 END,"At" FROM `+qualified+` WHERE "Id"=42`).Scan(&gotDecimal, &gotText, &bodyNull, &blobNull, &at))
	require.Equal(t, decimal, gotDecimal)
	require.Equal(t, text, gotText)
	require.Zero(t, bodyNull)
	require.Zero(t, blobNull)
	require.Equal(t, 123456789, at.Nanosecond())
	require.NoError(t, w.db.QueryRowContext(ctx, `SELECT CASE WHEN "Body" IS NULL THEN 1 ELSE 0 END,CASE WHEN "Payload" IS NULL THEN 1 ELSE 0 END FROM `+qualified+` WHERE "Id"=43`).Scan(&bodyNull, &blobNull))
	require.Equal(t, 1, bodyNull)
	require.Equal(t, 1, blobNull)
	rows, err := w.CountRows(ctx, name)
	require.NoError(t, err)
	require.Equal(t, int64(2), rows)
}
