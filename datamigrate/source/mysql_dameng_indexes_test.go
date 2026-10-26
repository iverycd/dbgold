package source

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const mysqlDamengIndexesQuery = `SELECT TABLE_NAME, INDEX_NAME, COLUMN_NAME, NON_UNIQUE, SUB_PART, INDEX_TYPE, COLLATION
 FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND INDEX_NAME != 'PRIMARY'
 ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`

func mysqlDamengIndexRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"TABLE_NAME", "INDEX_NAME", "COLUMN_NAME", "NON_UNIQUE", "SUB_PART", "INDEX_TYPE", "COLLATION"})
}

func TestMySQLDamengGetIndexesPreservesCompleteKeys(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "MixedSchema")
	r.damengTarget = true
	mock.ExpectQuery(regexp.QuoteMeta(mysqlDamengIndexesQuery)).WithArgs("MixedSchema").
		WillReturnRows(mysqlDamengIndexRows().
			AddRow("MixedTable", "ByName", "TenantID", 1, nil, "BTREE", "A").
			AddRow("MixedTable", "ByName", "DisplayName", 1, nil, "BTREE", "D").
			AddRow("MixedTable", "ByRow", "Row_ID", 1, nil, "BTREE", "A").
			AddRow("MixedTable", "UniqueRow", "Row_ID", 0, nil, "BTREE", "A").
			AddRow("OtherTable", "ByRow", "OtherID", 1, nil, "BTREE", "A").
			AddRow("A", "B.C", "First", 1, nil, "BTREE", "A").
			AddRow("A.B", "C", "Second", 0, nil, "BTREE", "A")).RowsWillBeClosed()

	indexes, err := r.GetIndexes(context.Background())
	require.NoError(t, err)
	require.Equal(t, []IndexInfo{
		{TableName: "MixedTable", IndexName: "ByName", Columns: []string{"TenantID", "DisplayName"}, Descending: []bool{false, true}, Method: "BTREE"},
		{TableName: "MixedTable", IndexName: "ByRow", Columns: []string{"Row_ID"}, Descending: []bool{false}, Method: "BTREE"},
		{TableName: "MixedTable", IndexName: "UniqueRow", Columns: []string{"Row_ID"}, IsUnique: true, Descending: []bool{false}, Method: "BTREE"},
		{TableName: "OtherTable", IndexName: "ByRow", Columns: []string{"OtherID"}, Descending: []bool{false}, Method: "BTREE"},
		{TableName: "A", IndexName: "B.C", Columns: []string{"First"}, Descending: []bool{false}, Method: "BTREE"},
		{TableName: "A.B", IndexName: "C", Columns: []string{"Second"}, IsUnique: true, Descending: []bool{false}, Method: "BTREE"},
	}, indexes)
}

func TestMySQLDamengGetIndexesIsolatesUnsupportedDefinitions(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	r.damengTarget = true
	mock.ExpectQuery(regexp.QuoteMeta(mysqlDamengIndexesQuery)).WithArgs("Src").
		WillReturnRows(mysqlDamengIndexRows().
			AddRow("MixedTable", "Before", "TenantID", 1, nil, "BTREE", "A").
			AddRow("MixedTable", "Expression", "TenantID", 0, nil, "BTREE", "A").
			AddRow("MixedTable", "Expression", nil, 0, nil, "BTREE", nil).
			AddRow("MixedTable", "Expression", "DisplayName", 0, nil, "BTREE", "D").
			AddRow("MixedTable", "Prefix", "TenantID", 0, nil, "BTREE", "A").
			AddRow("MixedTable", "Prefix", "DisplayName", 0, 10, "BTREE", "A").
			AddRow("MixedTable", "Fulltext", "DisplayName", 1, nil, "FULLTEXT", nil).
			AddRow("MixedTable", "Spatial", "Location", 1, nil, "SPATIAL", "A").
			AddRow("MixedTable", "Hash", "TenantID", 1, nil, "HASH", nil).
			AddRow("MixedTable", "UnknownDirection", "TenantID", 1, nil, "BTREE", nil).
			AddRow("MixedTable", "After", "DisplayName", 1, nil, "BTREE", "D")).RowsWillBeClosed()

	indexes, err := r.GetIndexes(context.Background())
	require.NoError(t, err)
	require.Len(t, indexes, 8)
	require.Empty(t, indexes[0].Unsupported)
	require.Equal(t, []string{"TenantID"}, indexes[0].Columns)
	for _, idx := range indexes[1:7] {
		require.Equal(t, "MixedTable", idx.TableName)
		require.NotEmpty(t, idx.Unsupported, idx.IndexName)
		require.Contains(t, idx.Unsupported, "手工适配")
		require.Empty(t, idx.Columns, "unsupported indexes must not expose partial usable keys")
		require.Empty(t, idx.Descending)
	}
	require.True(t, indexes[1].IsUnique)
	require.Contains(t, indexes[1].Unsupported, "函数/表达式")
	require.True(t, indexes[2].IsUnique)
	require.Contains(t, indexes[2].Unsupported, "前缀")
	require.Equal(t, "FULLTEXT", indexes[3].Method)
	require.Equal(t, "SPATIAL", indexes[4].Method)
	require.Contains(t, indexes[6].Unsupported, "排序方向未知")
	require.Empty(t, indexes[7].Unsupported)
	require.Equal(t, []string{"DisplayName"}, indexes[7].Columns)
	require.Equal(t, []bool{true}, indexes[7].Descending)
}

func TestMySQLDamengGetIndexesNeverReturnsPartialMetadata(t *testing.T) {
	for _, scenario := range []string{"query", "stream", "scan"} {
		t.Run(scenario, func(t *testing.T) {
			r, mock := newIndexTestMySQLReader(t, "Src")
			r.damengTarget = true
			failure := errors.New("index metadata interrupted")
			query := mock.ExpectQuery(regexp.QuoteMeta(mysqlDamengIndexesQuery)).WithArgs("Src")
			switch scenario {
			case "query":
				query.WillReturnError(failure)
			case "stream":
				query.WillReturnRows(mysqlDamengIndexRows().
					AddRow("MixedTable", "Composite", "First", 1, nil, "BTREE", "A").
					AddRow("MixedTable", "Composite", "Second", 1, nil, "BTREE", "D").
					RowError(1, failure)).RowsWillBeClosed()
			case "scan":
				query.WillReturnRows(mysqlDamengIndexRows().
					AddRow("MixedTable", "Before", "First", 1, nil, "BTREE", "A").
					AddRow("MixedTable", "Invalid", "Second", nil, nil, "BTREE", "D")).RowsWillBeClosed()
			}
			indexes, err := r.GetIndexes(context.Background())
			require.Error(t, err)
			require.Nil(t, indexes)
			if scenario != "scan" {
				require.ErrorIs(t, err, failure)
			}
		})
	}
}
