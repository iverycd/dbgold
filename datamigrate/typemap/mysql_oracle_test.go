package typemap

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOracleTypeBoundaries(t *testing.T) {
	cases := []struct {
		c        source.ColumnInfo
		national bool
		want     string
	}{
		{source.ColumnInfo{DataType: "bigint", FormattedType: "bigint unsigned"}, false, "NUMBER(20,0)"},
		{source.ColumnInfo{DataType: "bit", Precision: 64}, false, "NUMBER(20,0)"},
		{source.ColumnInfo{DataType: "bit", Precision: 1}, false, "NUMBER(1,0)"},
		{source.ColumnInfo{DataType: "decimal", Precision: 38, Scale: 18}, false, "NUMBER(38,18)"},
		{source.ColumnInfo{DataType: "varchar", Length: 1000}, false, "VARCHAR2(1000 CHAR)"},
		{source.ColumnInfo{DataType: "varchar", Length: 1001}, false, "CLOB"},
		{source.ColumnInfo{DataType: "char", Length: 500}, false, "CHAR(500 CHAR)"},
		{source.ColumnInfo{DataType: "char", Length: 501}, false, "CLOB"},
		{source.ColumnInfo{DataType: "varchar", Length: 1000}, true, "NVARCHAR2(1000)"},
		{source.ColumnInfo{DataType: "varchar", Length: 1001}, true, "NCLOB"},
		{source.ColumnInfo{DataType: "datetime", DateTimePrecision: 6}, false, "TIMESTAMP(6)"},
		{source.ColumnInfo{DataType: "time"}, false, "VARCHAR2(32 CHAR)"},
		{source.ColumnInfo{DataType: "float"}, false, "BINARY_FLOAT"},
		{source.ColumnInfo{DataType: "double"}, false, "BINARY_DOUBLE"},
		{source.ColumnInfo{DataType: "geometry"}, false, "BLOB"},
		{source.ColumnInfo{DataType: "json"}, false, "CLOB"},
		{source.ColumnInfo{DataType: "enum", Length: 2048}, false, "CLOB"},
	}
	for _, tc := range cases {
		got, err := OracleType(tc.c, tc.national, 4, 4)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
	for _, c := range []source.ColumnInfo{{DataType: "decimal", Precision: 39}, {DataType: "decimal", Precision: 10, Scale: 11}, {DataType: "varchar"}, {DataType: "uuid"}} {
		_, err := OracleType(c, false, 4, 4)
		require.Error(t, err)
	}
	mapper, ok := Get("mysql", "oracle")
	require.True(t, ok)
	require.Equal(t, "NUMBER(20,0)", mapper(source.ColumnInfo{DataType: "bigint"}, false, false))
	_, ok = Get("postgres", "oracle")
	require.False(t, ok)
}
