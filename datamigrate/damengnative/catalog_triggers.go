package damengnative

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type finalizationVerifierKey struct{}

func WithFinalizationVerifier(ctx context.Context, verify func(context.Context) error) context.Context {
	return context.WithValue(ctx, finalizationVerifierKey{}, verify)
}

func VerifyFinalization(ctx context.Context) error {
	verify, ok := ctx.Value(finalizationVerifierKey{}).(func(context.Context) error)
	if !ok || verify == nil {
		return fmt.Errorf("恢复自动执行策略前缺少整任务目录与内容核验")
	}
	return verify(ctx)
}

// DM8's event target syntax is ON [schema.]SCHEMA or ON DATABASE. The
// triggering schema need not be the trigger owner's schema:
// https://eco.dameng.com/document/dm/zh-cn/pm/trigger
// A quoted "SCHEMA"/"DATABASE" is an ordinary table identifier, not a target
// keyword. The body is deliberately never inspected for target-like text.
func nativeTriggerTarget(ddl, owner string) (kind, schema string, err error) {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return "", "", err
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
			if i+3 < len(ts) && ts[i+2].value == "." {
				if (next.kind == 'q' || next.kind == 'i') && ts[i+3].kind == 'i' && strings.EqualFold(ts[i+3].value, "SCHEMA") {
					scope := next.value
					if next.kind == 'i' {
						scope = strings.ToUpper(scope)
					}
					return "schema", scope, nil
				}
				return "table", "", nil
			}
			if next.kind == 'i' && strings.EqualFold(next.value, "DATABASE") {
				return "database", "", nil
			}
			if next.kind == 'i' && strings.EqualFold(next.value, "SCHEMA") {
				return "schema", owner, nil
			}
			if next.kind == 'i' || next.kind == 'q' {
				return "table", "", nil
			}
			return "", "", fmt.Errorf("无法解析触发器 ON 目标")
		}
	}
	return "", "", fmt.Errorf("无法定位触发器 ON 目标")
}

// Event hooks use the dedicated installer and final state phase rather than
// the ordinary DML trigger path.
func nativeTriggerIsEvent(ddl string) bool {
	kind, _, err := nativeTriggerTarget(ddl, "")
	return err != nil || kind == "schema" || kind == "database"
}

func EventTriggerScope(ddl, owner string) (kind, schema string, err error) {
	return nativeTriggerTarget(ddl, owner)
}

// DisabledEventTriggerDDL installs the original hook in the disabled state
// atomically. DM8 accepts DISABLE immediately before DECLARE/BEGIN, after the
// complete event/schedule/WHEN header. A trailing DISABLE after END does not
// have this meaning. Keeping the state in CREATE closes the background TIMER
// and CHECKPOINT window that CREATE followed by SP_ENABLE_EVT_TRIGGER leaves.
// Only that header clause changes; identifiers, conditions, comments, strings
// and the complete procedural body retain their original bytes.
func DisabledEventTriggerDDL(ddl string) (string, error) {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return "", err
	}
	if kind, _, err := nativeTriggerTarget(ddl, ""); err != nil || kind != "schema" && kind != "database" {
		return "", fmt.Errorf("原子禁用安装需要完整的事件触发器定义")
	}
	for i, token := range ts {
		if token.kind != 'i' || !strings.EqualFold(token.value, "DECLARE") && !strings.EqualFold(token.value, "BEGIN") {
			continue
		}
		if i > 0 && ts[i-1].kind == 'i' {
			previous := ts[i-1]
			if strings.EqualFold(previous.value, "DISABLE") {
				return ddl, nil
			}
			if strings.EqualFold(previous.value, "ENABLE") {
				return ddl[:previous.start] + "DISABLE" + ddl[previous.end:], nil
			}
		}
		return ddl[:token.start] + "DISABLE " + ddl[token.start:], nil
	}
	return "", fmt.Errorf("事件触发器缺少完整的 DECLARE/BEGIN 执行单元")
}

// EventTriggerHasTimer detects the event header only; names, comments and
// procedural text containing TIMER do not change an ordinary trigger's kind.
func EventTriggerHasTimer(ddl string) bool {
	return eventTriggerHeaderContains(ddl, "TIMER")
}

func EventTriggerHasCheckpoint(ddl string) bool {
	return eventTriggerHeaderContains(ddl, "CHECKPOINT")
}

