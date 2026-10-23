package datamigrate

import (
	"context"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

func TestNativeOwnedNameCollisionStopsOverwriteBeforeAnyWrite(t *testing.T) {
	for _, kind := range []string{"constraint", "index"} {
		t.Run(kind, func(t *testing.T) {
			m, r, w := nativeTestMigrator(Config{OverwriteExisting: true, CaptureRepair: true})
			r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{ID: "source-owned", Kind: kind, Schema: "Src", Name: "SharedName", Table: "Mixed", ParentID: "source-table", Managed: true})
			w.snapshot.Objects = []damengnative.Object{
				{ID: "replace", Kind: "table", Schema: "Dst", Name: "Mixed", DropSQL: `DROP TABLE "Dst"."Mixed"`},
				{ID: "retained", Kind: "table", Schema: "Dst", Name: "Keep"},
				{ID: "target-owned", Kind: kind, Schema: "Dst", Name: "SharedName", Table: "Keep", ParentID: "retained", Managed: true},
			}
			p := m.PreflightDamengNative(context.Background())
			require.False(t, p.Ready)
			require.Len(t, p.Conflicts, 2)
			for _, drop := range p.Drops {
				require.NotEqual(t, "retained", drop.ID)
				require.NotEqual(t, "target-owned", drop.ID)
			}
			require.True(t, m.Run(context.Background()).HasFailures())
			require.Empty(t, w.statements)
			require.Empty(t, w.copied)
			require.Zero(t, r.scans)
		})
	}
}

func TestNativeOwnedNamesFollowOnlyTheirReplacedParent(t *testing.T) {
	planned := []damengnative.Object{{Kind: "constraint", Name: "Pk", Managed: true}}
	actual := []damengnative.Object{{ID: "table", Kind: "table", Name: "T"}, {ID: "pk", Kind: "constraint", Name: "Pk", Managed: true, ParentID: "table", Table: "T"}}
	conflicts, issues := nativeOwnedNameConflicts(planned, actual, map[string]bool{"table": true})
	require.Empty(t, conflicts)
	require.Empty(t, issues)
	planned[0].Generated = true
	conflicts, issues = nativeOwnedNameConflicts(planned, actual, nil)
	require.Empty(t, conflicts, "系统生成且不回放的名称不占用目标名称")
	require.Empty(t, issues)
}

func TestNativeExplicitNameCannotTakeGeneratedNameOfRetainedTable(t *testing.T) {
	for _, kind := range []string{"constraint", "index"} {
		planned := []damengnative.Object{{Kind: kind, Name: "InternalLookingName", Managed: true}}
		actual := []damengnative.Object{{ID: "retained", Kind: "table", Name: "Keep"}, {ID: "occupied", Kind: kind, Name: "InternalLookingName", Managed: true, Generated: true, ParentID: "retained", Table: "Keep"}}
		conflicts, issues := nativeOwnedNameConflicts(planned, actual, nil)
		require.Len(t, conflicts, 1, kind)
		require.Len(t, issues, 1, kind)
		conflicts, issues = nativeOwnedNameConflicts(planned, actual, map[string]bool{"retained": true})
		require.Empty(t, conflicts, kind)
		require.Empty(t, issues, kind)
	}
}

func TestNativeTargetOnlyNamedConstraintProtectsItsOwnerDuringOverwrite(t *testing.T) {
	m, _, w := nativeTestMigrator(Config{OverwriteExisting: true})
	w.snapshot.Objects = []damengnative.Object{
		{ID: "existing", Kind: "table", Schema: "Dst", Name: "Mixed", DropSQL: `DROP TABLE "Dst"."Mixed"`},
		{ID: "extra-check", Kind: "constraint", Schema: "Dst", Name: "TargetOnlyCheck", Table: "Mixed", ParentID: "existing", Managed: true, Dependencies: []string{"existing"}},
	}
	require.False(t, m.PreflightDamengNative(context.Background()).Ready)
	require.Empty(t, w.statements)
}
