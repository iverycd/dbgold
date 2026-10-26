package target

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func eventActivationUnit(object damengnative.Object) damengnative.Unit {
	return damengnative.Unit{SQL: damengnative.EventTriggerStateSQL(object.Schema, object.Name, object.Status == "ENABLED"), Phase: "event_activate"}
}

func TestNativeEventFinalizationFailurePausesEveryAttemptOnTheSameSession(t *testing.T) {
	for _, mode := range []string{"second_call", "ledger", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			w, mock := mockNativeDM(t)
			a, _ := nativeEventFixture()
			b := a
			b.Name = "SecondEvent"
			au, bu := eventActivationUnit(a), eventActivationUnit(b)
			mock.ExpectExec(`SET SCHEMA`).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(regexp.QuoteMeta(au.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
			if mode == "second_call" {
				mock.ExpectExec(regexp.QuoteMeta(bu.SQL)).WillReturnError(errors.New("second activation failed"))
				mock.ExpectExec(regexp.QuoteMeta(damengnative.EventTriggerStateSQL(b.Schema, b.Name, false))).WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs(b.Schema, b.Name).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("N"))
			}
			mock.ExpectExec(regexp.QuoteMeta(damengnative.EventTriggerStateSQL(a.Schema, a.Name, false))).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs(a.Schema, a.Name).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("N"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := w.WithDamengNativeEventFinalization(ctx, func(final context.Context) error {
				if err := w.SetDamengNativeEventState(final, a, au); err != nil {
					return err
				}
				if mode == "cancelled" {
					cancel()
					return context.Canceled
				}
				if mode == "ledger" {
					return errors.New("durable group state unavailable")
				}
				// A nested repair uses exactly the same activation coordinator.
				return w.WithDamengNativeEventFinalization(final, func(nested context.Context) error {
					return w.SetDamengNativeEventState(nested, b, bu)
				})
			})
			var failure *damengnative.EventFinalizationFailure
			require.ErrorAs(t, err, &failure)
			require.NotEmpty(t, failure.States)
			for _, state := range failure.States {
				require.Equal(t, "pending", state.Status)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestNativeEventCleanupAcknowledgementLossUsesCatalogProof(t *testing.T) {
	w, mock := mockNativeDM(t)
	a, _ := nativeEventFixture()
	u := eventActivationUnit(a)
	mock.ExpectExec(`SET SCHEMA`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(u.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(damengnative.EventTriggerStateSQL(a.Schema, a.Name, false))).WillReturnError(errors.New("acknowledgement lost"))
	mock.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs(a.Schema, a.Name).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("N"))
	err := w.WithDamengNativeEventFinalization(context.Background(), func(final context.Context) error {
		require.NoError(t, w.SetDamengNativeEventState(final, a, u))
		return errors.New("later failure")
	})
	var failure *damengnative.EventFinalizationFailure
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "pending", failure.States[0].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventUnprovedCleanupRemainsUnknown(t *testing.T) {
	w, mock := mockNativeDM(t)
	a, _ := nativeEventFixture()
	u := eventActivationUnit(a)
	mock.ExpectExec(`SET SCHEMA`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(u.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(damengnative.EventTriggerStateSQL(a.Schema, a.Name, false))).WillReturnError(errors.New("disconnected"))
	mock.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs(a.Schema, a.Name).WillReturnError(errors.New("disconnected"))
	err := w.WithDamengNativeEventFinalization(context.Background(), func(final context.Context) error {
		require.NoError(t, w.SetDamengNativeEventState(final, a, u))
		return errors.New("later failure")
	})
	var failure *damengnative.EventFinalizationFailure
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "unknown", failure.States[0].Status)
	require.Contains(t, failure.States[0].Error, "disconnected")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventRepairLedgerFailureCompensatesAndReloadCanRetryOnlyPolicy(t *testing.T) {
	w, mock := mockNativeDM(t)
	o := nativeEventRepairFixture()
	u := o.Native.Units[1]
	mock.ExpectExec(`SET SCHEMA`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(u.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(damengnative.EventTriggerStateSQL(o.Schema, o.TargetName, false))).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs(o.Schema, o.TargetName).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("N"))
	proofs := 0
	ctx := damengnative.WithFinalizationVerifier(context.Background(), func(context.Context) error { proofs++; return nil })
	failed := false
	out, err := w.executeNativeRepair(ctx, o, o.Units, func(units []repair.Unit) error {
		if units[1].Status == "succeeded" && !failed {
			failed = true
			return errors.New("one lost ledger update")
		}
		return nil
	})
	require.ErrorContains(t, err, "one lost ledger update")
	require.Equal(t, "succeeded", out[0].Status)
	require.Equal(t, "pending", out[1].Status)
	o.Units = out
	encoded, err := json.Marshal(o)
	require.NoError(t, err)
	var reloaded repair.Object
	require.NoError(t, json.Unmarshal(encoded, &reloaded))
	require.NoError(t, mock.ExpectationsWereMet())
	retry, retryMock := mockNativeDM(t)
	retryMock.ExpectExec(`SET SCHEMA`).WillReturnResult(sqlmock.NewResult(0, 0))
	retryMock.ExpectExec(regexp.QuoteMeta(u.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	out, err = retry.executeNativeRepair(ctx, reloaded, reloaded.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", out[0].Status)
	require.Equal(t, "succeeded", out[1].Status)
	require.Equal(t, 2, proofs)
	require.NoError(t, retryMock.ExpectationsWereMet())
}

func TestNativeEventUncertainPolicyCannotOpenCatalogAfterAnotherEventActivation(t *testing.T) {
	w, mock := mockNativeDM(t)
	a, _ := nativeEventFixture()
	au := eventActivationUnit(a)
	b := nativeEventRepairFixture()
	b.Native.Name, b.Native.Status = "DisabledB", "DISABLED"
	b.Name, b.TargetName = b.Native.Name, b.Native.Name
	b.Native.Units[0].SQL = `CREATE TRIGGER "MixedOwner"."DisabledB" AFTER DDL ON SCHEMA DISABLE BEGIN NULL; END;`
	b.Native.Units[1] = eventActivationUnit(*b.Native)
	for i := range b.Units {
		b.Units[i].SQL, b.Units[i].OriginalSQL = b.Native.Units[i].SQL, b.Native.Units[i].SQL
	}
	b.NativeFingerprint = damengnative.Fingerprint(b.Native)
	b.Units[1].Status = "unknown"
	mock.ExpectExec(`SET SCHEMA`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(au.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(damengnative.EventTriggerStateSQL(a.Schema, a.Name, false))).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs(a.Schema, a.Name).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("N"))
	err := w.WithDamengNativeEventFinalization(context.Background(), func(final context.Context) error {
		require.NoError(t, w.SetDamengNativeEventState(final, a, au))
		_, err := w.executeNativeRepair(final, b, b.Units, nil)
		return err
	})
	require.ErrorContains(t, err, "最终事件会话之前完成整组核对")
	require.NoError(t, mock.ExpectationsWereMet()) // Only cleanup queries; no catalog scan/new login.
}
