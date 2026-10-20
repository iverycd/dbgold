package dialect

import (
	"slices"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func oracleDamengBoundsSequence() source.SequenceInfo {
	return source.SequenceInfo{
		TableName: "MixedTable", ColumnName: "MixedId", StartValue: 100, StartValueText: "100",
		Native: &source.NativeSequenceInfo{
			Name: "MixedSeq", IncrementBy: "1", MinValue: "1", MaxValue: "9999999999999999999999999999", CacheSize: "20", Ordered: true,
			Bindings: []source.SequenceBinding{{TableName: "MixedTable", ColumnName: "MixedId", SourceColumnName: "SourceId", Kind: "trigger", TriggerName: "MixedTrigger", OnlyWhenNull: true}},
		},
	}
}

func TestNormalizeOracleDamengSequenceBounds(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		start, step, min, max       string
		wantMin, wantMax, errorText string
		cycle                       bool
	}{
		{name: "ascending default", start: "100", step: "1", min: "1", max: "9999999999999999999999999999", wantMin: "1", wantMax: "9223372036854775807"},
		{name: "ascending 27 digit max", start: "100", step: "1", min: "1", max: "999999999999999999999999999", wantMin: "1", wantMax: "9223372036854775807"},
		{name: "ascending custom 24 digit max", start: "100", step: "1", min: "1", max: "123456789012345678901234", wantMin: "1", wantMax: "9223372036854775807"},
		{name: "ascending numeric default", start: "100", step: "+01", min: "1", max: "+09999999999999999999999999999", wantMin: "1", wantMax: "9223372036854775807"},
		{name: "descending default", start: "-100", step: "-1", min: "-999999999999999999999999999", max: "-1", wantMin: "-9223372036854775808", wantMax: "-1"},
		{name: "descending numeric default", start: "-100", step: "-01", min: "-0999999999999999999999999999", max: "-1", wantMin: "-9223372036854775808", wantMax: "-1"},
		{name: "finite ascending", start: "100", step: "5", min: "-10", max: "1000", wantMin: "-10", wantMax: "1000"},
		{name: "finite descending", start: "100", step: "-5", min: "-1000", max: "110", wantMin: "-1000", wantMax: "110"},
		{name: "int64 bounds", start: "0", step: "1", min: "-9223372036854775808", max: "9223372036854775807", wantMin: "-9223372036854775808", wantMax: "9223372036854775807"},
		{name: "both source bounds exceed int64", start: "100", step: "1", min: "-123456789012345678901234", max: "123456789012345678901234", wantMin: "-9223372036854775808", wantMax: "9223372036854775807"},
		{name: "custom oversized max", start: "100", step: "1", min: "1", max: "9999999999999999999999999998", wantMin: "1", wantMax: "9223372036854775807"},
		{name: "custom oversized min", start: "-100", step: "-1", min: "-999999999999999999999999998", max: "-1", wantMin: "-9223372036854775808", wantMax: "-1"},
		{name: "descending wide maximum", start: "100", step: "-1", min: "1", max: "9999999999999999999999999999", wantMin: "1", wantMax: "9223372036854775807"},
		{name: "ascending wide minimum", start: "100", step: "1", min: "-999999999999999999999999999", max: "1000", wantMin: "-9223372036854775808", wantMax: "1000"},
		{name: "in range decimal text preserved", start: "+0100", step: "+01", min: "-0010", max: "+01000", wantMin: "-0010", wantMax: "+01000"},
		{name: "range lies above int64", start: "100", step: "1", min: "9223372036854775808", max: "999999999999999999999999", errorText: "最小值超出达梦 int64"},
		{name: "range lies below int64", start: "-100", step: "-1", min: "-999999999999999999999999", max: "-9223372036854775809", errorText: "最大值超出达梦 int64"},
		{name: "intersection has only upper endpoint", start: "9223372036854775807", step: "1", min: "9223372036854775807", max: "999999999999999999999999", errorText: "起值、步长或边界无效"},
		{name: "intersection has only lower endpoint", start: "-9223372036854775808", step: "-1", min: "-999999999999999999999999", max: "-9223372036854775808", errorText: "起值、步长或边界无效"},
		{name: "invalid minimum", start: "100", step: "1", min: "invalid", max: "999999999999999999999999", errorText: "最小值超出达梦 int64"},
		{name: "invalid maximum", start: "100", step: "1", min: "1", max: "invalid", errorText: "最大值超出达梦 int64"},
		{name: "actual start overflows", start: "9223372036854775808", step: "1", min: "1", max: "9999999999999999999999999999", errorText: "起始值超出达梦 int64"},
		{name: "actual descending start overflows", start: "-9223372036854775809", step: "-1", min: "-999999999999999999999999999", max: "-1", errorText: "起始值超出达梦 int64"},
		{name: "start below finite min", start: "100", step: "1", min: "101", max: "9999999999999999999999999999", errorText: "起值、步长或边界无效"},
		{name: "oversized step", start: "100", step: "9223372036854775808", min: "1", max: "9999999999999999999999999999", errorText: "步长超出达梦 int64"},
		{name: "excluded minimum step", start: "-100", step: "-9223372036854775808", min: "-999999999999999999999999999", max: "-1", errorText: "起值、步长或边界无效"},
		{name: "zero step", start: "100", step: "0", min: "1", max: "1000", errorText: "起值、步长或边界无效"},
		{name: "step exceeds finite range", start: "100", step: "100", min: "100", max: "110", errorText: "起值、步长或边界无效"},
		{name: "equal bounds", start: "100", step: "1", min: "100", max: "100", errorText: "起值、步长或边界无效"},
		{name: "cycle", start: "100", step: "1", min: "1", max: "9999999999999999999999999999", cycle: true, errorText: "循环序列"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seq := oracleDamengBoundsSequence()
			seq.StartValueText = tc.start
			seq.Native.IncrementBy, seq.Native.MinValue, seq.Native.MaxValue = tc.step, tc.min, tc.max
			seq.Native.Cycle = tc.cycle
			before := seq
			native := *seq.Native
			native.Bindings = slices.Clone(native.Bindings)
			before.Native = &native

			got, err := NormalizeOracleDamengSequence(seq)
			require.Equal(t, before, seq, "normalizing must not change source metadata")
			require.NotSame(t, seq.Native, got.Native)
			if tc.errorText != "" {
				require.ErrorContains(t, err, tc.errorText)
				require.Nil(t, NewDaMeng().SequenceStatements("APP", seq, ""))
			} else {
				require.NoError(t, err)
				expected := before
				expectedNative := *before.Native
				expectedNative.MinValue, expectedNative.MaxValue = tc.wantMin, tc.wantMax
				expected.Native = &expectedNative
				require.Equal(t, expected, got, "only source bounds outside the target range may change")
				require.NoError(t, ValidateOracleDamengSequence(got))
				statements := NewDaMeng().SequenceStatements("APP", seq, "")
				require.Len(t, statements, 2)
				require.Contains(t, statements[0].SQL, "START WITH "+tc.start+" INCREMENT BY "+tc.step)
				require.Contains(t, statements[0].SQL, "MINVALUE "+tc.wantMin+" MAXVALUE "+tc.wantMax)
				require.Contains(t, statements[0].SQL, "NOCYCLE CACHE 20 ORDER")
			}
			require.Equal(t, before, seq, "rendering must not change source metadata")
			got.Native.Name = "ChangedSeq"
			got.Native.Bindings[0].ColumnName = "ChangedColumn"
			require.Equal(t, before, seq, "returned metadata and bindings must be independent")
		})
	}
}

func TestNormalizeOracleDamengSequenceRetainsStrictValidation(t *testing.T) {
	seq := oracleDamengBoundsSequence()
	// The validator still describes the actual target range. Adaptation belongs
	// exclusively to the normalization helper.
	require.ErrorContains(t, ValidateOracleDamengSequence(seq), "最大值超出达梦 int64")
	seq.Unsupported = "unsupported source generator"
	_, err := NormalizeOracleDamengSequence(seq)
	require.ErrorContains(t, err, seq.Unsupported)
	_, err = NormalizeOracleDamengSequence(source.SequenceInfo{})
	require.ErrorContains(t, err, "缺少 Oracle 命名序列元数据")
}
