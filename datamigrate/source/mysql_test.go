package source

import "testing"

func TestNormalizeMySQLValues(t *testing.T) {
	values := []interface{}{[]byte("text\x00value"), []byte{0, 1}, []byte{2}, int64(3)}
	normalizeMySQLValues(values, []string{"VARCHAR", "BLOB", "BIT", "BIGINT"})
	if values[0] != "textvalue" {
		t.Fatalf("text value=%q", values[0])
	}
	if _, ok := values[1].([]byte); !ok {
		t.Fatal("BLOB must remain []byte")
	}
	if _, ok := values[2].([]byte); !ok {
		t.Fatal("BIT must remain []byte")
	}
}
