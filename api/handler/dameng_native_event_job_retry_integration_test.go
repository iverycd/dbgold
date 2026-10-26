package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// Reproduce a legacy/process-crash ledger with A already enabled/succeeded and
// B still pending. Retrying B must safely coordinate the whole event group;
// neither success-only skipping nor a preflight rejection can finish this job.
func TestDamengNativeEventJobRetryPartialActivationIntegration(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		enabled      bool
		missing      bool
		attached     bool
	}{
		{"enabled_partial", "pending", true, false, false},
		{"disabled_unknown", "unknown", false, false, false},
		{"disabled_running", "running", false, false, false},
		{"missing_create_stale_policy", "succeeded", true, true, false},
		{"attached_group_disabled_unknown", "unknown", false, false, true},
		{"attached_group_disabled_running", "running", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testNativeEventJobRetry(t, tc.enabled, tc.status, tc.missing, tc.attached)
		})
	}
}

func testNativeEventJobRetry(t *testing.T, bEnabled bool, bStatus string, missing, attached bool) {
	sd, ss, dd, ds := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA"), os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if sd == "" || ss == "" || dd == "" || ds == "" || os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" {
		t.Skip("需要两套预建空DM8专用Schema、SOURCE/TARGET_DSN/SCHEMA及ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	src, err := sql.Open("dm", sd)
	require.NoError(t, err)
	defer src.Close()
	dst, err := sql.Open("dm", dd)
	require.NoError(t, err)
	defer dst.Close()
	ss, err = damengnative.ResolveSchema(ctx, src, ss)
	require.NoError(t, err)
	ds, err = damengnative.ResolveSchema(ctx, dst, ds)
	require.NoError(t, err)
	for _, side := range []struct {
		db     *sql.DB
		schema string
	}{{src, ss}, {dst, ds}} {
		var count int
		require.NoError(t, side.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, side.schema).Scan(&count))
		require.Zero(t, count, "必须使用空的专用测试Schema")
	}
	sq := func(name string) string { return damengnative.Qualified(ss, name) }
	dq := func(name string) string { return damengnative.Qualified(ds, name) }
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, side := range []struct {
			db     *sql.DB
			schema string
		}{{src, ss}, {dst, ds}} {
			for _, name := range []string{"RetryA", "RetryB"} {
				_, _ = side.db.ExecContext(clean, damengnative.EventTriggerStateSQL(side.schema, name, false))
			}
			for _, name := range []string{"RetryA", "RetryB"} {
				_, _ = side.db.ExecContext(clean, "DROP TRIGGER "+damengnative.Qualified(side.schema, name))
			}
			for _, name := range []string{"RetryGuard", "RetryAudit"} {
				_, _ = side.db.ExecContext(clean, "DROP TABLE "+damengnative.Qualified(side.schema, name))
			}
		}
	}()
	_, err = src.ExecContext(ctx, "CREATE TABLE "+sq("RetryAudit")+"(N INT)")
	require.NoError(t, err)
	for _, name := range []string{"RetryA", "RetryB"} {
		_, err = src.ExecContext(ctx, "CREATE TRIGGER "+sq(name)+" AFTER DDL ON SCHEMA DISABLE BEGIN INSERT INTO "+sq("RetryAudit")+" VALUES(1); END;")
		require.NoError(t, err)
	}
	for _, name := range []string{"RetryA", "RetryB"} {
		_, err = src.ExecContext(ctx, damengnative.EventTriggerStateSQL(ss, name, name == "RetryA" || bEnabled))
		require.NoError(t, err)
	}
	r, err := source.NewDaMengNative(sd, ss, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	w, err := target.NewDaMengNative(dd, ds, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	var saved datamigrate.RepairManifest
	cfg := datamigrate.Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: ds, CaptureRepair: true, ValidateData: true, RepairSink: func(manifest datamigrate.RepairManifest) error { saved = manifest; return nil }}
	m := datamigrate.NewMigrator(r, w, &datamigrate.Job{LogCh: make(chan string, 4096)}, cfg)
	report := m.Run(ctx)
	require.False(t, report.HasFailures(), "%+v", report.NativeSchema)
	_, err = dst.ExecContext(ctx, damengnative.EventTriggerStateSQL(ds, "RetryB", false))
	require.NoError(t, err)
	if missing {
		_, err = dst.ExecContext(ctx, damengnative.EventTriggerStateSQL(ds, "RetryA", false))
		require.NoError(t, err)
		_, err = dst.ExecContext(ctx, "DROP TRIGGER "+dq("RetryB"))
		require.NoError(t, err)
		_, err = dst.ExecContext(ctx, damengnative.EventTriggerStateSQL(ds, "RetryA", true))
		require.NoError(t, err)
	}
	// JSON is the only input to the retry, as after an application restart.
	encoded, err := json.Marshal(saved)
	require.NoError(t, err)
	var reloaded datamigrate.RepairManifest
	require.NoError(t, json.Unmarshal(encoded, &reloaded))
	repairPersistenceDB(t)
	const jobID = "native-partial-event-retry"
	var current repair.Object
	for _, object := range reloaded.Objects {
		if object.TargetName == "RetryB" && object.Native != nil && object.Native.EventTrigger {
			for i := range object.Units {
				if object.Native.Units[i].Phase == "event_activate" {
					object.Units[i].Status, object.Units[i].Selected = bStatus, bStatus != "succeeded"
				}
				if missing && object.Native.Units[i].Phase == "event_create" {
					object.Units[i].Status, object.Units[i].Selected = "unknown", true
				}
			}
			object.Status = "failed"
			current = object
		}
		require.NoError(t, persistMigrationRepairObject(jobID, object))
	}
	require.NotNil(t, current.Native)
	var units []repair.Unit
	if attached {
		// A was compensated to disabled/pending. B's original disable had an
		// unknown response and was excluded from the table's selected related
		// objects. Finalization must reload and reconcile B before enabling A.
		_, err = dst.ExecContext(ctx, damengnative.EventTriggerStateSQL(ds, "RetryA", false))
		require.NoError(t, err)
		all, _, loadErr := repairObjects(jobID)
		require.NoError(t, loadErr)
		var selected []repair.Object
		for _, object := range all {
			if object.TargetName != "RetryA" || object.Native == nil || !object.Native.EventTrigger {
				continue
			}
			for i := range object.Units {
				if object.Native.Units[i].Phase == "event_activate" {
					object.Units[i].Status, object.Units[i].Selected = "pending", true
				}
			}
			require.NoError(t, persistMigrationRepairObject(jobID, object))
			selected = append(selected, object)
		}
		require.Len(t, selected, 1)
		_, err = executeNativeRecoveryFinalStates(ctx, selected, nativeRecoveryFinalization{
			prepareEvents: func(prepare context.Context, _ []repair.Object) ([]repair.Object, error) {
				return prepareNativeEventJobPolicies(prepare, jobID, w)
			},
			proof: func(proof context.Context) error {
				all, _, err := repairObjects(jobID)
				if err != nil {
					return err
				}
				if err = datamigrate.NativeRepairFinalizationReady(all, ""); err != nil {
					return err
				}
				return datamigrate.VerifyNativeRepairFinalization(proof, r, w, cfg, all)
			},
			execute: func(run context.Context, object repair.Object, selected []repair.Unit, progress func([]repair.Unit) error) ([]repair.Unit, error) {
				return target.ExecuteRepair(run, w, object, selected, progress)
			},
			pinEvents: w.WithDamengNativeEventFinalization,
			progress: func(object repair.Object, units []repair.Unit) error {
				object.Units = units
				return persistMigrationRepairObject(jobID, object)
			},
			save: func(object repair.Object) error { return persistMigrationRepairObject(jobID, object) },
		})
		require.NoError(t, err)
		after, _, loadErr := repairObjects(jobID)
		require.NoError(t, loadErr)
		for _, object := range after {
			if object.ID == current.ID {
				units = object.Units
			}
		}
	} else {
		units, err = retryNativeEventJob(ctx, jobID, current, r, w, cfg, nil)
	}
	require.NoError(t, err)
	for _, unit := range units {
		require.Equal(t, "succeeded", unit.Status)
	}
	objects, _, err := repairObjects(jobID)
	require.NoError(t, err)
	for _, object := range objects {
		if object.Native != nil && object.Native.EventTrigger {
			require.True(t, object.AllUnitsSucceeded(), object.Name)
			require.True(t, object.TrustedNative())
		}
	}
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("RetryAudit")).Scan(&count))
	require.Zero(t, count, "核验/补迁不能触发导入事件的业务DDL")
	_, err = dst.ExecContext(ctx, "CREATE TABLE "+dq("RetryGuard")+"(N INT)")
	require.NoError(t, err)
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("RetryAudit")).Scan(&count))
	expected := 1
	if bEnabled {
		expected++
	}
	require.Equal(t, expected, count, "A必须恢复，B必须保持源原始启停状态")
}
