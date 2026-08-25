package valueconv

import (
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
