package jdbcbridge

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCursorProtocolPreservesExactValues(t *testing.T) {
	e := &encoder{}
	e.bool(true)
	e.uint32(1)
	e.uint32(5)
	e.byte(valueNull)
	e.byte(valueDecimal)
	e.string("12345678901234567890.12345678901234567890")
	e.byte(valueString)
	e.string("空\x00格 ")
	e.byte(valueBytes)
	e.bytes([]byte{0, 255, 0})
	e.byte(valueTimestamp)
	e.string("2026-10-22 11:12:13.123456789")
	rows, done, err := decodeCursorRows(e.Bytes(), 2, 1024)
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, [][]any{{nil, "12345678901234567890.12345678901234567890", "空\x00格 ", []byte{0, 255, 0}, "2026-10-22 11:12:13.123456789"}}, rows)
}
func TestCursorProtocolRejectsTruncatedOrOversizedResponses(t *testing.T) {
	e := &encoder{}
	e.bool(false)
	e.uint32(2)
	_, _, err := decodeCursorRows(e.Bytes(), 1, 1024)
	require.ErrorContains(t, err, "row limit")
	e = &encoder{}
	e.bool(false)
	e.uint32(1)
	e.uint32(1)
	e.byte(valueBytes)
	e.uint32(100)
	_, _, err = decodeCursorRows(e.Bytes(), 1, 1024)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	e = &encoder{}
	e.bool(false)
	e.uint32(0)
	e.byte(42)
	_, _, err = decodeCursorRows(e.Bytes(), 1, 1024)
	require.ErrorContains(t, err, "trailing")
	_, _, err = decodeCursorRows(make([]byte, 200), 1, 32)
	require.ErrorContains(t, err, "byte limit")
}
func TestCursorMetadataRetainsNamesAndPrecision(t *testing.T) {
	e := &encoder{}
	e.uint32(1)
	e.string("MiXeD")
	e.string("DECIMAL")
	e.int32(3)
	e.int64(38)
	e.int64(19)
	e.int64(38)
	e.uint32(1)
	e.string("MiXeD")
	m, err := decodeTableMetadata(e.Bytes())
	require.NoError(t, err)
	require.Equal(t, "MiXeD", m.Columns[0].Name)
	require.Equal(t, int64(19), m.Columns[0].Scale)
	require.Equal(t, []string{"MiXeD"}, m.PrimaryKey)
}
func TestCursorMethodsNeverRestartClosedBridge(t *testing.T) {
	m := NewManager(Config{})
	ctx := context.Background()
	_, err := m.DescribeTable(ctx, 1, "s", "t")
	require.ErrorContains(t, err, "not running")
	_, err = m.OpenCursor(ctx, 1, "SELECT 1")
	require.ErrorContains(t, err, "not running")
	_, _, err = m.FetchCursor(ctx, 1, 100, 1024)
	require.ErrorContains(t, err, "not running")
	require.ErrorContains(t, m.CloseCursor(ctx, 1), "not running")
}
