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
		WillReturnRows(mysqlIndexRows().
			AddRow("MixedTable", "IX_Composite", "SecondColumn", 1, nil, "BTREE", "A").
			AddRow("MixedTable", "IX_Composite", "FirstColumn", 1, 8, "BTREE", "D").
			AddRow("MixedTable", "IX_Plain", "CamelCase", 1, nil, "BTREE", "A").
			AddRow("MixedTable", "UX_Composite", "Code", 0, 8, "BTREE", "A").
			AddRow("MixedTable", "UX_Composite", "TenantID", 0, 3, "BTREE", "A").
			AddRow("OtherTable", "IX_Plain", "OtherColumn", 1, nil, "BTREE", "A")).RowsWillBeClosed()

	indexes, err := r.GetIndexes(context.Background())
	require.NoError(t, err)
	require.Equal(t, []IndexInfo{
		{TableName: "MixedTable", IndexName: "IX_Composite", Columns: []string{"SecondColumn", "FirstColumn"}, Method: "BTREE", Descending: []bool{false, true}, MySQLPrefixLengths: []int{0, 8}},
		{TableName: "MixedTable", IndexName: "IX_Plain", Columns: []string{"CamelCase"}, Method: "BTREE", Descending: []bool{false}},
		{TableName: "MixedTable", IndexName: "UX_Composite", Columns: []string{"Code", "TenantID"}, Method: "BTREE", Descending: []bool{false, false}, IsUnique: true, MySQLPrefixLengths: []int{8, 3}},
		{TableName: "OtherTable", IndexName: "IX_Plain", Columns: []string{"OtherColumn"}, Method: "BTREE", Descending: []bool{false}},
	}, indexes)
}

func TestMySQLGetIndexesExpressionDoesNotBlockOtherIndexes(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexesQuery)).WithArgs("Src").
		WillReturnRows(mysqlIndexRows().
			AddRow("MixedTable", "IX_Before", "BeforeColumn", 1, nil, "BTREE", "A").
			AddRow("MixedTable", "IX_Expression", nil, 1, nil, "BTREE", "A").
			AddRow("MixedTable", "IX_Mixed", "TenantID", 0, nil, "BTREE", "A").
			AddRow("MixedTable", "IX_Mixed", nil, 0, nil, "BTREE", "A").
			AddRow("MixedTable", "IX_Mixed", "DisplayName", 0, nil, "BTREE", "A").
			AddRow("MixedTable", "IX_After", "AfterColumn", 1, nil, "BTREE", "A")).RowsWillBeClosed()

	indexes, err := r.GetIndexes(context.Background())
	require.NoError(t, err)
	require.Len(t, indexes, 4)
	require.Equal(t, IndexInfo{TableName: "MixedTable", IndexName: "IX_Before", Columns: []string{"BeforeColumn"}, Method: "BTREE", Descending: []bool{false}}, indexes[0])
	for _, idx := range indexes[1:3] {
		require.Equal(t, "MixedTable", idx.TableName)
		require.Contains(t, idx.Unsupported, "函数/表达式索引")
		require.Empty(t, idx.Columns, "an unsupported mixed index must not expose a usable partial column list")
	}
	require.Equal(t, "IX_Expression", indexes[1].IndexName)
	require.False(t, indexes[1].IsUnique)
	require.Equal(t, "IX_Mixed", indexes[2].IndexName)
	require.True(t, indexes[2].IsUnique)
	require.Equal(t, IndexInfo{TableName: "MixedTable", IndexName: "IX_After", Columns: []string{"AfterColumn"}, Method: "BTREE", Descending: []bool{false}}, indexes[3])
}

func TestMySQLGetIndexesKeepsDottedNamesDistinct(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexesQuery)).WithArgs("Src").
		WillReturnRows(mysqlIndexRows().
			AddRow("A", "B.C", "First", 1, nil, "BTREE", "A").
			AddRow("A.B", "C", "Second", 0, nil, "BTREE", "A")).RowsWillBeClosed()

	indexes, err := r.GetIndexes(context.Background())
	require.NoError(t, err)
	require.Equal(t, []IndexInfo{
		{TableName: "A", IndexName: "B.C", Columns: []string{"First"}, Method: "BTREE", Descending: []bool{false}},
		{TableName: "A.B", IndexName: "C", Columns: []string{"Second"}, Method: "BTREE", Descending: []bool{false}, IsUnique: true},
	}, indexes)
}

func TestMySQLGetIndexesReturnsReadErrorsWithoutPartialIndexes(t *testing.T) {
	readErr := errors.New("metadata stream interrupted")
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexesQuery)).WithArgs("Src").
		WillReturnRows(mysqlIndexRows().
			AddRow("MixedTable", "IX_Composite", "FirstColumn", 1, 8, "BTREE", "D").
			AddRow("MixedTable", "IX_Composite", "SecondColumn", 1, nil, "BTREE", "A").
			RowError(1, readErr)).RowsWillBeClosed()

	indexes, err := r.GetIndexes(context.Background())
	require.ErrorIs(t, err, readErr)
	require.Nil(t, indexes, "a truncated composite index must not be mistaken for a complete index")
}

func TestMySQLGetIndexesReturnsScanErrors(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexesQuery)).WithArgs("Src").
		WillReturnRows(mysqlIndexRows().
			AddRow("MixedTable", "IX_Invalid", "ID", nil, nil, "BTREE", "A")).RowsWillBeClosed()

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

func TestMySQLPartitionIndexesRetainSecondaryPrefixes(t *testing.T) {
	r, m := newIndexTestMySQLReader(t, "Src")
	m.ExpectQuery("SELECT INDEX_NAME,NON_UNIQUE,SEQ_IN_INDEX").WithArgs("Src", "Mixed").WillReturnRows(sqlmock.NewRows(
		[]string{"name", "nonunique", "seq", "col", "prefix", "method", "direction"}).
		AddRow("ByGuid", 0, 1, "PartitionID", nil, "BTREE", "A").
		AddRow("ByGuid", 0, 2, "RowGuid", 8, "BTREE", "D").
		AddRow("PRIMARY", 0, 1, "PartitionID", 4, "BTREE", "A").
		AddRow("Expr", 1, 1, nil, nil, "BTREE", nil))
	indexes, err := r.PartitionIndexes(context.Background(), "Mixed")
	require.NoError(t, err)
	require.Equal(t, IndexInfo{TableName: "Mixed", IndexName: "ByGuid", IsUnique: true, Method: "BTREE", Locality: "local",
		Columns: []string{"PartitionID", "RowGuid"}, Descending: []bool{false, true}, MySQLPrefixLengths: []int{0, 8}}, indexes[0])
	require.NotEmpty(t, indexes[1].Unsupported)
	require.NotEmpty(t, indexes[2].Unsupported)
}
