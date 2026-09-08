package valueconv

import (
	goora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/converters"
	"github.com/stretchr/testify/require"
	"math"
	"testing"
	"time"
)

func TestOracleExactValues(t *testing.T) {
	for _, tc := range []struct {
		value     interface{}
		src, want string
		p, s      int64
	}{
		{[]byte{255, 255, 255, 255, 255, 255, 255, 255}, "BIT", "18446744073709551615", 20, 0},
		{uint64(math.MaxUint64), "UNSIGNED BIGINT", "18446744073709551615", 20, 0},
		{[]byte("12345678901234567890.123456789012345678"), "DECIMAL", "12345678901234567890.123456789012345678", 38, 18},
	} {
		v, w, e := ConvertOracle(tc.value, tc.src, OracleColumn{DataType: "NUMBER", Precision: tc.p, Scale: tc.s}, nil)
		require.NoError(t, e)
		require.Empty(t, w)
		s, e := v.(*goora.Number).String()
		require.NoError(t, e)
		require.Equal(t, tc.want, s)
	}
	_, _, e := ConvertOracle("1.001", "DECIMAL", OracleColumn{DataType: "NUMBER", Precision: 10, Scale: 2}, nil)
	require.Error(t, e)
	v, _, e := ConvertOracle(math.MaxFloat64, "DOUBLE", OracleColumn{DataType: "BINARY_DOUBLE"}, nil)
	require.NoError(t, e)
	require.Equal(t, "1.7976931348623157e+308", v)
	v, _, e = ConvertOracle([]byte("2024-02-29 12:34:56.123456"), "DATETIME", OracleColumn{DataType: "TIMESTAMP(6)", Scale: 6}, nil)
	require.NoError(t, e)
	require.Equal(t, 123456000, time.Time(v.(goora.TimeStamp)).Nanosecond())
	_, _, e = ConvertOracle("2024-02-30", "DATE", OracleColumn{DataType: "DATE", Nullable: true}, nil)
	require.Error(t, e)
	v, _, e = ConvertOracle([]byte("-838:59:59.999999"), "TIME", OracleColumn{DataType: "VARCHAR2", CharLength: 32, CharUsed: "C"}, nil)
	require.NoError(t, e)
	require.Equal(t, "-838:59:59.999999", v)
}
func TestOracleNullZeroAndLOB(t *testing.T) {
	for _, s := range []string{"0000-00-00", "2024-00-12", "2024-12-00"} {
		v, w, e := ConvertOracle([]byte(s), "DATE", OracleColumn{DataType: "DATE", Nullable: true}, nil)
		require.NoError(t, e)
		require.Nil(t, v)
		require.Equal(t, "零日期转 NULL", w)
		_, _, e = ConvertOracle(s, "DATE", OracleColumn{DataType: "DATE"}, nil)
		require.ErrorContains(t, e, "非空列")
	}
	v, w, e := ConvertOracle("", "VARCHAR", OracleColumn{DataType: "VARCHAR2", Nullable: true}, nil)
	require.NoError(t, e)
	require.Nil(t, v)
	require.Equal(t, "空字符串转 NULL", w)
	_, _, e = ConvertOracle("", "VARCHAR", OracleColumn{DataType: "VARCHAR2"}, nil)
	require.Error(t, e)
	v, w, e = ConvertOracle("", "TEXT", OracleColumn{DataType: "CLOB"}, nil)
	require.NoError(t, e)
	require.Empty(t, w)
	require.True(t, v.(goora.Clob).Valid)
	v, _, e = ConvertOracle([]byte{0, 1, 0}, "GEOMETRY", OracleColumn{DataType: "BLOB"}, nil)
	require.NoError(t, e)
	require.Equal(t, []byte{0, 1, 0}, v.(goora.Blob).Data)
	v, _, e = ConvertOracle([]byte("中文\x00😀"), "TEXT", OracleColumn{DataType: "CLOB"}, converters.NewStringConverter(873))
	require.NoError(t, e)
	require.Equal(t, "中文\x00😀", v.(goora.Clob).String)
	_, _, e = ConvertOracle("中文", "VARCHAR", OracleColumn{DataType: "VARCHAR2", Nullable: true}, converters.NewStringConverter(1))
	require.ErrorContains(t, e, "字符集")
}
