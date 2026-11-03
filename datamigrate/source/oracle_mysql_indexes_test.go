package source

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func oracleMySQLIndexesReader(t *testing.T) (*OracleReader, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		db.Close()
	})
	return &OracleReader{db: db, owner: "APP"}, mock
}

func oracleMySQLIndexRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"TABLE_NAME", "INDEX_NAME", "COLUMN_NAME", "UNIQUENESS", "COLUMN_POSITION", "DESCEND", "INDEX_TYPE", "STATUS"})
}

func oracleMySQLColumnRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"TABLE_NAME", "COLUMN_NAME", "HIDDEN_COLUMN", "VIRTUAL_COLUMN", "NULLABLE"})
}

func oracleMySQLExpressionRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"TABLE_NAME", "INDEX_NAME", "COLUMN_POSITION", "COLUMN_EXPRESSION"})
}

func expectOracleMySQLIndexMetadata(mock sqlmock.Sqlmock, indexes, columns *sqlmock.Rows) {
	mock.ExpectQuery(oracleMySQLIndexesSQL).WithArgs("APP", "APP").WillReturnRows(indexes).RowsWillBeClosed()
	mock.ExpectQuery(oracleMySQLIndexColumnsSQL).WithArgs("APP").WillReturnRows(columns).RowsWillBeClosed()
}

func TestOracleMySQLIndexesResolveHiddenDescendingScreenshotKey(t *testing.T) {
	r, mock := oracleMySQLIndexesReader(t)
	expectOracleMySQLIndexMetadata(mock, oracleMySQLIndexRows().
		AddRow("P_USAGE_LOG", "IDX_P_USAGE_LOG_SERVERTS", "SYS_NC00013$", "NONUNIQUE", 1, "DESC", "FUNCTION-BASED NORMAL", "VALID").
		AddRow("P_USAGE_LOG", "IDX_P_USAGE_LOG_SERVERTS", "ID", "NONUNIQUE", 2, "ASC", "FUNCTION-BASED NORMAL", "VALID").
		AddRow("P_USAGE_LOG", "IX_ID", "ID", "NONUNIQUE", 1, "ASC", "NORMAL", "VALID"), oracleMySQLColumnRows().
		AddRow("P_USAGE_LOG", "SERVER_TS", "NO", "NO", "Y").
		AddRow("P_USAGE_LOG", "ID", "NO", "NO", "N").
		AddRow("P_USAGE_LOG", "SYS_NC00013$", "YES", "YES", "Y"))
	mock.ExpectQuery(oracleMySQLIndexExpressionsSQL).WithArgs("APP", "APP").WillReturnRows(oracleMySQLExpressionRows().
		AddRow("P_USAGE_LOG", "IDX_P_USAGE_LOG_SERVERTS", 1, `SYS_OP_DESCEND("SERVER_TS")`)).RowsWillBeClosed()
	indexes, err := r.GetIndexesForMySQL(context.Background())
	require.NoError(t, err)
	require.Len(t, indexes, 2)
	require.Equal(t, "P_USAGE_LOG", indexes[0].TableName)
	require.Equal(t, "IDX_P_USAGE_LOG_SERVERTS", indexes[0].IndexName)
	require.Equal(t, []string{"SERVER_TS", "ID"}, indexes[0].Columns)
	require.Equal(t, []bool{true, false}, indexes[0].Descending)
	require.Empty(t, indexes[0].Unsupported)
	require.Equal(t, []string{"ID"}, indexes[1].Columns)
	require.Empty(t, indexes[1].Unsupported)
}

