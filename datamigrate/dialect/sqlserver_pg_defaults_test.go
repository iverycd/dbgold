package dialect

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestSQLServerPGDefaultsPreserveLiteralMeaning(t *testing.T) {
	cases := []struct {
		name, raw, clause string
	}{
		{"quoted number", "('0')", " DEFAULT '0'"},
		{"nested Unicode number", " ( (N'0') ) ", " DEFAULT '0'"},
		{"numeric expression", "((0))", " DEFAULT '0'"},
		{"exact decimal", "((-12345678901234567890.1234567890))", " DEFAULT '-12345678901234567890.1234567890'"},
		{"quote escape", "(N'中文 O''Brien')", " DEFAULT '中文 O''Brien'"},
		{"lowercase Unicode", "((n'中文'))", " DEFAULT '中文'"},
		{"empty string", "('')", " DEFAULT ''"},
		{"space string", "(N' ')", " DEFAULT ' '"},
		{"literal null", "('NULL')", " DEFAULT 'NULL'"},
		{"literal true", "('TRUE')", " DEFAULT 'TRUE'"},
		{"literal function", "('GETDATE()')", " DEFAULT 'GETDATE()'"},
		{"literal keyword", "('CURRENT_TIMESTAMP')", " DEFAULT 'CURRENT_TIMESTAMP'"},
		{"opening parenthesis string", "(('('))", " DEFAULT '('"},
		{"closing parenthesis string", "((')'))", " DEFAULT ')'"},
		{"actual quote characters", "('''0''')", " DEFAULT '''0'''"},
		{"actual null", "((NULL))", " DEFAULT NULL"},
		{"actual function", "((GETDATE()))", " DEFAULT CURRENT_TIMESTAMP"},
		{"actual UUID function", "(NEWID())", " DEFAULT gen_random_uuid()"},
	}
	for _, target := range []string{"postgres", "kingbase", "highgo", "vastbase", "gbase"} {
		for _, tc := range cases {
			t.Run(target+"/"+tc.name, func(t *testing.T) {
				info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "V", DataType: "nvarchar", Length: 100, IsNullable: true, Default: &tc.raw}}}
				statements, err := NewPostgres(target).CreateTableStatements("App", info, "sqlserver", TypeOpt{}, identity)
				require.NoError(t, err)
				require.Contains(t, JoinSQL(statements), `"V" varchar(100)`+tc.clause+"\n")
			})
		}
	}
}

func TestSQLServerPGQuotedNumericDefaults(t *testing.T) {
	zero := "('0')"
	info := &source.TableDDLInfo{TableName: "frame_myaddressgroup_snapshot", Columns: []source.ColumnInfo{
		{Name: "ordernumber", DataType: "int", IsNullable: true, Default: &zero},
		{Name: "xznum", DataType: "decimal", Precision: 11, IsNullable: true, Default: &zero},
	}}
	statements, err := NewPostgres("kingbase").CreateTableStatements("epointwjsj", info, "sqlserver", TypeOpt{}, identity)
	require.NoError(t, err)
	ddl := JoinSQL(statements)
	require.Contains(t, ddl, `"ordernumber" int DEFAULT '0'`)
	require.Contains(t, ddl, `"xznum" decimal(11,0) DEFAULT '0'`)
	require.NotContains(t, ddl, "'''0'''")
}

func TestSQLServerPGDefaultOnlyRecognizesCompleteStrings(t *testing.T) {
	for _, raw := range []string{"('a' + 'b')", "('a') + ('b')", "N'a' + 'b'"} {
		_, literal, err := sqlServerPGDefault(raw)
		require.NoError(t, err)
		require.False(t, literal, raw)
	}
	def, literal, err := sqlServerPGDefault("('a') + ('b')")
	require.NoError(t, err)
	require.False(t, literal)
	require.Equal(t, "('a') + ('b')", def)
}

func TestSQLServerPGMalformedDefaultReportsColumn(t *testing.T) {
	for _, raw := range []string{"((0)", "('abc)", "(N'abc'", "(0 /* ) )"} {
		t.Run(raw, func(t *testing.T) {
			info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "V", DataType: "int", Default: &raw}}}
			_, err := NewPostgres("kingbase").CreateTableStatements("App", info, "sqlserver", TypeOpt{}, identity)
			require.ErrorContains(t, err, "T.V:")
		})
	}
}

func TestSQLServerPGDefaultFunctionMappingsRemainTargetSpecific(t *testing.T) {
	def := "((NEWID()))"
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "V", DataType: "uniqueidentifier", IsNullable: true, Default: &def}}}
	for target, function := range map[string]string{"gaussdb": "uuid()", "seabox": "sys_guid()"} {
		statements, err := NewPostgres(target).CreateTableStatements("", info, "sqlserver", TypeOpt{}, identity)
		require.NoError(t, err)
		require.True(t, strings.Contains(JoinSQL(statements), " DEFAULT "+function))
	}
}
