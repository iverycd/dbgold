package typemap

import (
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestOracleDamengTypesPreserveExactSemantics(t *testing.T) {
	for _, tc := range []struct {
		c    source.ColumnInfo
		want string
	}{
		{source.ColumnInfo{DataType: "DATE"}, "TIMESTAMP(0)"},
		{source.ColumnInfo{DataType: "TIMESTAMP(9) WITH LOCAL TIME ZONE"}, "TIMESTAMP(9) WITH LOCAL TIME ZONE"},
		{source.ColumnInfo{DataType: "NUMBER", Precision: 38, Scale: 19}, "NUMBER(38,19)"},
		{source.ColumnInfo{DataType: "FLOAT", Precision: 126}, "NUMBER"},
		{source.ColumnInfo{DataType: "VARCHAR2", Length: 10, DataLength: 40, CharUsed: "C"}, "VARCHAR2(10 CHAR)"},
		{source.ColumnInfo{DataType: "VARCHAR2", Length: 40, DataLength: 40, CharUsed: "B"}, "VARCHAR2(40)"},
		{source.ColumnInfo{DataType: "RAW", DataLength: 2000}, "VARBINARY(2000)"},
		{source.ColumnInfo{DataType: "INTERVAL DAY(9) TO SECOND(6)"}, "INTERVAL DAY(9) TO SECOND(6)"},
	} {
		require.NoError(t, ValidateOracleDamengColumn(tc.c))
		require.Equal(t, tc.want, OracleToDameng(tc.c, false, false))
	}
	mapper, ok := Get("oracle", "dameng")
	require.True(t, ok)
	require.Equal(t, "TIMESTAMP(0)", mapper(source.ColumnInfo{DataType: "DATE"}, false, false))
}

func TestOracleDamengRejectsLossyColumns(t *testing.T) {
	for _, c := range []source.ColumnInfo{
		{DataType: "NUMBER", Precision: 5, Scale: -2}, {DataType: "NUMBER", Precision: 4, Scale: 5},
		{DataType: "INTERVAL DAY(2) TO SECOND(9)"}, {DataType: "XMLTYPE"}, {DataType: "BFILE"},
		{DataType: "NUMBER", Identity: "ALWAYS"}, {DataType: "NUMBER", Identity: "BY DEFAULT"},
		{DataType: "NUMBER", Generated: "VIRTUAL"},
	} {
		require.Error(t, ValidateOracleDamengColumn(c))
	}
	require.NoError(t, ValidateOracleDamengColumn(source.ColumnInfo{DataType: "NUMBER", Identity: "BY DEFAULT ON NULL"}))
}
