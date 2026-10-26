package target

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"dbgold/datamigrate/mysqldmtrigger"
)

type mysqlDMTriggerInvalidError struct{ name string }

func (e mysqlDMTriggerInvalidError) Error() string {
	return fmt.Sprintf("目标触发器 %s 已创建但编译无效，需人工核对编译错误", e.name)
}

func mysqlDMTriggerInspectionStatus(err error) string {
	var invalid mysqlDMTriggerInvalidError
	if errors.As(err, &invalid) {
		return "failed"
	}
	return "unknown"
}

func (w *DaMengWriter) mysqlDMTriggerObject(ctx context.Context, name string) (bool, error) {
	rows, err := w.db.QueryContext(ctx, `SELECT OBJECT_TYPE,STATUS FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=?`, w.schema, name)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var kind, status string
		if err = rows.Scan(&kind, &status); err != nil {
			return false, err
		}
		count++
		if count == 1 && strings.TrimSpace(kind) == "TRIGGER" && strings.TrimSpace(status) == "INVALID" {
			return false, mysqlDMTriggerInvalidError{name: w.schema + "." + name}
		}
		if count != 1 || strings.TrimSpace(kind) != "TRIGGER" || strings.TrimSpace(status) != "VALID" {
			return false, fmt.Errorf("目标同名对象 %s.%s 不是唯一且编译有效的 TRIGGER（类型=%s，状态=%s），禁止覆盖", w.schema, name, kind, status)
		}
	}
	return count == 1, rows.Err()
}

func (w *DaMengWriter) inspectMySQLDamengTrigger(ctx context.Context, p mysqldmtrigger.Plan) (bool, error) {
	exists, err := w.mysqlDMTriggerObject(ctx, p.Name)
	if err != nil {
		return false, err
	}
	rows, err := w.db.QueryContext(ctx, `SELECT OWNER,TRIGGER_NAME,TABLE_OWNER,TABLE_NAME,TRIGGERING_TYPE,TRIGGERING_EVENT,STATUS,DESCRIPTION,TRIGGER_BODY FROM ALL_TRIGGERS WHERE TABLE_OWNER=? AND TABLE_NAME=? ORDER BY TRIGGER_NAME`, p.Schema, p.Table)
	if err != nil {
		return false, err
	}
	actual := map[string]oracleDamengSequenceTrigger{}
	for rows.Next() {
		trigger, scanErr := scanOracleDamengSequenceTrigger(rows)
		if scanErr != nil {
			rows.Close()
			return false, scanErr
		}
		if _, duplicate := actual[trigger.name]; duplicate {
			rows.Close()
			return false, fmt.Errorf("目标触发器目录返回重复对象 %s", trigger.name)
		}
		actual[trigger.name] = trigger
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, err
	}
	found := false
	for name, trigger := range actual {
		if name == p.Name {
			if !exists || !mysqlDMTriggerMatches(trigger, p.TriggerSQL, p.Schema) {
				return false, fmt.Errorf("目标触发器 %s 完整定义、表、事件、状态或顺序与冻结计划不一致", name)
			}
			found = true
			continue
		}
		state := strings.TrimSpace(trigger.status)
		if state == "N" || state == "DISABLED" {
			continue
		}
		if !mysqlDMTriggerTiming(trigger.timing, strings.ToUpper(p.Source.Timing)) || strings.TrimSpace(trigger.event) != strings.ToUpper(p.Source.Event) {
			continue
		}
		ancestorSQL, known := p.Options.PredecessorSQLs[name]
		if !known {
			ancestorSQL, known = p.Options.PeerSQLs[name]
		}
		if !known || !mysqlDMTriggerMatches(trigger, ancestorSQL, p.Schema) {
			return false, fmt.Errorf("目标表 %s 存在同类启用触发器 %s，无法证明其属于冻结顺序链或同组定义", p.Table, name)
		}
		valid, err := w.mysqlDMTriggerObject(ctx, name)
		if err != nil {
			return false, err
		}
		if !valid {
			return false, fmt.Errorf("触发器前驱 %s 编译状态不可见", name)
		}
	}
	for name, expected := range p.Options.PredecessorSQLs {
		ancestor, ok := actual[name]
		if !ok || !mysqlDMTriggerMatches(ancestor, expected, p.Schema) {
			return false, fmt.Errorf("冻结顺序链前驱 %s 缺失、未启用或完整定义已变化", name)
		}
	}
	if p.Predecessor != "" {
		predecessor, ok := actual[p.Predecessor]
		expected, frozen := p.Options.PredecessorSQLs[p.Predecessor]
		if !ok || !frozen || !mysqlDMTriggerMatches(predecessor, expected, p.Schema) {
			return false, fmt.Errorf("前驱触发器 %s 尚未成功创建或定义已变化", p.Predecessor)
		}
	}
	if exists != found {
		return false, fmt.Errorf("目标触发器对象和表触发器目录不一致")
	}
	return found, nil
}

func mysqlDMTriggerTiming(actual, expected string) bool {
	actual = strings.TrimSpace(actual)
	return actual == expected+" EACH ROW" || actual == expected+" ROW"
}

