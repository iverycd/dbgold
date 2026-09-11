package typemap

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSQLServerDamengTypes(t *testing.T) {
	cases := []struct {
		c    source.ColumnInfo
		want string
	}{
		{source.ColumnInfo{DataType: "tinyint"}, "SMALLINT"},
		{source.ColumnInfo{DataType: "bit"}, "NUMBER(1)"},
		{source.ColumnInfo{DataType: "numeric", Precision: 38, Scale: 19}, "NUMBER(38,19)"},
		{source.ColumnInfo{DataType: "nvarchar", Length: -1}, "CLOB"},
		{source.ColumnInfo{DataType: "nvarchar", Length: 80}, "VARCHAR2(80 CHAR)"},
		{source.ColumnInfo{DataType: "varbinary", Length: 32}, "BLOB"},
		{source.ColumnInfo{DataType: "timestamp"}, "VARBINARY(8)"},
		{source.ColumnInfo{DataType: "rowversion"}, "VARBINARY(8)"},
		{source.ColumnInfo{DataType: "uniqueidentifier"}, "VARCHAR2(36)"},
		{source.ColumnInfo{DataType: "datetimeoffset", DateTimePrecision: 6}, "TIMESTAMP(6) WITH TIME ZONE"},
		{source.ColumnInfo{DataType: "datetime2", DateTimePrecision: 0}, "TIMESTAMP(0)"},
		{source.ColumnInfo{DataType: "time", DateTimePrecision: 6}, "TIME(6)"},
		{source.ColumnInfo{DataType: "money"}, "NUMBER(19,4)"},
		{source.ColumnInfo{DataType: "smallmoney"}, "NUMBER(10,4)"},
		{source.ColumnInfo{DataType: "float", Precision: 53}, "DOUBLE"},
	}
	mapper, ok := Get("sqlserver", "dameng")
	require.True(t, ok)
	for _, tc := range cases {
		t.Run(tc.c.DataType+tc.want, func(t *testing.T) {
			require.NoError(t, ValidateSQLServerDamengColumn(tc.c))
			require.Equal(t, tc.want, mapper(tc.c, true, false))
		})
	}
	require.Equal(t, "NVARCHAR2(80)", mapper(source.ColumnInfo{DataType: "nvarchar", Length: 80}, true, true))
	for _, c := range []source.ColumnInfo{{DataType: "time", DateTimePrecision: 7}, {DataType: "datetime2", DateTimePrecision: 7}, {DataType: "geometry"}, {DataType: "hierarchyid"}, {DataType: "sql_variant"}, {DataType: "decimal", Precision: 39}, {DataType: "int", Generated: "computed"}, {DataType: "nvarchar", Length: 0}} {
		require.Error(t, ValidateSQLServerDamengColumn(c), "%+v", c)
	}
}
