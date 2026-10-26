package handler

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"github.com/stretchr/testify/require"
)

func nativeRecoveryFixture(id string, phases ...string) repair.Object {
	native := &damengnative.Object{ID: id, Kind: "trigger", Name: id, Schema: "Dst"}
	object := repair.Object{ID: id, Kind: "trigger", Name: id, TargetName: id, Schema: "Dst", Native: native, DataStatus: "not_required"}
	for _, phase := range phases {
		sql := id + ":" + phase
		native.Units = append(native.Units, damengnative.Unit{SQL: sql, Phase: phase})
		object.Units = append(object.Units, repair.Unit{SQL: sql, OriginalSQL: sql, Status: "pending", Selected: true})
	}
	object.NativeFingerprint = damengnative.Fingerprint(native)
	return object
}

func TestNativeRecoveryInstallDoesNotActivateFinalStateOrChangeSQL(t *testing.T) {
	object := nativeRecoveryFixture("MV", "after_data", "mv_prepare", "mv_rebase", "mv_restore", "event_activate")
	object.Units[0].Status = "succeeded"
	selected := nativeRecoveryPhaseUnits(object, "install")
	for i, unit := range selected {
		require.Equal(t, object.Units[i].SQL, unit.SQL)
		require.Equal(t, i == 1 || i == 2, unit.Selected)
	}
	selected[1].Status, selected[2].Status = "succeeded", "succeeded"
	object.Units = nativeRecoveryMergeSelections(object, selected)
	require.True(t, object.TrustedNative())
	require.True(t, nativeRecoveryInstallationComplete(object))
	require.False(t, object.AllUnitsSucceeded())
	require.True(t, object.Units[3].Selected)
	require.True(t, object.Units[4].Selected)
	legacy := repair.Object{Units: []repair.Unit{{SQL: "CREATE INDEX legacy", Selected: true}}}
	require.True(t, nativeRecoveryHasSelected(nativeRecoveryPhaseUnits(legacy, "install")))
	require.False(t, nativeRecoveryHasSelected(nativeRecoveryPhaseUnits(legacy, "mv_restore")))
}

func TestNativeRecoveryProvesOncePoliciesBeforeOnePinnedEventSession(t *testing.T) {
	items := []repair.Object{nativeRecoveryFixture("EventA", "event_activate"), nativeRecoveryFixture("MV", "mv_restore"), nativeRecoveryFixture("EventB", "event_activate")}
	var order []string
	type pinnedKey struct{}
	run := nativeRecoveryFinalization{
		proof: func(context.Context) error { order = append(order, "proof"); return nil },
		pinEvents: func(ctx context.Context, fn func(context.Context) error) error {
			order = append(order, "pin")
			return fn(context.WithValue(ctx, pinnedKey{}, true))
		},
		execute: func(ctx context.Context, object repair.Object, units []repair.Unit, step func([]repair.Unit) error) ([]repair.Unit, error) {
			require.NoError(t, damengnative.VerifyFinalization(ctx))
			for i := range units {
				if !units[i].Selected {
					continue
				}
				if object.Native.Units[i].Phase == "event_activate" {
					require.Equal(t, true, ctx.Value(pinnedKey{}))
				}
				order = append(order, units[i].SQL)
				units[i].Status = "succeeded"
				units[i].Selected = false
			}
			return units, nil
		},
	}
	out, err := executeNativeRecoveryFinalStates(context.Background(), items, run)
	require.NoError(t, err)
	require.Equal(t, []string{"proof", "MV:mv_restore", "pin", "EventA:event_activate", "EventB:event_activate"}, order)
	for _, object := range out {
		require.Equal(t, "succeeded", object.Status)
		require.True(t, object.TrustedNative())
		require.True(t, object.AllUnitsSucceeded())
	}
}

