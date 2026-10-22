package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
)

// RebaseDamengNativeMaterialized establishes the target engine's own refresh
// watermark after copying a proven-current source snapshot. Ordinary values
// and duplicate counts must match before the non-rollbackable REFRESH. Direct
// ROWID outputs alone may acquire the new base tables' physical ROWIDs.
func (w *DaMengNativeWriter) RebaseDamengNativeMaterialized(ctx context.Context, object damengnative.Object) error {
	statement, err := w.materializedBaselineStatement(object)
	if err != nil {
		return err
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }); _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "SET SCHEMA "+damengnative.Quote(w.schema)); err != nil {
		return err
	}
	if err = repair.Observe(ctx, statement, "running", nil); err != nil {
		return fmt.Errorf("保存物化视图基线执行前状态: %w", err)
	}
	state := object.Materialized
	if err = checkMaterializedReadOnly(ctx, conn, w.schema, object.Name, state, state.RowIDColumns); err != nil {
		err = fmt.Errorf("物化视图 %s 初始化前快照核验失败，未执行 REFRESH: %w", object.Name, err)
		return errors.Join(err, repair.Observe(ctx, statement, "failed", err))
	}
	if _, err = conn.ExecContext(ctx, statement); err != nil {
		return errors.Join(err, repair.Observe(ctx, statement, repairErrorStatus(err), err))
	}
	// A successful Exec does not complete the ledger step until the postcondition
	// proves that only permitted physical ROWID values could have changed.
	if err = checkMaterializedReadOnly(ctx, conn, w.schema, object.Name, state, nil); err != nil {
		err = fmt.Errorf("物化视图 %s 已执行 COMPLETE，但基线后核验失败: %w", object.Name, err)
		return errors.Join(err, repair.Observe(ctx, statement, "unknown", err))
	}
	if err = repair.Observe(ctx, statement, "succeeded", nil); err != nil {
		return fmt.Errorf("物化视图基线已初始化，保存核验结果失败: %w", err)
	}
	return nil
}

// The final policy is verified separately from data: an active timer may refresh
// the view once migration releases it. No later full-catalog read is needed.
func (w *DaMengNativeWriter) VerifyDamengNativeMaterializedPolicy(ctx context.Context, object damengnative.Object) error {
	if object.Schema != w.schema || damengnative.NormalizeKind(object.Kind) != "materialized_view" || object.Materialized == nil {
		return fmt.Errorf("物化视图刷新策略不属于冻结目标对象")
	}
	prefix := "ALTER MATERIALIZED VIEW " + damengnative.Qualified(w.schema, object.Name) + " "
	expected := ""
	for _, unit := range object.Units {
		if unit.Phase == "mv_restore" && strings.HasPrefix(unit.SQL, prefix) {
			expected = strings.TrimSpace(strings.TrimPrefix(unit.SQL, prefix))
		}
	}
	if expected == "" {
		return fmt.Errorf("物化视图 %s 没有冻结的原始刷新策略", object.Name)
	}
	var ddl string
	if err := w.db.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL(?,?,?)`, "MATERIALIZED_VIEW", object.Name, w.schema).Scan(&ddl); err != nil {
		return fmt.Errorf("物化视图 %s 刷新策略不可核验: %w", object.Name, err)
	}
	actual, err := damengnative.MaterializedPolicy(ddl)
	if err != nil {
		return err
	}
	if !nativeDefinitionsEqual(expected, actual) {
		return fmt.Errorf("物化视图 %s 的原始刷新策略未完整恢复", object.Name)
	}
	return nil
}

func (w *DaMengNativeWriter) materializedBaselineStatement(object damengnative.Object) (string, error) {
	state := object.Materialized
	if object.Schema != w.schema || damengnative.NormalizeKind(object.Kind) != "materialized_view" || state == nil || !state.Baseline || state.Staleness != "PROVEN_CURRENT" || (state.RefreshMethod != "FAST" && state.RefreshMethod != "FORCE") {
		return "", fmt.Errorf("缺少可信、属于目标模式且已证明为当前快照的物化视图基线计划")
	}
	statement := damengnative.MaterializedBaselineSQL(w.schema, object.Name)
	found := false
	for _, unit := range object.Units {
		if unit.Phase == "mv_rebase" && unit.SQL == statement {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("物化视图基线 SQL 不在冻结的执行清单中")
	}
	if _, err := damengnative.MaterializedBagSQL(w.schema, object.Name, state.Query, state.SnapshotColumns, state.RowIDColumns); err != nil {
		return "", err
	}
	return statement, nil
}

func checkMaterializedReadOnly(ctx context.Context, conn *sql.Conn, schema, name string, state *damengnative.MaterializedState, ignore []int) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		return err
	}
	if err = damengnative.CheckMaterializedBag(ctx, tx, schema, name, state.Query, state.SnapshotColumns, ignore); err != nil {
		return err
	}
	return tx.Rollback()
}

// An unknown baseline can be retried only after re-proving ordinary values.
// Repeating COMPLETE then produces the same logical bag and a valid new native
// watermark; catalog presence alone never marks this operation as successful.
func (w *DaMengNativeWriter) ReconcileDamengNativeMaterialized(ctx context.Context, object damengnative.Object) (done, retry bool, err error) {
	if _, err = w.materializedBaselineStatement(object); err != nil {
		return false, false, err
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }); _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "SET SCHEMA "+damengnative.Quote(w.schema)); err != nil {
		return false, false, err
	}
	err = checkMaterializedReadOnly(ctx, conn, w.schema, object.Name, object.Materialized, object.Materialized.RowIDColumns)
	return false, err == nil, err
}
