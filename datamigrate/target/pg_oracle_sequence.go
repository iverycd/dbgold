package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/pgoracle"
)

func (w *OracleWriter) InspectPGOracleSequence(ctx context.Context, p pgoracle.SequencePlan) error {
	if err := w.validatePGOracleSequenceNames(p); err != nil {
		return err
	}
	if p.Schema != w.schema {
		return fmt.Errorf("目标序列 schema 与冻结计划不一致")
	}
	var kind string
	err := w.db.QueryRowContext(ctx, `SELECT OBJECT_TYPE FROM ALL_OBJECTS WHERE OWNER=:1 AND OBJECT_NAME=:2 AND OBJECT_TYPE IN ('TABLE','VIEW','SEQUENCE','SYNONYM','MATERIALIZED VIEW','PACKAGE','PROCEDURE','FUNCTION','TYPE') AND ROWNUM=1`, w.schema, p.Name).Scan(&kind)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if kind != "SEQUENCE" || p.Identity == nil {
			return fmt.Errorf("同名对象 %s 已存在，无法证明本工具序列归属，拒绝覆盖", p.Name)
		}
		if _, err = w.PGOracleSequenceIdentity(ctx, p); err != nil {
			return err
		}
		if err = w.validatePGOracleSequenceConsumers(ctx, p); err != nil {
			return err
		}
	} else if p.Identity != nil {
		return fmt.Errorf("原工具序列 %s 已不存在，禁止重新创建或回退状态", p.Name)
	}
	for _, b := range p.Bindings {
		exists, e := w.inspectPGOracleSequenceTrigger(ctx, p, b)
		if e != nil {
			return e
		}
		if exists && p.Identity == nil {
			return fmt.Errorf("同名触发器 %s 已存在，拒绝覆盖", b.Trigger)
		}
	}
	return nil
}

func (w *OracleWriter) PGOracleSequenceIdentity(ctx context.Context, p pgoracle.SequencePlan) (*pgoracle.SequenceIdentity, error) {
	if err := w.validatePGOracleSequenceNames(p); err != nil {
		return nil, err
	}
	if p.Schema != w.schema {
		return nil, fmt.Errorf("目标序列 schema 与冻结计划不一致")
	}
	var id int64
	if err := w.db.QueryRowContext(ctx, `SELECT OBJECT_ID FROM ALL_OBJECTS WHERE OWNER=:1 AND OBJECT_NAME=:2 AND OBJECT_TYPE='SEQUENCE'`, w.schema, p.Name).Scan(&id); err != nil {
		return nil, err
	}
	var increment, minimum, maximum, cache, cycle, order string
	err := w.db.QueryRowContext(ctx, `SELECT TO_CHAR(INCREMENT_BY,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),TO_CHAR(MIN_VALUE,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),TO_CHAR(MAX_VALUE,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),TO_CHAR(CACHE_SIZE,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),CYCLE_FLAG,ORDER_FLAG FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=:1 AND SEQUENCE_NAME=:2`, w.schema, p.Name).Scan(&increment, &minimum, &maximum, &cache, &cycle, &order)
	if err != nil {
		return nil, err
	}
	if id <= 0 || (cycle != "Y" && cycle != "N") || (order != "Y" && order != "N") {
		return nil, fmt.Errorf("序列 %s 身份或属性不可确认", p.Name)
	}
	identity := &pgoracle.SequenceIdentity{ObjectID: id, Fingerprint: pgoracle.SequenceFingerprint(increment, minimum, maximum, cache, cycle == "Y", order == "Y")}
	if identity.Fingerprint != p.Fingerprint() {
		return nil, fmt.Errorf("序列 %s 定义与冻结计划不一致", p.Name)
	}
	if p.Identity != nil && (p.Identity.ObjectID != id || p.Identity.Fingerprint != identity.Fingerprint) {
		return nil, fmt.Errorf("序列 %s 对象身份或定义已经改变", p.Name)
	}
	return identity, nil
}

// Consumers are checked schema-wide: a later trigger or function attached to a
// tool sequence is not silently advanced as a side effect of table recovery.
func (w *OracleWriter) validatePGOracleSequenceConsumers(ctx context.Context, p pgoracle.SequencePlan) error {
	allowed := map[string]bool{}
	for _, b := range p.Bindings {
		allowed[b.Trigger] = true
	}
	rows, err := w.db.QueryContext(ctx, `SELECT OWNER,NAME,TYPE FROM ALL_DEPENDENCIES WHERE REFERENCED_OWNER=:1 AND REFERENCED_NAME=:2 AND REFERENCED_TYPE='SEQUENCE' ORDER BY OWNER,NAME,TYPE`, w.schema, p.Name)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var owner, name, kind string
		if err := rows.Scan(&owner, &name, &kind); err != nil {
			return err
		}
		if owner != w.schema || kind != "TRIGGER" || !allowed[name] {
			return fmt.Errorf("序列 %s 存在未冻结的使用对象 %s.%s (%s)，拒绝校准", p.Name, owner, name, kind)
		}
	}
	return rows.Err()
}

