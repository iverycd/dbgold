package cdc

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"dbgold/datamigrate/valueconv"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestDamengBit64BinlogMatchesSnapshotConversion(t *testing.T) {
	table := &TableInfo{Name: "Bits", Columns: []string{"Value"}, ColumnTypes: []string{"BIT"}}
	for _, value := range []uint64{0, 1, math.MaxInt64, uint64(1) << 63, math.MaxUint64} {
		raw := make([]byte, 8)
		binary.BigEndian.PutUint64(raw, value)
		snapshot := valueconv.NewDaMeng().Convert(raw, "mysql", "BIT")
		binlog, err := convertDamengBinlogRow(table, []any{int64(value)})
		require.NoError(t, err)
		require.Equal(t, snapshot, binlog[0], "BIT(64) value=%d", value)
	}
}

func TestConvertDamengBinlogRowMatchesSnapshotValues(t *testing.T) {
	table := &TableInfo{Name: "MixedCase", Columns: []string{"E", "S", "U8", "U24", "U64", "D", "DT", "TXT", "BIN", "BIT"},
		ColumnTypes:       []string{"ENUM", "SET", "TINYINT", "MEDIUMINT", "BIGINT", "DATE", "DATETIME", "VARCHAR", "BLOB", "BIT"},
		ColumnDefinitions: []string{`enum('a,b','it''s','slash\\')`, `set('中文','x','y')`, "tinyint unsigned", "mediumint unsigned", "bigint unsigned"}}
	input := []any{int64(2), int64(5), int8(-1), int32(-1), int64(-1), "2024-00-01", "2024-02-29 23:59:59.123456", "a\x00b", []byte{0, 255}, int64(1)}
	got, err := convertDamengBinlogRow(table, input)
	require.NoError(t, err)
	require.Equal(t, []any{"it's", "中文,y", int64(255), int64(16777215), "18446744073709551615", "1900-01-01", "2024-02-29 23:59:59.123456", "ab", []byte{0, 255}, int64(1)}, got)
	require.Equal(t, "a\x00b", input[7], "conversion must not change before image")
}

func TestDamengEnumSetEscapesAndBounds(t *testing.T) {
	members, err := mysqlEnumSetMembers(`enum('a,b','it\'s','quote''d','back\\slash','line\nfeed','')`, "ENUM")
	require.NoError(t, err)
	require.Equal(t, []string{"a,b", "it's", "quote'd", `back\slash`, "line\nfeed", ""}, members)
	_, err = damengEnumSetValue(int64(3), `enum('a','b')`, "ENUM")
	require.ErrorContains(t, err, "超出")
	_, err = damengEnumSetValue(int64(4), `set('a','b')`, "SET")
	require.ErrorContains(t, err, "超出")
	got, err := damengEnumSetValue(int64(0), `enum('a')`, "ENUM")
	require.NoError(t, err)
	require.Equal(t, "", got)
	labels := make([]string, 64)
	for i := range labels {
		labels[i] = "'x'"
	}
	got, err = damengEnumSetValue(int64(math.MinInt64), "set("+strings.Join(labels, ",")+")", "SET")
	require.NoError(t, err)
	require.Equal(t, "x", got)
	for _, definition := range []string{"", "enum()", "enum('a',)", "enum('a)", "enum(a)", "set('a')"} {
		_, err = mysqlEnumSetMembers(definition, "ENUM")
		require.Error(t, err, definition)
	}
}

func TestDamengBinlogRowRejectsIncompleteMetadataAndInvalidDate(t *testing.T) {
	_, err := convertDamengBinlogRow(&TableInfo{Columns: []string{"d"}}, []any{"x"})
	require.Error(t, err)
	_, err = convertDamengBinlogRow(&TableInfo{Name: "T", Columns: []string{"d"}, ColumnTypes: []string{"DATE"}}, []any{"2024-02-31"})
	require.ErrorContains(t, err, "T.d")
	_, err = convertDamengBinlogRow(&TableInfo{Name: "T", Columns: []string{"e"}, ColumnTypes: []string{"ENUM"}}, []any{int64(1)})
	require.ErrorContains(t, err, "COLUMN_TYPE")
	got, err := convertDamengBinlogRow(&TableInfo{Columns: []string{"d", "e"}, ColumnTypes: []string{"DATE", "ENUM"}}, []any{nil, nil})
	require.NoError(t, err)
	require.Equal(t, []any{nil, nil}, got)
}

func TestDamengUnsignedWidths(t *testing.T) {
	for _, test := range []struct {
		dt    string
		input any
		want  any
	}{
		{"SMALLINT", int16(-1), int64(65535)}, {"INT", int32(-1), int64(4294967295)},
		{"BIGINT", uint64(math.MaxUint64), "18446744073709551615"}, {"BIGINT", int64(7), int64(7)},
	} {
		got, err := damengUnsignedValue(test.input, test.dt)
		require.NoError(t, err)
		require.Equal(t, test.want, got)
	}
}

func TestDamengBinlogCharacterSetsPreserveSnapshotText(t *testing.T) {
	gbk, err := simplifiedchinese.GBK.NewEncoder().String("中文")
	require.NoError(t, err)
	table := &TableInfo{Columns: []string{"GBK", "LATIN", "UTF16", "BINARY"}, ColumnTypes: []string{"VARCHAR", "TEXT", "VARCHAR", "VARBINARY"}, ColumnCharsets: []string{"gbk", "latin1", "utf16", "binary"}}
	got, err := convertDamengBinlogRow(table, []any{gbk, []byte{0x80, 0xe9}, []byte{0, 'A', 0x4e, 0x2d}, []byte{0xff, 0}})
	require.NoError(t, err)
	require.Equal(t, []any{"中文", "€é", "A中", []byte{0xff, 0}}, got)
	for _, test := range []struct {
		charset string
		raw     []byte
	}{{"utf8mb4", []byte{0xff}}, {"ascii", []byte{0x80}}, {"gbk", []byte{0x81}}, {"unknown", []byte("hello")}} {
		_, err := decodeDamengMySQLText(test.raw, test.charset)
		require.Error(t, err, test.charset)
	}
}
