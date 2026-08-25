package typemap

import (
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
