package validation

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCanonicalEquivalentRepresentations(t *testing.T) {
	tests := []struct {
		name                    string
		logical, source, target Column
		a, b                    any
	}{
		{"decimal", Column{Kind: Number}, Column{Kind: Number}, Column{Kind: Number}, "001.2300", "1.23e0"},
		{"huge integer", Column{Kind: Number}, Column{Kind: Number}, Column{Kind: Text}, "18446744073709551615", "18446744073709551615.000"},
		{"binary float widening", Column{Kind: Float}, Column{Kind: Float, FloatBits: 32}, Column{Kind: Float, FloatBits: 64}, float64(float32(.1)), float64(float32(.1))},
		{"negative zero", Column{Kind: Float}, Column{Kind: Float, FloatBits: 64}, Column{Kind: Float, FloatBits: 64}, math.Copysign(0, -1), float64(0)},
		{"bool", Column{Kind: Bool}, Column{Kind: Bool}, Column{Kind: Number}, true, "1.000"},
		{"bits", Column{Kind: Bits, Length: 8}, Column{Kind: Bits, Length: 8}, Column{Kind: Number}, "00001010", "10"},
		{"uuid", Column{Kind: UUID}, Column{Kind: UUID}, Column{Kind: Text}, "A57D036D-C9A9-4B9D-98D0-2C63E2E37517", "a57d036d-c9a9-4b9d-98d0-2c63e2e37517"},
		{"date to midnight", Column{Kind: Date}, Column{Kind: Date}, Column{Kind: Timestamp}, "2026-10-22", "2026-10-22T00:00:00.000000000"},
		{"wall time", Column{Kind: Timestamp}, Column{Kind: Timestamp}, Column{Kind: Timestamp}, "+2026-10-22T10:20:30.12", "2026-10-22 10:20:30.120000000"},
		{"instant", Column{Kind: TimestampTZ}, Column{Kind: TimestampTZ}, Column{Kind: TimestampTZ}, "2026-10-22T10:20:30.123456789+08:00", "2026-10-22 02:20:30.123456789+00"},
		{"time", Column{Kind: Time}, Column{Kind: Time}, Column{Kind: Time}, "-838:59:59.123456", "-838:59:59.123456000"},
		{"json", Column{Kind: JSON}, Column{Kind: JSON}, Column{Kind: Text}, `{"b":[1.0,true,null],"a":"\u4f60\u597d"}`, `{ "a":"你好", "b":[1e0,true,null] }`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, e := canonical(tt.a, tt.logical, tt.source)
			require.NoError(t, e)
			b, e := canonical(tt.b, tt.logical, tt.target)
			require.NoError(t, e)
			require.Equal(t, string(a), string(b))
		})
	}
}

func TestCanonicalPreservesInformationDifferences(t *testing.T) {
	tests := []struct {
		name                    string
		logical, source, target Column
		a, b                    any
	}{
		{"null and empty", Column{Kind: Text}, Column{Kind: Text}, Column{Kind: Text}, nil, ""},
		{"NUL cleaning", Column{Kind: Text}, Column{Kind: Text}, Column{Kind: Text}, "a\x00b", "ab"},
		{"trailing spaces", Column{Kind: Text}, Column{Kind: Text}, Column{Kind: Text}, "a ", "a"},
		{"decimal precision", Column{Kind: Number}, Column{Kind: Number}, Column{Kind: Number}, "9007199254740993", "9007199254740992"},
		{"float narrowing", Column{Kind: Float}, Column{Kind: Float, FloatBits: 64}, Column{Kind: Float, FloatBits: 32}, .1, float64(float32(.1))},
		{"decimal to approximate float", Column{Kind: Number}, Column{Kind: Number}, Column{Kind: Float, FloatBits: 64}, "0.1", .1},
		{"timestamp precision", Column{Kind: Timestamp}, Column{Kind: Timestamp}, Column{Kind: Timestamp}, "2026-10-22T10:20:30.123456789", "2026-10-22T10:20:30.123456"},
		{"date target has time", Column{Kind: Date}, Column{Kind: Date}, Column{Kind: Timestamp}, "2026-10-22", "2026-10-22T00:00:01"},
		{"zero date replaced", Column{Kind: Date}, Column{Kind: Date}, Column{Kind: Date}, "0000-00-00", "1900-01-01"},
		{"array order", Column{Kind: JSON}, Column{Kind: JSON}, Column{Kind: JSON}, `[1,2]`, `[2,1]`},
		{"binary", Column{Kind: Binary}, Column{Kind: Binary}, Column{Kind: Binary}, []byte{0, 1, 2}, []byte{0, 2, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, e := canonical(tt.a, tt.logical, tt.source)
			require.NoError(t, e)
			b, e := canonical(tt.b, tt.logical, tt.target)
			require.NoError(t, e)
			require.NotEqual(t, string(a), string(b))
		})
	}
}