func TestOracleMySQLIndexesPreserveQuotedCaseAndDistinctDottedIdentities(t *testing.T) {
	r, mock := oracleMySQLIndexesReader(t)
	expectOracleMySQLIndexMetadata(mock, oracleMySQLIndexRows().
		AddRow("A.B", "C", "SYS_NC1$", "NONUNIQUE", 1, "DESC", "FUNCTION-BASED NORMAL", "VALID").
		AddRow("A", "B.C", "SYS_NC1$", "NONUNIQUE", 1, "DESC", "FUNCTION-BASED NORMAL", "N/A").
		AddRow("A", "RealHiddenLookingName", "SYS_NC00013$", "UNIQUE", 1, "ASC", "NORMAL", "VALID"), oracleMySQLColumnRows().
		AddRow("A.B", `Mi"x.ed`, "NO", "NO", "Y").
		AddRow("A.B", "SYS_NC1$", "YES", "YES", "Y").
		AddRow("A", "SERVER_TS", "NO", "NO", "Y").
		AddRow("A", "SYS_NC1$", "YES", "YES", "Y").
		AddRow("A", "SYS_NC00013$", "NO", "NO", "Y"))
	mock.ExpectQuery(oracleMySQLIndexExpressionsSQL).WithArgs("APP", "APP").WillReturnRows(oracleMySQLExpressionRows().
		AddRow("A.B", "C", 1, `SYS_OP_DESCEND("Mi""x.ed")`).
		AddRow("A", "B.C", 1, `server_ts`)).RowsWillBeClosed()
	indexes, err := r.GetIndexesForMySQL(context.Background())
	require.NoError(t, err)
	require.Len(t, indexes, 3)
	require.Equal(t, []string{`Mi"x.ed`}, indexes[0].Columns)
	require.Equal(t, []string{"SERVER_TS"}, indexes[1].Columns)
	require.Equal(t, []string{"SYS_NC00013$"}, indexes[2].Columns)
	for _, index := range indexes {
		require.Empty(t, index.Unsupported)
	}
}

func TestOracleMySQLIndexesKeepWholeMixedExpressionIndexUnsupported(t *testing.T) {
	r, mock := oracleMySQLIndexesReader(t)
	expectOracleMySQLIndexMetadata(mock, oracleMySQLIndexRows().
		AddRow("MixedTable", "IxExpression", "ID", "NONUNIQUE", 1, "ASC", "FUNCTION-BASED NORMAL", "VALID").
		AddRow("MixedTable", "IxExpression", "SYS_NC2$", "NONUNIQUE", 2, "ASC", "FUNCTION-BASED NORMAL", "VALID").
		AddRow("MixedTable", "IxExpression", "Other", "NONUNIQUE", 3, "DESC", "FUNCTION-BASED NORMAL", "VALID").
		AddRow("MixedTable", "IxNormal", "ID", "NONUNIQUE", 1, "ASC", "NORMAL", "VALID"), oracleMySQLColumnRows().
		AddRow("MixedTable", "ID", "NO", "NO", "N").
		AddRow("MixedTable", "Label", "NO", "NO", "Y").
		AddRow("MixedTable", "Other", "NO", "NO", "N").
		AddRow("MixedTable", "SYS_NC2$", "YES", "YES", "Y"))
	mock.ExpectQuery(oracleMySQLIndexExpressionsSQL).WithArgs("APP", "APP").WillReturnRows(oracleMySQLExpressionRows().
		AddRow("MixedTable", "IxExpression", 2, `UPPER("Label")`)).RowsWillBeClosed()
	indexes, err := r.GetIndexesForMySQL(context.Background())
	require.NoError(t, err)
	require.Len(t, indexes, 2)
	require.Equal(t, []string{"ID", "SYS_NC2$", "Other"}, indexes[0].Columns)
	require.Contains(t, indexes[0].Unsupported, `UPPER(\"Label\")`)
	require.Empty(t, indexes[1].Unsupported)
}

