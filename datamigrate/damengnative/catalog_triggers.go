package damengnative

import (
	"database/sql"
	"fmt"
	"strings"
)

// DM8 documents CREATE TRIGGER as immediately enabled and only supports
// disabling it through a later ALTER. Event hooks therefore cannot safely be
// installed atomically disabled, unlike ordinary table DML triggers.
func nativeTriggerIsEvent(ddl string) bool {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return true
	}
	timing := false
	for i, t := range ts {
		if t.kind != 'i' {
			continue
		}
		if strings.EqualFold(t.value, "BEGIN") || strings.EqualFold(t.value, "DECLARE") {
			break
		}
		if strings.EqualFold(t.value, "BEFORE") || strings.EqualFold(t.value, "AFTER") || strings.EqualFold(t.value, "INSTEAD") {
			timing = true
			continue
		}
		if timing && strings.EqualFold(t.value, "ON") && i+1 < len(ts) {
			next := ts[i+1]
			return next.kind == 'i' && (strings.EqualFold(next.value, "DATABASE") || strings.EqualFold(next.value, "SCHEMA"))
		}
	}
	return false
}

// Target inventory checks every active event hook in the database. A schema
// event in another schema is harmless here; database-wide hooks can execute on
// our DDL or on the writer's new sessions and must block before any DROP.
func (r *catalogReader) checkActiveEventTriggers() error {
	rows, err := r.db.QueryContext(r.ctx, `SELECT s.NAME,o.NAME,o.INFO1 FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE o.SUBTYPE$='TRIG' AND (o.INFO1 & 2)=0 AND (o.INFO1 & 8192)<>0 ORDER BY s.NAME,o.NAME`)
	if err != nil {
		return fmt.Errorf("无法核验目标数据库已启用的事件触发器: %w", err)
	}
	type event struct {
		owner, name string
		flags       int64
	}
	var events []event
	for rows.Next() {
		var e event
		if err = rows.Scan(&e.owner, &e.name, &e.flags); err != nil {
			rows.Close()
			return err
		}
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range events {
		if e.flags&32 != 0 { // Engine-generated timer hooks belong to native MV
			// parent objects; user-created event hooks do not carry this flag.
			continue
		}
		if e.owner == r.schema {
			r.snapshot.Issues = append(r.snapshot.Issues, Issue{Kind: "trigger", Name: e.name, Severity: "error", Reason: "目标 Schema 存在已启用的事件触发器；迁移 DDL/新连接可能执行其业务逻辑，请由用户先停用"})
			continue
		}
		var ddl sql.NullString
		if err = r.db.QueryRowContext(r.ctx, `SELECT DBMS_METADATA.GET_DDL('TRIGGER',?,?)`, e.name, e.owner).Scan(&ddl); err != nil || !ddl.Valid {
			return fmt.Errorf("无法只读确认其他 Schema 事件触发器 %s 的作用域，已阻止目标修改", Qualified(e.owner, e.name))
		}
		if triggerOnDatabase(ddl.String) {
			r.snapshot.Issues = append(r.snapshot.Issues, Issue{Kind: "trigger", Name: Qualified(e.owner, e.name), Severity: "error", Reason: "目标实例存在已启用的数据库级事件触发器；迁移 DDL/新连接可能执行其业务逻辑，请由用户先停用"})
		}
	}
	return nil
}
func triggerOnDatabase(ddl string) bool {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return true
	}
	timing := false
	for i, t := range ts {
		if t.kind != 'i' {
			continue
		}
		if strings.EqualFold(t.value, "BEGIN") || strings.EqualFold(t.value, "DECLARE") {
			break
		}
		if strings.EqualFold(t.value, "BEFORE") || strings.EqualFold(t.value, "AFTER") {
			timing = true
		}
		if timing && strings.EqualFold(t.value, "ON") && i+1 < len(ts) {
			return ts[i+1].kind == 'i' && strings.EqualFold(ts[i+1].value, "DATABASE")
		}
	}
	return false
}
