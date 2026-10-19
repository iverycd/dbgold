package valueconv

import (
	"math/big"
	"testing"
	"time"

	"gitee.com/chunanyong/dm"
	"github.com/stretchr/testify/require"
)

func TestOracleDamengExactNumberBinding(t *testing.T) {
	for _, raw := range []string{"99999999999999999999999999999999999999", "-123456789012345678.1234567890123456789", "-1234567890123456789.123456789012345678", "-12345678901234567890.123456789012345678", "1e-128", "0", "1e125"} {
		v, e := OracleDameng(raw, "NUMBER")
		require.NoError(t, e, raw)
		dec, ok := v.(*dm.DmDecimal)
		require.True(t, ok)
		want, ok := new(big.Rat).SetString(raw)
		require.True(t, ok)
		got, ok := new(big.Rat).SetString(dec.String())
		require.True(t, ok)
		require.Zero(t, want.Cmp(got), raw)
	}
	for _, raw := range []string{"1e-129", "1e126", "-1e124", "-1234567890123456789.1234567890123456789", "123456789012345678901234567890123456789", "not a number"} {
		_, e := OracleDameng(raw, "NUMBER")
		require.Error(t, e)
	}
	_, e := OracleDameng(float64(1.5), "NUMBER")
	require.Error(t, e)
}

func TestOracleDamengTimestampAndIntervals(t *testing.T) {
	for _, tc := range []struct{ raw, kind, want string }{
		{"2024-01-02 03:04:05", "DATE", "2024-01-02 03:04:05"},
		{"2024-01-02 03:04:05.123456789", "TIMESTAMP(9)", "2024-01-02 03:04:05.123456789"},
		{"2024-01-02 03:04:05.123456789 -05:00", "TIMESTAMP(9) WITH LOCAL TIME ZONE", "2024-01-02 03:04:05.123456789 -05:00"},
	} {
		v, e := OracleDameng(tc.raw, tc.kind)
		require.NoError(t, e)
		require.Equal(t, tc.want, v)
	}
	v, e := OracleDameng(time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.FixedZone("", 19800)), "TIMESTAMP(9) WITH TIME ZONE")
	require.NoError(t, e)
	require.Equal(t, "2024-01-02 03:04:05.123456789 +05:30", v)
	for _, tc := range []struct{ raw, kind string }{{"-000000002-03", "INTERVAL YEAR(9) TO MONTH"}, {"-000000002 12:23:34.456789000", "INTERVAL DAY(9) TO SECOND(6)"}} {
		v, e := OracleDameng(tc.raw, tc.kind)
		require.NoError(t, e)
		require.Contains(t, v.(interface{ String() string }).String(), "-")
	}
	_, e = OracleDameng("+1 01:02:03.123456789", "INTERVAL DAY(2) TO SECOND(9)")
	require.ErrorContains(t, e, "禁止截断")
	_, e = OracleDameng("1582-10-10 12:00:00", "DATE")
	require.ErrorContains(t, e, "禁止静默修改")
	_, e = OracleDameng("+00 00:00:00.000249", "INTERVAL DAY(2) TO SECOND(6)")
	require.ErrorContains(t, e, "无法精确保留")
	_, e = OracleDameng(time.Date(2024, 1, 2, 3, 4, 5, 0, time.FixedZone("", 19801)), "TIMESTAMP(6) WITH TIME ZONE")
	require.ErrorContains(t, e, "整分钟")
	for _, raw := range []string{"2024-01-02 03:04:05.", "2024-01-02 03:04:05.1234567890", "-0044-01-02 03:04:05"} {
		_, err := OracleDameng(raw, "TIMESTAMP(9)")
		require.Error(t, err)
	}
	value, err := OracleDameng("+2024-01-02 03:04:05", "DATE")
	require.NoError(t, err)
	require.Equal(t, "2024-01-02 03:04:05", value)
	require.Equal(t, "TO_TIMESTAMP_TZ(?, 'YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM')", OracleDamengPlaceholder("TIMESTAMP(9) WITH TIME ZONE"))
}