func (w *OracleWriter) inspectPGOracleSequenceTrigger(ctx context.Context, p pgoracle.SequencePlan, b pgoracle.Binding) (bool, error) {
	var owner, table, event, kind, enabled, valid string
	err := w.db.QueryRowContext(ctx, `SELECT t.TABLE_OWNER,t.TABLE_NAME,t.TRIGGERING_EVENT,t.TRIGGER_TYPE,t.STATUS,o.STATUS FROM ALL_TRIGGERS t JOIN ALL_OBJECTS o ON o.OWNER=t.OWNER AND o.OBJECT_NAME=t.TRIGGER_NAME AND o.OBJECT_TYPE='TRIGGER' WHERE t.OWNER=:1 AND t.TRIGGER_NAME=:2`, w.schema, b.Trigger).Scan(&owner, &table, &event, &kind, &enabled, &valid)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if p.Identity == nil {
		return true, fmt.Errorf("同名触发器 %s 已存在，缺少工具序列身份，拒绝覆盖", b.Trigger)
	}
	if owner != p.Schema || table != b.Table || event != "INSERT" || kind != "BEFORE EACH ROW" || enabled != "ENABLED" || valid != "VALID" {
		return true, fmt.Errorf("序列触发器 %s 的绑定、事件或状态已漂移", b.Trigger)
	}
	rows, err := w.db.QueryContext(ctx, `SELECT TEXT FROM ALL_SOURCE WHERE OWNER=:1 AND NAME=:2 AND TYPE='TRIGGER' ORDER BY LINE`, w.schema, b.Trigger)
	if err != nil {
		return true, err
	}
	var text strings.Builder
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			break
		}
		text.WriteString(line)
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return true, err
	}
	normalize := func(s string) string {
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ";"))
		s = strings.TrimPrefix(s, "CREATE OR REPLACE ")
		return strings.Replace(s, "TRIGGER "+w.dia.QualifyTable(p.Schema, b.Trigger), "TRIGGER "+w.dia.QuoteIdent(b.Trigger), 1)
	}
	if normalize(text.String()) != normalize(p.TriggerSQL(b)) {
		return true, fmt.Errorf("工具触发器 %s 的定义与冻结计划不一致", b.Trigger)
	}
	return true, nil
}

// ValidatePGOracleSequenceBindings checks every frozen target consumer, not
// only the table currently being recovered. Missing triggers may be left for
// their separate repair object, but an existing trigger must match exactly.
func (w *OracleWriter) ValidatePGOracleSequenceBindings(ctx context.Context, p pgoracle.SequencePlan, requireTriggers bool) error {
	if err := w.validatePGOracleSequenceNames(p); err != nil {
		return err
	}
	if p.Schema != w.schema {
		return fmt.Errorf("序列 schema 已变化")
	}
	for _, b := range p.Bindings {
		var typ, virtual string
		var scale sql.NullInt64
		var def sql.NullString
		err := w.db.QueryRowContext(ctx, `SELECT DATA_TYPE,DATA_SCALE,VIRTUAL_COLUMN,DATA_DEFAULT FROM ALL_TAB_COLS WHERE OWNER=:1 AND TABLE_NAME=:2 AND COLUMN_NAME=:3 AND HIDDEN_COLUMN='NO'`, w.schema, b.Table, b.Column).Scan(&typ, &scale, &virtual, &def)
		if err != nil {
			return fmt.Errorf("核验序列绑定 %s.%s: %w", b.Table, b.Column, err)
		}
		if typ != "NUMBER" || !scale.Valid || scale.Int64 != 0 || virtual != "NO" || def.Valid && strings.TrimSpace(def.String) != "" {
			return fmt.Errorf("序列绑定列 %s.%s 的类型、默认值或生成规则已变化", b.Table, b.Column)
		}
		exists, err := w.inspectPGOracleSequenceTrigger(ctx, p, b)
		if err != nil {
			return err
		}
		if requireTriggers && !exists {
			return fmt.Errorf("序列绑定触发器 %s 不存在", b.Trigger)
		}
	}
	return nil
}

func (w *OracleWriter) validatePGOracleSequenceNames(p pgoracle.SequencePlan) error {
	if err := p.Validate(); err != nil {
		return err
	}
	names := []string{p.Schema, p.Name}
	for _, b := range p.Bindings {
		names = append(names, b.Table, b.Column, b.Trigger)
	}
	for _, name := range names {
		if err := w.ValidatePGOracleName(name); err != nil {
			return err
		}
	}
	return nil
}
