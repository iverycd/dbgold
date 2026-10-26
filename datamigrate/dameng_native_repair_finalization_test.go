package datamigrate

import (
	"context"
	"strings"
	"testing"

	"dbgold/datamigrate/damengnative"
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

func TestNativeRepairFinalizationNormalizesLegacyEventInstallHeaderOnly(t *testing.T) {
	for _, drift := range []string{"none", "body", "state"} {
		t.Run(drift, func(t *testing.T) {
			r, w, objects := nativeFinalizationManifest(t)
			writes := len(w.statements)
			for i := range r.snapshot.Objects {
				hook := &r.snapshot.Objects[i]
				if !hook.EventTrigger {
					continue
				}
				for j := range hook.Units {
					if hook.Units[j].Phase != "event_create" {
						continue
					}
					var err error
					hook.Units[j].SQL, err = damengnative.DisabledEventTriggerDDL(hook.Units[j].SQL)
					require.NoError(t, err)
					if drift == "body" {
						hook.Units[j].SQL = strings.Replace(hook.Units[j].SQL, "NULL;", "PRINT 'changed'; NULL;", 1)
					}
				}
				if drift == "state" {
					hook.Status = "DISABLED"
				}
			}
			err := VerifyNativeRepairFinalization(context.Background(), r, w, Config{Mode: "all", Content: "both", TargetSchema: "Dst"}, objects)
			if drift == "none" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Len(t, w.statements, writes)
		})
	}
}

func TestNativeManualTriggerFinalizationUsesOriginalSourceAndApprovedTarget(t *testing.T) {
	for _, change := range []string{"none", "source_drift", "source_state", "original_missing", "original_tampered", "identity_changed", "active_tampered"} {
		t.Run(change, func(t *testing.T) {
			r, w, objects := nativeFinalizationManifest(t)
			source := damengnative.Object{ID: "row-trigger", Kind: "trigger", Schema: "Src", Name: "RowTrigger", Table: "Mixed", Status: "DISABLED", Dependencies: []string{"source-table"}, Units: []damengnative.Unit{{SQL: `CREATE TRIGGER "Src"."RowTrigger" AFTER DELETE ON "Src"."Mixed" FOR EACH ROW BEGIN NULL; END;`, Phase: "after_data"}}}
			r.snapshot.Objects = append(r.snapshot.Objects, source)
			originalNative := source
			originalNative.Schema = "Dst"
			originalNative.Units = append([]damengnative.Unit{}, source.Units...)
			var err error
			originalNative.Units[0], _, err = damengnative.RewriteUnit(source.Units[0], "Src", "Dst")
			require.NoError(t, err)
			original := repair.Object{ID: source.ID, Kind: source.Kind, Schema: "Dst", Name: source.Name, TargetName: source.Name, Table: source.Table, Native: &originalNative, NativeFingerprint: damengnative.Fingerprint(&originalNative), DataStatus: "not_required", Status: "failed", Units: []repair.Unit{{ID: "native-1", SQL: originalNative.Units[0].SQL, OriginalSQL: originalNative.Units[0].SQL, Status: "failed"}}}
			require.True(t, original.TrustedNative())
			activeNative := originalNative
			activeNative.Units = append([]damengnative.Unit{}, originalNative.Units...)
			activeNative.Units[0].SQL = `CREATE OR REPLACE TRIGGER "Dst"."RowTrigger" AFTER DELETE ON "Dst"."Mixed" FOR EACH ROW BEGIN INSERT INTO "Src"."Audit" VALUES ('manual'); END;`
			active := original
			active.Native = &activeNative
			active.NativeFingerprint = damengnative.Fingerprint(active.Native)
			active.Units = []repair.Unit{{ID: "manual-1", SQL: activeNative.Units[0].SQL, OriginalSQL: activeNative.Units[0].SQL, Status: "succeeded"}}
			active.Status = "succeeded"
			active.ManualTrigger = &repair.ManualTriggerPlan{SQL: activeNative.Units[0].SQL, Revision: 1, Original: &original}
			switch change {
			case "source_drift":
				r.snapshot.Objects[len(r.snapshot.Objects)-1].Units = []damengnative.Unit{{SQL: strings.Replace(source.Units[0].SQL, "NULL;", "PRINT 'changed';", 1), Phase: "after_data"}}
			case "source_state":
				r.snapshot.Objects[len(r.snapshot.Objects)-1].Status = "ENABLED"
			case "original_missing":
				active.ManualTrigger.Original = nil
			case "original_tampered":
				active.ManualTrigger.Original.NativeFingerprint = "changed"
			case "identity_changed":
				active.ManualTrigger.Original.Table = "Other"
			case "active_tampered":
				active.NativeFingerprint = "changed"
			}
			objects = append(objects, active)
			err = VerifyNativeRepairFinalization(context.Background(), r, w, Config{Mode: "all", Content: "both", TargetSchema: "Dst"}, objects)
			if change != "none" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NoError(t, NativeRepairFinalizationReady(objects, "event"))
			var expected *damengnative.Object
			for i := range w.verifySnapshot.Objects {
				if w.verifySnapshot.Objects[i].ID == source.ID {
					expected = &w.verifySnapshot.Objects[i]
				}
			}
			require.NotNil(t, expected)
			require.Equal(t, "Dst", expected.Schema)
			require.Equal(t, "DISABLED", expected.Status)
			require.Equal(t, activeNative.Units, expected.Units)
			require.Equal(t, source.Units, r.snapshot.Objects[len(r.snapshot.Objects)-1].Units, "target proof must not mutate the live source snapshot")
		})
	}
}
