package dialect

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestDamengForbiddenColumnNames(t *testing.T) {
	for _, target := range []string{"ROWID", "TRXID", "VERSIONS_STARTTIME", "VERSIONS_ENDTIME", "VERSIONS_STARTTRXID", "VERSIONS_ENDTRXID", "VERSIONS_OPERATION"} {
		for _, original := range []string{target, strings.ToLower(target), strings.ToLower(target[:1]) + target[1:]} {
			t.Run(original, func(t *testing.T) {
				info := &source.TableDDLInfo{TableName: "Mixed", Columns: []source.ColumnInfo{{Name: original, DataType: "int"}}}
				stmts, err := NewDaMeng().CreateTableStatements("APP", info, "mysql", TypeOpt{}, strings.ToLower)
				require.Empty(t, stmts, "invalid DDL must never be executable")
				var issue *DamengColumnNameError
				require.ErrorAs(t, err, &issue)
				require.Contains(t, issue.OriginalDDL, `"`+target+`" INT`)
				require.Equal(t, []DamengColumnRename{{Source: original, Target: "SRC_" + target}}, issue.ColumnRenames)
				require.Contains(t, issue.SuggestedDDL, `"SRC_`+target+`" INT`)
				require.NotContains(t, issue.SuggestedDDL, "DROP TABLE")
				require.True(t, strings.HasSuffix(issue.SuggestedDDL, ");"))
				require.Equal(t, original, info.Columns[0].Name)
			})
		}
	}
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "ROWGUID", DataType: "int"}, {Name: "SXROWGUID", DataType: "int"}, {Name: "ordinary", DataType: "int"}}}
	stmts, err := NewDaMeng().CreateTableStatements("APP", info, "mysql", TypeOpt{}, identity)
	require.NoError(t, err)
	require.Len(t, stmts, 2)
}

func TestDamengSuggestionPreservesDefinitionsAndAvoidsCollisions(t *testing.T) {
	info := &source.TableDDLInfo{TableName: "gj_data", Columns: []source.ColumnInfo{
		{Name: "rowid", DataType: "int", Extra: "auto_increment", IdentityInfo: &source.IdentityInfo{Seed: 10, Increment: 5}},
		{Name: "TrxId", DataType: "int", IsNullable: true},
		{Name: "src_rowid", DataType: "int"},
		{Name: "SRC_ROWID_2", DataType: "int"},
		{Name: "src_trxid", DataType: "int"},
		{Name: "事项名称", DataType: "nvarchar", Length: 1000, IsNullable: true, Default: strptr("N'ROWID 原样保留'")},
	}}
	before := append([]source.ColumnInfo(nil), info.Columns...)
	for i := 0; i < 2; i++ {
		_, err := NewDaMeng().CreateTableStatements("SHYJS_SXK", info, "sqlserver", TypeOpt{CharInLength: true}, identity)
		var issue *DamengColumnNameError
		require.ErrorAs(t, err, &issue)
		require.Equal(t, []DamengColumnRename{{Source: "rowid", Target: "SRC_ROWID_3"}, {Source: "TrxId", Target: "SRC_TRXID_2"}}, issue.ColumnRenames)
		require.Contains(t, issue.SuggestedDDL, `CREATE TABLE "SHYJS_SXK"."GJ_DATA"`)
		require.Contains(t, issue.SuggestedDDL, `"SRC_ROWID_3" INT IDENTITY(10, 5) NOT NULL`)
		require.Contains(t, issue.SuggestedDDL, `"SRC_TRXID_2" INT,`)
		require.Contains(t, issue.SuggestedDDL, `"事项名称" VARCHAR2(1000 CHAR) DEFAULT 'ROWID 原样保留'`)
		require.Equal(t, before, info.Columns)
	}
}

func TestDamengSuggestionIncompleteGeneration(t *testing.T) {
	for _, other := range []source.ColumnInfo{
		{Name: "Bad", DataType: "geometry"},
		{Name: "Bad", DataType: "int", Default: strptr("newid()")},
		{Name: "Bad", DataType: "int", Generated: "1+1"},
		{Name: "ROWID", DataType: "int"},
	} {
		info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "rowid", DataType: "int"}, other}}
		stmts, err := NewDaMeng().CreateTableStatements("APP", info, "sqlserver", TypeOpt{}, identity)
		var issue *DamengColumnNameError
		require.ErrorAs(t, err, &issue)
		require.Empty(t, stmts)
		require.Empty(t, issue.SuggestedDDL)
		require.NotEmpty(t, issue.ColumnRenames)
		require.Contains(t, issue.Suggestion, "无法生成完整建议 DDL")
	}
}
