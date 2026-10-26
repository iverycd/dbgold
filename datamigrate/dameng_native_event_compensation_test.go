package datamigrate

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

type nativeCompensatedEventWriter struct{ *nativeTestWriter }

func (w *nativeCompensatedEventWriter) WithDamengNativeEventFinalization(ctx context.Context, activate func(context.Context) error) error {
	if err := activate(ctx); err != nil {
		return err
	}
	return &damengnative.EventFinalizationFailure{Cause: errors.New("later final-state persistence failed"), States: []damengnative.EventStateRecovery{
		{Schema: "Dst", Name: "Hook", SQL: damengnative.EventTriggerStateSQL("Dst", "Hook", true), Status: "pending"},
		{Schema: "Dst", Name: "Second", SQL: damengnative.EventTriggerStateSQL("Dst", "Second", true), Status: "pending"},
	}}
}

func TestDamengNativeRunCompensatedEventsDoNotKeepSuccessfulActivationInLedger(t *testing.T) {
	m, r, w := nativeTestMigrator(Config{Mode: "all", CaptureRepair: true})
	addNativeTestEvent(r)
	second := r.snapshot.Objects[len(r.snapshot.Objects)-1]
	second.ID, second.Name = "second-event", "Second"
	second.CompileSQL = `ALTER TRIGGER "Src"."Second" COMPILE`
	second.Units = []damengnative.Unit{
		{SQL: `CREATE TRIGGER "Src"."Second" AFTER DDL ON SCHEMA BEGIN NULL; END;`, Phase: "event_create"},
		{SQL: damengnative.EventTriggerStateSQL("Src", "Second", true), Phase: "event_activate"},
	}
	r.snapshot.Objects = append(r.snapshot.Objects, second)
	m.writer = &nativeCompensatedEventWriter{nativeTestWriter: w}
	var saved RepairManifest
	m.cfg.RepairSink = func(manifest RepairManifest) error { saved = manifest; return nil }
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	for _, object := range saved.Objects {
		if object.Native == nil || !object.Native.EventTrigger {
			continue
		}
		require.True(t, object.TrustedNative())
		for i, unit := range object.Units {
			if object.Native.Units[i].Phase == "event_activate" {
				require.Equal(t, "pending", unit.Status)
				require.True(t, unit.Selected)
			} else {
				require.Equal(t, "succeeded", unit.Status)
			}
		}
	}
}
