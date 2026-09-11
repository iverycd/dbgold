package valueconv

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestSQLServerDamengValues(t *testing.T) {
	guid := []byte{0x33, 0x22, 0x11, 0x00, 0x55, 0x44, 0x77, 0x66, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	v, e := SQLServerDameng(guid, "UNIQUEIDENTIFIER")
	require.NoError(t, e)
	require.Equal(t, "00112233-4455-6677-8899-AABBCCDDEEFF", v)
	for _, dt := range []string{"DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY"} {
		v, e = SQLServerDameng([]byte("12345678901234567890.123456789012345678"), dt)
		require.NoError(t, e)
		require.Equal(t, "12345678901234567890.123456789012345678", fmt.Sprint(v))
	}
	raw := []byte{0, 1, 0, 255}
	v, e = SQLServerDameng(raw, "TIMESTAMP")
	require.NoError(t, e)
	require.Equal(t, raw, v)
	v, e = SQLServerDameng([]byte("中文\x00😀"), "NVARCHAR")
	require.NoError(t, e)
	require.Equal(t, "中文\x00😀", v)
	v, e = SQLServerDameng(nil, "BIT")
	require.NoError(t, e)
	require.Nil(t, v)
	v, e = SQLServerDameng(true, "BIT")
	require.NoError(t, e)
	require.Equal(t, int64(1), v)
	moment := time.Date(2024, 2, 29, 23, 34, 56, 123456000, time.FixedZone("offset", -210*60))
	v, e = SQLServerDameng(moment, "DATETIMEOFFSET")
	require.NoError(t, e)
	require.Equal(t, "2024-02-29 23:34:56.123456 -03:30", v)
	require.Contains(t, SQLServerDamengPlaceholder("DATETIMEOFFSET"), "TO_TIMESTAMP_TZ")
	_, e = SQLServerDameng(moment.Add(100*time.Nanosecond), "DATETIME2")
	require.Error(t, e)
	_, e = SQLServerDameng([]byte{1}, "UNIQUEIDENTIFIER")
	require.Error(t, e)
}
