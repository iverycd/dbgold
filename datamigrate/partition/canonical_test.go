package partition

import (
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestPartitionCanonicalListIgnoresExtractorFlagsAndPreservesNegativeConstants(t *testing.T) {
	want := compilerListFixture().Partition
	actual := source.ClonePartitionSpec(want)
	actual.Partitions[0].LowerInclusive = true
	actual.Partitions[1].UpperInclusive = true
	actual.Partitions[0].Values = [][]source.PartitionValue{{{Kind: "null"}}, {{Kind: "number", Text: "+0.0"}}, {{Kind: "number", Text: "-1.000"}}}
	actual.SourceDDL = "source-specific evidence"
	actual.Keys[0].DataType = "integer"
	require.NoError(t, Equivalent(want, actual))
	actual.Partitions[0].Values[2][0].Text = "1"
	require.Error(t, Equivalent(want, actual), "negative LIST values must never normalize to their positive counterpart")
}

func TestPartitionCanonicalExpressionIgnoresOnlyFormattingAndKeywordCase(t *testing.T) {
	first := source.PartitionKey{Expression: `(( ABS ( "MiXeD" ) ))`}
	second := source.PartitionKey{Expression: `abs(/* catalog whitespace */"MiXeD")`}
	require.Equal(t, CanonicalKey(first), CanonicalKey(second))
	require.NotEqual(t, CanonicalKey(first), CanonicalKey(source.PartitionKey{Expression: `abs("mixed")`}))
	require.NotEqual(t, CanonicalKey(source.PartitionKey{Expression: `coalesce("Key",'ABC')`}), CanonicalKey(source.PartitionKey{Expression: `coalesce("Key",'abc')`}))
}

func TestPartitionCanonicalRangeIncludesBoundsNamesAndFutureWriteSemantics(t *testing.T) {
	want := compilerRangeFixture().Partition
	actual := source.ClonePartitionSpec(want)
	actual.Partitions[0].Upper[0].Text = "0.000e1"
	actual.Partitions[1].Lower[0].Text = "-0.0"
	require.NoError(t, Equivalent(want, actual))
	for _, change := range []string{"bound", "name", "schema", "inclusivity", "row movement", "automatic"} {
		t.Run(change, func(t *testing.T) {
			actual := source.ClonePartitionSpec(want)
			switch change {
			case "bound":
				actual.Partitions[0].Upper[0].Text = "1"
			case "name":
				actual.Partitions[0].Name = "Renamed"
			case "schema":
				actual.Partitions[0].Schema = "Other"
			case "inclusivity":
				actual.Partitions[0].UpperInclusive = true
			case "row movement":
				actual.RowMovement = "disabled"
			case "automatic":
				actual.Automatic = "1"
			}
			require.Error(t, Equivalent(want, actual))
		})
	}
}

func TestPartitionCanonicalRejectsUnresolvedExtractionEvidenceAtAnyLevel(t *testing.T) {
	want := compilerRangeFixture().Partition
	actual := source.ClonePartitionSpec(want)
	actual.Unsupported = "native options not fully understood"
	require.Error(t, Equivalent(want, actual), "unknown native clauses must not be discarded as harmless source evidence")
	want = compilerRangeFixture().Partition
	want.Partitions[0].Subpartition = compilerListFixture().Partition
	actual = source.ClonePartitionSpec(want)
	actual.Partitions[0].Subpartition.Unsupported = "child shape not understood"
	require.Error(t, Equivalent(want, actual))
}

func TestPartitionCanonicalLiteralValidationDoesNotAcceptExecutableSQL(t *testing.T) {
	for _, value := range []source.PartitionValue{{Kind: "number", Text: "1;SELECT danger()"}, {Kind: "date", Text: "2024-02-30"}, {Kind: "timestamp", Text: "2024-01-01 00:00:00+08:00"}, {Kind: "sql", Text: "MAXVALUE"}} {
		_, err := canonicalValue(value)
		require.Error(t, err)
	}
	value, err := canonicalValue(source.PartitionValue{Kind: "number", Text: "-123456789012345678901234567890.1200"})
	require.NoError(t, err)
	require.Equal(t, "-123456789012345678901234567890.12", value.Text)
}

func TestPartitionCanonicalHashAlgorithmIdentityRemainsSignificant(t *testing.T) {
	want := &source.PartitionSpec{Method: "HASH", Algorithm: "mysql-mod", Keys: []source.PartitionKey{{Column: "Id"}}, Partitions: []source.Partition{{Name: "P", Ordinal: 1, HashModulus: 1}}}
	actual := source.ClonePartitionSpec(want)
	actual.Algorithm = "mysql-linear-hash"
	require.Error(t, Equivalent(want, actual))
}
