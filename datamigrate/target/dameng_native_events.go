package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
)

type nativeEventSessionKey struct{}

func nativeEventIdentity(schema string, object damengnative.Object, unit damengnative.Unit) error {
	if object.Kind != "trigger" || !object.EventTrigger || object.Schema != schema || object.Name == "" || strings.ContainsRune(object.Name, 0) {
		return fmt.Errorf("事件触发器不在冻结的目标 Schema 中")
	}
	if err := validateDamengNativeSQL(schema, unit.SQL); err != nil {
		return err
	}
	tokens, err := nativeSQLTokens(unit.SQL)
	if err != nil {
		return err
	}
	i := 1
	if len(tokens) > 3 && strings.EqualFold(tokens[1], "OR") && strings.EqualFold(tokens[2], "REPLACE") {
		i = 3
	}
	if len(tokens) <= i+1 || !strings.EqualFold(tokens[0], "CREATE") || !strings.EqualFold(tokens[i], "TRIGGER") {
		return fmt.Errorf("事件触发器安装必须使用原始 CREATE TRIGGER")
	}
	end, err := nativeHeadIdentity(tokens, i+1, schema)
	if err != nil || end == 0 || nativeTokenName(tokens[end-1]) != object.Name {
		return fmt.Errorf("事件触发器名称与冻结清单不一致")
	}
	_, err = damengnative.DisabledEventTriggerDDL(unit.SQL)
	return err
}

// InstallDamengNativeEvent creates every event hook, including TIMER and
// CHECKPOINT, with its native DISABLE header clause. It never publishes an
// enabled hook during import. The original final state is a separate frozen
// event_activate unit. Legacy frozen units without DISABLE are normalized here
// too, without changing their body or weakening the generic SQL executor.
func (w *DaMengNativeWriter) InstallDamengNativeEvent(ctx context.Context, object damengnative.Object, unit damengnative.Unit) error {
	if err := nativeEventIdentity(w.schema, object, unit); err != nil {
		return err
	}
	ddl, err := damengnative.DisabledEventTriggerDDL(unit.SQL)
	if err != nil {
		return err
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer discardNativeEventConnection(conn)
	if _, err = conn.ExecContext(ctx, "SET SCHEMA "+damengnative.Quote(w.schema)); err != nil {
		return err
	}
	if err = repair.Observe(ctx, unit.SQL, "running", nil); err != nil {
		return fmt.Errorf("保存事件触发器安装前状态: %w", err)
	}
	_, createErr := conn.ExecContext(ctx, ddl)
	if createErr != nil {
		return errors.Join(createErr, repair.Observe(ctx, unit.SQL, repairErrorStatus(createErr), createErr))
	}
	// Confirm the native state before reporting success. Cancellation or a
	// failed ledger callback cannot turn this CREATE into an enabled object.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	var status string
	err = conn.QueryRowContext(cleanup, `SELECT STATUS FROM ALL_TRIGGERS WHERE OWNER=? AND TRIGGER_NAME=?`, w.schema, object.Name).Scan(&status)
	if err != nil {
		err = fmt.Errorf("读取新事件触发器禁用状态失败，目标状态待核验: %w", err)
		return errors.Join(err, repair.Observe(ctx, unit.SQL, "unknown", err))
	}
	if state := strings.ToUpper(strings.TrimSpace(status)); state != "N" && state != "0" && state != "DISABLED" && state != "DISABLE" {
		// Defensive cleanup for an incompatible server or unexpected catalog
		// state; this is never the mechanism used to install disabled hooks.
		_, disableErr := conn.ExecContext(cleanup, damengnative.EventTriggerStateSQL(w.schema, object.Name, false))
		err = errors.Join(fmt.Errorf("新事件触发器未按冻结计划原子禁用，目标状态待核验: %s", status), disableErr)
		return errors.Join(err, repair.Observe(ctx, unit.SQL, "unknown", err))
	}
	return repair.Observe(ctx, unit.SQL, "succeeded", nil)
}

func discardNativeEventConnection(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// WithDamengNativeEventFinalization pins the final migration session and
// discards idle sessions while all imported event hooks are still disabled.
// The callback must execute only final state units: after it enables the first
// hook, it must not issue validation queries or other migration DDL. Restoring
// event states ends the stopped-write import window; closing this last session
// can run a restored LOGOUT hook as normal database behavior.
func (w *DaMengNativeWriter) WithDamengNativeEventFinalization(ctx context.Context, activate func(context.Context) error) error {
	if activate == nil {
		return fmt.Errorf("缺少事件触发器最终状态回调")
	}
	if conn, ok := ctx.Value(nativeEventSessionKey{}).(*sql.Conn); ok && conn != nil {
		// A recovery group owns one final session for all event objects.
		return activate(ctx)
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer discardNativeEventConnection(conn)
	// The native writer is job-local. All import workers have joined before
	// this method is called, so no additional pool connections may be opened.
	w.db.SetMaxIdleConns(0)
	if _, err = conn.ExecContext(ctx, "SET SCHEMA "+damengnative.Quote(w.schema)); err != nil {
		return err
	}
	return activate(context.WithValue(ctx, nativeEventSessionKey{}, conn))
}

// SetDamengNativeEventState executes only a frozen per-object state command on
// the final pinned session. Refusing an implicit new connection prevents LOGIN
// events between restoring two hooks.
func (w *DaMengNativeWriter) SetDamengNativeEventState(ctx context.Context, object damengnative.Object, unit damengnative.Unit) error {
	if err := validateNativeEventState(w.schema, object, unit); err != nil {
		return err
	}
	conn, ok := ctx.Value(nativeEventSessionKey{}).(*sql.Conn)
	if !ok || conn == nil {
		return fmt.Errorf("事件触发器状态必须在最终固定会话中恢复")
	}
	_, err := execObserved(ctx, conn, unit.SQL)
	return err
}

func validateNativeEventState(schema string, object damengnative.Object, unit damengnative.Unit) error {
	if object.Kind != "trigger" || !object.EventTrigger || object.Schema != schema || object.Name == "" || strings.ContainsRune(object.Name, 0) {
		return fmt.Errorf("事件触发器不在冻结的目标 Schema 中")
	}
	enabled := strings.EqualFold(object.Status, "ENABLED") || object.Status == "Y" || object.Status == "1"
	disabled := strings.EqualFold(object.Status, "DISABLED") || object.Status == "N" || object.Status == "0"
	if !enabled && !disabled || unit.SQL != damengnative.EventTriggerStateSQL(schema, object.Name, enabled) {
		return fmt.Errorf("事件触发器状态语句与冻结清单不一致")
	}
	return nil
}

func validateNativeObjectUnit(schema string, object damengnative.Object, unit damengnative.Unit) error {
	if unit.Phase == "event_activate" {
		return validateNativeEventState(schema, object, unit)
	}
	if unit.Phase == "event_create" {
		return nativeEventIdentity(schema, object, unit)
	}
	return validateDamengNativeSQL(schema, unit.SQL)
}
