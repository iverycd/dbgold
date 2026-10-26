package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnative"
)

// PauseDamengNativeEventRecovery establishes a stopped-event proof boundary
// after a process crash or legacy partial finalization. Every existing hook is
// authenticated against its immutable CREATE before the first policy write.
// This optional native capability never accepts arbitrary repair SQL.
func (w *DaMengNativeWriter) PauseDamengNativeEventRecovery(ctx context.Context, objects []damengnative.Object, observe func(damengnative.EventStateRecovery) error) error {
	if observe == nil {
		return fmt.Errorf("事件恢复缺少停用状态持久化回调")
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer discardNativeEventConnection(conn)
	type pause struct {
		object damengnative.Object
		unit   damengnative.Unit
		active bool
		absent bool
	}
	var plan []pause
	for _, object := range objects {
		if object.Schema != w.schema || object.Managed || !object.EventTrigger || object.Kind != "trigger" {
			return fmt.Errorf("事件恢复对象不属于冻结的目标Schema")
		}
		var create, activation *damengnative.Unit
		for i := range object.Units {
			switch object.Units[i].Phase {
			case "event_create":
				if create != nil {
					return fmt.Errorf("事件恢复包含重复CREATE")
				}
				create = &object.Units[i]
			case "event_activate":
				if activation != nil {
					return fmt.Errorf("事件恢复包含重复最终状态")
				}
				activation = &object.Units[i]
			}
		}
		if create == nil || activation == nil {
			return fmt.Errorf("事件恢复缺少完整的冻结生命周期")
		}
		if err = nativeEventIdentity(w.schema, object, *create); err != nil {
			return err
		}
		if err = validateNativeEventState(w.schema, object, *activation); err != nil {
			return err
		}
		var subtype string
		var status sql.NullString
		err = conn.QueryRowContext(ctx, `SELECT o.SUBTYPE$,t.STATUS FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID LEFT JOIN ALL_TRIGGERS t ON t.OWNER=s.NAME AND t.TRIGGER_NAME=o.NAME WHERE s.TYPE$='SCH' AND s.NAME=? AND o.NAME=?`, w.schema, object.Name).Scan(&subtype, &status)
		if errors.Is(err, sql.ErrNoRows) {
			// The selected failed CREATE can still be absent. Ordinary recovery
			// creates it disabled before the subsequent whole-job proof. Its
			// policy cannot remain "succeeded" from a stale ledger in that case.
			plan = append(plan, pause{object: object, unit: *activation, absent: true})
			continue
		}
		if err != nil {
			return err
		}
		if subtype != "TRIG" || !status.Valid {
			return fmt.Errorf("事件 %s 的实际类型或启停状态不可见", object.Name)
		}
		active := !nativeEventDisabled(status.String)
		if active && !nativeEventEnabled(status.String) {
			return fmt.Errorf("事件 %s 的启停状态无法识别", object.Name)
		}
		var actual string
		if err = conn.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TRIGGER',?,?)`, object.Name, w.schema).Scan(&actual); err != nil {
			return err
		}
		actual, err = damengnative.DisabledEventTriggerDDL(actual)
		if err != nil {
			return err
		}
		expected, err := damengnative.DisabledEventTriggerDDL(create.SQL)
		if err != nil || !nativeDefinitionsEqual(expected, actual) {
			return fmt.Errorf("事件 %s 的定义已偏离冻结计划，未停用任何事件", object.Name)
		}
		plan = append(plan, pause{object: object, unit: *activation, active: active})
	}
	for _, item := range plan {
		state := damengnative.EventStateRecovery{Schema: item.object.Schema, Name: item.object.Name, SQL: item.unit.SQL, Status: "pending"}
		if item.absent {
			if err = observe(state); err != nil {
				return err
			}
			continue
		}
		if item.active {
			state.Status, state.Error = "unknown", "已保存事件停用意图，等待目录确认"
			if err = observe(state); err != nil {
				return err
			}
			_, disableErr := conn.ExecContext(ctx, damengnative.EventTriggerStateSQL(w.schema, item.object.Name, false))
			var status string
			proofErr := conn.QueryRowContext(ctx, `SELECT STATUS FROM ALL_TRIGGERS WHERE OWNER=? AND TRIGGER_NAME=?`, w.schema, item.object.Name).Scan(&status)
			if proofErr != nil || !nativeEventDisabled(status) {
				err = errors.Join(disableErr, proofErr)
				if err == nil {
					err = fmt.Errorf("事件 %s 未确认停用", item.object.Name)
				}
				state.Error = err.Error()
				return errors.Join(err, observe(state))
			}
		}
		// Complete a measured original-DISABLED policy now, including unknown
		// or running ledger states. Otherwise its later reconciliation would
		// open a connection/read catalog after another event was enabled.
		state.Status, state.Error = "pending", ""
		if nativeEventDisabled(item.object.Status) {
			state.Status = "succeeded"
		}
		if err = observe(state); err != nil {
			return err
		}
	}
	return nil
}

func nativeEventEnabled(status string) bool {
	status = strings.ToUpper(strings.TrimSpace(status))
	return !nativeEventDisabled(status) && (status == "Y" || status == "1" || status == "ENABLED" || status == "ENABLE")
}
