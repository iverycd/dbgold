package cdc

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This test creates and removes its own schema in an explicitly supplied,
// disposable DM instance. It exercises real LOB binds and session state.
func TestDamengApplierIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_CDC_DM_DSN")
	if dsn == "" && os.Getenv("CDC_DAMENG_INTEGRATION") == "1" {
		dsn = os.Getenv("CDC_DAMENG_DSN")
	}
	if dsn == "" {
		t.Skip("requires a disposable DM instance in DBGOLD_CDC_DM_DSN")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	schema := fmt.Sprintf("CDC_APPLY_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+quoteIdent(schema)+" AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	t.Cleanup(func() { _, e := db.Exec("DROP SCHEMA " + quoteIdent(schema) + " CASCADE"); require.NoError(t, e) })
	q := func(table string) string { return quoteIdent(schema) + "." + quoteIdent(table) }
	for _, table := range []string{"A", "B"} {
		_, err = db.ExecContext(ctx, "CREATE TABLE "+q(table)+` ("ID" BIGINT IDENTITY(1,1) PRIMARY KEY,"VALUE" VARCHAR(100) CHECK("VALUE"<>'bad'))`)
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx, "CREATE TABLE "+q("KEYLESS")+` ("N" INT,"TXT" CLOB,"RAW" BLOB,"AMOUNT" DECIMAL(30,6),"STAMP" TIMESTAMP(6))`)
	require.NoError(t, err)
	a, err := newDamengApplier(ctx, Config{TargetDBType: "dameng", TargetDSN: dsn, TargetSchema: schema, JobID: "integration"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	a.db.SetMaxOpenConns(1)
	info := func(name string) *TableInfo {
		return &TableInfo{Name: name, Columns: []string{"Id", "Value"}, ColumnTypes: []string{"BIGINT", "VARCHAR"}, PrimaryKey: []int{0}, AutoIncrement: []int{0}, LocatorStrategy: LocatorPrimaryKey, LocatorColumns: []string{"Id"}}
	}
	ta, tb := info("A"), info("B")
	position := func(n uint32) Position { return Position{File: "bin.000001", Pos: n} }
	stats, err := a.Apply(ctx, []Change{{Action: "insert", Table: ta, After: []any{int64(100), "a"}}, {Action: "insert", Table: tb, After: []any{int64(200), "b"}}}, position(100))
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.Inserts)
	_, err = a.Apply(ctx, []Change{{Action: "update", Table: ta, Before: []any{int64(100), "a"}, After: []any{int64(100), "changed"}}, {Action: "update", Table: tb, Before: []any{int64(200), "b"}, After: []any{int64(201), "changed"}}}, position(200))
	require.NoError(t, err)
	_, err = a.Apply(ctx, []Change{{Action: "insert", Table: ta, After: []any{int64(101), "ok"}}, {Action: "insert", Table: tb, After: []any{int64(202), "bad"}}}, position(300))
	require.Error(t, err)
	cp, exists, err := a.LoadCheckpoint(ctx)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, position(200), cp)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q("A")+` WHERE "ID"=101`).Scan(&count))
	require.Zero(t, count)
	_, err = a.db.ExecContext(ctx, "INSERT INTO "+q("B")+` ("ID","VALUE") VALUES(900,'explicit')`)
	require.Error(t, err, "identity state must be OFF on reused connection after rollback")
	_, err = a.db.ExecContext(ctx, "INSERT INTO "+q("A")+` ("VALUE") VALUES('generated')`)
	require.NoError(t, err)
	var generated int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT \"ID\" FROM "+q("A")+` WHERE "VALUE"='generated'`).Scan(&generated))
	require.Greater(t, generated, int64(100))
	require.NoError(t, a.SyncSequences(ctx, []TableInfo{*ta, *tb}))

	keyless := &TableInfo{Name: "KEYLESS", Columns: []string{"N", "Txt", "Raw", "Amount", "Stamp"}, ColumnTypes: []string{"INT", "LONGTEXT", "LONGBLOB", "DECIMAL", "DATETIME"}, LocatorStrategy: LocatorFullRow, LocatorColumns: []string{"N", "Txt", "Raw", "Amount", "Stamp"}}
	text := strings.Repeat("中文", 180000)
	binary := bytes.Repeat([]byte{0, 1, 255}, 350000)
	before := []any{int64(1), text, binary, "123456789012345678901234.123456", "2024-02-03 04:05:06.123456"}
	_, err = a.Apply(ctx, []Change{{Action: "insert", Table: keyless, After: before}, {Action: "insert", Table: keyless, After: before}}, position(400))
	require.NoError(t, err)
	after := append([]any(nil), before...)
	after[0] = int64(2)
	after[1] = text + "尾"
	_, err = a.Apply(ctx, []Change{{Action: "update", Table: keyless, Before: before, After: after}}, position(500))
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q("KEYLESS")+` WHERE "N"=1`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q("KEYLESS")+` WHERE "N"=2`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = a.Apply(ctx, []Change{{Action: "delete", Table: keyless, Before: before}}, position(600))
	require.NoError(t, err)
	_, err = a.Apply(ctx, []Change{{Action: "delete", Table: keyless, Before: before}}, position(700))
	require.ErrorIs(t, err, ErrRowConflict)
	cp, _, err = a.LoadCheckpoint(ctx)
	require.NoError(t, err)
	require.Equal(t, position(600), cp)
	nullRow := []any{nil, nil, nil, nil, nil}
	_, err = a.Apply(ctx, []Change{{Action: "insert", Table: keyless, After: nullRow}, {Action: "delete", Table: keyless, Before: nullRow}}, position(800))
	require.NoError(t, err)
	emptyRow := []any{int64(3), "", []byte{}, nil, nil}
	_, err = a.Apply(ctx, []Change{{Action: "insert", Table: keyless, After: emptyRow}, {Action: "delete", Table: keyless, Before: emptyRow}}, position(900))
	require.NoError(t, err)

	// Cancellation while waiting for a target lock must leave the old
	// checkpoint intact and release/discard the physical session safely.
	blocker, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = blocker.ExecContext(ctx, "LOCK TABLE "+q("A")+" IN EXCLUSIVE MODE")
	require.NoError(t, err)
	cancelCtx, cancelApply := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, e := a.Apply(cancelCtx, []Change{{Action: "insert", Table: ta, After: []any{int64(500), "cancelled"}}}, position(1000))
		done <- e
	}()
	time.Sleep(100 * time.Millisecond)
	cancelApply()
	require.NoError(t, blocker.Rollback())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-ctx.Done():
		t.Fatal("cancelled CDC transaction did not finish")
	}
	cp, _, err = a.LoadCheckpoint(ctx)
	require.NoError(t, err)
	require.Equal(t, position(900), cp)
	_, err = a.db.ExecContext(ctx, "INSERT INTO "+q("A")+` ("ID","VALUE") VALUES(501,'explicit')`)
	require.Error(t, err)
}

func TestDamengTriggerDDLInterlockIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_CDC_DM_DSN")
	if dsn == "" && os.Getenv("CDC_DAMENG_INTEGRATION") == "1" {
		dsn = os.Getenv("CDC_DAMENG_DSN")
	}
	if dsn == "" {
		t.Skip("requires a disposable DM instance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	schema := fmt.Sprintf("CDC_LOCK_%d", time.Now().UnixNano())
	q := func(name string) string { return quoteIdent(schema) + "." + quoteIdent(name) }
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+quoteIdent(schema)+" AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	t.Cleanup(func() { _, e := db.Exec("DROP SCHEMA " + quoteIdent(schema) + " CASCADE"); require.NoError(t, e) })
	_, err = db.ExecContext(ctx, "CREATE TABLE "+q("T")+" (ID INT)")
	require.NoError(t, err)
	triggerDDL := func(name string) string {
		return "CREATE TRIGGER " + q(name) + " BEFORE INSERT ON " + q("T") + " FOR EACH ROW BEGIN :NEW.ID := :NEW.ID + 1; END;"
	}
	_, err = db.ExecContext(ctx, triggerDDL("TRI"))
	require.NoError(t, err)
	mode, err := damengCDCWriteLock(ctx, db)
	require.NoError(t, err)
	for _, statement := range []string{"ALTER TRIGGER " + q("TRI") + " DISABLE", "ALTER TRIGGER " + q("TRI") + " ENABLE", triggerDDL("TRI2")} {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "LOCK TABLE "+q("T")+" IN "+mode+" MODE")
		require.NoError(t, err)
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() { close(started); _, e := db.ExecContext(ctx, statement); done <- e }()
		<-started
		select {
		case err := <-done:
			_ = tx.Rollback()
			t.Fatalf("trigger DDL did not wait for CDC table lock: %s: %v", statement, err)
		case <-time.After(300 * time.Millisecond):
		}
		require.NoError(t, tx.Commit())
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("trigger DDL did not resume after commit")
		}
	}
}
