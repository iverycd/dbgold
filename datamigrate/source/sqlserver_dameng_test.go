package source

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"regexp"
	"testing"
)

func dmSQLReader(t *testing.T) (*SQLServerDamengReader, sqlmock.Sqlmock) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); db.Close() })
	return &SQLServerDamengReader{&SQLServerReader{db: db}}, m
}
func TestDamengSQLServerScope(t *testing.T) {
	r, m := dmSQLReader(t)
	ctx := context.Background()
	m.ExpectQuery("SELECT t.name FROM sys.tables.*s.name = 'dbo'").WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("MixedCase"))
	tables, e := r.ListTables(ctx)
	require.NoError(t, e)
	require.Equal(t, []string{"MixedCase"}, tables)
	m.ExpectQuery("SELECT v.name,m.definition.*v.schema_id=SCHEMA_ID\\('dbo'\\)").WillReturnRows(sqlmock.NewRows([]string{"name", "definition"}).AddRow("MixedView", "CREATE VIEW dbo.MixedView AS SELECT N'AbC' AS X").AddRow("Encrypted", nil))
	views, e := r.GetViews(ctx)
	require.NoError(t, e)
	require.Contains(t, views[0].Definition, "'AbC'")
	require.Empty(t, views[1].Definition)
}
func TestDamengSQLServerReadPageEscapesAndPreserves(t *testing.T) {
	r, m := dmSQLReader(t)
	query := "SELECT * FROM [dbo].[T]]name] ORDER BY [Id]]Key] OFFSET @p1 ROWS FETCH NEXT @p2 ROWS ONLY"
	columns := []*sqlmock.Column{sqlmock.NewColumn("Id]Key").OfType("INT", int64(0)), sqlmock.NewColumn("Text").OfType("NVARCHAR", ""), sqlmock.NewColumn("Version").OfType("TIMESTAMP", []byte{})}
	m.ExpectQuery(regexp.QuoteMeta(query)).WithArgs(int64(0), int64(10)).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(columns...).AddRow(1, "AbC\x00中文", []byte{0, 0xff}))
	cols, types, rows, e := r.ReadPage(context.Background(), "T]name", []string{"Id]Key"}, 0, 10)
	require.NoError(t, e)
	require.Equal(t, "Id]Key", cols[0])
	require.Equal(t, "TIMESTAMP", types[2])
	require.Equal(t, "AbC\x00中文", rows[0][1])
	require.Equal(t, []byte{0, 0xff}, rows[0][2])
	m.ExpectQuery(regexp.QuoteMeta("SELECT COUNT_BIG(*) FROM [dbo].[T]]name]")).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	_, e = r.CountRows(context.Background(), "T]name")
	require.NoError(t, e)
}
func TestDamengSQLServerColumns(t *testing.T) {
	r, m := dmSQLReader(t)
	m.ExpectQuery("SELECT c.name, ty.name.*t.schema_id=SCHEMA_ID\\('dbo'\\).*t.name=@p1").WithArgs("T").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "len", "prec", "scale", "nullable", "default", "computed", "udt", "seed", "step", "last"}).AddRow("Id", "int", 4, 10, 0, false, nil, false, false, 10, 5, 50).AddRow("Text", "nvarchar", 50, 0, 0, true, "(N'中文')", false, false, nil, nil, nil).AddRow("At", "datetime2", 8, 27, 6, true, nil, false, false, nil, nil, nil))
	info, e := r.GetTableDDLInfo(context.Background(), "T")
	require.NoError(t, e)
	require.Equal(t, int64(10), info.Columns[0].IdentityInfo.Seed)
	require.Equal(t, int64(5), info.Columns[0].IdentityInfo.Increment)
	require.Equal(t, int64(50), *info.Columns[0].IdentityInfo.Last)
	require.Equal(t, int64(6), info.Columns[2].DateTimePrecision)
}
func TestDamengSQLServerRejectsSpecialIndexMetadata(t *testing.T) {
	r, m := dmSQLReader(t)
	m.ExpectQuery("SELECT t.name,i.name.*t.schema_id=SCHEMA_ID\\('dbo'\\)").WillReturnRows(sqlmock.NewRows([]string{"table", "index", "unique", "type", "filter", "disabled", "hypothetical", "column", "included", "descending"}).AddRow("T", "IX", false, 2, false, false, false, "A", false, false).AddRow("T", "IX", false, 2, false, false, false, "B", true, false))
	indexes, e := r.GetIndexes(context.Background())
	require.NoError(t, e)
	require.Equal(t, []string{"A"}, indexes[0].Columns)
	require.NotEmpty(t, indexes[0].Unsupported)
}

var _ Reader = (*SQLServerDamengReader)(nil)

func TestDamengSQLServerLargeUnselectedIdentityDoesNotBreakMetadata(t *testing.T) {
	r, m := dmSQLReader(t)
	m.ExpectQuery("SELECT t.name,c.name,CONVERT\\(varchar").WillReturnRows(sqlmock.NewRows([]string{"table", "column", "seed", "step", "last"}).AddRow("Normal", "Id", "10", "5", "50").AddRow("TooLarge", "Id", "999999999999999999999999", "1", nil))
	seqs, e := r.GetSequences(context.Background())
	require.NoError(t, e)
	require.Empty(t, seqs[0].Unsupported)
	require.Equal(t, int64(50), *seqs[0].IdentityInfo.Last)
	require.NotEmpty(t, seqs[1].Unsupported)
}
