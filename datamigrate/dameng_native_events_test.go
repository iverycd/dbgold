package datamigrate

import (
	"context"
	"errors"
	"slices"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

func (w *nativeTestWriter) InstallDamengNativeEvent(ctx context.Context, _ damengnative.Object, unit damengnative.Unit) error {
	return w.ExecuteDamengNative(ctx, unit.SQL)
}
func (w *nativeTestWriter) WithDamengNativeEventFinalization(ctx context.Context, activate func(context.Context) error) error {
	w.lifecycle = append(w.lifecycle, "final_session")
	return activate(ctx)
}
func (w *nativeTestWriter) SetDamengNativeEventState(ctx context.Context, _ damengnative.Object, unit damengnative.Unit) error {
	return w.ExecuteDamengNative(ctx, unit.SQL)
}
func (w *nativeTestWriter) VerifyDamengNativeMaterializedPolicy(_ context.Context, object damengnative.Object) error {
	w.lifecycle = append(w.lifecycle, "policy_verify:"+object.Name)
	return w.policyVerifyErr
}

func addNativeTestEvent(r *nativeTestReader) {
	r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{ID: "event", Kind: "trigger", Schema: "Src", Name: "Hook", EventTrigger: true, Status: "ENABLED", Dependencies: []string{"source-table"}, CompileSQL: `ALTER TRIGGER "Src"."Hook" COMPILE`, Units: []damengnative.Unit{
		{SQL: `CREATE TRIGGER "Src"."Hook" AFTER DDL ON SCHEMA BEGIN NULL; END;`, Phase: "event_create"},
		{SQL: damengnative.EventTriggerStateSQL("Src", "Hook", true), Phase: "event_activate"},
	}})
}

func TestDamengNativeEventActivationFollowsEveryTargetCheck(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true})
	addNativeTestEvent(r)
	var saved RepairManifest
	m.cfg.RepairSink = func(manifest RepairManifest) error { saved = manifest; return nil }
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report.NativeSchema.Verification)
	activation := damengnative.EventTriggerStateSQL("Dst", "Hook", true)
	require.Equal(t, activation, w.lifecycle[len(w.lifecycle)-1])
	require.Less(t, slices.Index(w.lifecycle, `ALTER TRIGGER "Dst"."Hook" COMPILE`), slices.Index(w.lifecycle, "verify"))
	require.Less(t, slices.Index(w.lifecycle, "verify"), slices.Index(w.lifecycle, "final_session"))
	require.Len(t, report.NativeSchema.Inventory, 2)
	require.Equal(t, 1, report.Triggers.Total)
	require.Equal(t, 1, report.Triggers.Success)
	require.Zero(t, report.Triggers.Failed)
	require.Equal(t, "ENABLED", r.snapshot.Objects[1].Status)
	require.Equal(t, "DISABLED", w.verifySnapshot.Objects[1].Status)
	require.Equal(t, "succeeded", saved.Objects[1].Status)
	require.True(t, saved.Objects[1].AllUnitsSucceeded())
}

func TestDamengNativeEventNeverActivatesAfterVerificationOrDataFailure(t *testing.T) {
	for _, failure := range []string{"verification", "data_validation", "ddl", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			m, r, w := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true})
			addNativeTestEvent(r)
			ctx := context.Background()
			switch failure {
			case "verification":
				w.verifyErr = errors.New("read failed")
			case "data_validation":
				m.cfg.ValidateData = true // The fake reader has no validation.Scanner.
			case "ddl":
				w.fail[`ALTER TRIGGER "Dst"."Hook" COMPILE`] = 1
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			report := m.Run(ctx)
			require.True(t, report.HasFailures())
			require.NotContains(t, w.lifecycle, "final_session")
			require.NotContains(t, w.statements, damengnative.EventTriggerStateSQL("Dst", "Hook", true))
		})
	}
}

func TestDamengNativeEventStateLiteralMapsWithoutRewritingSource(t *testing.T) {
	m, r, _ := nativeTestMigrator(Config{Mode: "all"})
	addNativeTestEvent(r)
	p := m.PreflightDamengNative(context.Background())
	require.True(t, p.Ready, "%+v", p.Issues)
	require.Equal(t, "event_create", p.Objects[1].Units[0].Phase)
	require.Equal(t, damengnative.EventTriggerStateSQL("Dst", "Hook", true), p.Objects[1].Units[1].SQL)
	require.Equal(t, damengnative.EventTriggerStateSQL("Src", "Hook", true), r.snapshot.Objects[1].Units[1].SQL)
}

func addNativeTestMVPolicy(r *nativeTestReader) string {
	const policy = `ALTER MATERIALIZED VIEW "Dst"."Mv" REFRESH COMPLETE ON DEMAND`
	r.snapshot.Objects = append(r.snapshot.Objects, damengnative.Object{ID: "mv", Kind: "materialized_view", Schema: "Src", Name: "Mv", Materialized: &damengnative.MaterializedState{Query: `SELECT "Value" FROM "Src"."Mixed"`, RefreshMethod: "COMPLETE", RefreshRule: "ON DEMAND"}, Units: []damengnative.Unit{
		{SQL: `CREATE MATERIALIZED VIEW "Src"."Mv" NEVER REFRESH AS SELECT "Value" FROM "Src"."Mixed"`, Phase: "after_data"},
		{SQL: `ALTER MATERIALIZED VIEW "Src"."Mv" REFRESH COMPLETE ON DEMAND`, Phase: "mv_restore"},
	}})
	return policy
}

func TestDamengNativePolicyRestoresAfterProofBeforeEventActivation(t *testing.T) {
	for _, withEvent := range []bool{false, true} {
		t.Run(map[bool]string{false: "policy_only", true: "policy_then_event"}[withEvent], func(t *testing.T) {
			m, r, w := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true})
			policy := addNativeTestMVPolicy(r)
			if withEvent {
				addNativeTestEvent(r)
			}
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), "%+v", report.NativeSchema.Verification)
			require.Less(t, slices.Index(w.lifecycle, "verify"), slices.Index(w.lifecycle, policy))
			require.Less(t, slices.Index(w.lifecycle, policy), slices.Index(w.lifecycle, "policy_verify:Mv"))
			if withEvent {
				require.Less(t, slices.Index(w.lifecycle, "policy_verify:Mv"), slices.Index(w.lifecycle, "final_session"))
			}
		})
	}
}

func TestDamengNativePolicyStaysPausedAfterFailedContentValidation(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true, ValidateData: true})
	policy := addNativeTestMVPolicy(r)
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.NotContains(t, w.statements, policy)
	require.NotContains(t, w.lifecycle, "policy_verify:Mv")
}

func TestDamengNativePolicyProofFailureBlocksEventsAndPersistsUnknown(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true})
	addNativeTestMVPolicy(r)
	addNativeTestEvent(r)
	w.policyVerifyErr = errors.New("policy mismatch")
	var saved RepairManifest
	m.cfg.RepairSink = func(manifest RepairManifest) error { saved = manifest; return nil }
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.NotContains(t, w.lifecycle, "final_session")
	found := false
	for _, object := range saved.Objects {
		if object.ID == "mv" {
			found = true
			require.Equal(t, "unknown", object.Units[1].Status)
			require.Contains(t, object.Units[1].Error, "policy mismatch")
		}
	}
	require.True(t, found)
}
