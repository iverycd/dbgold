package dialect

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOracleDDLDefaultsAndPreflight(t *testing.T) {
	d := NewOracle()
	s := func(v string) *string { return &v }
	info := &source.TableDDLInfo{TableName: "Mixed", Columns: []source.ColumnInfo{
		{Name: "Id", DataType: "bigint", Extra: "auto_increment"},
		{Name: "Txt", DataType: "varchar", Length: 30, Default: s("NULL"), IsNullable: true},
		{Name: "At", DataType: "timestamp", DateTimePrecision: 6, Default: s("CURRENT_TIMESTAMP(6)"), Extra: "DEFAULT_GENERATED"},
		{Name: "Start", DataType: "date", Default: s("2024-02-29")},
		{Name: "Optional", DataType: "date", IsNullable: true, Default: s("2024-00-01")},
	}}
	stmts, err := d.CreateTableStatements("APP", info, "mysql", TypeOpt{}, strings.ToUpper)
	require.NoError(t, err)
	ddl := JoinSQL(stmts)
	require.Contains(t, ddl, `"APP"."MIXED"`)
	require.Contains(t, ddl, `"TXT" VARCHAR2(30 CHAR) DEFAULT 'NULL'`)
	require.Contains(t, ddl, "DEFAULT CURRENT_TIMESTAMP NOT NULL")
	require.Contains(t, ddl, "DEFAULT DATE '2024-02-29'")
	require.NotContains(t, ddl, "IDENTITY")
	for _, bad := range []source.ColumnInfo{
		{Name: "x", DataType: "decimal", Precision: 65, Scale: 2},
		{Name: "x", DataType: "varchar", Length: 5, Default: s("")},
		{Name: "x", DataType: "datetime", Extra: "on update CURRENT_TIMESTAMP"},
		{Name: "x", DataType: "int", Generated: "a+1"},
		{Name: "x", DataType: "date", Default: s("2024-02-30")},
		{Name: "x", DataType: "varchar", Length: 5, Default: s("uuid()"), Extra: "DEFAULT_GENERATED"},
	} {
		_, err = d.CreateTableStatements("APP", &source.TableDDLInfo{TableName: "t", Columns: []source.ColumnInfo{bad}}, "mysql", TypeOpt{}, strings.ToUpper)
		require.Error(t, err, bad)
	}
	_, err = d.CreateTableStatements("APP", &source.TableDDLInfo{TableName: "t", Columns: []source.ColumnInfo{{Name: "a", DataType: "int"}, {Name: "A", DataType: "int"}}}, "mysql", TypeOpt{}, strings.ToUpper)
	require.ErrorContains(t, err, "冲突")
}
func TestOracleGeneratedNamesAndSequence(t *testing.T) {
	for _, s := range []string{strings.Repeat("X", 80), strings.Repeat("表", 20)} {
		n := OracleObjectName("SEQ", s, "ID")
		require.LessOrEqual(t, len(n), 30)
		require.True(t, utf8.ValidString(n))
		require.Equal(t, n, OracleObjectName("SEQ", s, "ID"))
		require.NotEqual(t, n, OracleObjectName("SEQ", s, "OTHER"))
	}
	require.NoError(t, ValidateOracleName(strings.Repeat("A", 30)))
	require.Error(t, ValidateOracleName(strings.Repeat("A", 31)))
	require.Error(t, ValidateOracleName("A\"B"))
	d := NewOracle()
	seq := source.SequenceInfo{TableName: "ORDERS", ColumnName: "ID", StartValueText: "18446744073709551615"}
	ddl := JoinSQL(d.SequenceStatements("APP", seq, ""))
	require.Contains(t, ddl, "18446744073709551615")
	require.Contains(t, ddl, `NVL(MAX("ID"), 0) + 1`)
	require.Contains(t, ddl, `IF :NEW."ID" IS NULL`)
	require.Contains(t, ddl, "DBGOLD_MYSQL_ORACLE:")
	require.NotContains(t, ddl, "DROP SEQUENCE")
	d.OwnedSequences[d.SequenceName(seq)] = true
	ddl = JoinSQL(d.SequenceStatements("APP", seq, ""))
	require.Contains(t, ddl, "LAST_NUMBER INTO old_n")
	require.Contains(t, ddl, "DROP SEQUENCE")
	require.Contains(t, ddl, ", old_n)")
	_, err := OracleSequenceStart(source.SequenceInfo{StartValueText: "1; DROP USER APP"})
	require.Error(t, err)
}
func TestOracleForeignKeys(t *testing.T) {
	f := source.FKInfo{TableName: "CHILD", ConstraintName: "parent", Columns: []string{"ID"}, RefTable: "PARENT", RefColumns: []string{"ID"}, OnDelete: "RESTRICT", OnUpdate: "NO ACTION"}
	d := NewOracle()
	require.NoError(t, ValidateOracleForeignKey(f))
	require.NotContains(t, JoinSQL(d.ForeignKeyStatements("APP", f)), "ON DELETE")
	f.OnDelete = "CASCADE"
	require.Contains(t, JoinSQL(d.ForeignKeyStatements("APP", f)), "ON DELETE CASCADE")
	f.OnUpdate = "CASCADE"
	require.ErrorContains(t, ValidateOracleForeignKey(f), "ON UPDATE CASCADE")
}