func TestNativeRecoveryPolicyFailurePreventsAllEventActivation(t *testing.T) {
	items := []repair.Object{nativeRecoveryFixture("Event", "event_activate"), nativeRecoveryFixture("MV", "mv_restore")}
	mutations := 0
	pin := false
	out, err := executeNativeRecoveryFinalStates(context.Background(), items, nativeRecoveryFinalization{
		proof: func(context.Context) error { return nil },
		execute: func(_ context.Context, _ repair.Object, units []repair.Unit, _ func([]repair.Unit) error) ([]repair.Unit, error) {
			mutations++
			units[0].Status = "unknown"
			return units, errors.New("policy proof failed")
		},
		pinEvents: func(context.Context, func(context.Context) error) error { pin = true; return nil },
	})
	require.ErrorContains(t, err, "policy proof failed")
	require.Equal(t, 1, mutations)
	require.False(t, pin)
	require.Equal(t, "unknown", out[1].Status)
	require.Equal(t, "pending", out[0].Units[0].Status)
}

func TestNativeRecoveryProofFailureIsReadOnlyAndSuccessfulUnitsStaySkipped(t *testing.T) {
	object := nativeRecoveryFixture("Event", "event_activate")
	object.Units[0].Status = "succeeded"
	_, err := executeNativeRecoveryFinalStates(context.Background(), []repair.Object{object}, nativeRecoveryFinalization{proof: func(context.Context) error { t.Fatal("已成功的步骤不应重复预检或执行"); return nil }})
	require.NoError(t, err)
	object.Units[0].Status = "pending"
	_, err = executeNativeRecoveryFinalStates(context.Background(), []repair.Object{object}, nativeRecoveryFinalization{proof: func(context.Context) error { return errors.New("content mismatch") }, execute: func(context.Context, repair.Object, []repair.Unit, func([]repair.Unit) error) ([]repair.Unit, error) {
		t.Fatal("核验失败后不能执行")
		return nil, nil
	}})
	require.ErrorContains(t, err, "content mismatch")
}

func TestNativeRecoveryManagedDependenciesFollowProvenInstalledParent(t *testing.T) {
	objects := []repair.Object{{ID: "storage", Native: &damengnative.Object{Managed: true, ParentID: "mv"}}, {ID: "index", Native: &damengnative.Object{Managed: true, ParentID: "storage"}}, {ID: "user", Native: &damengnative.Object{Managed: false, ParentID: "mv"}}}
	completed := map[string]bool{"mv": true}
	nativeRecoveryCompleteManaged(objects, completed)
	require.True(t, completed["storage"])
	require.True(t, completed["index"])
	require.False(t, completed["user"])
}

func TestNativeRecoveryCompensatedActivationUpdatesEveryObjectAndCanReload(t *testing.T) {
	items := []repair.Object{nativeRecoveryFixture("EventA", "event_create", "event_activate"), nativeRecoveryFixture("EventB", "event_create", "event_activate")}
	for i := range items {
		items[i].Native.EventTrigger, items[i].Native.Status = true, "ENABLED"
		items[i].NativeFingerprint = damengnative.Fingerprint(items[i].Native)
		items[i].Units[0].Status, items[i].Units[0].Selected = "succeeded", false
	}
	var saved []repair.Object
	var executed []string
	out, err := executeNativeRecoveryFinalStates(context.Background(), items, nativeRecoveryFinalization{
		proof: func(context.Context) error { return nil },
		execute: func(_ context.Context, object repair.Object, units []repair.Unit, _ func([]repair.Unit) error) ([]repair.Unit, error) {
			executed = append(executed, object.ID)
			units[1].Status = "succeeded"
			return units, nil
		},
		pinEvents: func(ctx context.Context, activate func(context.Context) error) error {
			require.NoError(t, activate(ctx))
			return &damengnative.EventFinalizationFailure{Cause: errors.New("last durable update failed"), States: []damengnative.EventStateRecovery{
				{Schema: "Dst", Name: "EventA", SQL: "EventA:event_activate", Status: "pending"},
				{Schema: "Dst", Name: "EventB", SQL: "EventB:event_activate", Status: "pending"},
			}}
		},
		save: func(object repair.Object) error { saved = append(saved, object); return nil },
	})
	require.ErrorContains(t, err, "last durable update failed")
	require.Equal(t, []string{"EventA", "EventB"}, executed)
	for _, object := range out {
		require.True(t, object.TrustedNative())
		require.Equal(t, "succeeded", object.Units[0].Status)
		require.Equal(t, "pending", object.Units[1].Status)
		require.True(t, object.Units[1].Selected)
	}
	require.Len(t, saved, 4)
}

