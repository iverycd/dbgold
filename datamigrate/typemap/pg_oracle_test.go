package typemap

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPGOracleTypeContract(t *testing.T) {
	for _, tc := range []struct {
		c        source.ColumnInfo
		national bool
		want     string
	}{
		{source.ColumnInfo{DataType: "int2"}, false, "NUMBER(5,0)"},
		{source.ColumnInfo{DataType: "int8"}, false, "NUMBER(19,0)"},
		{source.ColumnInfo{DataType: "numeric"}, false, "NUMBER"},
		{source.ColumnInfo{DataType: "numeric", Precision: 3, Scale: -2}, false, "NUMBER(3,-2)"},
		{source.ColumnInfo{DataType: "numeric", Precision: 3, Scale: 5}, false, "NUMBER(3,5)"},
		{source.ColumnInfo{DataType: "varchar", Length: 1000}, false, "VARCHAR2(1000 CHAR)"},
		{source.ColumnInfo{DataType: "varchar"}, true, "NCLOB"},
		{source.ColumnInfo{DataType: "varchar", Length: 500}, true, "NVARCHAR2(1000)"},
		{source.ColumnInfo{DataType: "jsonb"}, true, "CLOB"},
		{source.ColumnInfo{DataType: "xml"}, true, "CLOB"},
		{source.ColumnInfo{DataType: "timestamptz", DateTimePrecision: 6}, false, "TIMESTAMP(6) WITH TIME ZONE"},
		{source.ColumnInfo{DataType: "bit", Length: 1000}, false, "VARCHAR2(1000 CHAR)"},
	} {
		got, err := PGToOracleChecked(tc.c, tc.national, 4, 4)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
	for _, c := range []source.ColumnInfo{
		{DataType: "varchar", Length: 1001}, {DataType: "bpchar", Length: 501}, {DataType: "numeric", Precision: 39},
		{DataType: "numeric", Precision: 3, Scale: -85}, {DataType: "numeric", Precision: 3, Scale: 128},
		{DataType: "time"}, {DataType: "timetz"}, {DataType: "interval"}, {DataType: "money"}, {DataType: "_int4"},
		{DataType: "int4", TypeKind: "d"}, {DataType: "int4", TypeSchema: "custom"}, {DataType: "int4", Generated: "s"},
	} {
		_, err := PGToOracleChecked(c, false, 4, 4)
		require.Error(t, err, c)
	}
	for _, p := range []string{"postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		mapper, ok := Get(p, "oracle")
		require.True(t, ok)
		require.Equal(t, "NUMBER(19,0)", mapper(source.ColumnInfo{DataType: "int8"}, false, false))
	}
	_, err := PGToOracleChecked(source.ColumnInfo{DataType: "bpchar", Length: 1}, true, 4, 4)
	require.ErrorContains(t, err, "填充")
}
