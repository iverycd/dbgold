package valueconv

import (
	"bytes"
	"dbgold/datamigrate/source"
	"testing"
	"time"
)

func TestPGMySQLTimeAndBinary(t *testing.T) {
	in := time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.FixedZone("+8", 8*3600))
	v, e := PGMySQLValue(in, source.ColumnInfo{DataType: "timestamptz"})
	if e != nil || v != "2024-01-01 19:04:05.123456" {
		t.Fatalf("%v %v", v, e)
	}
	b := []byte{0, 255, 1}
	v, e = PGMySQLValue(b, source.ColumnInfo{DataType: "bytea"})
	if e != nil || !bytes.Equal(v.([]byte), b) {
		t.Fatal(v, e)
	}
}
func TestPGMySQLRejectLossyData(t *testing.T) {
	for _, tc := range []struct {
		v any
		c source.ColumnInfo
	}{{"12:00:00.123456789", source.ColumnInfo{DataType: "time"}}, {"NaN", source.ColumnInfo{DataType: "numeric", Precision: 10, Scale: 2}}, {"1.001", source.ColumnInfo{DataType: "numeric", Precision: 10, Scale: 2}}, {"1000", source.ColumnInfo{DataType: "numeric", Precision: 3}}, {"infinity", source.ColumnInfo{DataType: "timestamp"}}, {"0999-01-01", source.ColumnInfo{DataType: "date"}}, {"32768", source.ColumnInfo{DataType: "int2"}}, {nil, source.ColumnInfo{DataType: "int4"}}, {`{"n":1e999}`, source.ColumnInfo{DataType: "json"}}} {
		if _, e := PGMySQLValue(tc.v, tc.c); e == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}
