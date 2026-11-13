package cdc

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func damengTestRow(t *testing.T, db *sql.DB, identity bool, full bool) *damengRow {
	t.Helper()
	cfg := Config{TargetDBType: "dameng", TargetSchema: "APP"}
	a := &DamengApplier{db: db, cfg: cfg, schema: "APP", jobID: "dm-unit"}
	table := &TableInfo{Name: "Items", Columns: []string{"Id", "Value"}, ColumnTypes: []string{"BIGINT", "VARCHAR"}, LocatorStrategy: LocatorPrimaryKey, LocatorColumns: []string{"Id"}}
	if full {
		table.LocatorStrategy = LocatorFullRow
		table.LocatorColumns = table.Columns
	}
	id := "NO"
	if identity {
		id = "YES"
	}
	r, err := newDamengRow(a, table, &targetTableMetadata{Columns: []targetColumnMetadata{{Name: "ID", DataType: "BIGINT", Identity: id}, {Name: "VALUE", DataType: "VARCHAR", Identity: "NO"}}})
	require.NoError(t, err)
	return r
}

func TestDamengUpsertDoesNotUpdateIdentity(t *testing.T) {
	db, mock := newSequenceMock(t)
	r := damengTestRow(t, db, true, false)
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	mock.ExpectExec(`UPDATE "APP"."ITEMS" SET "VALUE"=? WHERE "ID"=?`).WithArgs("changed", int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, r.apply(context.Background(), tx, Change{Action: "update", Before: []any{int64(7), "before"}, After: []any{int64(7), "changed"}}, func(string) error { t.Fatal("existing identity must not need explicit insert"); return nil }))
	require.NoError(t, tx.Commit())
}

