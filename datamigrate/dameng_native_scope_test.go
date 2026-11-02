package datamigrate

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

func addNativeHealthyTable(r *nativeTestReader, name, id string) {
	r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{ID: id, Kind: "table", Name: name, Schema: "Src", Units: []damengnative.Unit{{SQL: `CREATE TABLE "Src"."` + name + `" ("Value" VARCHAR(100))`, Phase: "before_data"}}, DropSQL: `DROP TABLE "Src"."` + name + `"`})
	r.snapshot.Tables = append(r.snapshot.Tables, damengnative.Table{ObjectID: id, Schema: "Src", Name: name, Rows: 0, Columns: []damengnative.Column{{Name: "Value", DataType: "VARCHAR", SQLType: "VARCHAR(100)", Writable: true}}})
}

func TestDamengNativeLocalTableFailuresContinueHealthyTables(t *testing.T) {
	for _, failure := range []string{"preflight", "drop", "create"} {
		t.Run(failure, func(t *testing.T) {
			m, r, w := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true, OverwriteExisting: failure == "drop"})
			r.snapshot.Tables[0].Rows = 0
			r.rows = nil
			addNativeHealthyTable(r, "Healthy", "healthy")
			for _, owner := range []string{"source-table", "healthy"} {
				name := map[string]string{"source-table": "Mixed", "healthy": "Healthy"}[owner]
				r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{ID: "idx-" + owner, Kind: "index", Name: "idx_" + name, Schema: "Src", Table: name, Dependencies: []string{owner}, Units: []damengnative.Unit{{SQL: `CREATE INDEX "Src"."idx_` + name + `" ON "Src"."` + name + `" ("Value")`, Phase: "after_data"}}})
			}
			switch failure {
			case "preflight":
				r.snapshot.Issues = []damengnative.Issue{{ObjectID: "source-table", Kind: "table", Name: "Mixed", Severity: "error", Reason: "unsupported local column"}}
			case "drop":
				w.snapshot.Objects = []damengnative.Object{{ID: "existing", Kind: "table", Name: "Mixed", Schema: "Dst", DropSQL: `DROP TABLE "Dst"."Mixed"`}}
				w.fail[`DROP TABLE "Dst"."Mixed"`] = 1
			case "create":
				w.fail[`CREATE TABLE "Dst"."Mixed" ("Value" VARCHAR(100))`] = 10
			}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.True(t, report.NativeSchema.Preflight.CanStart)
			require.Contains(t, w.statements, `CREATE TABLE "Dst"."Healthy" ("Value" VARCHAR(100))`)
			require.Contains(t, w.statements, `CREATE INDEX "Dst"."idx_Healthy" ON "Dst"."Healthy" ("Value")`)
			require.NotContains(t, w.statements, `CREATE INDEX "Dst"."idx_Mixed" ON "Dst"."Mixed" ("Value")`)
			require.Equal(t, 1, report.Tables.Success)
			require.Equal(t, 1, report.Tables.Failed)
			require.Equal(t, 1, report.Data.Success)
			require.Equal(t, 1, report.Indexes.Success)
			require.Equal(t, 1, report.Indexes.Failed)
			require.True(t, m.migrationTableBlocked("Mixed"))
			require.False(t, m.migrationTableBlocked("Healthy"))
		})
	}
}

func TestDamengNativeUnknownObjectIsLocalPreflightFailure(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{Mode: "all"})
	r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{ID: "unknown", Kind: "unsupported", Name: "u", Schema: "Src"})
	report := m.Run(context.Background())
	require.False(t, report.NativeSchema.Preflight.Ready)
	require.True(t, report.NativeSchema.Preflight.CanStart)
	require.Equal(t, 1, report.Tables.Success)
	require.NotEmpty(t, w.copied)
}

func TestDamengNativeRetainedTargetChildProtectsOnlyItsParents(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{Mode: "all", OverwriteExisting: true})
	addNativeHealthyTable(r, "Parent", "parent")
	addNativeHealthyTable(r, "Other", "other")
	r.snapshot.Issues = []damengnative.Issue{{ObjectID: "source-table", Severity: "error", Reason: "local failure"}}
	w.snapshot.Objects = []damengnative.Object{
		{ID: "old-child", Kind: "table", Name: "Mixed", Schema: "Dst", DropSQL: `DROP TABLE "Dst"."Mixed"`, Dependencies: []string{"old-parent"}},
		{ID: "old-parent", Kind: "table", Name: "Parent", Schema: "Dst", DropSQL: `DROP TABLE "Dst"."Parent"`},
	}
	p := m.PreflightDamengNative(context.Background())
	require.True(t, p.CanStart)
	require.Contains(t, p.Blocked, "source-table")
	require.Contains(t, p.Blocked, "parent")
	require.NotContains(t, p.Blocked, "other")
	require.Empty(t, p.Drops)
}

