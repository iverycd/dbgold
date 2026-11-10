package source

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"math"
	"regexp"
	"testing"
	"time"

	"dbgold/datamigrate/failure"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// Owner and object type are part of identity: a same-name table in another
// schema (or a same-name non-table object) must not contribute identity columns.
const damengSequenceCatalogSQL = `SELECT b.TABLE_NAME, a.NAME AS COLUMN_NAME
 FROM ALL_TABLES b
 JOIN ALL_OBJECTS c ON c.OWNER = b.OWNER AND c.OBJECT_NAME = b.TABLE_NAME AND c.OBJECT_TYPE = 'TABLE'
 JOIN SYS.SYSCOLUMNS a ON a.ID = c.OBJECT_ID
 WHERE b.OWNER = ? AND a.INFO2 & 0x01 = 0x01
 ORDER BY b.TABLE_NAME, a.COLID`

func damengSequenceReader(t *testing.T, owner string) (*DaMengReader, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		db.Close()
	})
	return &DaMengReader{db: db, schema: owner}, mock
}

func expectDamengSequenceCatalog(mock sqlmock.Sqlmock, owner string, rows *sqlmock.Rows) {
	mock.ExpectQuery(regexp.QuoteMeta(damengSequenceCatalogSQL)).WithArgs(owner).WillReturnRows(rows).RowsWillBeClosed()
}

func TestDamengSequencesPreserveAndQuoteNames(t *testing.T) {
	r, mock := damengSequenceReader(t, `MiXeD"Owner`)
	expectDamengSequenceCatalog(mock, r.schema, sqlmock.NewRows([]string{"table", "column"}).
		AddRow(`My.Table"'`, `Id"`).AddRow("EmptyTable", "ID").AddRow("Descending", "Id"))
	for _, tc := range []struct {
		qualified     string
		step, current int64
	}{
		{`"MiXeD""Owner"."My.Table""'"`, 5, 10},
		{`"MiXeD""Owner"."EmptyTable"`, 1, 1},
		{`"MiXeD""Owner"."Descending"`, -2, 10},
	} {
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT IDENT_INCR(?), IDENT_CURRENT(?)`)).WithArgs(tc.qualified, tc.qualified).
			WillReturnRows(sqlmock.NewRows([]string{"increment", "current"}).AddRow(tc.step, tc.current))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	seqs, err := r.GetSequences(ctx)
	require.NoError(t, err)
	require.Equal(t, []SequenceInfo{
		{TableName: `My.Table"'`, ColumnName: `Id"`, StartValue: 15},
		{TableName: "EmptyTable", ColumnName: "ID", StartValue: 2},
		{TableName: "Descending", ColumnName: "Id", StartValue: 8},
	}, seqs)
}

func TestDamengSequencesNoIdentityColumns(t *testing.T) {
	r, mock := damengSequenceReader(t, "App")
	expectDamengSequenceCatalog(mock, "App", sqlmock.NewRows([]string{"table", "column"}))
	seqs, err := r.GetSequences(context.Background())
	require.NoError(t, err)
	require.Empty(t, seqs)
}

func TestDamengSequencesObjectFailureDoesNotBlockHealthyTables(t *testing.T) {
	for _, tc := range []struct {
		name               string
		increment, current interface{}
		err                error
	}{
		{name: "invalid table", err: errors.New("Error -7019: 无效的表名")},
		{name: "missing row", err: sql.ErrNoRows},
		{name: "null increment", current: 10},
		{name: "null current", increment: 1},
		{name: "zero increment", increment: 0, current: 10},
		{name: "positive overflow", increment: 1, current: int64(math.MaxInt64)},
		{name: "negative overflow", increment: -1, current: int64(math.MinInt64)},
		{name: "unrepresentable number", increment: 1, current: "9223372036854775808"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := damengSequenceReader(t, "App")
			expectDamengSequenceCatalog(mock, "App", sqlmock.NewRows([]string{"table", "column"}).AddRow("Bad", "Id").AddRow("Good", "Id"))
			bad := mock.ExpectQuery(regexp.QuoteMeta(`SELECT IDENT_INCR(?), IDENT_CURRENT(?)`)).WithArgs(`"App"."Bad"`, `"App"."Bad"`)
			if tc.err != nil {
				bad.WillReturnError(tc.err)
			} else {
				bad.WillReturnRows(sqlmock.NewRows([]string{"increment", "current"}).AddRow(tc.increment, tc.current))
			}
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT IDENT_INCR(?), IDENT_CURRENT(?)`)).WithArgs(`"App"."Good"`, `"App"."Good"`).
				WillReturnRows(sqlmock.NewRows([]string{"increment", "current"}).AddRow(1, 20))
			seqs, err := r.GetSequences(context.Background())
			require.NoError(t, err)
			require.Len(t, seqs, 2)
			require.Equal(t, "metadata", seqs[0].FailureStage)
			require.Contains(t, seqs[0].Unsupported, `"App"."Bad"."Id"`)
			require.Zero(t, seqs[0].StartValue, "never invent a start for an unreadable identity")
			require.Empty(t, seqs[1].Unsupported)
			require.EqualValues(t, 21, seqs[1].StartValue)
		})
	}
}

func TestDamengSequencesTaskFailurePreservesCause(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF} {
		t.Run(cause.Error(), func(t *testing.T) {
			r, mock := damengSequenceReader(t, "App")
			expectDamengSequenceCatalog(mock, "App", sqlmock.NewRows([]string{"table", "column"}).AddRow("Bad", "Id").AddRow("Good", "Id"))
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT IDENT_INCR(?), IDENT_CURRENT(?)`)).WithArgs(`"App"."Bad"`, `"App"."Bad"`).WillReturnError(cause)
			seqs, err := r.GetSequences(context.Background())
			require.ErrorIs(t, err, cause)
			require.True(t, failure.IsTaskError(err))
			require.Nil(t, seqs)
		})
	}
}

func TestDamengSequencesCatalogFailuresReturnError(t *testing.T) {
	for _, mode := range []string{"query", "scan", "rows"} {
		t.Run(mode, func(t *testing.T) {
			r, mock := damengSequenceReader(t, "App")
			cause := errors.New("catalog unavailable")
			query := mock.ExpectQuery(regexp.QuoteMeta(damengSequenceCatalogSQL)).WithArgs("App")
			switch mode {
			case "query":
				query.WillReturnError(cause)
			case "scan":
				query.WillReturnRows(sqlmock.NewRows([]string{"table", "column"}).AddRow(nil, "Id")).RowsWillBeClosed()
			case "rows":
				query.WillReturnRows(sqlmock.NewRows([]string{"table", "column"}).AddRow("Bad", "Id").RowError(0, cause)).RowsWillBeClosed()
			}
			seqs, err := r.GetSequences(context.Background())
			require.Error(t, err)
			require.Nil(t, seqs)
		})
	}
}
