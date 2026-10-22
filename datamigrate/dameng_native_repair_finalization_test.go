package datamigrate

import (
	"context"
	"testing"

	"dbgold/datamigrate/repair"
	"github.com/stretchr/testify/require"
)

func nativeFinalizationManifest(t *testing.T) (*nativeTestReader, *nativeTestWriter, []repair.Object) {
	t.Helper()
	m, r, w := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true})
	addNativeTestEvent(r)
	var saved RepairManifest
	m.cfg.RepairSink = func(manifest RepairManifest) error { saved = manifest; return nil }
	require.False(t, m.Run(context.Background()).HasFailures())
	return r, w, saved.Objects
}

func TestNativeRepairFinalizationReadinessRequiresEveryOtherDataAndDDLUnit(t *testing.T) {
	_, w, objects := nativeFinalizationManifest(t)
	writes := len(w.statements)
	require.NoError(t, NativeRepairFinalizationReady(objects, "event"))
	objects[0].DataStatus = "failed"
	require.ErrorContains(t, NativeRepairFinalizationReady(objects, "event"), "数据尚未完成")
	objects[0].DataStatus = "succeeded"
	objects[0].Units[0].Status = "unknown"
	require.ErrorContains(t, NativeRepairFinalizationReady(objects, "event"), "冻结步骤尚未完成")
	require.Len(t, w.statements, writes, "readiness must not issue target SQL")
}

func TestNativeRepairFinalizationCanKeepAllFinalStatesPendingTogether(t *testing.T) {
	_, _, objects := nativeFinalizationManifest(t)
	objects[1].Units[1].Status = "pending"
	objects[1].Status = "failed"
	require.NoError(t, NativeRepairFinalizationReady(objects, ""))
	require.True(t, NativeRepairHasFinalState(objects[1]))
}

func TestNativeRepairFinalizationRechecksSourcePlanAndConfiguredContent(t *testing.T) {
	for _, kind := range []string{"unchanged", "source_drift", "content_unavailable"} {
		t.Run(kind, func(t *testing.T) {
			r, w, objects := nativeFinalizationManifest(t)
			writes := len(w.statements)
			cfg := Config{Mode: "all", Content: "both", TargetSchema: "Dst"}
			switch kind {
			case "source_drift":
				r.snapshot.Objects[0].Units[0].SQL += " /* altered definition */"
			case "content_unavailable":
				cfg.ValidateData = true
			}
			err := VerifyNativeRepairFinalization(context.Background(), r, w, cfg, objects)
			if kind == "unchanged" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Len(t, w.statements, writes, "finalization proof must be read-only")
		})
	}
}
