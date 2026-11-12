package source

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
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

const mysqlPGIndexesQuery = `SELECT TABLE_NAME, INDEX_NAME, COLUMN_NAME, NON_UNIQUE, SUB_PART, INDEX_TYPE, COLLATION, EXPRESSION
 FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND INDEX_NAME != 'PRIMARY'
 ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`

func mysqlPGIndexRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"TABLE_NAME", "INDEX_NAME", "COLUMN_NAME", "NON_UNIQUE", "SUB_PART", "INDEX_TYPE", "COLLATION", "EXPRESSION"})
}

func TestMySQLGetIndexesForPGRetainsOnlySingleUniqueBTREEExpression(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	const expr = "(case when (`OutCaseCode` <> _utf8mb4'') then concat(`Channel`,_utf8mb4':',`OutCaseCode`) end)"
	mock.ExpectQuery(regexp.QuoteMeta(mysqlPGIndexesQuery)).WithArgs("Src").
		WillReturnRows(mysqlPGIndexRows().
			AddRow("MixedTable", "Ascending", nil, 0, nil, "BTREE", "A", expr).
			AddRow("MixedTable", "ColumnThenExpression", "TenantID", 0, nil, "BTREE", "A", nil).
			AddRow("MixedTable", "ColumnThenExpression", nil, 0, nil, "BTREE", "A", expr).
			AddRow("MixedTable", "Descending", nil, 0, nil, "BTREE", "D", expr).
			AddRow("MixedTable", "Empty", nil, 0, nil, "BTREE", "A", "  ").
			AddRow("MixedTable", "ExpressionThenColumn", nil, 0, nil, "BTREE", "A", expr).
			AddRow("MixedTable", "ExpressionThenColumn", "TenantID", 0, nil, "BTREE", "A", nil).
			AddRow("MixedTable", "Hash", nil, 0, nil, "HASH", "A", expr).
			AddRow("MixedTable", "NonUnique", nil, 1, nil, "BTREE", "A", expr).
			AddRow("MixedTable", "Plain", "TenantID", 0, 8, "BTREE", "D", nil).
			AddRow("MixedTable", "Prefix", nil, 0, 8, "BTREE", "A", expr).
			AddRow("MixedTable", "UnknownDirection", nil, 0, nil, "BTREE", nil, expr)).RowsWillBeClosed()

	indexes, err := r.GetIndexesForPG(context.Background())
	require.NoError(t, err)
	require.Len(t, indexes, 10)
	require.Equal(t, "MixedTable", indexes[0].TableName)
	require.Equal(t, "Ascending", indexes[0].IndexName)
	require.True(t, indexes[0].IsUnique)
	require.Equal(t, expr, indexes[0].MySQLExpression)
	for _, index := range indexes {
		if index.IndexName == "Plain" {
			require.Equal(t, IndexInfo{TableName: "MixedTable", IndexName: "Plain", Columns: []string{"TenantID"}, IsUnique: true,
				Method: "BTREE", Descending: []bool{true}, MySQLPrefixLengths: []int{8}}, index)
			continue
		}
		if index.IndexName == "Descending" {
			require.Equal(t, expr, index.MySQLExpression)
		} else if index.IndexName != "Ascending" {
			require.Empty(t, index.MySQLExpression, index.IndexName)
		}
		require.Contains(t, index.Unsupported, "函数/表达式索引", index.IndexName)
		require.Empty(t, index.Columns, index.IndexName)
	}
}

func TestMySQLGetIndexesForPGFallbackOnlyForMissingExpressionColumn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		fallback bool
	}{
		{"old server", &mysql.MySQLError{Number: 1054, Message: "Unknown column 'EXPRESSION' in 'field list'"}, true},
		{"other column", &mysql.MySQLError{Number: 1054, Message: "Unknown column 'EXPRESSION_OTHER' in 'field list'"}, false},
		{"permission denied", &mysql.MySQLError{Number: 1142, Message: "SELECT command denied for column 'EXPRESSION'"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := newIndexTestMySQLReader(t, "Src")
			mock.ExpectQuery(regexp.QuoteMeta(mysqlPGIndexesQuery)).WithArgs("Src").WillReturnError(tc.err)
			if tc.fallback {
				mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexesQuery)).WithArgs("Src").
					WillReturnRows(mysqlIndexRows().
						AddRow("MixedTable", "IX_Fulltext", "Title", 1, nil, "FULLTEXT", nil).
						AddRow("MixedTable", "IX_Fulltext", "Body", 1, nil, "FULLTEXT", nil).
						AddRow("MixedTable", "IX_Plain", "CamelCase", 0, nil, "BTREE", "A")).RowsWillBeClosed()
			}
			indexes, err := r.GetIndexesForPG(context.Background())
			if tc.fallback {
				require.NoError(t, err)
				require.Equal(t, []IndexInfo{
					{TableName: "MixedTable", IndexName: "IX_Fulltext", Columns: []string{"Title", "Body"}, Method: "FULLTEXT"},
					{TableName: "MixedTable", IndexName: "IX_Plain", Columns: []string{"CamelCase"}, IsUnique: true, Method: "BTREE", Descending: []bool{false}},
				}, indexes)
			} else {
				require.ErrorIs(t, err, tc.err)
				require.Nil(t, indexes)
			}
		})
	}
}

func TestMySQLGetIndexesForPGRetainsFulltextKeys(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlPGIndexesQuery)).WithArgs("Src").
		WillReturnRows(mysqlPGIndexRows().
			AddRow("Frame_AttachStorage", "I_FRAME_ATTACHSTORAGE_CGCT", "CLIENGTAG", 1, nil, "FULLTEXT", nil, nil).
			AddRow("Frame_AttachStorage", "I_FRAME_ATTACHSTORAGE_CGCT", "CLIENGGUID", 1, nil, "FULLTEXT", nil, nil).
			AddRow("MixedTable", "Incomplete", "TextA", 1, nil, "FULLTEXT", nil, nil).
			AddRow("MixedTable", "Incomplete", nil, 1, nil, "FULLTEXT", nil, "lower(`Body`)").
			AddRow("MixedTable", "Incomplete", "TextZ", 1, nil, "FULLTEXT", nil, nil).
			AddRow("MixedTable", `Single"Key`, `Bod"y`, 1, nil, "Fulltext", nil, nil)).RowsWillBeClosed()

	indexes, err := r.GetIndexesForPG(context.Background())
	require.NoError(t, err)
	require.Len(t, indexes, 3)
	require.Equal(t, IndexInfo{TableName: "Frame_AttachStorage", IndexName: "I_FRAME_ATTACHSTORAGE_CGCT",
		Columns: []string{"CLIENGTAG", "CLIENGGUID"}, Method: "FULLTEXT"}, indexes[0])
	require.Contains(t, indexes[1].Unsupported, "函数/表达式索引")
	require.Empty(t, indexes[1].Columns)
	require.Equal(t, IndexInfo{TableName: "MixedTable", IndexName: `Single"Key`, Columns: []string{`Bod"y`}, Method: "Fulltext"}, indexes[2])
}

func TestMySQLGetIndexesGenericFulltextRemainsUnsupported(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexesQuery)).WithArgs("Src").
		WillReturnRows(mysqlIndexRows().AddRow("MixedTable", "Fulltext", "Body", 1, nil, "FULLTEXT", nil)).RowsWillBeClosed()

	indexes, err := r.GetIndexes(context.Background())
	require.NoError(t, err)
	require.Len(t, indexes, 1)
	require.Contains(t, indexes[0].Unsupported, "非 BTREE 索引")
	require.Empty(t, indexes[0].Columns)
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