func TestCanonicalFailsClosed(t *testing.T) {
	tests := []struct {
		name            string
		v               any
		logical, native Column
	}{
		{"unsupported even NULL", nil, Column{Kind: Unsupported}, Column{Kind: Unsupported}},
		{"unknown float width", .1, Column{Kind: Float}, Column{Kind: Float}},
		{"rounded exact number", float64(9007199254740992), Column{Kind: Number}, Column{Kind: Number}},
		{"JSON duplicate key", `{"a":1,"a":2}`, Column{Kind: JSON}, Column{Kind: JSON}},
		{"JSON lone surrogate", `{"a":"\ud800"}`, Column{Kind: JSON}, Column{Kind: JSON}},
		{"infinite decimal", `Infinity`, Column{Kind: Number}, Column{Kind: Number}},
		{"numeric exponent resource bound", `1e999999999999`, Column{Kind: Number}, Column{Kind: Number}},
		{"timezone lost", `2026-10-22T10:20:30`, Column{Kind: TimestampTZ}, Column{Kind: Timestamp}},
		{"BC date", `-0044-03-15T00:00:00`, Column{Kind: Timestamp}, Column{Kind: Timestamp}},
		{"excess timestamp precision", `2026-10-22T10:20:30.1234567891`, Column{Kind: Timestamp}, Column{Kind: Timestamp}},
		{"too large", strings.Repeat("x", MaxValueBytes+1), Column{Kind: Text}, Column{Kind: Text}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, e := canonical(tt.v, tt.logical, tt.native)
			var unsupported *UnsupportedError
			require.True(t, errors.As(e, &unsupported), "%v", e)
		})
	}
	b, e := canonical(`{"a":"\ud83d\ude00"}`, Column{Kind: JSON}, Column{Kind: JSON})
	require.NoError(t, e)
	require.Contains(t, string(b), "😀")
}

func TestCanonicalEncodingDoesNotMixCells(t *testing.T) {
	cols := []Column{{Name: "one", Kind: Text}, {Name: "two", Kind: Text}}
	a, _, e := rowRecord([]any{"a\x00b", "c"}, cols, cols, nil)
	require.NoError(t, e)
	b, _, e := rowRecord([]any{"a", "b\x00c"}, cols, cols, nil)
	require.NoError(t, e)
	require.NotEqual(t, a.row, b.row)
	null, _, e := rowRecord([]any{nil, ""}, cols, cols, nil)
	require.NoError(t, e)
	empty, _, e := rowRecord([]any{"", nil}, cols, cols, nil)
	require.NoError(t, e)
	require.False(t, bytes.Equal(null.row[:], empty.row[:]))
}

func TestSummaryBoundsAndBinary(t *testing.T) {
	s := summarize(strings.Repeat("你", 300), Column{Kind: Text})
	require.True(t, s.Truncated)
	require.Equal(t, strings.Repeat("你", 256), s.Text)
	require.EqualValues(t, 900, s.Length)
	b := summarize([]byte{0, 1, 2}, Column{Kind: Binary})
	require.Equal(t, "[binary]", b.Text)
	require.EqualValues(t, 3, b.Length)
	require.Len(t, b.SHA256, 64)
	tm := time.Date(2026, 10, 22, 1, 2, 3, 123, time.UTC)
	require.Contains(t, summarize(tm, Column{Kind: Timestamp}).Text, "000000123")
}
