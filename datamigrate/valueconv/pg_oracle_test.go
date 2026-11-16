package valueconv

import (
	"dbgold/datamigrate/source"
	goora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/converters"
	"github.com/stretchr/testify/require"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPGOracleExactNumbers(t *testing.T) {
	for _, raw := range []string{"0", "-9223372036854775808", "12345678901234567890.123456789012345678", "1e125", "1e-130", "-1e-130", "1000000000000000000000000000000000000000000000"} {
		n, err := PGOracleNumber(raw, 0, 0)
		require.NoError(t, err, raw)
		// The bound go-ora NUMBER uses at most 21 digit/exponent bytes plus
		// its optional negative terminator, including tiny nonzero values.
		require.LessOrEqual(t, reflect.ValueOf(n).Elem().FieldByName("data").Len(), 22)
	}
	for _, raw := range []string{"NaN", "Infinity", "1e126", "1e-131", "123456789012345678901234567890123456789", "0.123456789012345678901234567890123456789"} {
		_, err := PGOracleNumber(raw, 0, 0)
		require.Error(t, err, raw)
	}
	for _, tc := range []struct {
		raw  string
		p, s int64
		good bool
	}{{"12300", 3, -2, true}, {"12301", 3, -2, false}, {"99900", 3, -2, true}, {"100000", 3, -2, false}, {".00999", 3, 5, true}, {".01", 3, 5, false}} {
		_, err := PGOracleNumber(tc.raw, tc.p, tc.s)
		if tc.good {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
}

func TestPGOracleTextAndSourceDomain(t *testing.T) {
	for _, dt := range []string{"varchar", "bpchar"} {
		c := source.ColumnInfo{DataType: dt, Length: 8, IsNullable: true}
		v, w, err := ConvertPGOracle("", c, OracleColumn{DataType: "VARCHAR2", Nullable: true}, nil)
		require.NoError(t, err)
		require.Nil(t, v)
		require.Equal(t, "空字符串转 NULL", w)
		c.IsNullable = false
		_, _, err = ConvertPGOracle("", c, OracleColumn{DataType: "VARCHAR2", Nullable: true}, nil)
		require.Error(t, err)
	}
	for _, dt := range []string{"text", "varchar"} {
		v, w, err := ConvertPGOracle("", source.ColumnInfo{DataType: dt}, OracleColumn{DataType: "CLOB"}, nil)
		require.NoError(t, err)
		require.Empty(t, w)
		require.True(t, v.(goora.Clob).Valid)
	}
	v, _, err := ConvertPGOracle("a  ", source.ColumnInfo{DataType: "bpchar", Length: 3}, OracleColumn{DataType: "CHAR", DataLength: 12, CharLength: 3, CharUsed: "C"}, nil)
	require.NoError(t, err)
	require.Equal(t, "a  ", v)
	_, _, err = ConvertPGOracle("四个字符", source.ColumnInfo{DataType: "varchar", Length: 3}, OracleColumn{DataType: "VARCHAR2"}, nil)
	require.ErrorContains(t, err, "源列字符长度")
	_, _, err = ConvertPGOracle("中文", source.ColumnInfo{DataType: "text"}, OracleColumn{DataType: "CLOB"}, converters.NewStringConverter(1))
	require.ErrorContains(t, err, "字符集")
	for _, tc := range []struct {
		dt    string
		value any
	}{{"int2", "32768"}, {"int8", "9223372036854775808"}, {"bool", "2"}, {"numeric", "1e126"}} {
		_, _, err = ConvertPGOracle(tc.value, source.ColumnInfo{DataType: tc.dt}, OracleColumn{DataType: "NUMBER"}, nil)
		require.Error(t, err)
	}
	_, _, err = ConvertPGOracle(strings.Repeat("1", 65), source.ColumnInfo{DataType: "bit", Length: 64}, OracleColumn{DataType: "VARCHAR2"}, nil)
	require.Error(t, err)
	_, _, err = ConvertPGOracle(math.Inf(1), source.ColumnInfo{DataType: "float8"}, OracleColumn{DataType: "BINARY_DOUBLE"}, nil)
	require.Error(t, err)
	v, _, err = ConvertPGOracle([]byte{0, 255, 0}, source.ColumnInfo{DataType: "bytea"}, OracleColumn{DataType: "BLOB"}, nil)
	require.NoError(t, err)
	require.Equal(t, []byte{0, 255, 0}, v.(goora.Blob).Data)
	c := source.ColumnInfo{DataType: "varchar", Length: 2}
	d := OracleColumn{DataType: "NVARCHAR2", CharLength: 4, DataLength: 8, CharUsed: "C"}
	v, _, err = ConvertPGOracle("😀😀", c, d, converters.NewStringConverter(2000))
	require.NoError(t, err)
	require.Equal(t, goora.NVarChar("😀😀"), v)
	d.CharLength = 3
	_, _, err = ConvertPGOracle("😀😀", c, d, converters.NewStringConverter(2000))
	require.ErrorContains(t, err, "代码单元")
	d.CharLength = 4
	_, _, err = ConvertPGOracle("abc", c, d, nil)
	require.ErrorContains(t, err, "源列字符长度")
}

func TestPGOracleTimestampZonesAndBoundaries(t *testing.T) {
	c := source.ColumnInfo{DataType: "timestamptz", DateTimePrecision: 6}
	d := OracleColumn{DataType: "TIMESTAMP(6) WITH TIME ZONE", Scale: 6}
	v, _, err := ConvertPGOracle("2024-11-03 01:30:00.123456-04:00", c, d, nil)
	require.NoError(t, err)
	got := time.Time(v.(goora.TimeStampTZ))
	require.Equal(t, "2024-11-03T05:30:00.123456Z", got.Format(time.RFC3339Nano))
	require.Equal(t, time.UTC, got.Location())
	for _, raw := range []string{"infinity", "-infinity", "0000-01-01 00:00:00+00", "2024-01-01 00:00:00.1234567+00", "2024-01-01 00:00:00"} {
		_, _, err = ConvertPGOracle(raw, c, d, nil)
		require.Error(t, err, raw)
	}
	c = source.ColumnInfo{DataType: "date"}
	v, _, err = ConvertPGOracle("0001-01-01", c, OracleColumn{DataType: "DATE"}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, time.Time(v.(goora.TimeStamp)).Year())
	c = source.ColumnInfo{DataType: "timestamp", DateTimePrecision: 3}
	_, _, err = ConvertPGOracle("2024-01-01 00:00:00.1234", c, OracleColumn{DataType: "TIMESTAMP(6)", Scale: 6}, nil)
	require.Error(t, err)
}
