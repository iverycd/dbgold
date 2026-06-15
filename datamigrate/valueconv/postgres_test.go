package valueconv

import (
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// TestPostgresConvert_MySQL 验证 MySQL 中立值 → PG 形态,
// 与 mysql.go ReadPage 旧的 PG 定制逻辑一致。
func TestPostgresConvert_MySQL(t *testing.T) {
	c := NewPostgres()
	// BIT: []byte{0x00} → hex "00" → [1:] = "0"
	if got := c.Convert([]byte{0x00}, "mysql", "BIT"); got != "0" {
		t.Errorf("mysql BIT 0x00: got %v want %q", got, "0")
	}
	if got := c.Convert([]byte{0x01}, "mysql", "BIT"); got != "1" {
		t.Errorf("mysql BIT 0x01: got %v want %q", got, "1")
	}
	// GEOMETRY: hex.EncodeToString("0000000001020304") 去前 8 字符 = "01020304"
	geo := []byte{0, 0, 0, 0, 1, 2, 3, 4}
	if got := c.Convert(geo, "mysql", "GEOMETRY"); got != "01020304" {
		t.Errorf("mysql GEOMETRY: got %v want %q", got, "01020304")
	}
	// 普通 []byte 不变(字符串清洗仍在 Reader 做)
	if got := c.Convert([]byte("abc"), "mysql", "VARCHAR"); string(got.([]byte)) != "abc" {
		t.Errorf("mysql VARCHAR passthrough failed: %v", got)
	}
	// nil 透传
	if got := c.Convert(nil, "mysql", "BIT"); got != nil {
		t.Errorf("nil should pass through, got %v", got)
	}
}

// TestPostgresConvert_SQLServer 验证 SqlServer 中立值 → PG 形态。
func TestPostgresConvert_SQLServer(t *testing.T) {
	c := NewPostgres()
	ts := time.Date(2024, 3, 15, 9, 30, 45, 123456000, time.UTC)
	if got := c.Convert(ts, "sqlserver", "DATETIME"); got != "2024-03-15 09:30:45.123456" {
		t.Errorf("sqlserver DATETIME: got %v", got)
	}
	if got := c.Convert(ts, "sqlserver", "TIME"); got != "09:30:45.123456" {
		t.Errorf("sqlserver TIME: got %v", got)
	}
	if got := c.Convert([]byte("12.50"), "sqlserver", "MONEY"); got != "12.50" {
		t.Errorf("sqlserver MONEY: got %v", got)
	}
	if got := c.Convert([]byte("<x/>"), "sqlserver", "XML"); got != "<x/>" {
		t.Errorf("sqlserver XML: got %v", got)
	}
	var uid mssql.UniqueIdentifier
	_ = uid.Scan("6F9619FF-8B86-D011-B42D-00C04FC964FF")
	if got := c.Convert(uid, "sqlserver", "UNIQUEIDENTIFIER"); got != uid.String() {
		t.Errorf("sqlserver UUID: got %v", got)
	}
	// BIT 在 SqlServer 已由 Reader 转 int64,Converter 不动
	if got := c.Convert(int64(1), "sqlserver", "BIT"); got != int64(1) {
		t.Errorf("sqlserver BIT passthrough: got %v", got)
	}
}

// TestPostgresConvert_DaMengOracle 验证达梦/Oracle 日期格式化。
func TestPostgresConvert_DaMengOracle(t *testing.T) {
	c := NewPostgres()
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := c.Convert(ts, "dameng", "TIMESTAMP"); got != "2024-01-02 03:04:05" {
		t.Errorf("dameng TIMESTAMP: got %v", got)
	}
	if got := c.Convert(ts, "oracle", "DATE"); got != "2024-01-02 03:04:05" {
		t.Errorf("oracle DATE: got %v", got)
	}
	if got := c.Convert(ts, "oracle", "TIMESTAMP WITH TIME ZONE"); got != "2024-01-02 03:04:05" {
		t.Errorf("oracle TIMESTAMPTZ: got %v", got)
	}
}
