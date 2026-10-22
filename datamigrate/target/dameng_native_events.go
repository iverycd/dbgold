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
	if damengnative.EventTriggerHasTimer(unit.SQL) {
		return fmt.Errorf("TIMER 触发器的 CREATE 与禁用之间存在后台调度窗口，不能使用普通事件安装器")
	}
	return nil
}

// InstallDamengNativeEvent installs one non-TIMER hook and disables it before
// releasing the physical session. DM8 does not fire an event trigger when the
// trigger itself is the event target; SP_ENABLE_EVT_TRIGGER is a routine, not
// an ALTER event. LOGIN has already occurred and LOGOUT is still disabled when
// this session is discarded. The observer sees one logical install operation,
// succeeded only after both CREATE and the required disable have succeeded.
// https://eco.dameng.com/document/dm/zh-cn/pm/trigger
// https://eco.dameng.com/document/dm/zh-cn/pm/sql-appendix
func (w *DaMengNativeWriter) InstallDamengNativeEvent(ctx context.Context, object damengnative.Object, unit damengnative.Unit) error {
	if err := nativeEventIdentity(w.schema, object, unit); err != nil {
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
	if damengnative.EventTriggerHasCheckpoint(unit.SQL) {
		if err = nativeCheckpointEventsDisabled(ctx, conn); err != nil {
			return err
		}
	}
	if err = repair.Observe(ctx, unit.SQL, "running", nil); err != nil {
		return fmt.Errorf("保存事件触发器安装前状态: %w", err)
	}
	_, createErr := conn.ExecContext(ctx, unit.SQL)
	// A cancelled request or failed persistence callback must never skip the
	// immediate disable of a hook that CREATE may already have committed.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_, disableErr := conn.ExecContext(cleanup, damengnative.EventTriggerStateSQL(w.schema, object.Name, false))
	if disableErr != nil {
		err = errors.Join(createErr, fmt.Errorf("禁用新事件触发器失败，目标启用状态待核验: %w", disableErr))
		return errors.Join(err, repair.Observe(ctx, unit.SQL, "unknown", err))
	}
	status := "succeeded"
	if createErr != nil {
		status = repairErrorStatus(createErr)
	}
	return errors.Join(createErr, repair.Observe(ctx, unit.SQL, status, createErr))
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

func nativeCheckpointEventsDisabled(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) error {
	var value string
	if err := query.QueryRowContext(ctx, `SELECT PARA_VALUE FROM V$DM_INI WHERE PARA_NAME='ENABLE_CKPT_EVENT_TRIG'`).Scan(&value); err != nil {
		return fmt.Errorf("无法只读确认 CHECKPOINT 事件调度已关闭: %w", err)
	}
	if strings.TrimSpace(value) != "0" {
		return fmt.Errorf("CHECKPOINT 事件调度当前启用；CREATE 与禁用之间存在后台触发窗口，已阻止安装")
	}
	return nil
}
