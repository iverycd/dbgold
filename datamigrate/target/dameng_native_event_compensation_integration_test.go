package target

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"github.com/stretchr/testify/require"
)

// CALL SP_ENABLE_EVT_TRIGGER is not rolled back by SQL transaction rollback.
// Exercise the actual native compensation and a JSON-reloaded policy-only
// retry, then prove both hooks execute after the successful final boundary.
func TestDamengNativeEventFinalizationCompensationIntegration(t *testing.T) {
	dsn, schema := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if dsn == "" || schema == "" || os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" {
		t.Skip("需要专用DM8目标Schema、TARGET_DSN/SCHEMA和ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	defer db.Close()
	schema, err = damengnative.ResolveSchema(ctx, db, schema)
	require.NoError(t, err)
	prefix := fmt.Sprintf("EvtComp%d", time.Now().UnixNano())
	q := func(suffix string) string { return damengnative.Qualified(schema, prefix+suffix) }
	exec := func(query string) { _, err := db.ExecContext(ctx, query); require.NoError(t, err, query) }
	exec("CREATE TABLE " + q("Audit") + "(N INT)")
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, suffix := range []string{"A", "B"} {
			_, _ = db.ExecContext(clean, damengnative.EventTriggerStateSQL(schema, prefix+suffix, false))
		}
		for _, suffix := range []string{"A", "B"} {
			_, _ = db.ExecContext(clean, "DROP TRIGGER "+q(suffix))
		}
		for _, suffix := range []string{"Guard", "After", "Audit"} {
			_, _ = db.ExecContext(clean, "DROP TABLE "+q(suffix))
		}
	}()
	w, err := NewDaMengNative(dsn, schema, ConnPoolConfig{MaxOpenConns: 1, MaxIdleConns: 1})
	require.NoError(t, err)
	defer w.Close()
	var objects []repair.Object
	for _, suffix := range []string{"A", "B"} {
		native := &damengnative.Object{ID: suffix, Kind: "trigger", Schema: schema, Name: prefix + suffix, Status: "ENABLED", EventTrigger: true}
		create := "CREATE OR REPLACE TRIGGER " + q(suffix) + " AFTER DDL ON SCHEMA DISABLE BEGIN INSERT INTO " + q("Audit") + " VALUES(1); END;"
		native.Units = []damengnative.Unit{{SQL: create, Phase: "event_create"}, eventActivationUnit(*native)}
		require.NoError(t, w.InstallDamengNativeEvent(ctx, *native, native.Units[0]))
		objects = append(objects, repair.Object{ID: suffix, Kind: "trigger", Schema: schema, Name: native.Name, TargetName: native.Name, Native: native, NativeFingerprint: damengnative.Fingerprint(native), DataStatus: "not_required", Units: []repair.Unit{
			{SQL: create, OriginalSQL: create, Status: "succeeded"}, {SQL: native.Units[1].SQL, OriginalSQL: native.Units[1].SQL, Status: "succeeded"},
		}})
	}
	err = w.WithDamengNativeEventFinalization(ctx, func(final context.Context) error {
		for _, object := range objects {
			if err := w.SetDamengNativeEventState(final, *object.Native, object.Native.Units[1]); err != nil {
				return err
			}
		}
		return errors.New("injected late finalization failure")
	})
	var failure *damengnative.EventFinalizationFailure
	require.ErrorAs(t, err, &failure)
	require.Len(t, failure.States, 2)
	for i := range objects {
		require.True(t, repair.ApplyNativeEventRecovery(&objects[i], failure.States))
		require.Equal(t, "pending", objects[i].Units[1].Status)
		var status string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT STATUS FROM ALL_TRIGGERS WHERE OWNER=? AND TRIGGER_NAME=?", schema, objects[i].TargetName).Scan(&status))
		require.True(t, nativeEventDisabled(status))
	}
	exec("CREATE TABLE " + q("Guard") + "(N INT)")
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q("Audit")).Scan(&count))
	require.Zero(t, count, "补偿后DDL不能执行尚未全部恢复的事件业务体")
	// Simulate a legacy/process-crash ledger: A was enabled and recorded as
	// complete, while B was left disabled although its saved state says complete.
	exec(damengnative.EventTriggerStateSQL(schema, objects[0].TargetName, true))
	for i := range objects {
		objects[i].Units[1].Status = "succeeded"
	}
	var nativeEvents []damengnative.Object
	for _, object := range objects {
		nativeEvents = append(nativeEvents, *object.Native)
	}
	var paused []damengnative.EventStateRecovery
	pauseErr := w.PauseDamengNativeEventRecovery(ctx, nativeEvents, func(state damengnative.EventStateRecovery) error {
		paused = append(paused, state)
		for i := range objects {
			repair.ApplyNativeEventRecovery(&objects[i], []damengnative.EventStateRecovery{state})
		}
		return nil
	})
	if pauseErr != nil {
		var actual string
		if err := db.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TRIGGER',?,?)`, objects[0].TargetName, schema).Scan(&actual); err == nil {
			t.Logf("fixture frozen=%s; native=%s", objects[0].Native.Units[0].SQL, actual)
		}
	}
	require.NoError(t, pauseErr)
	require.Len(t, paused, 3) // A intent + A disabled proof + B read-only proof.
	for _, object := range objects {
		require.Equal(t, "pending", object.Units[1].Status)
	}
	encoded, err := json.Marshal(objects)
	require.NoError(t, err)
	var reloaded []repair.Object
	require.NoError(t, json.Unmarshal(encoded, &reloaded))
	proofs := 0
	proof := damengnative.WithFinalizationVerifier(ctx, func(context.Context) error { proofs++; return nil })
	err = w.WithDamengNativeEventFinalization(proof, func(final context.Context) error {
		for _, object := range reloaded {
			units, err := w.executeNativeRepair(final, object, object.Units, nil)
			if err != nil {
				return err
			}
			require.Equal(t, "succeeded", units[0].Status)
			require.Equal(t, "succeeded", units[1].Status)
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, proofs)
	exec("CREATE TABLE " + q("After") + "(N INT)")
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q("Audit")).Scan(&count))
	require.Equal(t, 2, count)
}
