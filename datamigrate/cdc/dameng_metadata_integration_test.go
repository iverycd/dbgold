package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Only use a disposable DM instance: the test creates and drops its own schema.
func TestDamengMetadataIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_CDC_DM_DSN")
	if dsn == "" {
		dsn = os.Getenv("CDC_DAMENG_DSN")
	}
	if dsn == "" {
		t.Skip("set DBGOLD_CDC_DM_DSN to a disposable DM instance")
	}
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	schema := fmt.Sprintf("CDC_META_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema+" AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, e := db.ExecContext(cleanup, "DROP SCHEMA "+schema+" CASCADE")
		require.NoError(t, e)
	})
	for _, statement := range []string{
		"CREATE TABLE " + schema + ".ORDERS (ID BIGINT IDENTITY(1,1) PRIMARY KEY, TENANT INT NOT NULL, EMAIL VARCHAR(100) NOT NULL, BODY CLOB, FLAG INT DEFAULT 7)",
		"CREATE UNIQUE INDEX " + schema + ".UQ_EMAIL ON " + schema + ".ORDERS(EMAIL,TENANT)",
		"CREATE TABLE " + schema + `."Mixed" ("Case" INT)`,
	} {
		_, err = db.ExecContext(ctx, statement)
		require.NoError(t, err)
	}
	metadata, err := loadConfiguredTargetSchemaMetadata(ctx, db, Config{TargetDBType: "dameng", TargetSchema: strings.ToLower(schema)})
	require.NoError(t, err)
	require.Equal(t, "YES", metadata.Tables["ORDERS"].Columns[0].Identity)
	require.Equal(t, "NO", metadata.Tables["ORDERS"].Columns[0].Nullable)
	require.Equal(t, "CLOB", metadata.Tables["ORDERS"].Columns[3].DataType)
	require.True(t, metadata.Tables["ORDERS"].Columns[4].DefaultValue.Valid)
	require.Contains(t, metadata.Tables["ORDERS"].UniqueSets, []string{"ID"})
	require.Contains(t, metadata.Tables["ORDERS"].UniqueSets, []string{"EMAIL", "TENANT"})
	require.Contains(t, metadata.Tables["Mixed"].ColumnNames, "Case")
}

func TestDamengSourceMetadataIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_CDC_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set DBGOLD_CDC_MYSQL_DSN to a disposable MySQL instance")
	}
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	database := fmt.Sprintf("cdc_meta_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE DATABASE "+database)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, e := db.Exec("DROP DATABASE " + database)
		require.NoError(t, e)
	})
	_, err = db.ExecContext(ctx, "CREATE TABLE "+database+".`MixedTable` (Id BIGINT UNSIGNED PRIMARY KEY, Label ENUM('a,b','it''s','back\\\\slash') NOT NULL, Tags SET('中文','x'), GBK_TEXT VARCHAR(100) CHARACTER SET gbk, LATIN_TEXT VARCHAR(100) CHARACTER SET latin1)")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO "+database+".`MixedTable` VALUES(1,'a,b','x','中文','€é')")
	require.NoError(t, err)
	tables, err := LoadTables(ctx, db, database, "all", "")
	require.NoError(t, err)
	require.Len(t, tables, 1)
	require.Equal(t, "MixedTable", tables[0].Name)
	require.Equal(t, []string{"Id", "Label", "Tags", "GBK_TEXT", "LATIN_TEXT"}, tables[0].Columns)
	require.Equal(t, "gbk", tables[0].ColumnCharsets[3])
	require.Equal(t, "latin1", tables[0].ColumnCharsets[4])
	var rawGBK, rawLatin []byte
	var snapshotGBK, snapshotLatin string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT CAST(GBK_TEXT AS BINARY),CAST(LATIN_TEXT AS BINARY),GBK_TEXT,LATIN_TEXT FROM "+database+".`MixedTable`").Scan(&rawGBK, &rawLatin, &snapshotGBK, &snapshotLatin))
	got, err := convertDamengBinlogRow(&tables[0], []any{int64(-1), int64(3), int64(1), rawGBK, rawLatin})
	require.NoError(t, err)
	require.Equal(t, []any{"18446744073709551615", `back\slash`, "中文", snapshotGBK, snapshotLatin}, got)
}
