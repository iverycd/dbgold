package valueconv

import (
	"math"
	"reflect"
	"testing"
	"time"

	"dbgold/internal/jdbcbridge"
)

func TestOscarValueConverter(t *testing.T) {
	c := NewOscar()
	if got := c.Convert([]byte{1}, "mysql", "BIT"); got != int64(1) {
		t.Fatalf("BIT conversion = %#v", got)
	}
	if got := c.Convert([]byte{0, 0, 0, 0, 0xaa, 0xbb}, "mysql", "GEOMETRY"); got != "aabb" {
		t.Fatalf("GEOMETRY conversion = %#v", got)
	}
	if got := c.Convert([]byte("123.4500"), "sqlserver", "MONEY"); got != jdbcbridge.Decimal("123.4500") {
		t.Fatalf("MONEY conversion = %#v", got)
	}
	moment := time.Date(2026, 8, 25, 12, 34, 56, 123000000, time.UTC)
	if got := c.Convert(moment, "dameng", "DATE"); got != jdbcbridge.Date("2026-08-25") {
		t.Fatalf("DATE conversion = %#v", got)
	}
	if got := c.Convert(moment, "oracle", "DATE"); got != jdbcbridge.Timestamp("2026-08-25 12:34:56.123") {
		t.Fatalf("Oracle DATE conversion = %#v", got)
	}
	if got := c.Convert(moment, "oracle", "TIMESTAMP"); got != jdbcbridge.Timestamp("2026-08-25 12:34:56.123") {
		t.Fatalf("TIMESTAMP conversion = %#v", got)
	}
}

func TestOscarMySQLBitUnsignedBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value []byte
		want  interface{}
	}{
		{"zero", []byte{0}, int64(0)},
		{"one", []byte{1}, int64(1)},
		{"multiple bits", []byte{0x01, 0x02}, int64(258)},
		{"signed maximum", []byte{0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, int64(math.MaxInt64)},
		{"high bit", []byte{0x80, 0, 0, 0, 0, 0, 0, 0}, jdbcbridge.Decimal("9223372036854775808")},
		{"unsigned maximum", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, jdbcbridge.Decimal("18446744073709551615")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]byte(nil), tc.value...)
			got := NewOscar().Convert(tc.value, "mysql", "BIT")
			if got != tc.want {
				t.Fatalf("got %#v (%T), want %#v (%T)", got, got, tc.want, tc.want)
			}
			if !reflect.DeepEqual(tc.value, original) {
				t.Fatal("source bytes were modified")
			}
		})
	}
	if got := NewOscar().Convert(nil, "mysql", "BIT"); got != nil {
		t.Fatalf("NULL BIT converted to %#v", got)
	}
}