func TestNativeHistoryGroupDoesNotDependOnUnrelatedFailure(t *testing.T) {
	history := &damengnative.MaterializedHistoryPlan{Views: []damengnative.MaterializedHistoryView{{ObjectID: "mv", Table: "Store", Staging: "Stage"}}, Logs: []damengnative.MaterializedHistoryLog{{ObjectID: "log", BaseTable: "Base", Staging: "LogStage"}}}
	p := &damengnative.Preflight{Objects: []damengnative.Object{
		{ID: "base", Kind: "table", Name: "Base"}, {ID: "store", Kind: "table", Name: "Store"}, {ID: "stage", Kind: "table", Name: "Stage"}, {ID: "logstage", Kind: "table", Name: "LogStage"},
		{ID: "log", Kind: "materialized_view_log", Name: "Log", Dependencies: []string{"base"}},
		{ID: "mv", Kind: "materialized_view", Name: "Mv", Dependencies: []string{"base", "log"}, Materialized: &damengnative.MaterializedState{History: history}},
		{ID: "other", Kind: "table", Name: "Other"},
	}}
	states := map[string]error{"other": errors.New("unrelated table failure")}
	blockNativeHistoryGroups(p, states)
	require.Nil(t, states["mv"])
	states["stage"] = errors.New("snapshot staging was not loaded")
	blockNativeHistoryGroups(p, states)
	require.Equal(t, "dependency", nativeFailureStage(states["mv"]))
}

func TestDamengNativeHealthyPolicyRestoresWhileGlobalEventsRemainPaused(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true})
	addNativeHealthyTable(r, "Broken", "broken")
	policy := addNativeTestMVPolicy(r)
	addNativeTestEvent(r)
	w.fail[`CREATE TABLE "Dst"."Broken" ("Value" VARCHAR(100))`] = 10
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, 1, report.Tables.Success)
	require.Contains(t, w.statements, policy)
	require.Contains(t, w.lifecycle, "policy_verify:Mv")
	require.NotContains(t, w.lifecycle, "final_session")
	require.NotContains(t, w.statements, damengnative.EventTriggerStateSQL("Dst", "Hook", true))
}

func TestDamengNativeConnectionFailureStopsRemainingDDL(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "without_manifest", true: "with_manifest"}[capture], func(t *testing.T) {
			m, r, w := nativeTestMigrator(Config{Mode: "all", CaptureRepair: capture})
			addNativeHealthyTable(r, "ZZHealthy", "healthy")
			w.executeErrors = map[string]error{`CREATE TABLE "Dst"."Mixed" ("Value" VARCHAR(100))`: driver.ErrBadConn}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.NotContains(t, w.statements, `CREATE TABLE "Dst"."ZZHealthy" ("Value" VARCHAR(100))`)
			require.Empty(t, w.copied)
		})
	}
}

func TestNativeManagedOldChildrenDoNotBlockHealthyReplacement(t *testing.T) {
	parent := damengnative.Object{ID: "source", Kind: "table", Name: "T"}
	p := &damengnative.Preflight{Objects: []damengnative.Object{parent}, TargetObjects: []damengnative.Object{
		{ID: "old", Kind: "table", Name: "T"},
		{ID: "old-index", Kind: "index", Name: "GeneratedOldIndex", Table: "T", ParentID: "old", Managed: true, Dependencies: []string{"old"}},
	}}
	states := map[string]error{}
	protectNativeRetainedTargets(p, states)
	require.Empty(t, states)
}

func TestNativeInlineChildPreflightFailureBlocksOwningTable(t *testing.T) {
	m, r, _ := nativeTestMigrator(Config{Mode: "all"})
	addNativeHealthyTable(r, "Healthy", "healthy")
	r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{ID: "inline", Kind: "index", Name: "Inline", Table: "Mixed", Managed: true, ParentID: "source-table", Dependencies: []string{"source-table"}})
	r.snapshot.Issues = []damengnative.Issue{{ObjectID: "inline", Kind: "index", Name: "Inline", Severity: "error", Reason: "inline name belongs to retained target table"}}
	p := m.PreflightDamengNative(context.Background())
	require.True(t, p.CanStart)
	require.Contains(t, p.Blocked, "source-table")
	require.Contains(t, p.Blocked, "inline")
	require.NotContains(t, p.Blocked, "healthy")
	require.Equal(t, "dependency", p.BlockedStages["inline"])
}
