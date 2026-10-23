package target

import (
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

func TestNativeDefinitionProofRequiresEveryDuplicateConstraint(t *testing.T) {
	named := damengnative.Unit{SQL: `ALTER TABLE "App"."T" ADD CONSTRAINT "Named" CHECK("Id">0) ENABLE NOVALIDATE`}
	first := damengnative.Unit{SQL: `ALTER TABLE "App"."T" ADD CHECK("Id">0) ENABLE NOVALIDATE /* first source object */`}
	second := damengnative.Unit{SQL: `ALTER TABLE "App"."T" ADD CHECK("Id">0) ENABLE NOVALIDATE /* second source object */`}
	candidates := []damengnative.Unit{named, first}
	used := make([]bool, len(candidates))
	require.True(t, nativeConsumeDefinitionCandidate(named, candidates, used))
	require.True(t, nativeConsumeDefinitionCandidate(first, candidates, used))
	require.False(t, nativeConsumeDefinitionCandidate(second, candidates, used), "one catalog constraint cannot satisfy two frozen ADDs")
	candidates = append(candidates, second)
	used = make([]bool, len(candidates))
	for _, expected := range []damengnative.Unit{named, first, second} {
		require.True(t, nativeConsumeDefinitionCandidate(expected, candidates, used))
	}
}