func TestOracleMySQLIndexesRejectIncompleteCatalogKeys(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		values       []driver.Value
	}{
		{"missing column", "缺少列名", []driver.Value{"T", "IX", nil, "NONUNIQUE", 1, "ASC", "NORMAL", "VALID"}},
		{"missing ordinal", "顺序", []driver.Value{"T", "IX", "ID", "NONUNIQUE", nil, "ASC", "NORMAL", "VALID"}},
		{"gapped ordinal", "顺序", []driver.Value{"T", "IX", "ID", "NONUNIQUE", 2, "ASC", "NORMAL", "VALID"}},
		{"missing direction", "方向", []driver.Value{"T", "IX", "ID", "NONUNIQUE", 1, nil, "NORMAL", "VALID"}},
		{"invalid direction", "方向", []driver.Value{"T", "IX", "ID", "NONUNIQUE", 1, "OTHER", "NORMAL", "VALID"}},
		{"missing uniqueness", "唯一性", []driver.Value{"T", "IX", "ID", nil, 1, "ASC", "NORMAL", "VALID"}},
		{"missing kind", "索引类型", []driver.Value{"T", "IX", "ID", "NONUNIQUE", 1, "ASC", nil, "VALID"}},
		{"bitmap", "BITMAP", []driver.Value{"T", "IX", "ID", "NONUNIQUE", 1, "ASC", "BITMAP", "VALID"}},
		{"invalid status", "索引状态", []driver.Value{"T", "IX", "ID", "NONUNIQUE", 1, "ASC", "NORMAL", "UNUSABLE"}},
		{"missing status", "索引状态", []driver.Value{"T", "IX", "ID", "NONUNIQUE", 1, "ASC", "NORMAL", nil}},
		{"unknown column", "源列元数据", []driver.Value{"T", "IX", "MISSING", "NONUNIQUE", 1, "ASC", "NORMAL", "VALID"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := oracleMySQLIndexesReader(t)
			expectOracleMySQLIndexMetadata(mock, oracleMySQLIndexRows().AddRow(tc.values...), oracleMySQLColumnRows().AddRow("T", "ID", "NO", "NO", "N"))
			indexes, err := r.GetIndexesForMySQL(context.Background())
			require.NoError(t, err)
			require.Len(t, indexes, 1)
			require.Contains(t, indexes[0].Unsupported, tc.reason)
		})
	}
}

func TestOracleMySQLIndexesRequireNotNullKeysForDescendingUnique(t *testing.T) {
	for _, tc := range []struct {
		name     string
		nullable driver.Value
		unique   string
		wantFail bool
	}{
		{"not null unique", "N", "UNIQUE", false},
		{"nullable unique", "Y", "UNIQUE", true},
		{"unknown nullability unique", nil, "UNIQUE", true},
		{"nullable ordinary", "Y", "NONUNIQUE", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := oracleMySQLIndexesReader(t)
			expectOracleMySQLIndexMetadata(mock, oracleMySQLIndexRows().
				AddRow("T", "IX", "SYS_NC1$", tc.unique, 1, "DESC", "FUNCTION-BASED NORMAL", "VALID").
				AddRow("T", "IX", "Second", tc.unique, 2, "ASC", "FUNCTION-BASED NORMAL", "VALID"), oracleMySQLColumnRows().
				AddRow("T", "ID", "NO", "NO", "N").
				AddRow("T", "Second", "NO", "NO", tc.nullable).
				AddRow("T", "SYS_NC1$", "YES", "YES", "Y"))
			mock.ExpectQuery(oracleMySQLIndexExpressionsSQL).WithArgs("APP", "APP").WillReturnRows(oracleMySQLExpressionRows().AddRow("T", "IX", 1, `"ID"`)).RowsWillBeClosed()
			indexes, err := r.GetIndexesForMySQL(context.Background())
			require.NoError(t, err)
			require.Equal(t, []string{"ID", "Second"}, indexes[0].Columns)
			if tc.wantFail {
				require.Contains(t, indexes[0].Unsupported, "NULL 唯一性语义")
			} else {
				require.Empty(t, indexes[0].Unsupported)
			}
		})
	}
}

