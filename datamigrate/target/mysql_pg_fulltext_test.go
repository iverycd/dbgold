package target

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const fulltextDeparse = `to_tsvector('simple'::regconfig, ((COALESCE(("CliengTag")::text, ''::text) || ' '::text) || COALESCE("Clieng""GUID", ''::text)))`

func TestPGFulltextExpressionIdentity(t *testing.T) {
	columns := []string{"CliengTag", `Clieng"GUID`}
	require.True(t, matchesPGFulltextExpression(fulltextDeparse, columns))
	require.True(t, matchesPGFulltextExpression(
		`to_tsvector('simple'::regconfig, COALESCE(body, ''::text))`, []string{"body"}))
	for name, expr := range map[string]string{
		"changed config":     strings.Replace(fulltextDeparse, "'simple'", "'english'", 1),
		"shadowing function": "custom." + fulltextDeparse,
		"column casing":      strings.Replace(fulltextDeparse, "CliengTag", "cliengtag", 1),
		"column cast":        strings.Replace(fulltextDeparse, `("CliengTag")::text`, `("CliengTag")::citext`, 1),
		"separator":          strings.Replace(fulltextDeparse, "' '::text", "''::text", 1),
		"right association":  `to_tsvector('simple'::regconfig, COALESCE(("CliengTag")::text, ''::text) || (' '::text || COALESCE("Clieng""GUID", ''::text)))`,
		"fallback grouping":  `to_tsvector('simple'::regconfig, COALESCE(("CliengTag")::text, (''::text || ' '::text) || COALESCE("Clieng""GUID", ''::text)))`,
	} {
		t.Run(name, func(t *testing.T) { require.False(t, matchesPGFulltextExpression(expr, columns)) })
	}
	require.False(t, matchesPGFulltextExpression(fulltextDeparse, []string{columns[1], columns[0]}))
}

func TestPGFulltextDuplicateVerification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     pq.ErrorCode
		rows      *sqlmock.Rows
		wantError string
	}{
		{name: "create"},
		{name: "verified duplicate", state: "42P07", rows: pgFulltextRows().AddRow(fulltextDeparse, true)},
		{name: "wrong table", state: "42P07", rows: pgFulltextRows(), wantError: "目标表不存在该名称"},
		{name: "wrong properties", state: "42710", rows: pgFulltextRows().AddRow(fulltextDeparse, false), wantError: "与原计划不符"},
		{name: "wrong expression", state: "42P07", rows: pgFulltextRows().AddRow(strings.Replace(fulltextDeparse, "'simple'", "'english'", 1), true), wantError: "与原计划不符"},
		{name: "GIN unavailable", state: "0A000", wantError: "create failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			db.SetMaxOpenConns(1)
			idx := pgFulltextFixture()
			d := dialect.NewPostgres("postgres")
			ddl := dialect.JoinSQL(d.IndexStatements("App", idx))
			require.Contains(t, ddl, "USING gin")
			require.NotContains(t, ddl, "IF NOT EXISTS")
			exec := mock.ExpectExec(regexp.QuoteMeta(ddl))
			if tc.state == "" {
				exec.WillReturnResult(sqlmock.NewResult(0, 0))
			} else {
				exec.WillReturnError(&pq.Error{Code: tc.state, Message: "create failed"})
			}
			if tc.rows != nil {
				expectPGFulltextInspection(mock, tc.rows)
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
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestPGFulltextUnqualifiedDuplicateUsesVisibleTableSchema(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	idx := pgFulltextFixture()
	d := dialect.NewPostgres("postgres")
	ddl := dialect.JoinSQL(d.IndexStatements("", idx))
	mock.ExpectExec(regexp.QuoteMeta(ddl)).WillReturnError(&pq.Error{Code: "42P07", Message: "already exists"})
	mock.ExpectQuery("pg_catalog.pg_table_is_visible").WithArgs(idx.TableName).
		WillReturnRows(sqlmock.NewRows([]string{"nspname"}).AddRow("App"))
	expectPGFulltextInspection(mock, pgFulltextRows().AddRow(fulltextDeparse, true))
	require.NoError(t, createPGIndex(context.Background(), db, d, "", idx))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGFulltextGeneratedRepairReusesVerifiedDuplicate(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	idx := pgFulltextFixture()
	d := dialect.NewPostgres("postgres")
	w := &PostgresWriter{db: db, schema: "App", dbType: "postgres", dia: d}
	obj := repair.Object{Kind: "index", Schema: "App", Table: idx.TableName, TargetName: idx.TargetIndexName, Index: &idx}
	ddl := dialect.JoinSQL(d.IndexStatements(obj.Schema, idx))
	obj.Units = []repair.Unit{{ID: "index-ddl", SQL: ddl, OriginalSQL: ddl, Selected: true}}
	mock.ExpectExec(regexp.QuoteMeta(ddl)).WillReturnError(&pq.Error{Code: "42P07", Message: "already exists"})
	expectPGFulltextInspection(mock, pgFulltextRows().AddRow(fulltextDeparse, true))
	units, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", units[0].Status)

	expectPGFulltextInspection(mock, pgFulltextRows().AddRow(fulltextDeparse, true))
	v, err := InspectRepair(context.Background(), w, obj)
	require.NoError(t, err)
	require.Equal(t, "succeeded", v.Status)
	obj.Units[0].SQL = "changed"
	v, err = InspectRepair(context.Background(), w, obj)
	require.NoError(t, err)
	require.Equal(t, "unknown", v.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func pgFulltextFixture() source.IndexInfo {
	return source.IndexInfo{TableName: "Storage", IndexName: "IX_Storage", TargetIndexName: "Storage_IX",
		Method: "FULLTEXT", PGFulltext: true, Columns: []string{"CliengTag", `Clieng"GUID`}}
}

func pgFulltextRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"expression", "expected_properties"})
}

func expectPGFulltextInspection(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL search_path TO pg_catalog, pg_temp").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(pgFulltextIndexSQL)).
		WithArgs("App", "Storage", "Storage_IX").WillReturnRows(rows).RowsWillBeClosed()
	mock.ExpectRollback()
}
