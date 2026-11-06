package dialect

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestOscarCreateTableAndQuoting(t *testing.T) {
	zero := "0"
	info := &source.TableDDLInfo{TableName: `Order"表`, Columns: []source.ColumnInfo{
		{Name: `ID"列`, DataType: "int", IsNullable: false},
		{Name: "created", DataType: "datetime", Default: &zero, IsNullable: true},
	}}
	d := NewOscar()
	stmts, err := d.CreateTableStatements("业务", info, "mysql", TypeOpt{}, func(v string) string { return v })
	if err != nil {
		t.Fatal(err)
	}
	got := JoinSQL(stmts)
	for _, fragment := range []string{`"业务"."Order""表"`, `"ID""列" INTEGER NOT NULL`, "DEFAULT 0"} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("DDL missing %q:\n%s", fragment, got)
		}
	}
}

func TestOscarIndexNamesAndSequenceNames(t *testing.T) {
	d := NewOscar()
	for _, table := range []string{"DZT_ZNGW3_HTXZXX", "DZT_ZNGW3_HTTSCVER", "中文表"} {
		for _, unique := range []bool{false, true} {
			idx := source.IndexInfo{TableName: table, IndexName: "IDX_XZLXGUID", Columns: []string{"XZLXGUID", "SECOND"}, IsUnique: unique}
			name := IndexName(d, idx)
			require.Equal(t, table+"_IDX_XZLXGUID", name)
			sql := JoinSQL(d.IndexStatements("MixedSchema", idx))
			require.Contains(t, sql, `INDEX "`+name+`" ON "MixedSchema"."`+table+`" ("XZLXGUID", "SECOND")`)
			require.Equal(t, unique, strings.Contains(sql, "UNIQUE INDEX"))
			require.NotContains(t, sql, "IF NOT EXISTS")
			require.Equal(t, "IDX_XZLXGUID", idx.IndexName)
		}
	}
	pk := source.IndexInfo{TableName: "T", IndexName: "PRIMARY", Columns: []string{"ID"}, IsPrimary: true}
	require.Equal(t, "PK_T", IndexName(d, pk))
	require.Equal(t, `ALTER TABLE "MixedSchema"."T" ADD CONSTRAINT "PK_T" PRIMARY KEY ("ID")`, JoinSQL(d.IndexStatements("MixedSchema", pk)))
	seq := source.SequenceInfo{TableName: "T", ColumnName: "ID", StartValue: 12}
	require.Equal(t, "SEQ_T_ID", SequenceName(d, seq))
	ddl := JoinSQL(d.SequenceStatements("MixedSchema", seq, ""))
	require.Contains(t, ddl, `CREATE SEQUENCE "MixedSchema"."SEQ_T_ID"`)
	require.Contains(t, ddl, `SET DEFAULT nextval('"MixedSchema"."SEQ_T_ID"')`)
	long := source.IndexInfo{TableName: strings.Repeat("T", 200), IndexName: "IDX", Columns: []string{"X"}}
	require.Contains(t, JoinSQL(d.IndexStatements("S", long)), `"`+long.TableName+`_IDX"`)
	require.Equal(t, "seq_T_ID", SequenceName(NewPostgres("postgres"), seq))
}

func TestOscarCapabilities(t *testing.T) {
	caps := NewOscar().Caps()
	if !caps.UsesSequences || caps.SupportsDistribute || !caps.SupportsChangeOwner {
		t.Fatalf("unexpected capabilities: %+v", caps)
	}
}

func TestOscarBitDefaultsMatchTargetType(t *testing.T) {
	for _, tc := range []struct {
		name, srcType, dataType, value, want string
		precision                            int64
	}{
		{"zero", "mysql", "bit", "b'0'", "BIGINT DEFAULT 0", 1},
		{"one", "mysql", "bit", "b'1'", "BIGINT DEFAULT 1", 1},
		{"all bits", "mysql", "bit", "b'1010'", "BIGINT DEFAULT 10", 4},
		{"uppercase", "mysql", "bit", "B'1011'", "BIGINT DEFAULT 11", 4},
		{"signed maximum", "mysql", "bit", "b'" + strings.Repeat("1", 63) + "'", "BIGINT DEFAULT 9223372036854775807", 63},
		{"high bit", "mysql", "bit", "b'1" + strings.Repeat("0", 63) + "'", "DECIMAL(20,0) DEFAULT 9223372036854775808", 64},
		{"unsigned maximum", "mysql", "bit", "b'" + strings.Repeat("1", 64) + "'", "DECIMAL(20,0) DEFAULT 18446744073709551615", 64},
		{"boolean zero", "mysql", "boolean", "b'0'", "BOOLEAN DEFAULT FALSE", 0},
		{"boolean nonzero", "mysql", "boolean", "b'10'", "BOOLEAN DEFAULT TRUE", 0},
		{"sqlserver boolean", "sqlserver", "bit", "((1))", "BOOLEAN DEFAULT TRUE", 0},
		{"sqlserver false", "sqlserver", "bit", "((0))", "BOOLEAN DEFAULT FALSE", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{
				Name: "Flag", DataType: tc.dataType, Precision: tc.precision, IsNullable: true, Default: &tc.value,
			}}}
			statements, err := NewOscar().CreateTableStatements("S", info, tc.srcType, TypeOpt{}, func(v string) string { return v })
			require.NoError(t, err)
			require.Contains(t, JoinSQL(statements), `"Flag" `+tc.want)
		})
	}
}