func eventTriggerHeaderContains(ddl, event string) bool {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return true
	}
	timing := false
	for _, t := range ts {
		if t.kind != 'i' {
			continue
		}
		if strings.EqualFold(t.value, "BEGIN") || strings.EqualFold(t.value, "DECLARE") || timing && strings.EqualFold(t.value, "ON") {
			break
		}
		if strings.EqualFold(t.value, "BEFORE") || strings.EqualFold(t.value, "AFTER") {
			timing = true
			continue
		}
		if timing && strings.EqualFold(t.value, event) {
			return true
		}
	}
	return false
}

// EventTriggerStateSQL uses the documented per-object routine rather than
// ALTER TRIGGER, which could itself run already restored DDL event hooks.
// It never changes the global event-trigger switch.
func EventTriggerStateSQL(schema, name string, enabled bool) string {
	literal := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	state := "0"
	if enabled {
		state = "1"
	}
	return "CALL SP_ENABLE_EVT_TRIGGER(" + literal(schema) + "," + literal(name) + "," + state + ")"
}

// Target inventory checks every active event hook in the database. A schema
// event targeting another schema is harmless here. Ownership alone does not
// establish its scope; database-wide hooks and hooks targeting our schema must
// block before any DROP.
func (r *catalogReader) checkActiveEventTriggers() error {
	// Read the named status instead of assuming an INFO1 enable-bit position:
	// supported DM8 builds expose Y/N here, while published bit descriptions
	// differ from actual native flags. An invisible/unknown state blocks writes.
	rows, err := r.db.QueryContext(r.ctx, `SELECT s.NAME,o.NAME,o.INFO1,t.STATUS FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID LEFT JOIN ALL_TRIGGERS t ON t.OWNER=s.NAME AND t.TRIGGER_NAME=o.NAME WHERE o.SUBTYPE$='TRIG' AND (o.INFO1 & 2)=0 ORDER BY s.NAME,o.NAME`)
	if err != nil {
		return fmt.Errorf("无法核验目标数据库已启用的事件触发器: %w", err)
	}
	type event struct {
		owner, name string
		flags       int64
		status      sql.NullString
	}
	var events []event
	for rows.Next() {
		var e event
		if err = rows.Scan(&e.owner, &e.name, &e.flags, &e.status); err != nil {
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
		state := strings.ToUpper(strings.TrimSpace(e.status.String))
		enabled := state == "Y" || state == "1" || state == "ENABLE" || state == "ENABLED"
		disabled := state == "N" || state == "0" || state == "DISABLE" || state == "DISABLED"
		if !e.status.Valid || !enabled && !disabled {
			return fmt.Errorf("事件触发器 %s 的启停状态不可见或无法识别，已阻止目标修改", Qualified(e.owner, e.name))
		}
		if e.owner == r.schema {
			if i, ok := r.index["trigger\x00"+e.name]; ok {
				r.snapshot.Objects[i].Status = "DISABLED"
				if enabled {
					r.snapshot.Objects[i].Status = "ENABLED"
				}
			}
		}
		if disabled {
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
		kind, schema, parseErr := nativeTriggerTarget(ddl.String, e.owner)
		if parseErr != nil || kind != "schema" && kind != "database" {
			return fmt.Errorf("无法只读确认其他 Schema 事件触发器 %s 的作用域，已阻止目标修改", Qualified(e.owner, e.name))
		}
		if kind == "database" {
			r.snapshot.Issues = append(r.snapshot.Issues, Issue{Kind: "trigger", Name: Qualified(e.owner, e.name), Severity: "error", Reason: "目标实例存在已启用的数据库级事件触发器；迁移 DDL/新连接可能执行其业务逻辑，请由用户先停用"})
		} else if schema == r.schema {
			r.snapshot.Issues = append(r.snapshot.Issues, Issue{Kind: "trigger", Name: Qualified(e.owner, e.name), Severity: "error", Reason: "其他 Schema 拥有的已启用事件触发器作用于目标 Schema；迁移 DDL 可能执行其业务逻辑，请由用户先停用"})
		}
	}
	return nil
}
func triggerOnDatabase(ddl string) bool {
	kind, _, err := nativeTriggerTarget(ddl, "")
	return err != nil || kind == "database"
}
