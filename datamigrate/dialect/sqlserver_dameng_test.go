package dialect

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSQLServerDamengDDL(t *testing.T) {
	def := "(N'中文 O''Brien')"
	info := &source.TableDDLInfo{TableName: `A"b`, Columns: []source.ColumnInfo{
		{Name: "Id", DataType: "int", Extra: "auto_increment", IdentityInfo: &source.IdentityInfo{Seed: 10, Increment: 5}},
		{Name: "Text", DataType: "nvarchar", Length: 100, IsNullable: true, Default: &def},
		{Name: "Version", DataType: "timestamp"},
	}}
	d := NewDaMeng()
	ss, e := d.CreateTableStatements("APP", info, "sqlserver", TypeOpt{CharInLength: true}, identity)
	require.NoError(t, e)
	sql := JoinSQL(ss)
	require.Contains(t, sql, `"APP"."A""B"`)
	require.Contains(t, sql, "IDENTITY(10, 5)")
	require.Contains(t, sql, "DEFAULT '中文 O''Brien'")
	require.Contains(t, sql, "VARBINARY(8)")
	seq := source.SequenceInfo{TableName: "T", ColumnName: "Id", StartValue: 55, IdentityInfo: &source.IdentityInfo{Increment: 5}}
	sql = JoinSQL(d.SequenceStatements("APP", seq, ""))
	require.Contains(t, sql, `ALTER TABLE "APP"."T" DROP IDENTITY`)
	require.Contains(t, sql, `ADD COLUMN "ID" IDENTITY(55, 5)`)
	require.NotContains(t, sql, "RESTART")
	bad := "newid()"
	info.Columns[1].Default = &bad
	_, e = d.CreateTableStatements("APP", info, "sqlserver", TypeOpt{}, identity)
	require.Error(t, e)
}