func TestOscarDefaultRenderingPreservesOtherDefaults(t *testing.T) {
	for _, tc := range []struct {
		dataType, value, extra, want string
	}{
		{"int", "99", "", "INTEGER DEFAULT 99"},
		{"varchar", "a'b", "", "VARCHAR(255) DEFAULT 'a''b'"},
		{"datetime", "NOW()", "", "TIMESTAMP DEFAULT CURRENT_TIMESTAMP"},
		{"int", "NULL", "", "INTEGER DEFAULT NULL"},
		{"boolean", "TRUE", "", "BOOLEAN DEFAULT TRUE"},
		{"bigint", "b'1'", "auto_increment", "BIGINT"},
	} {
		t.Run(tc.value+tc.extra, func(t *testing.T) {
			info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "C", DataType: tc.dataType, IsNullable: true, Default: &tc.value, Extra: tc.extra}}}
			statements, err := NewOscar().CreateTableStatements("", info, "mysql", TypeOpt{}, func(v string) string { return v })
			require.NoError(t, err)
			ddl := JoinSQL(statements)
			require.Contains(t, ddl, `"C" `+tc.want)
			if tc.extra == "auto_increment" {
				require.NotContains(t, ddl, "DEFAULT")
			}
		})
	}
}

func TestOscarRejectsOverflowingBitDefault(t *testing.T) {
	value := "b'" + strings.Repeat("1", 65) + "'"
	info := &source.TableDDLInfo{TableName: "SourceTable", Columns: []source.ColumnInfo{{Name: "SourceColumn", DataType: "bit", Precision: 64, Default: &value}}}
	statements, err := NewOscar().CreateTableStatements("", info, "mysql", TypeOpt{}, func(v string) string { return v })
	require.ErrorContains(t, err, "SourceTable.SourceColumn")
	require.ErrorContains(t, err, "invalid MySQL BIT default")
	require.Empty(t, statements)
}

func TestOscarMySQLCharacterDefaultsRemainLiteral(t *testing.T) {
	for _, tc := range []struct {
		name, dataType, value, want string
		length                      int64
	}{
		{"true", "varchar", "true", "VARCHAR(255) DEFAULT 'true'", 0},
		{"false", "varchar", "FALSE", "VARCHAR(255) DEFAULT 'FALSE'", 0},
		{"null text", "enum", "NULL", "VARCHAR(255) DEFAULT 'NULL'", 0},
		{"leading zeroes", "char", "001", "CHAR(3) DEFAULT '001'", 3},
		{"bit-like text", "varchar", "b'10'", "VARCHAR(255) DEFAULT 'b''10'''", 0},
		{"spaces", "text", "  ", "CLOB DEFAULT '  '", 0},
		{"quoted text", "varchar", "'quoted'", "VARCHAR(255) DEFAULT '''quoted'''", 0},
		{"parentheses", "varchar", "('value')", "VARCHAR(255) DEFAULT '(''value'')'", 0},
		{"function text", "varchar", "uuid()", "VARCHAR(255) DEFAULT 'uuid()'", 0},
		{"promoted varchar", "varchar", "NULL", "CLOB DEFAULT 'NULL'", 10000},
		{"json boolean text", "json", "false", "CLOB DEFAULT 'false'", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "C", DataType: tc.dataType, Length: tc.length, IsNullable: true, Default: &tc.value}}}
			statements, err := NewOscar().CreateTableStatements("", info, "mysql", TypeOpt{}, identity)
			require.NoError(t, err)
			require.Contains(t, JoinSQL(statements), `"C" `+tc.want)
			require.Equal(t, tc.value, *info.Columns[0].Default)
			require.Equal(t, tc.dataType, info.Columns[0].DataType)
			require.Equal(t, tc.length, info.Columns[0].Length)
		})
	}
}

func TestOscarMySQLExpressionDefaultsKeepExpressionPath(t *testing.T) {
	for _, tc := range []struct {
		dataType, value, extra, want string
	}{
		{"varchar", "uuid()", "DEFAULT_GENERATED", "VARCHAR(255) DEFAULT uuid()"},
		{"varchar", "('expression')", "default_generated", "VARCHAR(255) DEFAULT 'expression'"},
		{"int", "(0)", "DEFAULT_GENERATED", "INTEGER DEFAULT 0"},
		{"datetime", "CURRENT_TIMESTAMP", "DEFAULT_GENERATED", "TIMESTAMP DEFAULT CURRENT_TIMESTAMP"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "C", DataType: tc.dataType, IsNullable: true, Default: &tc.value, Extra: tc.extra}}}
			statements, err := NewOscar().CreateTableStatements("", info, "mysql", TypeOpt{}, identity)
			require.NoError(t, err)
			require.Contains(t, JoinSQL(statements), `"C" `+tc.want)
		})
	}
}
