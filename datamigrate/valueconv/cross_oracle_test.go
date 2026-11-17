package valueconv

import (
	"dbgold/datamigrate/source"
	goora "github.com/sijms/go-ora/v2"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestCrossOracleValues(t *testing.T) {
	decimal := source.ColumnInfo{DataType: "decimal", Precision: 38, Scale: 18}
	dst := OracleColumn{DataType: "NUMBER", Precision: 38, Scale: 18}
	v, _, e := ConvertCrossOracle("12345678901234567890.123456789012345678", decimal, "sqlserver", dst, nil)
	require.NoError(t, e)
	require.IsType(t, &goora.Number{}, v)
	_, _, e = ConvertCrossOracle("12345678901234567890.1234567890123456789", decimal, "sqlserver", dst, nil)
	require.Error(t, e)
	tm := time.Date(2026, 1, 2, 3, 4, 5, 123456700, time.FixedZone("offset", 8*3600))
	v, _, e = ConvertCrossOracle(tm, source.ColumnInfo{DataType: "datetimeoffset"}, "sqlserver", OracleColumn{DataType: "TIMESTAMP(7) WITH TIME ZONE", Scale: 7}, nil)
	require.NoError(t, e)
	require.Equal(t, goora.TimeStampTZ(tm), v)
	_, _, e = ConvertCrossOracle(tm, source.ColumnInfo{DataType: "datetimeoffset"}, "sqlserver", OracleColumn{DataType: "TIMESTAMP(6) WITH TIME ZONE", Scale: 6}, nil)
	require.Error(t, e)
	v, _, e = ConvertCrossOracle(tm, source.ColumnInfo{DataType: "time"}, "sqlserver", OracleColumn{DataType: "VARCHAR2", DataLength: 18}, nil)
	require.NoError(t, e)
	require.Equal(t, "03:04:05.1234567", v)
	for _, typ := range []string{"varchar", "nvarchar", "text"} {
		v, w, e := ConvertCrossOracle("", source.ColumnInfo{DataType: typ}, "sqlserver", OracleColumn{DataType: "CLOB", Nullable: true}, nil)
		require.NoError(t, e)
		require.Nil(t, v)
		require.Contains(t, w, "NULL")
		_, _, e = ConvertCrossOracle("", source.ColumnInfo{DataType: typ}, "sqlserver", OracleColumn{DataType: "VARCHAR2"}, nil)
		require.Error(t, e)
	}
	raw := []byte{0, 1, 2, 3, 4, 5, 6, 255}
	v, _, e = ConvertCrossOracle(raw, source.ColumnInfo{DataType: "timestamp"}, "sqlserver", OracleColumn{DataType: "RAW", DataLength: 8}, nil)
	require.NoError(t, e)
	require.Equal(t, raw, v)
	v, _, e = ConvertCrossOracle("中\x00文", source.ColumnInfo{DataType: "nvarchar"}, "sqlserver", OracleColumn{DataType: "NVARCHAR2", DataLength: 100}, nil)
	require.NoError(t, e)
	require.Equal(t, goora.NVarChar("中\x00文"), v)
}
