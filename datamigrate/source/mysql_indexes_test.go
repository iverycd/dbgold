package source

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const mysqlIndexesQuery = `SELECT TABLE_NAME, INDEX_NAME, COLUMN_NAME, NON_UNIQUE
		 FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = ? AND INDEX_NAME != 'PRIMARY'
		 ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`

func newIndexTestMySQLReader(t *testing.T, database string) (*MySQLReader, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		_ = db.Close()
	})
	return &MySQLReader{db: db, dbName: database}, mock
}

func TestMySQLGetIndexesPreservesColumnsNamesAndUniqueness(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "MixedSchema")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexesQuery)).WithArgs("MixedSchema").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "INDEX_NAME", "COLUMN_NAME", "NON_UNIQUE"}).
			AddRow("MixedTable", "IX_Composite", "SecondColumn", 1).
			AddRow("MixedTable", "IX_Composite", "FirstColumn", 1).
			AddRow("MixedTable", "IX_Plain", "CamelCase", 1).
			AddRow("MixedTable", "UX_Composite", "Code", 0).
			AddRow("MixedTable", "UX_Composite", "TenantID", 0).
			AddRow("OtherTable", "IX_Plain", "OtherColumn", 1)).RowsWillBeClosed()

	indexes, err := r.GetIndexes(context.Background())
	require.NoError(t, err)
	require.Equal(t, []IndexInfo{
		{TableName: "MixedTable", IndexName: "IX_Composite", Columns: []string{"SecondColumn", "FirstColumn"}},
		{TableName: "MixedTable", IndexName: "IX_Plain", Columns: []string{"CamelCase"}},
		{TableName: "MixedTable", IndexName: "UX_Composite", Columns: []string{"Code", "TenantID"}, IsUnique: true},
		{TableName: "OtherTable", IndexName: "IX_Plain", Columns: []string{"OtherColumn"}},
	}, indexes)
}

func TestMySQLGetIndexesExpressionDoesNotBlockOtherIndexes(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexesQuery)).WithArgs("Src").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "INDEX_NAME", "COLUMN_NAME", "NON_UNIQUE"}).
			AddRow("MixedTable", "IX_Before", "BeforeColumn", 1).
			AddRow("MixedTable", "IX_Expression", nil, 1).
			AddRow("MixedTable", "IX_Mixed", "TenantID", 0).
			AddRow("MixedTable", "IX_Mixed", nil, 0).
			AddRow("MixedTable", "IX_Mixed", "DisplayName", 0).
			AddRow("MixedTable", "IX_After", "AfterColumn", 1)).RowsWillBeClosed()

	indexes, err := r.GetIndexes(context.Background())
	require.NoError(t, err)
	require.Len(t, indexes, 4)
	require.Equal(t, IndexInfo{TableName: "MixedTable", IndexName: "IX_Before", Columns: []string{"BeforeColumn"}}, indexes[0])
	for _, idx := range indexes[1:3] {
		require.Equal(t, "MixedTable", idx.TableName)
		require.Contains(t, idx.Unsupported, "函数/表达式索引")
		require.Empty(t, idx.Columns, "an unsupported mixed index must not expose a usable partial column list")
	}
	require.Equal(t, "IX_Expression", indexes[1].IndexName)
	require.False(t, indexes[1].IsUnique)
	require.Equal(t, "IX_Mixed", indexes[2].IndexName)
	require.True(t, indexes[2].IsUnique)
	require.Equal(t, IndexInfo{TableName: "MixedTable", IndexName: "IX_After", Columns: []string{"AfterColumn"}}, indexes[3])
}

func TestMySQLGetIndexesKeepsDottedNamesDistinct(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexesQuery)).WithArgs("Src").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "INDEX_NAME", "COLUMN_NAME", "NON_UNIQUE"}).
			AddRow("A", "B.C", "First", 1).
			AddRow("A.B", "C", "Second", 0)).RowsWillBeClosed()

	indexes, err := r.GetIndexes(context.Background())
	require.NoError(t, err)
	require.Equal(t, []IndexInfo{
		{TableName: "A", IndexName: "B.C", Columns: []string{"First"}},
		{TableName: "A.B", IndexName: "C", Columns: []string{"Second"}, IsUnique: true},
	}, indexes)
}

func TestMySQLGetIndexesReturnsReadErrorsWithoutPartialIndexes(t *testing.T) {
	readErr := errors.New("metadata stream interrupted")
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexesQuery)).WithArgs("Src").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "INDEX_NAME", "COLUMN_NAME", "NON_UNIQUE"}).
			AddRow("MixedTable", "IX_Composite", "FirstColumn", 1).
			AddRow("MixedTable", "IX_Composite", "SecondColumn", 1).
			RowError(1, readErr)).RowsWillBeClosed()

	indexes, err := r.GetIndexes(context.Background())
	require.ErrorIs(t, err, readErr)
	require.Nil(t, indexes, "a truncated composite index must not be mistaken for a complete index")
}

func TestMySQLGetIndexesReturnsScanErrors(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexesQuery)).WithArgs("Src").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "INDEX_NAME", "COLUMN_NAME", "NON_UNIQUE"}).
			AddRow("MixedTable", "IX_Invalid", "ID", nil)).RowsWillBeClosed()

	indexes, err := r.GetIndexes(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "NON_UNIQUE")
	require.Nil(t, indexes)
}

func TestMySQLGetTableSourceDDLPreservesOriginalAndEscapesIdentifiers(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Mi`xed.Schema")
	query := "SHOW CREATE TABLE `Mi``xed.Schema`.`Ta``ble.Name`"
	ddl := "CREATE TABLE `Ta``ble.Name` (\n  `DisplayName` varchar(100) DEFAULT 'AbC',\n  KEY `IX_Expression` ((lower(`DisplayName`)))\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	mock.ExpectQuery(regexp.QuoteMeta(query)).
		WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("Ta`ble.Name", ddl)).RowsWillBeClosed()

	got, err := r.GetTableSourceDDL(context.Background(), "Ta`ble.Name")
	require.NoError(t, err)
	require.Equal(t, ddl, got)
}

func TestMySQLGetTableSourceDDLReturnsReadError(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	readErr := errors.New("SHOW CREATE permission denied")
	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `Src`.`MixedTable`")).WillReturnError(readErr)

	ddl, err := r.GetTableSourceDDL(context.Background(), "MixedTable")
	require.ErrorIs(t, err, readErr)
	require.Empty(t, ddl)
}

func TestMySQLGetTableSourceDDLReturnsMissingTable(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `Src`.`MissingTable`")).
		WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"})).RowsWillBeClosed()

	ddl, err := r.GetTableSourceDDL(context.Background(), "MissingTable")
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Empty(t, ddl)
}
