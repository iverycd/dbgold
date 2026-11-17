package valueconv

import (
	"dbgold/datamigrate/source"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSQLServerDecimalDigits(t *testing.T) {
	for _, tt := range []struct {
		value             any
		integer, fraction int
	}{
		{nil, 0, 0}, {"0E-120", 0, 0}, {"-12.0300", 2, 2}, {[]byte("0.001000"), 0, 3}, {"123000e-2", 4, 0}, {"4.2e36", 37, 0},
	} {
		integer, fraction, err := SQLServerDecimalDigits(tt.value)
		require.NoError(t, err)
		require.Equal(t, tt.integer, integer)
		require.Equal(t, tt.fraction, fraction)
	}
	_, _, err := SQLServerDecimalDigits("NaN")
	require.Error(t, err)
}

func TestSQLServerCrossSourceValues(t *testing.T) {
	for _, tc := range []struct {
		src         string
		col         source.ColumnInfo
		value, want any
		bad         bool
	}{
		{"oracle", source.ColumnInfo{DataType: "NUMBER", SQLServerPrecision: 38, SQLServerScale: 2}, "999999999999999999999999999999999999.21", "999999999999999999999999999999999999.21", false},
		{"oracle", source.ColumnInfo{DataType: "NUMBER", SQLServerPrecision: 38, SQLServerScale: 2}, "0.011", nil, true},
		{"oracle", source.ColumnInfo{DataType: "DATE"}, "2026-11-17 12:13:14", "2026-11-17T12:13:14", false},
		{"oracle", source.ColumnInfo{DataType: "DATE"}, "-0044-03-15 00:00:00", nil, true},
		{"oracle", source.ColumnInfo{DataType: "TIMESTAMP(9) WITH TIME ZONE", DateTimePrecision: 9}, "2026-11-17 12:13:14.123456700 +08:00", "2026-11-17T12:13:14.1234567+08:00", false},
		{"oracle", source.ColumnInfo{DataType: "TIMESTAMP(9)", DateTimePrecision: 9}, "2026-11-17 12:13:14.123456789", nil, true},
		{"postgres", source.ColumnInfo{DataType: "timestamptz", DateTimePrecision: 6}, "0001-01-01 00:00:00+01", nil, true},
		{"postgres", source.ColumnInfo{DataType: "date"}, "infinity", nil, true},
		{"postgres", source.ColumnInfo{DataType: "time", DateTimePrecision: 6}, "24:00:00", nil, true},
		{"postgres", source.ColumnInfo{DataType: "float4"}, "1e-40", nil, true},
		{"postgres", source.ColumnInfo{DataType: "float8"}, "1e-999", nil, true},
		{"postgres", source.ColumnInfo{DataType: "bool"}, true, true, false},
		{"postgres", source.ColumnInfo{DataType: "varchar", Length: 4}, "中😀\x00 ", "中😀\x00 ", false},
		{"postgres", source.ColumnInfo{DataType: "bytea"}, []byte{}, []byte{}, false},
		{"postgres", source.ColumnInfo{DataType: "timestamp", DateTimePrecision: 6}, time.Date(2026, 11, 17, 12, 13, 14, 5000, time.FixedZone("x", 3600)), "2026-11-17T12:13:14.000005", false},
	} {
		got, err := ConvertSQLServerForSource(tc.value, tc.col, tc.src)
		if tc.bad {
			require.Error(t, err, "%s %s %v", tc.src, tc.col.DataType, tc.value)
		} else {
			require.NoError(t, err, "%s %s %v", tc.src, tc.col.DataType, tc.value)
			require.Equal(t, tc.want, got)
		}
	}
}
