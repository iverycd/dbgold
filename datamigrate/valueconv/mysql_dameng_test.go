package valueconv

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMySQLDamengDate(t *testing.T) {
	for _, tc := range []struct {
		name, typ   string
		value, want interface{}
	}{
		{"zero date", "DATE", "0000-00-00", "1900-01-01"},
		{"zero datetime", "DATETIME", []byte("0000-00-00 00:00:00"), "1900-01-01 00:00:00.000000"},
		{"zero timestamp fraction", "TIMESTAMP", "0000-00-00 00:00:00.000000", "1900-01-01 00:00:00.000000"},
		{"partial year", "DATE", "0000-12-15", "1900-01-01"},
		{"partial month", "DATETIME", "2024-00-15 12:34:56.123456", "1900-01-01 00:00:00.000000"},
		{"partial day", "TIMESTAMP", "2024-12-00 12:34:56", "1900-01-01 00:00:00.000000"},
		{"leap day", "DATE", "2024-02-29", "2024-02-29"},
		{"microseconds", "DATETIME", []byte("2024-02-29 12:34:56.123456"), "2024-02-29 12:34:56.123456"},
		{"one digit fraction", "timestamp", "2024-02-29 12:34:56.1", "2024-02-29 12:34:56.100000"},
		{"valid year one", "DATE", "0001-01-01", "0001-01-01"},
		{"valid Go zero time", "TIMESTAMP", time.Time{}, "0001-01-01 00:00:00.000000"},
		{"null", "DATETIME", nil, nil},
		{"text", "VARCHAR", "0000-00-00 00:00:00", "0000-00-00 00:00:00"},
		{"text bytes", "VARCHAR", []byte("0000-00-00"), []byte("0000-00-00")},
		{"time midnight", "TIME", "00:00:00", "00:00:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MySQLDamengDate(tc.value, tc.typ)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestMySQLDamengInvalidDates(t *testing.T) {
	for _, tc := range []struct {
		typ   string
		value interface{}
	}{
		{"DATE", "2023-02-29"},
		{"DATE", "2024-02-30"},
		{"DATE", "0000-00-00 invalid"},
		{"DATETIME", "0000-00-00 25:00:00"},
		{"DATETIME", "2024-02-29 25:00:00"},
		{"TIMESTAMP", "2024-02-29 12:34:56.1234567"},
		{"TIMESTAMP", time.Date(2024, 1, 1, 0, 0, 0, 1, time.UTC)},
		{"DATE", ""},
		{"DATE", 0},
	} {
		_, err := MySQLDamengDate(tc.value, tc.typ)
		require.Error(t, err)
	}
}
