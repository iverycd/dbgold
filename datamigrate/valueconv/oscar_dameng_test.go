package valueconv

import (
	"math"
	"testing"

	"gitee.com/chunanyong/dm"
	"github.com/stretchr/testify/require"
)

func TestOscarDamengConvertsTypedWireValues(t *testing.T) {
	for _, test := range []struct {
		typ         string
		value, want any
	}{
		{"BOOLEAN", true, int64(1)},
		{"BOOL", false, int64(0)},
		{"BIT", "00101", []byte{0, 0, 1, 0, 1}},
		{"BIT VARYING", "", []byte{}},
		{"VARBINARY", []byte{0, 17, 255}, []byte{0, 17, 255}},
		{"DATE", "0024-01-02", "0024-01-02"},
		{"TIME", "15:04:05.012345", "2000-01-01 15:04:05.012345000"},
		{"TIMETZ", "15:04:05.012345+8:00:00", "15:04:05.012345 +08:00"},
		{"TIMESTAMP", "2026-01-01T15:04:05.012345678", "2026-01-01 15:04:05.012345678"},
		{"TIMESTAMPTZ", "2026-01-01T15:04:05.012345678-1230", "2026-01-01 15:04:05.012345678 -12:30"},
		{"INTERVAL YEAR TO MONTH", "-000000023-07", "-000000023-07"},
		{"INTERVAL DAY TO SECOND", "+1 02:03:04.000249000", "+1 02:03:04.000249"},
		{"VARCHAR", "", ""},
		{"XMLTYPE", nil, nil},
	} {
		got, err := OscarDameng(test.value, test.typ)
		require.NoError(t, err, test.typ)
		require.Equal(t, test.want, got, test.typ)
	}
	const precise = "1234567890123456789.1234567890123456789"
	got, err := OscarDameng(precise, "NUMERIC")
	require.NoError(t, err)
	require.Equal(t, precise, got.(*dm.DmDecimal).String())
	const arrayText = "[0:2]={中文,NULL,空}"
	got, err = OscarDameng(arrayText, "ARRAY")
	require.NoError(t, err)
	text, err := got.(*dm.DmClob).ReadString(1, len([]rune(arrayText)))
	require.NoError(t, err)
	require.Equal(t, arrayText, text)
}

func TestOscarDamengRejectsLossyValues(t *testing.T) {
	for _, test := range []struct {
		typ   string
		value any
	}{
		{"BIT", "not a bit string"},
		{"NUMERIC", float64(0.1)},
		{"NUMERIC", "NaN"},
		{"TIMETZ", "15:04:05.000000001 +08:00"},
		{"TIMESTAMPTZ", "2026-01-01 15:04:05"},
		{"TIMESTAMPTZ", "2026-01-01 15:04:05 -13:00"},
		{"TIMESTAMPTZ", "2026-01-01 15:04:05 +08:00:01"},
		{"DATE", "2026-01-01 00:00:01"},
		{"INTERVAL YEAR TO MONTH", "1-13"},
		{"INTERVAL DAY TO SECOND", "1 00:00:00.0000001"},
		{"FLOAT", math.Inf(1)},
	} {
		_, err := OscarDameng(test.value, test.typ)
		require.Error(t, err, test.typ)
	}
}