func TestNativeRecoveryDisabledPolicyProofPrecedesFirstEnableAndSkipsLateReconcile(t *testing.T) {
	for _, uncertain := range []string{"unknown", "running"} {
		t.Run(uncertain, func(t *testing.T) {
			a := nativeRecoveryFixture("A", "event_activate")
			b := nativeRecoveryFixture("B", "event_create", "event_activate")
			b.Native.EventTrigger, b.Native.Status = true, "DISABLED"
			b.Native.Units[1].SQL = damengnative.EventTriggerStateSQL("Dst", "B", false)
			b.NativeFingerprint = damengnative.Fingerprint(b.Native)
			b.Units[0].Status = "succeeded"
			b.Units[1].SQL, b.Units[1].OriginalSQL = b.Native.Units[1].SQL, b.Native.Units[1].SQL
			b.Units[1].Status = uncertain
			var order []string
			out, err := executeNativeRecoveryFinalStates(context.Background(), []repair.Object{a, b}, nativeRecoveryFinalization{
				prepareEvents: func(_ context.Context, items []repair.Object) ([]repair.Object, error) {
					order = append(order, "pause-and-reconcile")
					require.True(t, repair.ApplyNativeEventRecovery(&items[1], []damengnative.EventStateRecovery{{Schema: "Dst", Name: "B", SQL: b.Native.Units[1].SQL, Status: "succeeded"}}))
					return items, nil
				},
				proof: func(context.Context) error { order = append(order, "whole-proof"); return nil },
				pinEvents: func(ctx context.Context, fn func(context.Context) error) error {
					order = append(order, "pinned")
					return fn(ctx)
				},
				execute: func(_ context.Context, object repair.Object, units []repair.Unit, _ func([]repair.Unit) error) ([]repair.Unit, error) {
					require.Equal(t, "A", object.ID, "B的不确定结果应在首次enable前归并，不能再进入会读取目录的reconcile")
					order = append(order, "enable-A")
					units[0].Status = "succeeded"
					return units, nil
				},
			})
			require.NoError(t, err)
			require.Equal(t, []string{"pause-and-reconcile", "whole-proof", "pinned", "enable-A"}, order)
			require.Equal(t, "succeeded", out[1].Units[1].Status)
		})
	}
}

func TestNativeRecoveryFinalizationProvesJobAfterReadOnlyPolicyReconciliation(t *testing.T) {
	object := nativeRecoveryFixture("Disabled", "event_activate")
	object.Units[0].Status = "succeeded"
	proofs := 0
	_, err := executeNativeRecoveryFinalStates(context.Background(), []repair.Object{object}, nativeRecoveryFinalization{
		forceProof: true,
		proof: func(context.Context) error {
			proofs++
			return errors.New("another table still differs")
		},
		pinEvents: func(context.Context, func(context.Context) error) error {
			t.Fatal("只读策略归并后没有待启用事件")
			return nil
		},
	})
	require.ErrorContains(t, err, "another table still differs")
	require.Equal(t, 1, proofs)
}

func TestNativeEventJobPauseExcludesFrozenOverwriteDropRecords(t *testing.T) {
	// Overwrite drops retain the original event flag for inventory purposes,
	// but they have no CREATE/activate lifecycle and must use ordinary DROP repair.
	drop := nativeRecoveryFixture("old-event", "drop")
	drop.Kind, drop.Native.Kind, drop.Native.EventTrigger = "native_drop", "native_drop", true
	drop.NativeFingerprint = damengnative.Fingerprint(drop.Native)
	require.False(t, nativeEventRepairObject(drop))
	require.NoError(t, pauseNativeFrozenEvents(context.Background(), nil, []repair.Object{drop}, func(repair.Object) error {
		t.Fatal("DROP不是待暂停的源事件")
		return nil
	}))
}
