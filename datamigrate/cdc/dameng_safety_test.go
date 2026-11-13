package cdc

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestDamengDDLCompareAndSetRejectsStaleCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		pos  int
		hash string
	}{
		{"advanced", 30, "old"}, {"changed schema", 10, "different"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newSequenceMock(t)
			a := &DamengApplier{db: db, schema: "APP", jobID: "job"}
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT BINLOG_FILE,BINLOG_POSITION,GTID,SOURCE_SCHEMA_HASH,BOOTSTRAP_STATE FROM "APP"."__dbgold_cdc_checkpoint" WHERE JOB_ID=? FOR UPDATE`).WithArgs("job").WillReturnRows(sqlmock.NewRows([]string{"file", "pos", "gtid", "hash", "state"}).AddRow("bin.000001", tc.pos, "", tc.hash, "completed"))
			mock.ExpectRollback()
			err := a.acknowledgeDDL(context.Background(), BootstrapRecord{Position: Position{File: "bin.000001", Pos: 10}, SourceSchema: SourceSchemaSnapshot{Hash: "old"}}, BootstrapRecord{Position: Position{File: "bin.000001", Pos: 20}})
			require.ErrorContains(t, err, "已变化")
		})
	}
}

type damengTestValueConverter struct{}

func (damengTestValueConverter) ConvertValue(v any) (driver.Value, error) { return v, nil }
func newDamengLOBMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual), sqlmock.ValueConverterOption(damengTestValueConverter{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mock.ExpectationsWereMet()); _ = db.Close() })
	return db, mock
}
func TestDamengDDLCheckpointAndSchemaRollbackTogether(t *testing.T) {
	db, mock := newDamengLOBMock(t)
	a := &DamengApplier{db: db, schema: "APP", jobID: "job"}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT BINLOG_FILE,BINLOG_POSITION,GTID,SOURCE_SCHEMA_HASH,BOOTSTRAP_STATE FROM "APP"."__dbgold_cdc_checkpoint" WHERE JOB_ID=? FOR UPDATE`).WithArgs("job").WillReturnRows(sqlmock.NewRows([]string{"file", "pos", "gtid", "hash", "state"}).AddRow("bin.000001", 10, "", "old", "completed"))
	mock.ExpectExec(`UPDATE "APP"."__dbgold_cdc_checkpoint" SET BINLOG_FILE=?,BINLOG_POSITION=?,GTID=?,SOURCE_SCHEMA_VERSION=?,SOURCE_SCHEMA=?,SOURCE_SCHEMA_HASH=?,LOCATOR_STRATEGIES=?,ACK_DDL_FILE=?,ACK_DDL_POSITION=?,ACK_DDL_GTID=?,UPDATED_AT=CURRENT_TIMESTAMP WHERE JOB_ID=?`).WillReturnError(errors.New("disk full"))
	mock.ExpectRollback()
	err := a.acknowledgeDDL(context.Background(), BootstrapRecord{Position: Position{File: "bin.000001", Pos: 10}, SourceSchema: SourceSchemaSnapshot{Hash: "old"}}, BootstrapRecord{Position: Position{File: "bin.000001", Pos: 20}, SourceSchema: SourceSchemaSnapshot{Version: 1, Hash: "new"}})
	require.ErrorContains(t, err, "disk full")
}
func TestDamengMetadataQueriesAreScopedAndBounded(t *testing.T) {
	for _, size := range []int{100, 1000, 3000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			db, mock := newSequenceMock(t)
			cfg := Config{TargetDBType: "dameng", TargetSchema: "APP"}
			// Regardless of schema size, only the two touched names are sent to every
			// catalog query. No unfiltered catalog read is accepted by this mock.
			names := []string{"TABLE_0", fmt.Sprintf("TABLE_%d", size-1)}
			args := []driver.Value{"APP", names[0], names[1]}
			for _, q := range []struct {
				sql  string
				cols []string
			}{
				{damengBusinessTriggersSQL, []string{"TABLE_NAME", "TRIGGER_NAME", "STATUS"}},
				{loadDamengTargetTablesSQL, []string{"TABLE_NAME"}},
				{loadDamengTargetColumnsSQL, []string{"TABLE_NAME", "COLUMN_NAME", "NULLABLE", "DATA_DEFAULT", "DATA_TYPE", "IDENTITY"}},
				{loadDamengTargetIndexesSQL, []string{"OWNER", "INDEX_NAME", "TABLE_OWNER", "TABLE_NAME", "UNIQUENESS", "STATUS", "INDEX_TYPE"}},
				{loadDamengTargetIndexColumnsSQL, []string{"INDEX_OWNER", "INDEX_NAME", "TABLE_OWNER", "TABLE_NAME", "COLUMN_NAME", "COLUMN_POSITION"}},
			} {
				query, err := scopeDamengMetadataQuery(q.sql, names)
				require.NoError(t, err)
				require.Contains(t, query, " IN (?,?)")
				mock.ExpectQuery(query).WithArgs(args...).WillReturnRows(sqlmock.NewRows(q.cols))
			}
			_, err := loadDamengTransactionMetadata(context.Background(), db, cfg, []TableInfo{{Name: names[1]}, {Name: names[0]}})
			require.NoError(t, err)
		})
	}
	_, err := scopeDamengMetadataQuery(loadDamengTargetTablesSQL, make([]string, 129))
	require.Error(t, err)
	q, err := scopeDamengMetadataQuery(loadDamengTargetColumnsSQL, make([]string, 128))
	require.NoError(t, err)
	require.Equal(t, 129, strings.Count(q, "?"))
}
func TestDamengStatementsReuseAndCapacity(t *testing.T) {
	db, mock := newSequenceMock(t)
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	cache := newDamengStatements(tx)
	query := "UPDATE t SET value=? WHERE id=?"
	prepared := mock.ExpectPrepare(query).WillBeClosed()
	for i := 0; i < 20; i++ {
		prepared.ExpectExec().WithArgs(i, 1).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	for i := 0; i < 20; i++ {
		_, err = cache.ExecContext(context.Background(), query, i, 1)
		require.NoError(t, err)
	}
	for i := 1; i < damengStatementCacheLimit; i++ {
		q := fmt.Sprintf("INSERT INTO t%d VALUES (?)", i)
		mock.ExpectPrepare(q).WillBeClosed().ExpectExec().WithArgs(i).WillReturnResult(sqlmock.NewResult(0, 1))
		_, err = cache.ExecContext(context.Background(), q, i)
		require.NoError(t, err)
	}
	mock.ExpectExec("INSERT INTO overflow VALUES (?)").WithArgs(1).WillReturnResult(sqlmock.NewResult(0, 1))
	_, err = cache.ExecContext(context.Background(), "INSERT INTO overflow VALUES (?)", 1)
	require.NoError(t, err)
	require.Len(t, cache.statements, 256)
	require.NoError(t, cache.Close())
	require.Empty(t, cache.statements)
	mock.ExpectCommit()
	require.NoError(t, tx.Commit())
}

func TestDamengMetadataSplitsTouchedTablesInto128TableBatches(t *testing.T) {
	db, mock := newSequenceMock(t)
	cfg := Config{TargetDBType: "dameng", TargetSchema: "APP"}
	var tables []TableInfo
	for i := 0; i < 257; i++ {
		tables = append(tables, TableInfo{Name: fmt.Sprintf("T%03d", i)})
	}
	names := damengTargetNames(cfg, tables)
	for start := 0; start < len(names); start += 128 {
		batch := names[start:min(start+128, len(names))]
		args := []driver.Value{"APP"}
		for _, name := range batch {
			args = append(args, name)
		}
		for _, query := range []string{damengBusinessTriggersSQL, loadDamengTargetTablesSQL, loadDamengTargetColumnsSQL, loadDamengTargetIndexesSQL, loadDamengTargetIndexColumnsSQL} {
			scoped, err := scopeDamengMetadataQuery(query, batch)
			require.NoError(t, err)
			mock.ExpectQuery(scoped).WithArgs(args...).WillReturnRows(sqlmock.NewRows([]string{"unused"}))
		}
	}
	_, err := loadDamengTransactionMetadata(context.Background(), db, cfg, tables)
	require.NoError(t, err)
}
