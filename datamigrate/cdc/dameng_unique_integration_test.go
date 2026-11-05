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

func TestDamengCompositeUniqueIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_CDC_DM_DSN")
	if dsn == "" && os.Getenv("CDC_DAMENG_INTEGRATION") == "1" {
		dsn = os.Getenv("CDC_DAMENG_DSN")
	}
	if dsn == "" {
		t.Skip("set DBGOLD_CDC_DM_DSN to a disposable DM instance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	schema := fmt.Sprintf("CDC_UNIQUE_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+quoteIdent(schema)+" AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + quoteIdent(schema) + " CASCADE"); require.NoError(t, err) })
	q := func(table string) string { return quoteIdent(schema) + "." + quoteIdent(table) }
	for _, ddl := range []string{
		"CREATE TABLE " + q("ITEMS") + ` ("TENANT" INT NOT NULL,"CODE" VARCHAR(40) NOT NULL,"PAYLOAD" VARCHAR(80),UNIQUE("TENANT","CODE"))`,
		"CREATE TABLE " + q("KEYSONLY") + ` ("TENANT" INT NOT NULL,"CODE" VARCHAR(40) NOT NULL,UNIQUE("TENANT","CODE"))`,
	} {
		_, err = db.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
	cfg := Config{TargetDBType: "dameng", TargetDSN: dsn, TargetSchema: strings.ToLower(schema), LowerCaseNames: true, JobID: "composite-unique"}
	sourceTables := []TableInfo{
		{Name: "Items", Columns: []string{"Tenant", "Code", "Payload"}, ColumnTypes: []string{"INT", "VARCHAR", "VARCHAR"}, UniqueIndexes: []UniqueIndexInfo{{Name: "source_pair", Columns: []string{"Tenant", "Code"}}}},
		{Name: "KeysOnly", Columns: []string{"Tenant", "Code"}, ColumnTypes: []string{"INT", "VARCHAR"}, UniqueIndexes: []UniqueIndexInfo{{Name: "source_keys", Columns: []string{"Tenant", "Code"}}}},
	}
	resolved, err := ResolveLocatorStrategies(ctx, cfg, sourceTables)
	require.NoError(t, err)
	for i := range resolved {
		require.Empty(t, resolved[i].PrimaryKey)
		require.Equal(t, LocatorUniqueKey, resolved[i].LocatorStrategy)
		require.Equal(t, []string{"Tenant", "Code"}, resolved[i].LocatorColumns)
		require.Equal(t, sourceTables[i].UniqueIndexes[0].Name, resolved[i].LocatorIndex)
	}
	// A target unique index alone, or a source key not present on the target,
	// must not qualify as the shared locator.
	for _, indexes := range [][]UniqueIndexInfo{nil, {{Name: "source_code_only", Columns: []string{"Code"}}}} {
		unmatched := sourceTables[0]
		unmatched.UniqueIndexes = indexes
		got, err := ResolveLocatorStrategies(ctx, cfg, []TableInfo{unmatched})
		require.NoError(t, err)
		require.Equal(t, LocatorFullRow, got[0].LocatorStrategy)
	}
	a, err := newDamengApplier(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	pos := uint32(4)
	apply := func(changes ...Change) {
		t.Helper()
		pos += 10
		position := Position{File: "bin.000001", Pos: pos}
		stats, err := a.Apply(ctx, changes, position)
		require.NoError(t, err)
		require.Equal(t, position, stats.Position)
		checkpoint, exists, err := a.LoadCheckpoint(ctx)
		require.NoError(t, err)
		require.True(t, exists)
		require.Equal(t, position, checkpoint)
	}
	type record struct {
		Tenant  int64
		Code    string
		Payload string
	}
	assertRows := func(table string, payload bool, want ...record) {
		t.Helper()
		columns := `"TENANT","CODE"`
		if payload {
			columns += `,"PAYLOAD"`
		}
		rows, err := db.QueryContext(ctx, "SELECT "+columns+" FROM "+q(table)+` ORDER BY "TENANT","CODE"`)
		require.NoError(t, err)
		got := make([]record, 0)
		for rows.Next() {
			var item record
			if payload {
				err = rows.Scan(&item.Tenant, &item.Code, &item.Payload)
			} else {
				err = rows.Scan(&item.Tenant, &item.Code)
			}
			require.NoError(t, err)
			got = append(got, item)
		}
		require.NoError(t, closeRows(rows))
		require.Equal(t, append([]record{}, want...), got)
	}
	items, keys := &resolved[0], &resolved[1]
	apply(
		Change{Action: "insert", Table: items, After: []any{int64(1), "same", "first"}},
		Change{Action: "insert", Table: items, After: []any{int64(2), "same", "other"}},
		Change{Action: "insert", Table: items, After: []any{int64(1), "different", "third"}},
	)
	assertRows("ITEMS", true, record{1, "different", "third"}, record{1, "same", "first"}, record{2, "same", "other"})
	apply(Change{Action: "insert", Table: items, After: []any{int64(1), "same", "duplicate update"}})
	assertRows("ITEMS", true, record{1, "different", "third"}, record{1, "same", "duplicate update"}, record{2, "same", "other"})
	apply(Change{Action: "update", Table: items, Before: []any{int64(1), "same", "duplicate update"}, After: []any{int64(1), "same", "普通更新 中文 "}})
	assertRows("ITEMS", true, record{1, "different", "third"}, record{1, "same", "普通更新 中文 "}, record{2, "same", "other"})
	apply(Change{Action: "update", Table: items, Before: []any{int64(1), "same", "普通更新 中文 "}, After: []any{int64(3), "moved", "普通更新 中文 "}})
	assertRows("ITEMS", true, record{1, "different", "third"}, record{2, "same", "other"}, record{3, "moved", "普通更新 中文 "})
	apply(Change{Action: "delete", Table: items, Before: []any{int64(2), "same", "other"}})
	assertRows("ITEMS", true, record{1, "different", "third"}, record{3, "moved", "普通更新 中文 "})

	// With only locator columns there is no UPDATE SET clause: both first
	// insertion and duplicate replay must use the existence-check branch.
	apply(Change{Action: "insert", Table: keys, After: []any{int64(1), "only"}})
	apply(Change{Action: "insert", Table: keys, After: []any{int64(1), "only"}})
	assertRows("KEYSONLY", false, record{1, "only", ""})
	apply(Change{Action: "update", Table: keys, Before: []any{int64(1), "only"}, After: []any{int64(1), "only"}})
	assertRows("KEYSONLY", false, record{1, "only", ""})
	apply(Change{Action: "update", Table: keys, Before: []any{int64(1), "only"}, After: []any{int64(2), "new"}})
	assertRows("KEYSONLY", false, record{2, "new", ""})
	apply(Change{Action: "delete", Table: keys, Before: []any{int64(2), "new"}})
	assertRows("KEYSONLY", false)
}
