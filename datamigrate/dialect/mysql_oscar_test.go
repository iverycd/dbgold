package dialect

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestOscarMySQLDDLUsesExactTypedDefaults(t *testing.T) {
	value := func(s string) *string { return &s }
	info := &source.TableDDLInfo{TableName: "Order`Line", Columns: []source.ColumnInfo{
		{Name: "Id", DataType: "BIGSERIAL", Extra: "auto_increment"},
		{Name: "Caption", DataType: "VARCHAR2", Length: 16, Default: value(`('O''Brien')::varchar`)},
		{Name: "Empty", DataType: "CHAR", Length: 2, Default: value(`''`)},
		{Name: "Exact", DataType: "NUMBER", Precision: 24, Scale: 2, Default: value(`9007199254740993e0`)},
		{Name: "Seen", DataType: "TIMESTAMP", DateTimePrecision: 6, Default: value(`CURRENT_TIMESTAMP(6)`)},
		{Name: "Flag", DataType: "BOOLEAN", Default: value(`FALSE::BOOLEAN`)},
		{Name: "Bits", DataType: "BIT", Length: 1, Default: value(`B'1'`)},
		{Name: "Raw", DataType: "VARBINARY", Length: 16, Default: value(`X'0041ff'`)},
		{Name: "At", DataType: "TIME", DateTimePrecision: 6, Default: value(`TIME '01:01:00.000001'`)},
		{Name: "Memo", DataType: "CLOB", IsNullable: true, Default: value(`NULL`)},
		{Name: "Payload", DataType: "BLOB", IsNullable: true},
	}}
	d := NewMySQL()
	statements, err := d.CreateTableStatements("Db", info, "oscar", TypeOpt{}, strings.ToLower)
	require.NoError(t, err)
	require.Equal(t, "DROP TABLE IF EXISTS `Db`.`order``line`", statements[0].SQL)
	for _, definition := range []string{
		"`id` bigint NOT NULL AUTO_INCREMENT", "KEY `__migration_auto` (`id`)",
		"`caption` varchar(16) NOT NULL DEFAULT 'O''Brien'", "`empty` char(2) NOT NULL DEFAULT ''",
		"`exact` decimal(24,2) NOT NULL DEFAULT 9007199254740993", "`seen` datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)",
		"`flag` tinyint(1) NOT NULL DEFAULT 0", "`bits` bit(1) NOT NULL DEFAULT b'1'", "`raw` varbinary(16) NOT NULL DEFAULT X'0041ff'",
		"`at` time(6) NOT NULL DEFAULT '01:01:00.000001'", "`memo` longtext", "`payload` longblob",
	} {
		require.Contains(t, statements[1].SQL, definition)
	}
	require.NotContains(t, statements[1].SQL, "PRIMARY KEY")
}

func TestOscarMySQLDDLRejectsUnrepresentableColumnsBeforeDrop(t *testing.T) {
	value := func(s string) *string { return &s }
	for _, tc := range []struct {
		name string
		col  source.ColumnInfo
	}{
		{"numeric overflow", source.ColumnInfo{DataType: "NUMBER", Precision: 66}},
		{"temporal overflow", source.ColumnInfo{DataType: "TIMESTAMP", DateTimePrecision: 7}},
		{"unsupported", source.ColumnInfo{DataType: "ARRAY"}},
		{"wide BIT", source.ColumnInfo{DataType: "BIT", Length: 4}},
		{"generated", source.ColumnInfo{DataType: "INT", Generated: "YES"}},
		{"function", source.ColumnInfo{DataType: "VARCHAR", Length: 12, Default: value(`lower('X')`)}},
		{"literal overflow", source.ColumnInfo{DataType: "TINYINT", Default: value(`128`)}},
		{"invalid numeric expression", source.ColumnInfo{DataType: "INT", Default: value(`1 2`)}},
		{"NULL on required column", source.ColumnInfo{DataType: "INT", Default: value(`NULL`)}},
		{"wrong cast type", source.ColumnInfo{DataType: "VARCHAR", Length: 12, Default: value(`1::int`)}},
		{"long text default", source.ColumnInfo{DataType: "CLOB", Default: value(`'text'`)}},
		{"nullable auto", source.ColumnInfo{DataType: "INT", Extra: "auto_increment", IsNullable: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.col.Name = "Invalid"
			info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{tc.col}}
			statements, err := NewMySQL().CreateTableStatements("Db", info, "oscar", TypeOpt{}, nil)
			require.Error(t, err)
			require.Empty(t, statements)
		})
	}
}
