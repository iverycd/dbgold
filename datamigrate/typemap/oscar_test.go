package typemap

import (
	"fmt"
	"testing"

	"dbgold/datamigrate/source"
)

func TestOscarTypeMappings(t *testing.T) {
	cases := []struct {
		name  string
		mapFn Mapper
		col   source.ColumnInfo
		want  string
	}{
		{"mysql json", MySQLToOscar, source.ColumnInfo{DataType: "json"}, "CLOB"},
		{"mysql blob", MySQLToOscar, source.ColumnInfo{DataType: "longblob"}, "BLOB"},
		{"sqlserver uuid", SQLServerToOscar, source.ColumnInfo{DataType: "uniqueidentifier"}, "VARCHAR(36)"},
		{"sqlserver money", SQLServerToOscar, source.ColumnInfo{DataType: "money"}, "DECIMAL(19,4)"},
		{"oracle date", OracleToOscar, source.ColumnInfo{DataType: "DATE"}, "TIMESTAMP"},
		{"oracle number", OracleToOscar, source.ColumnInfo{DataType: "NUMBER", Precision: 8}, "INTEGER"},
		{"dameng varbinary", DaMengToOscar, source.ColumnInfo{DataType: "VARBINARY", Length: 64}, "VARBINARY(64)"},
		{"dameng boolean", DaMengToOscar, source.ColumnInfo{DataType: "BOOLEAN"}, "BOOLEAN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mapFn(tc.col, false, false); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	for _, src := range []string{"mysql", "sqlserver", "oracle", "dameng"} {
		if _, ok := Get(src, "oscar"); !ok {
			t.Fatalf("missing %s -> oscar registration", src)
		}
	}
}

func TestMySQLToOscarLargeVarcharCapacity(t *testing.T) {
	for _, dataType := range []string{"varchar", "enum"} {
		for _, useNvarchar2 := range []bool{false, true} {
			for _, length := range []int64{0, 8000, 8001, 10000} {
				t.Run(fmt.Sprintf("%s/nvarchar2=%t/length=%d", dataType, useNvarchar2, length), func(t *testing.T) {
					want := "CLOB"
					if length <= 8000 {
						name := "VARCHAR"
						if useNvarchar2 {
							name = "NVARCHAR2"
						}
						n := length
						if n <= 0 {
							n = 255
						}
						want = fmt.Sprintf("%s(%d)", name, n)
					}
					if got := MySQLToOscar(source.ColumnInfo{DataType: dataType, Length: length}, false, useNvarchar2); got != want {
						t.Fatalf("got %q, want %q", got, want)
					}
				})
			}
		}
	}
}

func TestMySQLToOscarUnsignedBitCapacity(t *testing.T) {
	for _, tc := range []struct {
		name string
		col  source.ColumnInfo
		want string
	}{
		{"bit1", source.ColumnInfo{DataType: "bit", Precision: 1}, "BIGINT"},
		{"bit63", source.ColumnInfo{DataType: "bit", Precision: 63}, "BIGINT"},
		{"bit64", source.ColumnInfo{DataType: "bit", Precision: 64}, "DECIMAL(20,0)"},
		{"original declaration wins", source.ColumnInfo{DataType: "bit", FormattedType: "BIT(64)", Precision: 1}, "DECIMAL(20,0)"},
		{"original small declaration wins", source.ColumnInfo{DataType: "bit", FormattedType: "bit(1)", Precision: 64}, "BIGINT"},
		{"legacy length", source.ColumnInfo{DataType: "bit", Length: 64}, "DECIMAL(20,0)"},
		{"unknown width", source.ColumnInfo{DataType: "bit"}, "BIGINT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MySQLToOscar(tc.col, false, false); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
