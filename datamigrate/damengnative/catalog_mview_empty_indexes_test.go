package damengnative

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmptyMaterializedEpochDefersOnlyIndependentBaseIndexes(t *testing.T) {
	r := catalogReader{snapshot: &Snapshot{Objects: []Object{
		{Materialized: &MaterializedState{History: &MaterializedHistoryPlan{Views: []MaterializedHistoryView{{EmptyTouch: &MaterializedEmptyTouch{Table: "Base"}}}}}},
		{Kind: "index", Table: "Base", Units: []Unit{{SQL: "expression", Phase: "after_data"}}},
		{Kind: "index", Table: "Base", Managed: true, Units: []Unit{{SQL: "constraint", Phase: "after_data"}}},
		{Kind: "index", Table: "Other", Units: []Unit{{SQL: "other", Phase: "after_data"}}},
		{Kind: "trigger", Table: "Base", Units: []Unit{{SQL: "trigger", Phase: "triggers"}}},
	}}}
	r.deferMaterializedEmptyIndexes()
	require.Equal(t, "after_history", r.snapshot.Objects[1].Units[0].Phase)
	require.Equal(t, "expression", r.snapshot.Objects[1].Units[0].SQL)
	require.Equal(t, "after_data", r.snapshot.Objects[2].Units[0].Phase)
	require.Equal(t, "after_data", r.snapshot.Objects[3].Units[0].Phase)
	require.Equal(t, "triggers", r.snapshot.Objects[4].Units[0].Phase)
}
