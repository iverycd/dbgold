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

func nativeMVBaselineObject() damengnative.Object {
	return damengnative.Object{ID: "mv", Kind: "materialized_view", Name: "Mv", Schema: "MixedOwner", Materialized: &damengnative.MaterializedState{Query: `SELECT "Id","Value" FROM "MixedOwner"."T"`, RefreshMethod: "FAST", RefreshRule: "PRIMARY KEY", Staleness: "PROVEN_CURRENT", Baseline: true, SnapshotColumns: []string{"Id", "Value"}}, Units: []damengnative.Unit{{SQL: damengnative.MaterializedBaselineSQL("MixedOwner", "Mv"), Phase: "mv_rebase"}}}
}

func expectTargetMVBag(t *testing.T, m sqlmock.Sqlmock, o damengnative.Object, ignore []int, differences int64) {
	m.ExpectBegin()
	m.ExpectExec("SET TRANSACTION READ ONLY").WillReturnResult(sqlmock.NewResult(0, 0))
	query, err := damengnative.MaterializedBagSQL(o.Schema, o.Name, o.Materialized.Query, o.Materialized.SnapshotColumns, ignore)
	require.NoError(t, err)
	m.ExpectQuery(regexp.QuoteMeta(query)).WillReturnRows(sqlmock.NewRows([]string{"differences"}).AddRow(differences))
	m.ExpectRollback()
}

func TestNativeMVBaselineRequiresBagProofBeforeAndAfterRefresh(t *testing.T) {
	w, m := mockNativeDM(t)
	o := nativeMVBaselineObject()
	m.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "MixedOwner"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectTargetMVBag(t, m, o, nil, 0)
	m.ExpectExec(regexp.QuoteMeta(o.Units[0].SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectTargetMVBag(t, m, o, nil, 0)
	var events []repair.Event
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { events = append(events, e); return nil })
	require.NoError(t, w.RebaseDamengNativeMaterialized(ctx, o))
	require.Equal(t, []repair.Event{{SQL: o.Units[0].SQL, Status: "running"}, {SQL: o.Units[0].SQL, Status: "succeeded"}}, events)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeMVBaselineBeforeProofFailureDoesNotRefresh(t *testing.T) {
	w, m := mockNativeDM(t)
	o := nativeMVBaselineObject()
	m.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "MixedOwner"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectTargetMVBag(t, m, o, nil, 1)
	var statuses []string
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { statuses = append(statuses, e.Status); return nil })
	require.ErrorContains(t, w.RebaseDamengNativeMaterialized(ctx, o), "未执行 REFRESH")
	require.Equal(t, []string{"running", "failed"}, statuses)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeMVBaselinePostProofFailureRemainsUnknown(t *testing.T) {
	w, m := mockNativeDM(t)
	o := nativeMVBaselineObject()
	o.Materialized.RowIDColumns = []int{1}
	m.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "MixedOwner"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectTargetMVBag(t, m, o, []int{1}, 0)
	m.ExpectExec(regexp.QuoteMeta(o.Units[0].SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectTargetMVBag(t, m, o, nil, 1)
	var statuses []string
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { statuses = append(statuses, e.Status); return nil })
	require.ErrorContains(t, w.RebaseDamengNativeMaterialized(ctx, o), "基线后核验失败")
	require.Equal(t, []string{"running", "unknown"}, statuses)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeMVBaselineCannotMutateWithoutDurableRunningState(t *testing.T) {
	w, m := mockNativeDM(t)
	o := nativeMVBaselineObject()
	m.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "MixedOwner"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	ctx := repair.WithObserver(context.Background(), func(repair.Event) error { return errors.New("ledger unavailable") })
	require.ErrorContains(t, w.RebaseDamengNativeMaterialized(ctx, o), "ledger unavailable")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeMVBaselineRequiresFrozenTargetPlan(t *testing.T) {
	w, m := mockNativeDM(t)
	for _, change := range []func(*damengnative.Object){func(o *damengnative.Object) { o.Schema = "Other" }, func(o *damengnative.Object) { o.Materialized.Staleness = "STALE" }, func(o *damengnative.Object) { o.Units = nil }, func(o *damengnative.Object) { o.Materialized.Query = `SELECT 1;DELETE FROM T` }} {
		o := nativeMVBaselineObject()
		change(&o)
		require.Error(t, w.RebaseDamengNativeMaterialized(context.Background(), o))
	}
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeMVUnknownBaselineRequiresProofBeforeSafeRetry(t *testing.T) {
	w, m := mockNativeDM(t)
	o := nativeMVBaselineObject()
	m.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "MixedOwner"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectTargetMVBag(t, m, o, nil, 0)
	done, retry, err := w.ReconcileDamengNativeMaterialized(context.Background(), o)
	require.NoError(t, err)
	require.False(t, done)
	require.True(t, retry)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeMVOriginalRefreshPolicyMustBeReadBack(t *testing.T) {
	for _, method := range []string{"FAST", "COMPLETE"} {
		t.Run(method, func(t *testing.T) {
			w, m := mockNativeDM(t)
			o := nativeMVBaselineObject()
			o.Units = append(o.Units, damengnative.Unit{SQL: `ALTER MATERIALIZED VIEW "MixedOwner"."Mv" REFRESH FAST ON COMMIT WITH PRIMARY KEY`, Phase: "mv_restore"})
			m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("MATERIALIZED_VIEW", "Mv", "MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE MATERIALIZED VIEW "MixedOwner"."Mv" FOR "MixedOwner"."Storage" ON PREBUILT TABLE REFRESH ` + method + ` ON COMMIT WITH PRIMARY KEY AS SELECT "Id","Value" FROM "MixedOwner"."T"`))
			err := w.VerifyDamengNativeMaterializedPolicy(context.Background(), o)
			if method == "FAST" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "刷新策略未完整恢复")
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
