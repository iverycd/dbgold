package dialect

import (
	"dbgold/datamigrate/source"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSQLServerCrossSourceTableDefaults(t *testing.T) {
	d := NewSQLServer()
	identity := func(s string) string { return s }
	name, created, amount, seq := `'O''Reilly'::character varying`, `CURRENT_TIMESTAMP`, `'-12.0300'::numeric`, `nextval('"Src"."Seq"'::regclass)`
	info := &source.TableDDLInfo{TableName: "Order]", Columns: []source.ColumnInfo{
		{Name: "ID", DataType: "int8", SQLServerIdentitySeed: "-3", SQLServerIdentityIncrement: "1"},
		{Name: "Name", DataType: "varchar", Length: 20, Default: &name},
		{Name: "CreatedAt", DataType: "timestamptz", DateTimePrecision: 6, Default: &created},
		{Name: "Amount", DataType: "numeric", SQLServerPrecision: 38, SQLServerScale: 2, Default: &amount},
		{Name: "Deferred", DataType: "int8", Default: &seq, SQLServerDeferredDefault: true},
	}}
	statements, err := d.CreateTableStatements("Dst", info, "postgres", TypeOpt{}, identity)
	require.NoError(t, err)
	ddl := JoinSQL(statements)
	require.Contains(t, ddl, "[Dst].[Order]]]")
	require.Contains(t, ddl, "[ID] BIGINT IDENTITY(-3,1) NOT NULL")
	require.Contains(t, ddl, "[Name] NVARCHAR(40) DEFAULT N'O''Reilly'")
	require.Contains(t, ddl, "[CreatedAt] DATETIMEOFFSET(6) DEFAULT SYSDATETIMEOFFSET()")
	require.Contains(t, ddl, "[Amount] DECIMAL(38,2) DEFAULT N'-12.03'")
	require.Contains(t, ddl, "[Deferred] BIGINT NOT NULL")
	require.NotContains(t, ddl, "nextval")
	value, present, err := SQLServerDefaultDecimalLiteral(info.Columns[3], "postgres")
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, "-12.0300", value)
	info.Columns[4].SQLServerDeferredDefault = false
	_, err = d.CreateTableStatements("Dst", info, "postgres", TypeOpt{}, identity)
	require.ErrorContains(t, err, "无法安全迁移默认值")
}

func TestSQLServerCrossSourceSequenceDDL(t *testing.T) {
	d := NewSQLServer()
	d.SetSourceType("oracle")
	seq := source.SequenceInfo{StartValueText: "11", Native: &source.NativeSequenceInfo{
		Name: "Shared]", Schema: "Source", IncrementBy: "3", MinValue: "-90", MaxValue: "1000", CacheSize: "5",
		Bindings: []source.SequenceBinding{{TableName: "Child", ColumnName: "Id", Kind: "default"}, {TableName: "Parent", ColumnName: "Code", Kind: "serial"}},
	}}
	statements := d.SequenceStatements("App", seq, "")
	require.Len(t, statements, 4)
	require.Contains(t, statements[0].SQL, "CREATE SEQUENCE [App].[Shared]]] AS DECIMAL(38,0)")
	require.Contains(t, statements[0].SQL, "MAX(CAST([Id] AS decimal(38,0))) FROM [App].[Child] WITH (TABLOCKX,HOLDLOCK)")
	require.Contains(t, statements[0].SQL, "INCREMENT BY 3 MINVALUE -90 MAXVALUE 1000 NO CYCLE CACHE 5")
	require.Contains(t, statements[1].SQL, SQLServerSequenceMarker)
	require.Contains(t, statements[2].SQL, "DEFAULT (NEXT VALUE FOR [App].[Shared]]]) FOR [Id]")
	require.Equal(t, "Shared]", SequenceName(d, seq))

	identity := source.SequenceInfo{TableName: "Parent", ColumnName: "Code", StartValueText: "-3"}
	d.Tables["Parent"] = &source.TableDDLInfo{TableName: "Parent", Columns: []source.ColumnInfo{{Name: "Code", DataType: "NUMBER", Precision: 9, SQLServerIdentitySeed: "-3", SQLServerIdentityIncrement: "-1"}}}
	ddl := JoinSQL(d.SequenceStatements("App", identity, ""))
	require.Contains(t, ddl, "MIN(CAST([Code] AS decimal(38,0)))")
	require.Contains(t, ddl, "IF @next<>@original")
	require.Contains(t, ddl, "DBCC CHECKIDENT")
	require.Equal(t, "Parent.Code", SequenceName(d, identity))
}
