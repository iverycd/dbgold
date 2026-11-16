package pgoracle

import (
	"math"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestSequenceNextRetainsNegativeStepsAndCycleBounds(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		last, step, min, max, want int64
		called, cycle              bool
		fail                       bool
	}{
		{"uncalled", 10, 3, 1, 100, 10, false, false, false},
		{"called", 10, 3, 1, 100, 13, true, false, false},
		{"negative", -10, -3, -100, -1, -13, true, false, false},
		{"positive_cycle", 99, 3, 1, 100, 1, true, true, false},
		{"negative_cycle", -99, -3, -100, -1, -1, true, true, false},
		{"positive_exhaustion", math.MaxInt64, 1, 1, math.MaxInt64, 0, true, false, true},
		{"negative_exhaustion", math.MinInt64, -1, math.MinInt64, -1, 0, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := SequencePlan{Source: source.PGSequence{Name: "Seq", LastValue: tc.last, Increment: tc.step, Min: tc.min, Max: tc.max, IsCalled: tc.called, Cycle: tc.cycle, Cache: 1}}
			n, err := p.Next()
			if tc.fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, n.Int64())
			}
		})
	}
}

func TestSequenceCycleCacheUsesCeilingAndRejectsCapacityLoss(t *testing.T) {
	p := SequencePlan{Schema: "APP", Name: "S", Source: source.PGSequence{Name: "Seq", Schema: "Mixed", LastValue: 1, Increment: 3, Min: 1, Max: 11, Cache: 3, Cycle: true}}
	statements, err := p.SequenceStatements()
	require.NoError(t, err)
	require.Contains(t, statements[0].SQL, "CACHE 3")
	p.Source.Cache = 4
	_, err = p.SequenceStatements()
	require.ErrorContains(t, err, "缓存大小")
}

func TestSequenceFingerprintIncludesOrderingAndIgnoresWatermark(t *testing.T) {
	p := SequencePlan{Source: source.PGSequence{Increment: 3, Min: 1, Max: 100, Cache: 1}}
	before := p.Fingerprint()
	p.Source.LastValue, p.Source.IsCalled = 90, true
	require.Equal(t, before, p.Fingerprint())
	require.NotEqual(t, before, SequenceFingerprint("3", "1", "100", "0", false, true))
}
