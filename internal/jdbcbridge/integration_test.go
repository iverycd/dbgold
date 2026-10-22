package jdbcbridge

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOscarIntegrationConnectionAndSchemas(t *testing.T) {
	url := os.Getenv("OSCAR_TEST_URL")
	username := os.Getenv("OSCAR_TEST_USER")
	password := os.Getenv("OSCAR_TEST_PASSWORD")
	schema := os.Getenv("OSCAR_TEST_SCHEMA")
	if url == "" || username == "" || password == "" || schema == "" {
		t.Skip("OSCAR_TEST_URL/USER/PASSWORD/SCHEMA are not all set")
	}
	manager := NewManager(Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := manager.OpenSession(ctx, SessionOptions{
		URL: url, Username: username, Password: password, Schema: schema,
		MaxOpenConns: 2, MaxIdleConns: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	defer manager.CloseSession(context.Background(), session)
	if err := manager.Ping(ctx, session); err != nil {
		t.Fatal(err)
	}
	exists, err := manager.SchemaExists(ctx, session, schema)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("configured schema %q not visible through DatabaseMetaData", schema)
	}
}

// Uses a fresh table and only removes that table. No existing application table
// is read or modified when the vendor integration environment is configured.
func TestOscarIntegrationValidationCursor(t *testing.T) {
	url, username, password, schema := os.Getenv("OSCAR_TEST_URL"), os.Getenv("OSCAR_TEST_USER"), os.Getenv("OSCAR_TEST_PASSWORD"), os.Getenv("OSCAR_TEST_SCHEMA")
	if url == "" || username == "" || password == "" || schema == "" {
		t.Skip("OSCAR_TEST_URL/USER/PASSWORD/SCHEMA are not all set")
	}
	manager := NewManager(Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := manager.OpenSession(ctx, SessionOptions{URL: url, Username: username, Password: password, Schema: schema, MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	defer manager.CloseSession(context.Background(), session)
	table := fmt.Sprintf("DBGOLD_VALIDATE_%d", time.Now().UnixNano())
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	qualified := quote(schema) + "." + quote(table)
	if err = manager.Exec(ctx, session, "CREATE TABLE "+qualified+` ("Id" BIGINT PRIMARY KEY,"Amount" DECIMAL(38,19),"Text" VARCHAR(100))`); err != nil {
		t.Fatal(err)
	}
	defer manager.Exec(context.Background(), session, "DROP TABLE "+qualified)
	batch, err := manager.BeginBatch(ctx, session, "INSERT INTO "+qualified+` ("Id","Amount","Text") VALUES(?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.AddBatch(ctx, batch, [][]any{{int64(1), Decimal("1234567890123456789.1234567890123456789"), "校验尾空格 "}, {int64(2), Decimal("1.0000000000000000000"), "second"}}); err != nil {
		_ = manager.AbortBatch(context.Background(), batch)
		t.Fatal(err)
	}
	if err = manager.CommitBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	meta, err := manager.DescribeTable(ctx, session, schema, table)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Columns) != 3 || meta.Columns[1].Precision != 38 || meta.Columns[1].Scale != 19 || len(meta.PrimaryKey) != 1 || meta.PrimaryKey[0] != "Id" {
		t.Fatalf("unexpected validation metadata: %+v", meta)
	}
	cursor, err := manager.OpenCursor(ctx, session, `SELECT "Id","Amount","Text" FROM `+qualified+` ORDER BY "Id"`)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseCursor(context.Background(), cursor)
	first, done, err := manager.FetchCursor(ctx, cursor, 1, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if done || len(first) != 1 || first[0][1] != "1234567890123456789.1234567890123456789" || first[0][2] != "校验尾空格 " {
		t.Fatalf("first validation cursor page not exact: done=%t rows=%v", done, first)
	}
	second, _, err := manager.FetchCursor(ctx, cursor, 1, 1024)
	if err != nil || len(second) != 1 {
		t.Fatalf("second cursor page: rows=%v err=%v", second, err)
	}
	final, done, err := manager.FetchCursor(ctx, cursor, 1, 1024)
	if err != nil || !done || len(final) != 0 {
		t.Fatalf("cursor final page: done=%t rows=%v err=%v", done, final, err)
	}
}
