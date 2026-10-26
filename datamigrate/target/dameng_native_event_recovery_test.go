package target

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func eventRecoveryObjects() []damengnative.Object {
	a, create := nativeEventFixture()
	a.Units = []damengnative.Unit{create, eventActivationUnit(a)}
	b := a
	b.Name = "Other"
	b.Units = []damengnative.Unit{{SQL: `CREATE TRIGGER "MixedOwner"."Other" AFTER DDL ON SCHEMA DISABLE BEGIN NULL; END;`, Phase: "event_create"}, eventActivationUnit(b)}
	return []damengnative.Object{a, b}
}

func expectEventRecoveryRead(mock sqlmock.Sqlmock, object damengnative.Object, status string, definition string) {
	mock.ExpectQuery(`SELECT o.SUBTYPE\$,t.STATUS FROM SYS.SYSOBJECTS`).WithArgs(object.Schema, object.Name).WillReturnRows(sqlmock.NewRows([]string{"type", "status"}).AddRow("TRIG", status))
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs(object.Name, object.Schema).WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(definition))
}

func TestNativeEventRecoveryAuthenticatesAllDefinitionsBeforeAnyPolicyWrite(t *testing.T) {
	w, mock := mockNativeDM(t)
	objects := eventRecoveryObjects()
	expectEventRecoveryRead(mock, objects[0], "Y", objects[0].Units[0].SQL)
	expectEventRecoveryRead(mock, objects[1], "Y", `CREATE TRIGGER "MixedOwner"."Other" AFTER DDL ON SCHEMA DISABLE BEGIN RAISE_APPLICATION_ERROR(-20001,'changed'); END;`)
	called := false
	err := w.PauseDamengNativeEventRecovery(context.Background(), objects, func(damengnative.EventStateRecovery) error { called = true; return nil })
	require.ErrorContains(t, err, "未停用任何事件")
	require.False(t, called)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventRecoveryPersistsIntentAndReconcilesDisabledCompletedPolicy(t *testing.T) {
	w, mock := mockNativeDM(t)
	objects := eventRecoveryObjects()
	expectEventRecoveryRead(mock, objects[0], "Y", objects[0].Units[0].SQL)
	expectEventRecoveryRead(mock, objects[1], "N", objects[1].Units[0].SQL)
	mock.ExpectExec(regexp.QuoteMeta(damengnative.EventTriggerStateSQL(objects[0].Schema, objects[0].Name, false))).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs(objects[0].Schema, objects[0].Name).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("N"))
	var states []damengnative.EventStateRecovery
	err := w.PauseDamengNativeEventRecovery(context.Background(), objects, func(state damengnative.EventStateRecovery) error { states = append(states, state); return nil })
	require.NoError(t, err)
	require.Len(t, states, 3)
	require.Equal(t, "unknown", states[0].Status)
	require.Equal(t, "pending", states[1].Status)
	require.Equal(t, "pending", states[2].Status)
	require.Equal(t, objects[1].Units[1].SQL, states[2].SQL)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventRecoveryRequiresDurableIntentBeforeDisabling(t *testing.T) {
	w, mock := mockNativeDM(t)
	objects := eventRecoveryObjects()[:1]
	expectEventRecoveryRead(mock, objects[0], "Y", objects[0].Units[0].SQL)
	err := w.PauseDamengNativeEventRecovery(context.Background(), objects, func(damengnative.EventStateRecovery) error { return errors.New("ledger unavailable") })
	require.ErrorContains(t, err, "ledger unavailable")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventRecoveryNeverRewritesDesiredDisabledCompletedState(t *testing.T) {
	w, mock := mockNativeDM(t)
	objects := eventRecoveryObjects()[:1]
	objects[0].Status = "DISABLED"
	objects[0].Units[1] = eventActivationUnit(objects[0])
	expectEventRecoveryRead(mock, objects[0], "N", objects[0].Units[0].SQL)
	err := w.PauseDamengNativeEventRecovery(context.Background(), objects, func(state damengnative.EventStateRecovery) error {
		require.Equal(t, "succeeded", state.Status, "只读证明不得重置已完成的原DISABLED策略")
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventRecoveryReconcilesUncertainOriginalDisabledBeforeActivation(t *testing.T) {
	for _, status := range []string{"unknown", "running", "failed", "pending"} {
		t.Run(status, func(t *testing.T) {
			w, mock := mockNativeDM(t)
			object := nativeEventRepairFixture()
			object.Native.Status = "DISABLED"
			object.Native.Units[1] = eventActivationUnit(*object.Native)
			object.NativeFingerprint = damengnative.Fingerprint(object.Native)
			object.Units[1].SQL = object.Native.Units[1].SQL
			object.Units[1].OriginalSQL = object.Native.Units[1].SQL
			object.Units[1].Status, object.Units[1].Selected = status, true
			expectEventRecoveryRead(mock, *object.Native, "N", object.Native.Units[0].SQL)
			err := w.PauseDamengNativeEventRecovery(context.Background(), []damengnative.Object{*object.Native}, func(state damengnative.EventStateRecovery) error {
				require.Equal(t, "succeeded", state.Status)
				require.True(t, repair.ApplyNativeEventRecovery(&object, []damengnative.EventStateRecovery{state}))
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, "succeeded", object.Units[0].Status)
			require.Equal(t, "succeeded", object.Units[1].Status)
			require.False(t, object.Units[1].Selected)
			require.True(t, object.TrustedNative())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestNativeEventRecoveryMissingCreateNeverProvesDisabledPolicySuccessful(t *testing.T) {
	w, mock := mockNativeDM(t)
	object := nativeEventRepairFixture()
	object.Native.Status = "DISABLED"
	object.Native.Units[1] = eventActivationUnit(*object.Native)
	object.NativeFingerprint = damengnative.Fingerprint(object.Native)
	object.Units[0].Status = "unknown"
	object.Units[1].SQL = object.Native.Units[1].SQL
	object.Units[1].OriginalSQL = object.Native.Units[1].SQL
	object.Units[1].Status = "succeeded"
	mock.ExpectQuery(`SELECT o.SUBTYPE\$,t.STATUS FROM SYS.SYSOBJECTS`).WithArgs(object.Schema, object.TargetName).WillReturnRows(sqlmock.NewRows([]string{"type", "status"}))
	err := w.PauseDamengNativeEventRecovery(context.Background(), []damengnative.Object{*object.Native}, func(state damengnative.EventStateRecovery) error {
		require.Equal(t, "pending", state.Status)
		require.True(t, repair.ApplyNativeEventRecovery(&object, []damengnative.EventStateRecovery{state}))
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, "unknown", object.Units[0].Status)
	require.Equal(t, "pending", object.Units[1].Status)
	require.True(t, object.Units[1].Selected)
	require.NoError(t, mock.ExpectationsWereMet())
}
