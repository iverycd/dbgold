package dialect

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestDamengPGDefaults(t *testing.T) {
	cases := []struct{ name, typ, raw, clause string }{
		{"integer false", "INT", "FALSE", "int DEFAULT 0"},
		{"integer true", "INTEGER", "true", "int DEFAULT 1"},
		{"nested boolean", "INT", " ( ( fAlSe ) ) ", "int DEFAULT 0"},
		{"quoted boolean", "INT", "('TRUE')", "int DEFAULT 1"},
		{"wrapped boolean", "BIGINT", "(''FALSE'')", "bigint DEFAULT 0"},
		{"bit", "BIT", "TRUE", "smallint DEFAULT 1"},
		{"boolean", "BOOLEAN", "FALSE", "smallint DEFAULT 0"},
		{"tinyint", "TINYINT", "FALSE", "smallint DEFAULT 0"},
		{"decimal boolean", "NUMBER", "TRUE", "decimal(38,10) DEFAULT 1"},
		{"numeric", "INT", "((99))", "int DEFAULT '99'"},
		{"quoted numeric", "INT", "'99'", "int DEFAULT '99'"},
		{"wrapped numeric", "INT", "''0''", "int DEFAULT '0'"},
		{"precision", "DECIMAL", "((-12345678901234567890.1234567890))", "decimal(38,10) DEFAULT '-12345678901234567890.1234567890'"},
		{"literal true", "VARCHAR", "'TRUE'", "varchar(100) DEFAULT 'TRUE'"},
		{"literal false", "VARCHAR", "(('FALSE'))", "varchar(100) DEFAULT 'FALSE'"},
		{"literal null", "VARCHAR", "'NULL'", "varchar(100) DEFAULT 'NULL'"},
		{"literal function", "VARCHAR", "'GETDATE()'", "varchar(100) DEFAULT 'GETDATE()'"},
		{"literal keyword", "VARCHAR", "'CURRENT_TIMESTAMP'", "varchar(100) DEFAULT 'CURRENT_TIMESTAMP'"},
		{"legacy string", "VARCHAR", "''abc''", "varchar(100) DEFAULT 'abc'"},
		{"quote escape", "VARCHAR", "'中文 O''Brien'", "varchar(100) DEFAULT '中文 O''Brien'"},
		{"empty", "VARCHAR", "''", "varchar(100) DEFAULT ''"},
		{"spaces", "VARCHAR", "('  ')", "varchar(100) DEFAULT '  '"},
		{"single quote", "VARCHAR", "''''", "varchar(100) DEFAULT ''''"},
		{"quoted value", "VARCHAR", "'''abc'''", "varchar(100) DEFAULT '''abc'''"},
		{"opening parenthesis", "VARCHAR", "(('('))", "varchar(100) DEFAULT '('"},
		{"closing parenthesis", "VARCHAR", "((')'))", "varchar(100) DEFAULT ')'"},
		{"null", "INT", "((NULL))", "int DEFAULT NULL"},
		{"current timestamp", "DATETIME", "((CURRENT_TIMESTAMP))", "timestamp without time zone DEFAULT CURRENT_TIMESTAMP"},
		{"function", "DATETIME", "(GETDATE())", "timestamp without time zone DEFAULT CURRENT_TIMESTAMP"},
	}
	for _, dst := range []string{"postgres", "gaussdb", "seabox", "highgo", "vastbase", "gbase", "kingbase"} {
		for _, tc := range cases {
			t.Run(dst+"/"+tc.name, func(t *testing.T) {
				info := &source.TableDDLInfo{TableName: "CMETA_TABLESTRUCT", Columns: []source.ColumnInfo{{
					Name: "todispingrid", DataType: tc.typ, Length: 100, Precision: 38, Scale: 10,
					IsNullable: true, Default: &tc.raw,
				}}}
				statements, err := NewPostgres(dst).CreateTableStatements("app", info, "dameng", TypeOpt{}, strings.ToLower)
				require.NoError(t, err)
				require.Contains(t, JoinSQL(statements), `"todispingrid" `+tc.clause+"\n")
				require.Equal(t, tc.raw, *info.Columns[0].Default, "source metadata must remain untouched")
			})
		}
	}
}

func TestDamengPGDefaultOnlyClassifiesWholeLiterals(t *testing.T) {
	for _, raw := range []string{"('a' || 'b')", "('a') || ('b')", "TRUE OR FALSE", "(1 = 1)", "CAST(FALSE AS INT)"} {
		_, ready, err := damengPGDefault(raw, "int")
		require.NoError(t, err)
		require.False(t, ready, raw)
	}
}

func TestDamengPGMalformedDefaultReportsTableAndColumn(t *testing.T) {
	for _, raw := range []string{"((FALSE)", "('FALSE)", "'unterminated", "(0 /* ) )"} {
		info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "C", DataType: "INT", Default: &raw}}}
		statements, err := NewPostgres("postgres").CreateTableStatements("app", info, "dameng", TypeOpt{}, identity)
		require.ErrorContains(t, err, "T.C: 达梦 默认值")
		require.Empty(t, statements, "do not drop a table when generation fails")
	}
}
