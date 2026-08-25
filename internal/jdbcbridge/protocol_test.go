package jdbcbridge

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

func TestEncodeRowsValueTags(t *testing.T) {
	rows := [][]any{{nil, true, int64(-2), uint64(math.MaxUint64), 1.25, Decimal("123.4500"), "中文", []byte{0, 1}, Date("2026-08-25"), Time("12:34:56"), Timestamp("2026-08-25 12:34:56")}}
	body, err := encodeRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	d := newDecoder(body)
	n, _ := d.uint32()
	columns, _ := d.uint32()
	if n != 1 || columns != uint32(len(rows[0])) {
		t.Fatalf("unexpected dimensions: %d x %d", n, columns)
	}
	wantTags := []byte{valueNull, valueBool, valueInt64, valueUint64, valueFloat64, valueDecimal, valueString, valueBytes, valueDate, valueTime, valueTimestamp}
	for i, want := range wantTags {
		got, err := d.byte()
		if err != nil || got != want {
			t.Fatalf("tag %d = %d, want %d (err=%v)", i, got, want, err)
		}
		switch got {
		case valueNull:
		case valueBool:
			_, _ = d.byte()
		case valueInt64, valueFloat64:
			_, _ = d.uint64()
		case valueBytes:
			length, _ := d.uint32()
			d.Seek(int64(length), 1)
		default:
			_, _ = d.string()
		}
	}
}

func TestEncodeRowsRejectsUnsupportedAndNeverFormatsSecrets(t *testing.T) {
	secret := "p@ssword-do-not-leak"
	_, err := encodeRows([][]any{{struct{ Password string }{Password: secret}}})
	if err == nil {
		t.Fatal("expected unsupported type error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked value: %v", err)
	}
}

func TestFrameEncodingIsBigEndian(t *testing.T) {
	e := &encoder{}
	e.uint32(0x01020304)
	if !bytes.Equal(e.Bytes(), []byte{1, 2, 3, 4}) {
		t.Fatalf("unexpected uint32 bytes: %v", e.Bytes())
	}
	if got := binary.BigEndian.Uint32(e.Bytes()); got != 0x01020304 {
		t.Fatalf("unexpected decoded value: %x", got)
	}
}
