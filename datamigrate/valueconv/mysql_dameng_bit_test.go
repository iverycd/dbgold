package valueconv

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMySQLDamengBitPreservesUnsignedSnapshotAndBinlogValues(t *testing.T) {
	converter := NewDaMeng()
	for _, tc := range []struct {
		value uint64
		want  any
	}{
		{0, int64(0)}, {1, int64(1)}, {255, int64(255)},
		{math.MaxInt64, int64(math.MaxInt64)},
		{uint64(1) << 63, "9223372036854775808"},
		{math.MaxUint64, "18446744073709551615"},
	} {
		raw := make([]byte, 8)
		binary.BigEndian.PutUint64(raw, tc.value)
		for _, input := range []any{raw, int64(tc.value), tc.value} {
			require.Equal(t, tc.want, converter.Convert(input, "mysql", "BIT"), "value=%d input=%T", tc.value, input)
		}
	}
	require.Nil(t, converter.Convert(nil, "mysql", "BIT"))
	require.Equal(t, int64(1), converter.Convert([]byte{1}, "mysql", "BIT"))
}
