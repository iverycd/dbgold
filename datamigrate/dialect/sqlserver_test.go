package dialect

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestSQLServer2008DDL(t *testing.T) {
	d := NewSQLServer()
	def := "O'Reilly"
	info := &source.TableDDLInfo{TableName: "Order]表", Columns: []source.ColumnInfo{{Name: "Id", DataType: "bigint", FormattedType: "bigint unsigned", Extra: "auto_increment"}, {Name: "Text", DataType: "varchar", Length: 20, Default: &def, IsNullable: true}}}
	ss, e := d.CreateTableStatements("dbo", info, "mysql", TypeOpt{}, func(s string) string { return s })
	require.NoError(t, e)
	ddl := JoinSQL(ss)
	require.Contains(t, ddl, "[Order]]表]")
	require.Contains(t, ddl, "DECIMAL(20,0) IDENTITY(1,1)")
	require.Contains(t, ddl, "N'O''Reilly'")
	require.Contains(t, ddl, "OBJECT_ID")
	for _, s := range []string{"DROP TABLE IF EXISTS", "CREATE OR ALTER", "CASCADE", "CREATE SEQUENCE"} {
		require.NotContains(t, ddl, s)
	}
	info.Columns[1].Generated = "Id+1"
	_, e = d.CreateTableStatements("dbo", info, "mysql", TypeOpt{}, strings.ToLower)
	require.Error(t, e)
}
func TestSQLServerDefaultsAndIndexes(t *testing.T) {
	d := NewSQLServer()
	empty := ""
	zero := "0000-00-00"
	v, e := sqlServerDefault(source.ColumnInfo{DataType: "varchar", Length: 10, Default: &empty})
	require.NoError(t, e)
	require.Equal(t, " DEFAULT N''", v)
	_, e = sqlServerDefault(source.ColumnInfo{DataType: "date", Default: &zero, IsNullable: true})
	require.Error(t, e)
	d.Tables["T"] = &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "A", DataType: "int", IsNullable: true}, {Name: "B", DataType: "int", IsNullable: true}}}
	i := source.IndexInfo{TableName: "T", IndexName: "U", Columns: []string{"A", "B"}, IsUnique: true}
	require.NoError(t, d.ValidateIndex(i))
	require.Contains(t, JoinSQL(d.IndexStatements("dbo", i)), "WHERE [A] IS NOT NULL AND [B] IS NOT NULL")
	i.Unsupported = "前缀索引"
	require.ErrorContains(t, d.ValidateIndex(i), "前缀")
	i.Unsupported = ""
	i.Columns = []string{"missing"}
	require.Error(t, d.ValidateIndex(i))
	s := JoinSQL(d.SequenceStatements("dbo", source.SequenceInfo{TableName: "T", ColumnName: "A", StartValueText: "18446744073709551615"}, ""))
	require.Contains(t, s, "18446744073709551615")
	require.Contains(t, s, "last_value")
	require.Contains(t, s, "DBCC CHECKIDENT")
}