func TestDamengChangedIdentityDeletesThenInserts(t *testing.T) {
	db, mock := newSequenceMock(t)
	r := damengTestRow(t, db, true, false)
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	mock.ExpectExec(`DELETE FROM "APP"."ITEMS" WHERE "ID"=?`).WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE "APP"."ITEMS" SET "VALUE"=? WHERE "ID"=?`).WithArgs("new", int64(9)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM "APP"."ITEMS" WHERE "ID"=?`).WithArgs(int64(9)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO "APP"."ITEMS" ("ID","VALUE") VALUES (?,?)`).WithArgs(int64(9), "new").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	var enabled string
	require.NoError(t, r.apply(context.Background(), tx, Change{Action: "update", Before: []any{int64(7), "old"}, After: []any{int64(9), "new"}}, func(table string) error { enabled = table; return nil }))
	require.Equal(t, `"APP"."ITEMS"`, enabled)
	require.NoError(t, tx.Commit())
}

func TestDamengFullRowOnlyChangesOneDuplicate(t *testing.T) {
	db, mock := newSequenceMock(t)
	r := damengTestRow(t, db, false, true)
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	mock.ExpectExec(`UPDATE "APP"."ITEMS" SET "ID"=?,"VALUE"=? WHERE ROWID=(SELECT ROWID FROM "APP"."ITEMS" WHERE "ID"=? AND "VALUE" IS NULL AND ROWNUM=1)`).WithArgs(int64(4), "new", int64(4)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, r.apply(context.Background(), tx, Change{Action: "update", Before: []any{int64(4), nil}, After: []any{int64(4), "new"}}, func(string) error { return nil }))
	require.NoError(t, tx.Commit())
}

func TestDamengFullRowConflictStopsWithoutInsert(t *testing.T) {
	db, mock := newSequenceMock(t)
	r := damengTestRow(t, db, false, true)
	r.table.ColumnTypes[1], r.columns[1].DataType = "INT", "INT"
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	mock.ExpectExec(`DELETE FROM "APP"."ITEMS" WHERE ROWID=(SELECT ROWID FROM "APP"."ITEMS" WHERE "ID"=? AND "VALUE"=? AND ROWNUM=1)`).WithArgs(int64(4), int64(99)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	err = r.apply(context.Background(), tx, Change{Action: "delete", Before: []any{int64(4), int64(99)}}, func(string) error { return nil })
	require.ErrorIs(t, err, ErrRowConflict)
	require.NoError(t, tx.Rollback())
}

func TestDamengFullRowLOBUsesCompleteCompare(t *testing.T) {
	db, _ := newSequenceMock(t)
	r := damengTestRow(t, db, false, true)
	r.columns[0].DataType = "BLOB"
	r.columns[1].DataType = "CLOB"
	where, args, err := r.predicate([]int{0, 1}, []any{[]byte{0, 1, 0, 255}, strings.Repeat("中文", 10000)})
	require.NoError(t, err)
	require.Equal(t, `DBMS_LOB.COMPARE("ID", CAST(? AS BLOB))=0 AND DBMS_LOB.COMPARE("VALUE", CAST(? AS CLOB))=0`, where)
	require.Len(t, args, 2)
	require.NotContains(t, where, "SUBSTR")
}

func TestDamengTriggerGuardFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, lock, status string
		wantErr            bool
	}{{"enabled", "0", "Y", true}, {"disabled", "0", "N", false}, {"unknown", "0", "UNKNOWN", true}, {"unsafe_lock", "1", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newSequenceMock(t)
			mock.ExpectQuery(damengTriggerLockSQL).WillReturnRows(sqlmock.NewRows([]string{"PARA_VALUE"}).AddRow(tc.lock))
			if tc.lock == "0" {
				mock.ExpectQuery(damengBusinessTriggersSQL).WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "TRIGGER_NAME", "STATUS"}).AddRow("ITEMS", "T_EFFECT", tc.status))
			}
			err := checkDamengBusinessTriggers(context.Background(), db, Config{TargetDBType: "dameng", TargetSchema: "app"}, []TableInfo{{Name: "Items"}})
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func scopedTestQuery(query string) string {
	scoped, err := scopeDamengMetadataQuery(query, []string{"ITEMS"})
	if err != nil {
		panic(err)
	}
	return scoped
}

func expectDamengApplyMetadata(mock sqlmock.Sqlmock, identity string) {
	mock.ExpectQuery(scopedTestQuery(damengBusinessTriggersSQL)).WithArgs("APP", "ITEMS").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "TRIGGER_NAME", "STATUS"}))
	mock.ExpectQuery(scopedTestQuery(loadDamengTargetTablesSQL)).WithArgs("APP", "ITEMS").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).AddRow("ITEMS"))
	mock.ExpectQuery(scopedTestQuery(loadDamengTargetColumnsSQL)).WithArgs("APP", "ITEMS").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "COLUMN_NAME", "NULLABLE", "DATA_DEFAULT", "DATA_TYPE", "IDENTITY"}).AddRow("ITEMS", "ID", "N", nil, "BIGINT", identity).AddRow("ITEMS", "VALUE", "Y", nil, "VARCHAR", "NO"))
	mock.ExpectQuery(scopedTestQuery(loadDamengTargetIndexesSQL)).WithArgs("APP", "ITEMS").WillReturnRows(sqlmock.NewRows([]string{"OWNER", "INDEX_NAME", "TABLE_OWNER", "TABLE_NAME", "UNIQUENESS", "STATUS", "INDEX_TYPE"}))
	mock.ExpectQuery(scopedTestQuery(loadDamengTargetIndexColumnsSQL)).WithArgs("APP", "ITEMS").WillReturnRows(sqlmock.NewRows([]string{"INDEX_OWNER", "INDEX_NAME", "TABLE_OWNER", "TABLE_NAME", "COLUMN_NAME", "COLUMN_POSITION"}))
}

func TestDamengApplyRollsBackAndClearsIdentityOnDMLFailure(t *testing.T) {
	db, mock := newSequenceMock(t)
	r := damengTestRow(t, db, true, false)
	mock.ExpectBegin()
	mock.ExpectQuery(damengTriggerLockSQL).WillReturnRows(sqlmock.NewRows([]string{"PARA_VALUE"}).AddRow("0"))
	mock.ExpectExec(`LOCK TABLE "APP"."ITEMS" IN ROW EXCLUSIVE MODE`).WillReturnResult(sqlmock.NewResult(0, 0))
	expectDamengApplyMetadata(mock, "YES")
	mock.ExpectPrepare(`UPDATE "APP"."ITEMS" SET "VALUE"=? WHERE "ID"=?`).WillBeClosed().ExpectExec().WithArgs("bad", int64(1)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectPrepare(`DELETE FROM "APP"."ITEMS" WHERE "ID"=?`).WillBeClosed().ExpectExec().WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`SET IDENTITY_INSERT "APP"."ITEMS" ON`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectPrepare(`INSERT INTO "APP"."ITEMS" ("ID","VALUE") VALUES (?,?)`).WillBeClosed().ExpectExec().WithArgs(int64(1), "bad").WillReturnError(errors.New("constraint failed"))
	mock.ExpectRollback()
	mock.ExpectExec(`SET IDENTITY_INSERT "APP"."ITEMS" OFF`).WillReturnResult(sqlmock.NewResult(0, 0))
	stats, err := r.applier.Apply(context.Background(), []Change{{Action: "insert", Table: r.table, After: []any{int64(1), "bad"}}}, Position{File: "bin.000001", Pos: 9})
	require.ErrorContains(t, err, "constraint failed")
	require.Equal(t, Stats{}, stats)
}

func TestDamengLegacyTriggerGuardUsesExclusiveLock(t *testing.T) {
	db, mock := newSequenceMock(t)
	mock.ExpectQuery(damengTriggerLockSQL).WillReturnRows(sqlmock.NewRows([]string{"PARA_VALUE"}))
	mode, err := damengCDCWriteLock(context.Background(), db)
	require.NoError(t, err)
	require.Equal(t, "EXCLUSIVE", mode)
}

func TestDamengApplyCheckpointFailureRollsBackRows(t *testing.T) {
	db, mock := newSequenceMock(t)
	r := damengTestRow(t, db, false, false)
	mock.ExpectBegin()
	mock.ExpectQuery(damengTriggerLockSQL).WillReturnRows(sqlmock.NewRows([]string{"PARA_VALUE"}).AddRow("0"))
	mock.ExpectExec(`LOCK TABLE "APP"."ITEMS" IN ROW EXCLUSIVE MODE`).WillReturnResult(sqlmock.NewResult(0, 0))
	expectDamengApplyMetadata(mock, "NO")
	mock.ExpectPrepare(`UPDATE "APP"."ITEMS" SET "VALUE"=? WHERE "ID"=?`).WillBeClosed().ExpectExec().WithArgs("ok", int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE "APP"."__DBGOLD_CDC_CHECKPOINT" SET BINLOG_FILE=?,BINLOG_POSITION=?,GTID=?,UPDATED_AT=CURRENT_TIMESTAMP WHERE JOB_ID=? AND BOOTSTRAP_STATE='completed'`).WithArgs("bin.000001", int64(99), nil, "dm-unit").WillReturnError(errors.New("checkpoint unavailable"))
	mock.ExpectRollback()
	stats, err := r.applier.Apply(context.Background(), []Change{{Action: "insert", Table: r.table, After: []any{int64(1), "ok"}}}, Position{File: "bin.000001", Pos: 99})
	require.ErrorContains(t, err, "checkpoint unavailable")
	require.Equal(t, Stats{}, stats)
}