func TestOracleMySQLIndexesExpressionReadErrorsDoNotBlockPlainIndexes(t *testing.T) {
	for _, failure := range []string{"query", "scan", "row"} {
		t.Run(failure, func(t *testing.T) {
			r, mock := oracleMySQLIndexesReader(t)
			expectOracleMySQLIndexMetadata(mock, oracleMySQLIndexRows().
				AddRow("T", "IxHidden", "SYS_NC1$", "NONUNIQUE", 1, "DESC", "FUNCTION-BASED NORMAL", "VALID").
				AddRow("T", "IxPlain", "ID", "NONUNIQUE", 1, "ASC", "NORMAL", "VALID"), oracleMySQLColumnRows().
				AddRow("T", "ID", "NO", "NO", "N").
				AddRow("T", "SYS_NC1$", "YES", "YES", "Y"))
			query := mock.ExpectQuery(oracleMySQLIndexExpressionsSQL).WithArgs("APP", "APP")
			switch failure {
			case "query":
				query.WillReturnError(errors.New("expression dictionary denied"))
			case "scan":
				query.WillReturnRows(oracleMySQLExpressionRows().AddRow("T", "IxHidden", "bad ordinal", `"ID"`)).RowsWillBeClosed()
			case "row":
				query.WillReturnRows(oracleMySQLExpressionRows().
					AddRow("T", "IxHidden", 1, `"ID"`).
					AddRow("T", "Irrelevant", 1, `"ID"`).RowError(1, errors.New("expression rows interrupted"))).RowsWillBeClosed()
			}
			indexes, err := r.GetIndexesForMySQL(context.Background())
			require.NoError(t, err)
			require.Len(t, indexes, 2)
			require.Contains(t, indexes[0].Unsupported, "表达式元数据读取失败")
			require.Equal(t, []string{"SYS_NC1$"}, indexes[0].Columns, "partial expression inventory must be discarded")
			require.Empty(t, indexes[1].Unsupported)
		})
	}
}

func TestOracleMySQLIndexesRejectUnprovenExpressions(t *testing.T) {
	for _, tc := range []struct {
		name, direction string
		expressions     []driver.Value
	}{
		{"missing", "DESC", nil},
		{"null", "DESC", []driver.Value{nil}},
		{"empty", "DESC", []driver.Value{" "}},
		{"duplicate", "DESC", []driver.Value{`"ID"`, `"ID"`}},
		{"quoted wrong case", "DESC", []driver.Value{`"id"`}},
		{"unknown column", "DESC", []driver.Value{`"Missing"`}},
		{"virtual reference", "DESC", []driver.Value{`"Virtual"`}},
		{"direction mismatch", "ASC", []driver.Value{`SYS_OP_DESCEND("ID")`}},
		{"nested function", "DESC", []driver.Value{`SYS_OP_DESCEND(UPPER("ID"))`}},
		{"qualified column", "DESC", []driver.Value{`"T"."ID"`}},
		{"extra tokens", "DESC", []driver.Value{`"ID" + 1`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := oracleMySQLIndexesReader(t)
			expectOracleMySQLIndexMetadata(mock, oracleMySQLIndexRows().AddRow("T", "IX", "SYS_NC1$", "NONUNIQUE", 1, tc.direction, "FUNCTION-BASED NORMAL", "VALID"), oracleMySQLColumnRows().
				AddRow("T", "ID", "NO", "NO", "N").
				AddRow("T", "Virtual", "NO", "YES", "Y").
				AddRow("T", "SYS_NC1$", "YES", "YES", "Y"))
			rows := oracleMySQLExpressionRows()
			for _, expression := range tc.expressions {
				rows.AddRow("T", "IX", 1, expression)
			}
			mock.ExpectQuery(oracleMySQLIndexExpressionsSQL).WithArgs("APP", "APP").WillReturnRows(rows).RowsWillBeClosed()
			indexes, err := r.GetIndexesForMySQL(context.Background())
			require.NoError(t, err)
			require.Len(t, indexes, 1)
			require.NotEmpty(t, indexes[0].Unsupported)
			require.Equal(t, []string{"SYS_NC1$"}, indexes[0].Columns)
		})
	}
}

