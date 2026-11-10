package target

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestPGUniqueConstraintDuplicateVerification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     pq.ErrorCode
		rows      *sqlmock.Rows
		queryErr  error
		wantError string
	}{
		{name: "new constraint"},
		{name: "matching constraint with duplicate relation state", state: "42P07", rows: pgUniqueConstraintRows().AddRow("Channel", true).AddRow("OutcaseCode", true)},
		{name: "matching constraint with duplicate object state", state: "42710", rows: pgUniqueConstraintRows().AddRow("Channel", true).AddRow("OutcaseCode", true)},
		{name: "plain index or other table", state: "42P07", rows: pgUniqueConstraintRows(), wantError: "目标表不存在该名称的唯一约束"},
		{name: "wrong key order", state: "42710", rows: pgUniqueConstraintRows().AddRow("OutcaseCode", true).AddRow("Channel", true), wantError: "键列或顺序"},
		{name: "wrong enforcement", state: "42710", rows: pgUniqueConstraintRows().AddRow("Channel", false), wantError: "检查时机或 NULL 语义"},
		{name: "catalog unavailable", state: "42710", queryErr: errors.New("catalog unavailable"), wantError: "catalog unavailable"},
		{name: "duplicate data", state: "23505", wantError: "create failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			db.SetMaxOpenConns(1)
			idx := pgUniqueConstraintFixture()
			d := dialect.NewPostgres("postgres")
			ddl := `ALTER TABLE "App"."Orders" ADD CONSTRAINT "Orders_UK_Channel" UNIQUE ("Channel", "OutcaseCode");`
			exec := mock.ExpectExec(regexp.QuoteMeta(ddl))
			if tc.state == "" {
				exec.WillReturnResult(sqlmock.NewResult(0, 0))
			} else {
				exec.WillReturnError(&pq.Error{Code: tc.state, Message: "create failed"})
			}
			if tc.rows != nil || tc.queryErr != nil {
				query := mock.ExpectQuery(regexp.QuoteMeta(pgUniqueConstraintSQL)).WithArgs("App", "Orders", "Orders_UK_Channel")
				if tc.queryErr != nil {
					query.WillReturnError(tc.queryErr)
				} else {
					query.WillReturnRows(tc.rows).RowsWillBeClosed()
				}
			}
			err = createPGIndex(context.Background(), db, d, "App", idx)
			if tc.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantError)
				var pgErr *pq.Error
				require.ErrorAs(t, err, &pgErr)
				require.Equal(t, tc.state, pgErr.Code)
			}
			require.Equal(t, tc.state == "23505", IsPGUniqueConstraintViolation(err))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestPGUniqueConstraintRepairInspection(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	idx := pgUniqueConstraintFixture()
	obj := repair.Object{Schema: "App", Table: "Orders", TargetName: idx.TargetIndexName, Index: &idx}
	mock.ExpectQuery(regexp.QuoteMeta(pgUniqueConstraintSQL)).WithArgs("App", "Orders", "Orders_UK_Channel").
		WillReturnRows(pgUniqueConstraintRows().AddRow("Channel", true).AddRow("OutcaseCode", true)).RowsWillBeClosed()
	result, err := inspectRepairIndex(context.Background(), repairTarget{db: db, family: "postgres"}, obj)
	require.NoError(t, err)
	require.Equal(t, "succeeded", result.Status)

	obj.Units = []repair.Unit{{SQL: "changed", OriginalSQL: "planned"}}
	result, err = inspectRepairIndex(context.Background(), repairTarget{db: db, family: "postgres"}, obj)
	require.NoError(t, err)
	require.Equal(t, "unknown", result.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGUniqueConstraintViolationPreservesWrappedSQLState(t *testing.T) {
	err := fmt.Errorf("write constraint: %w", &pq.Error{Code: "23505"})
	require.True(t, IsPGUniqueConstraintViolation(err))
	require.False(t, IsPGUniqueConstraintViolation(errors.Join(err, errors.New("persisting outcome failed"))))
}

func pgUniqueConstraintFixture() source.IndexInfo {
	return source.IndexInfo{TableName: "Orders", IndexName: "UK_Channel", TargetIndexName: "Orders_UK_Channel",
		IsUnique: true, PGUniqueConstraint: true, Columns: []string{"Channel", "OutcaseCode"}}
}

func pgUniqueConstraintRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"name", "enforced"})
}
