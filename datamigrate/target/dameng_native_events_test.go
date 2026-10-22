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

func nativeEventFixture() (damengnative.Object, damengnative.Unit) {
	o := damengnative.Object{Kind: "trigger", Schema: "MixedOwner", Name: "Event", EventTrigger: true, Status: "ENABLED"}
	u := damengnative.Unit{SQL: `CREATE TRIGGER "MixedOwner"."Event" AFTER DDL ON SCHEMA DISABLE BEGIN NULL; END;`, Phase: "event_create"}
	return o, u
}

func TestNativeEventInstallDisablesBeforeSuccessOrSessionRelease(t *testing.T) {
	w, mock := mockNativeDM(t)
	o, u := nativeEventFixture()
	mock.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "MixedOwner"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(u.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs(o.Schema, o.Name).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("N"))
	var states []string
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error {
		require.Equal(t, u.SQL, e.SQL)
		states = append(states, e.Status)
		return nil
	})
	require.NoError(t, w.InstallDamengNativeEvent(ctx, o, u))
	require.Equal(t, []string{"running", "succeeded"}, states)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventInstallStillDisablesWhenResultPersistenceFails(t *testing.T) {
	w, mock := mockNativeDM(t)
	o, u := nativeEventFixture()
	mock.ExpectExec(`SET SCHEMA`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(u.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs(o.Schema, o.Name).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("N"))
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error {
		if e.Status == "succeeded" {
			return errors.New("ledger unavailable")
		}
		return nil
	})
	require.ErrorContains(t, w.InstallDamengNativeEvent(ctx, o, u), "ledger unavailable")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventInstallCancelledCreateCannotBeSuccessful(t *testing.T) {
	w, mock := mockNativeDM(t)
	o, u := nativeEventFixture()
	mock.ExpectExec(`SET SCHEMA`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(u.SQL)).WillReturnError(context.Canceled)
	var states []string
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { states = append(states, e.Status); return nil })
	require.ErrorIs(t, w.InstallDamengNativeEvent(ctx, o, u), context.Canceled)
	require.Equal(t, []string{"running", "unknown"}, states)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventInstallStateProofFailureCannotBeSuccessful(t *testing.T) {
	w, mock := mockNativeDM(t)
	o, u := nativeEventFixture()
	mock.ExpectExec(`SET SCHEMA`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(u.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs(o.Schema, o.Name).WillReturnError(errors.New("connection lost"))
	var states []string
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { states = append(states, e.Status); return nil })
	require.ErrorContains(t, w.InstallDamengNativeEvent(ctx, o, u), "目标状态待核验")
	require.Equal(t, []string{"running", "unknown"}, states)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventInstallUnexpectedEnabledStateIsDisabledAndUnknown(t *testing.T) {
	w, mock := mockNativeDM(t)
	o, u := nativeEventFixture()
	mock.ExpectExec(`SET SCHEMA`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(u.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs(o.Schema, o.Name).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("Y"))
	mock.ExpectExec(regexp.QuoteMeta(damengnative.EventTriggerStateSQL(o.Schema, o.Name, false))).WillReturnResult(sqlmock.NewResult(0, 0))
	var states []string
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { states = append(states, e.Status); return nil })
	require.ErrorContains(t, w.InstallDamengNativeEvent(ctx, o, u), "未按冻结计划原子禁用")
	require.Equal(t, []string{"running", "unknown"}, states)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventInstallRequiresFrozenIdentity(t *testing.T) {
	w, mock := mockNativeDM(t)
	o, u := nativeEventFixture()
	u.SQL = `CREATE TRIGGER "MixedOwner"."Different" AFTER DDL ON SCHEMA BEGIN NULL; END;`
	require.ErrorContains(t, w.InstallDamengNativeEvent(context.Background(), o, u), "名称与冻结清单不一致")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventFinalizationUsesOnePinnedSession(t *testing.T) {
	w, mock := mockNativeDM(t)
	o, _ := nativeEventFixture()
	u := damengnative.Unit{SQL: damengnative.EventTriggerStateSQL(o.Schema, o.Name, true), Phase: "event_activate"}
	require.ErrorContains(t, w.SetDamengNativeEventState(context.Background(), o, u), "最终固定会话")
	mock.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "MixedOwner"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(u.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	disabled := o
	disabled.Name = "DisabledEvent"
	disabled.Status = "DISABLED"
	du := damengnative.Unit{SQL: damengnative.EventTriggerStateSQL(disabled.Schema, disabled.Name, false), Phase: "event_activate"}
	mock.ExpectExec(regexp.QuoteMeta(du.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, w.WithDamengNativeEventFinalization(context.Background(), func(ctx context.Context) error {
		if err := w.SetDamengNativeEventState(ctx, o, u); err != nil {
			return err
		}
		return w.SetDamengNativeEventState(ctx, disabled, du)
	}))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventStateCannotChangeOtherObjectOrDesiredState(t *testing.T) {
	w, mock := mockNativeDM(t)
	o, _ := nativeEventFixture()
	for _, query := range []string{
		damengnative.EventTriggerStateSQL(o.Schema, o.Name, false),
		damengnative.EventTriggerStateSQL("Other", o.Name, true),
		damengnative.EventTriggerStateSQL(o.Schema, "Other", true),
		`CALL SP_ENABLE_ALL_EVT_TRIGGER(1)`,
	} {
		require.ErrorContains(t, w.SetDamengNativeEventState(context.Background(), o, damengnative.Unit{SQL: query}), "与冻结清单不一致")
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeBackgroundEventsInstallDisabledWithoutChangingInstance(t *testing.T) {
	for _, header := range []string{"CHECKPOINT ON DATABASE", "TIMER ON DATABASE FOR EACH 1 DAY FOR EACH 1 MINUTE"} {
		t.Run(header, func(t *testing.T) {
			w, mock := mockNativeDM(t)
			o, u := nativeEventFixture()
			u.SQL = `CREATE TRIGGER "MixedOwner"."Event" AFTER ` + header + ` BEGIN NULL; END;`
			disabled, err := damengnative.DisabledEventTriggerDDL(u.SQL)
			require.NoError(t, err)
			mock.ExpectExec(`SET SCHEMA`).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(regexp.QuoteMeta(disabled)).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs(o.Schema, o.Name).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("N"))
			require.NoError(t, w.InstallDamengNativeEvent(context.Background(), o, u))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func nativeEventRepairFixture() repair.Object {
	native, create := nativeEventFixture()
	native.Units = []damengnative.Unit{create, {SQL: damengnative.EventTriggerStateSQL(native.Schema, native.Name, true), Phase: "event_activate"}}
	return repair.Object{Kind: "trigger", Name: native.Name, Schema: native.Schema, TargetName: native.Name, Native: &native, NativeFingerprint: damengnative.Fingerprint(native), DataStatus: "not_required", Units: []repair.Unit{
		{SQL: create.SQL, OriginalSQL: create.SQL, Status: "succeeded"},
		{SQL: native.Units[1].SQL, OriginalSQL: native.Units[1].SQL, Status: "pending", Selected: true},
	}}
}

func TestNativeEventRepairCannotActivateWithoutWholeJobProof(t *testing.T) {
	w, mock := mockNativeDM(t)
	object := nativeEventRepairFixture()
	_, err := w.executeNativeRepair(context.Background(), object, object.Units, nil)
	require.ErrorContains(t, err, "整任务目录与内容核验")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeEventRepairProofFailureHasNoTargetSQL(t *testing.T) {
	w, mock := mockNativeDM(t)
	object := nativeEventRepairFixture()
	ctx := damengnative.WithFinalizationVerifier(context.Background(), func(context.Context) error { return errors.New("another table incomplete") })
	_, err := w.executeNativeRepair(ctx, object, object.Units, nil)
	require.ErrorContains(t, err, "another table incomplete")
	require.NoError(t, mock.ExpectationsWereMet())
}