type mysqlDMTriggerHeader struct{ schema, name, timing, event, tableSchema, table, predecessorSchema, predecessor string }

func mysqlDMReadTriggerHeader(tokens []string, defaultSchema string) (h mysqlDMTriggerHeader, end int, ok bool) {
	i := 0
	take := func(word string) bool {
		if i < len(tokens) && tokens[i] == word {
			i++
			return true
		}
		return false
	}
	if take("CREATE") {
		if take("OR") && !take("REPLACE") {
			return h, i, false
		}
		if !take("TRIGGER") {
			return h, i, false
		}
	} else {
		take("TRIGGER")
	}
	ident := func() (string, bool) {
		if i >= len(tokens) || tokens[i] == "" {
			return "", false
		}
		t := tokens[i]
		i++
		if t[0] == '"' && len(t) > 1 && t[len(t)-1] == '"' {
			return strings.ReplaceAll(t[1:len(t)-1], `""`, `"`), true
		}
		for _, b := range []byte(t) {
			if !isOracleDamengSequenceWord(b) {
				return "", false
			}
		}
		return strings.ToUpper(t), true
	}
	name := func() (string, string, bool) {
		first, valid := ident()
		if !valid {
			return "", "", false
		}
		if take(".") {
			second, valid := ident()
			return first, second, valid
		}
		return defaultSchema, first, defaultSchema != ""
	}
	h.schema, h.name, ok = name()
	if !ok {
		return h, i, false
	}
	if take("BEFORE") {
		h.timing = "BEFORE"
	} else if take("AFTER") {
		h.timing = "AFTER"
	} else {
		return h, i, false
	}
	if i >= len(tokens) {
		return h, i, false
	}
	h.event = tokens[i]
	i++
	if h.event != "INSERT" && h.event != "UPDATE" && h.event != "DELETE" || !take("ON") {
		return h, i, false
	}
	h.tableSchema, h.table, ok = name()
	if !ok || !take("FOR") || !take("EACH") || !take("ROW") {
		return h, i, false
	}
	if take("FOLLOWS") {
		h.predecessorSchema, h.predecessor, ok = name()
		if !ok {
			return h, i, false
		}
	}
	return h, i, true
}

func mysqlDMTriggerMatches(actual oracleDamengSequenceTrigger, expected, schema string) bool {
	wantTokens, ok := mysqlDMProgramTokens(expected)
	if !ok {
		return false
	}
	want, end, ok := mysqlDMReadTriggerHeader(wantTokens, schema)
	if !ok || end >= len(wantTokens) {
		return false
	}
	if actual.owner != want.schema || actual.name != want.name || actual.tableOwner != want.tableSchema || actual.table != want.table || !mysqlDMTriggerTiming(actual.timing, want.timing) || strings.TrimSpace(actual.event) != want.event {
		return false
	}
	state := strings.TrimSpace(actual.status)
	if state != "Y" && state != "ENABLED" {
		return false
	}
	gotTokens, ok := mysqlDMProgramTokens(actual.body)
	if !ok || len(gotTokens) == 0 {
		return false
	}
	if gotTokens[0] == "CREATE" || gotTokens[0] == "TRIGGER" {
		got, bodyStart, valid := mysqlDMReadTriggerHeader(gotTokens, schema)
		return valid && got == want && slices.Equal(gotTokens[bodyStart:], wantTokens[end:])
	}
	if !actual.description.Valid {
		return false
	}
	headerTokens, valid := mysqlDMProgramTokens(actual.description.String)
	if !valid {
		return false
	}
	got, headerEnd, valid := mysqlDMReadTriggerHeader(headerTokens, schema)
	return valid && got == want && headerEnd == len(headerTokens) && slices.Equal(gotTokens, wantTokens[end:])
}

// Normalize formatting and ordinary keyword case only. Literal bytes, quoted
// identifiers, and comment boundaries are part of the execution evidence.
func mysqlDMProgramTokens(s string) ([]string, bool) {
	var out []string
	for i := 0; i < len(s); {
		if r, size := utf8.DecodeRuneInString(s[i:]); unicode.IsSpace(r) {
			i += size
			continue
		}
		start := i
		if strings.HasPrefix(s[i:], "--") {
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				end = len(s) - i
			}
			i += end
			out = append(out, s[start:i])
			continue
		}
		if strings.HasPrefix(s[i:], "/*") {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, false
			}
			i += end + 4
			out = append(out, s[start:i])
			continue
		}
		if s[i] == '\'' || s[i] == '"' {
			quote := s[i]
			i++
			closed := false
			for i < len(s) {
				if s[i] == quote {
					i++
					if i < len(s) && s[i] == quote {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, false
			}
			out = append(out, s[start:i])
			continue
		}
		if isOracleDamengSequenceWord(s[i]) {
			for i < len(s) && isOracleDamengSequenceWord(s[i]) {
				i++
			}
			out = append(out, strings.ToUpper(s[start:i]))
			continue
		}
		i++
		out = append(out, s[start:i])
	}
	return out, true
}