func TestOracleMySQLIndexesBaseReadErrorsNeverReturnPartialInventory(t *testing.T) {
	for _, failure := range []string{"query", "scan", "row", "columns query", "columns scan", "columns row"} {
		t.Run(failure, func(t *testing.T) {
			r, mock := oracleMySQLIndexesReader(t)
			query := mock.ExpectQuery(oracleMySQLIndexesSQL).WithArgs("APP", "APP")
			switch failure {
			case "query":
				query.WillReturnError(errors.New("index catalog denied"))
			case "scan":
				query.WillReturnRows(oracleMySQLIndexRows().AddRow("T", "IX", "ID", "NONUNIQUE", "bad ordinal", "ASC", "NORMAL", "VALID")).RowsWillBeClosed()
			case "row":
				query.WillReturnRows(oracleMySQLIndexRows().
					AddRow("T", "IX", "ID", "NONUNIQUE", 1, "ASC", "NORMAL", "VALID").
					AddRow("T", "IY", "ID", "NONUNIQUE", 1, "ASC", "NORMAL", "VALID").RowError(1, errors.New("index catalog interrupted"))).RowsWillBeClosed()
			default:
				query.WillReturnRows(oracleMySQLIndexRows().AddRow("T", "IX", "ID", "NONUNIQUE", 1, "ASC", "NORMAL", "VALID")).RowsWillBeClosed()
				columns := mock.ExpectQuery(oracleMySQLIndexColumnsSQL).WithArgs("APP")
				switch failure {
				case "columns query":
					columns.WillReturnError(errors.New("column catalog denied"))
				case "columns scan":
					columns.WillReturnRows(oracleMySQLColumnRows().AddRow(nil, "ID", "NO", "NO", "N")).RowsWillBeClosed()
				case "columns row":
					columns.WillReturnRows(oracleMySQLColumnRows().
						AddRow("T", "ID", "NO", "NO", "N").
						AddRow("T", "Other", "NO", "NO", "N").RowError(1, errors.New("column catalog interrupted"))).RowsWillBeClosed()
				}
			}
			indexes, err := r.GetIndexesForMySQL(context.Background())
			require.Error(t, err)
			require.Nil(t, indexes)
		})
	}
}

func TestOracleMySQLIndexesRejectVisibleVirtualAndDuplicateColumns(t *testing.T) {
	r, mock := oracleMySQLIndexesReader(t)
	expectOracleMySQLIndexMetadata(mock, oracleMySQLIndexRows().
		AddRow("T", "IxVirtual", "Virtual", "NONUNIQUE", 1, "ASC", "NORMAL", "VALID").
		AddRow("T", "IxDuplicate", "ID", "NONUNIQUE", 1, "ASC", "NORMAL", "VALID"), oracleMySQLColumnRows().
		AddRow("T", "Virtual", "NO", "YES", "Y").
		AddRow("T", "ID", "NO", "NO", "N").
		AddRow("T", "ID", "NO", "NO", "N"))
	indexes, err := r.GetIndexesForMySQL(context.Background())
	require.NoError(t, err)
	require.Contains(t, indexes[0].Unsupported, "虚拟列")
	require.Contains(t, indexes[1].Unsupported, "源列元数据")
}

func TestOracleMySQLIndexesEmptyInventoryAvoidsOtherCatalogReads(t *testing.T) {
	r, mock := oracleMySQLIndexesReader(t)
	mock.ExpectQuery(oracleMySQLIndexesSQL).WithArgs("APP", "APP").WillReturnRows(oracleMySQLIndexRows()).RowsWillBeClosed()
	indexes, err := r.GetIndexesForMySQL(context.Background())
	require.NoError(t, err)
	require.Empty(t, indexes)
}

func TestOracleMySQLIndexExpressionAcceptsOnlyCompleteColumnGrammar(t *testing.T) {
	for _, tc := range []struct {
		raw, name string
		desc      bool
	}{
		{` "Mixed.Case" `, "Mixed.Case", false},
		{`"Mi""xed"`, `Mi"xed`, false},
		{`foo$1#`, "FOO$1#", false},
		{`Sys_Op_Descend ( "Column" ) `, "Column", true},
		{`SYS_OP_DESCEND(foo)`, "FOO", true},
	} {
		name, desc, err := oracleMySQLIndexExpressionColumn(tc.raw)
		require.NoError(t, err, tc.raw)
		require.Equal(t, tc.name, name)
		require.Equal(t, tc.desc, desc)
	}
	for _, raw := range []string{"", `""`, `"unfinished`, `A.B`, `"SYS_OP_DESCEND"("ID")`, `SYS_OP_DESCEND("ID","Other")`, `SYS_OP_DESCEND(("ID"))`, `SYS_OP_DESCEND("ID");`, `UPPER("ID")`, `"ID" -- comment`, "`ID`"} {
		_, _, err := oracleMySQLIndexExpressionColumn(raw)
		require.Error(t, err, raw)
	}
}
