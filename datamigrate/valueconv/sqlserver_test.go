package valueconv

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSQLServerStrictValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		c    source.ColumnInfo
		v    interface{}
		bad  bool
	}{
		{"unsigned max", source.ColumnInfo{DataType: "bigint", FormattedType: "bigint unsigned"}, "18446744073709551615", false},
		{"unsigned overflow", source.ColumnInfo{DataType: "bigint", FormattedType: "bigint unsigned"}, "18446744073709551616", true},
		{"signed overflow", source.ColumnInfo{DataType: "tinyint"}, 128, true},
		{"decimal exact", source.ColumnInfo{DataType: "decimal", Precision: 38, Scale: 6}, "12345678901234567890123456789012.123456", false},
		{"decimal rounding", source.ColumnInfo{DataType: "decimal", Precision: 5, Scale: 2}, "1.001", true},
		{"decimal overflow", source.ColumnInfo{DataType: "decimal", Precision: 5, Scale: 2}, "1000.00", true},
		{"unicode", source.ColumnInfo{DataType: "varchar", Length: 2}, "中😀", false},
		{"text overflow", source.ColumnInfo{DataType: "varchar", Length: 1}, "中文", true},
		{"empty string", source.ColumnInfo{DataType: "varchar", Length: 1}, "", false},
		{"null", source.ColumnInfo{DataType: "varchar", Length: 1, IsNullable: true}, nil, false},
		{"null required", source.ColumnInfo{DataType: "varchar", Length: 1}, nil, true},
		{"NUL", source.ColumnInfo{DataType: "varchar", Length: 3}, "a\x00b", false},
		{"binary", source.ColumnInfo{DataType: "varbinary", Length: 3}, []byte{0, 255, 0}, false},
		{"zero date", source.ColumnInfo{DataType: "date"}, "0000-00-00", true},
		{"partial zero", source.ColumnInfo{DataType: "date"}, "2020-00-01", true},
		{"invalid leap", source.ColumnInfo{DataType: "date"}, "2023-02-29", true},
		{"early date", source.ColumnInfo{DataType: "date"}, "1000-01-01", false},
		{"datetime precision", source.ColumnInfo{DataType: "datetime", DateTimePrecision: 6}, "2024-02-29 12:30:45.123456", false},
		{"datetime rounding", source.ColumnInfo{DataType: "datetime", DateTimePrecision: 3}, "2024-02-29 12:30:45.123456", true},
		{"negative duration", source.ColumnInfo{DataType: "time", DateTimePrecision: 6}, "-01:00:00", true},
		{"long duration", source.ColumnInfo{DataType: "time", DateTimePrecision: 6}, "24:00:00", true},
		{"time", source.ColumnInfo{DataType: "time", DateTimePrecision: 6}, "23:59:59.999999", false},
		{"bit64", source.ColumnInfo{DataType: "bit", Precision: 64}, []byte{255, 255, 255, 255, 255, 255, 255, 255}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, e := ConvertSQLServer(tt.v, tt.c)
			if tt.bad {
				require.Error(t, e)
			} else {
				require.NoError(t, e)
			}
		})
	}
}
