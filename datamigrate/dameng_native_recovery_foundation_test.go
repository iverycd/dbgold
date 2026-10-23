package datamigrate

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"github.com/stretchr/testify/require"
)

func TestNativeDataRecoveryRequiresEveryFoundationToHaveSucceeded(t *testing.T) {
	for _, phase := range []string{"before_data", "drop", "interval_prepare"} {
		for _, status := range []string{"failed", "pending", "unknown", "running"} {
			for _, dataComplete := range []bool{false, true} {
				dataState := map[bool]string{false: "data_pending", true: "data_done"}[dataComplete]
				t.Run(phase+"/"+status+"/"+dataState, func(t *testing.T) {
					m, r, w, object := nativeRecoveryFixture(t, true)
					index := 0
					if phase != "before_data" {
						unit := damengnative.Unit{Phase: phase, SQL: "frozen " + phase + " operation"}
						if phase == "interval_prepare" {
							unit.SQL = damengnative.IntervalPartitionSQL("Dst", "Mixed", &damengnative.IntervalPartitionPlan{})
						}
						object.Native.Units = append(object.Native.Units, unit)
						object.Units = append(object.Units, repair.Unit{SQL: unit.SQL, OriginalSQL: unit.SQL, Status: "succeeded"})
						index = len(object.Units) - 1
						object.NativeFingerprint = damengnative.Fingerprint(object.Native)
					}
					if dataComplete {
						object.DataStatus = "succeeded"
						w.copied = append([][]any{}, r.rows...)
					}
					require.True(t, object.TrustedNative())
					// A preview persisted by an earlier version must be rechecked on
					// execution, even if its fingerprint still matches its own data.
					preview, err := PreviewRecovery(context.Background(), r, w, m.cfg, object, nil, nil)
					require.NoError(t, err)
					preview.NativeUnits[index].Status = status
					preview.Fingerprint = recoveryFingerprint(m.cfg, preview)
					object.Units[index].Status = status
					w.statements = nil
					w.batchSizes = nil
					r.scans = 0
					copied := damengnative.Fingerprint(w.copied)

					_, err = PreviewRecovery(context.Background(), r, w, m.cfg, object, nil, nil)
					require.ErrorContains(t, err, "冻结 DDL 修复入口")
					res, err := RunRecovery(context.Background(), r, w, m.cfg, preview, true, nil)
					require.ErrorContains(t, err, "冻结 DDL 修复入口")
					require.Equal(t, "failed", res.Status)
					require.Zero(t, r.scans, "unprepared table recovery must not open a source cursor")
					require.Empty(t, w.statements, "neither truncation nor raw typed-marker SQL may execute")
					require.Empty(t, w.batchSizes)
					require.Equal(t, copied, damengnative.Fingerprint(w.copied))
				})
			}
		}
	}
}

func TestNativePendingTableUnitsCheckAllFoundationsBeforeAnyDDL(t *testing.T) {
	_, _, w := nativeTestMigrator(Config{})
	for _, status := range []string{"failed", "pending", "unknown", "running"} {
		t.Run(status, func(t *testing.T) {
			after := damengnative.Unit{SQL: `ALTER TABLE "Dst"."Mixed" ADD CHECK("Value" IS NOT NULL)`, Phase: "after_data"}
			prepare := damengnative.Unit{SQL: damengnative.IntervalPartitionSQL("Dst", "Mixed", &damengnative.IntervalPartitionPlan{}), Phase: "interval_prepare"}
			p := repair.RecoveryPreview{NativeObject: &damengnative.Object{Units: []damengnative.Unit{after, prepare}}, NativeUnits: []repair.Unit{{SQL: after.SQL, Status: "pending"}, {SQL: prepare.SQL, Status: status}}}
			err := executePendingNativeTable(context.Background(), w, p, map[string]bool{prepare.SQL: true})
			require.ErrorContains(t, err, "冻结 DDL 修复入口")
			require.Empty(t, w.statements, "a later incomplete foundation must prevent all earlier pending DDL, regardless of a restore SQL skip map")
		})
	}
}

func TestNativeDataRecoveryDoesNotReplaySuccessfulIntervalPreparation(t *testing.T) {
	m, r, w, flow := nativeIntervalFixture(t)
	m.cfg.TargetSchema = "Dst"
	m.cfg.RepairSink = func(RepairManifest) error {
		flow.mu.Lock()
		flow.persisted = true
		flow.mu.Unlock()
		return nil
	}
	w.copyErr = errors.New("data batch interrupted after interval preparation")
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, 1, flow.calls)
	require.Len(t, report.RepairManifest.Objects, 1)
	object := report.RepairManifest.Objects[0]
	require.Equal(t, "succeeded", object.Units[1].Status)
	w.copyErr = nil
	w.snapshot.Tables = append([]damengnative.Table{}, r.snapshot.Tables...)
	w.snapshot.Tables[0].Schema = "Dst"
	statements := len(w.statements)
	preview, err := PreviewRecovery(context.Background(), m.reader, m.writer, m.cfg, object, nil, nil)
	require.NoError(t, err)
	res, err := RunRecovery(context.Background(), m.reader, m.writer, m.cfg, preview, true, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", res.DataStatus)
	require.Equal(t, r.rows, w.copied)
	require.Equal(t, 1, flow.calls, "typed interval preparation already proved by the durable ledger must not run again")
	require.Equal(t, []string{`TRUNCATE TABLE "Dst"."Mixed"`}, w.statements[statements:])
}
