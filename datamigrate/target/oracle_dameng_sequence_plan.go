package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

// A preflight failure proves that no sequence/trigger statement was attempted.
// Keep the cause available to the task-level connection failure classifier.
type oracleDamengSequencePreflightError struct{ cause error }

func (e *oracleDamengSequencePreflightError) Error() string { return e.cause.Error() }
func (e *oracleDamengSequencePreflightError) Unwrap() error { return e.cause }

func OracleDamengSequencePreflight(err error) error {
	if err == nil || IsOracleDamengSequencePreflight(err) {
		return err
	}
	return &oracleDamengSequencePreflightError{cause: err}
}

func IsOracleDamengSequencePreflight(err error) bool {
	var preflight *oracleDamengSequencePreflightError
	return errors.As(err, &preflight)
}

type oracleDamengSequencePlan struct {
	statements      []dialect.Statement
	reused          []dialect.Statement
	sequenceExists  bool
	missingTriggers []string
	current         *big.Int
	next            *big.Int
	step            *big.Int
}

// PlanOracleDamengSequence reads the actual target state and returns only the
// necessary statements. Callers can save these SQL units for review; planning
// never changes the database or emits execution/verification events.
func (w *DaMengWriter) PlanOracleDamengSequence(ctx context.Context, seq source.SequenceInfo) ([]dialect.Statement, error) {
	plan, err := w.planOracleDamengSequence(ctx, seq)
	if err != nil {
		return nil, err
	}
	return slices.Clone(plan.statements), nil
}

func (w *DaMengWriter) ensureOracleDamengSequence(ctx context.Context, seq source.SequenceInfo) error {
	plan, err := w.planOracleDamengSequence(ctx, seq)
	if err != nil {
		return err
	}
	for _, statement := range plan.reused {
		if err := repair.Observe(ctx, statement.SQL, "reused", nil); err != nil {
			return OracleDamengSequencePreflight(fmt.Errorf("保存已有序列对象核验结果: %w", err))
		}
	}
	return w.execStatements(ctx, plan.statements)
}

func (w *DaMengWriter) planOracleDamengSequence(ctx context.Context, original source.SequenceInfo) (plan oracleDamengSequencePlan, err error) {
	defer func() { err = OracleDamengSequencePreflight(err) }()
	seq, err := dialect.NormalizeOracleDamengSequence(original)
	if err != nil {
		return plan, err
	}
	n := seq.Native
	generated := w.dia.SequenceStatements(w.schema, seq, "")
	if len(generated) != 1+len(n.Bindings) {
		return plan, fmt.Errorf("序列 %s 缺少有效目标 DDL", n.Name)
	}
	plan.sequenceExists, err = w.oracleDamengSequenceObject(ctx, n.Name, "SEQUENCE")
	if err != nil {
		return plan, err
	}
	step, _ := repairInteger(n.IncrementBy)
	lo, _ := repairInteger(n.MinValue)
	hi, _ := repairInteger(n.MaxValue)
	start, _ := recoveryStart(seq)
	current := new(big.Int).Set(start)
	if plan.sequenceExists {
		var actualStep, actualMin, actualMax, actualCurrent, cycle, cache, ordered string
		err = w.db.QueryRowContext(ctx, `SELECT INCREMENT_BY,MIN_VALUE,MAX_VALUE,LAST_NUMBER,CYCLE_FLAG,CACHE_SIZE,ORDER_FLAG FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=? AND SEQUENCE_NAME=?`, w.schema, strings.ToUpper(n.Name)).Scan(&actualStep, &actualMin, &actualMax, &actualCurrent, &cycle, &cache, &ordered)
		if err != nil {
			return plan, fmt.Errorf("读取已有序列 %s.%s 元数据: %w", w.schema, n.Name, err)
		}
		for _, attribute := range []struct{ label, got, want string }{
			{"步长", actualStep, n.IncrementBy}, {"最小边界", actualMin, n.MinValue},
			{"最大边界", actualMax, n.MaxValue}, {"缓存", cache, n.CacheSize},
		} {
			got, parseErr := repairInteger(attribute.got)
			want, _ := repairInteger(attribute.want)
			if parseErr != nil || got.Cmp(want) != 0 {
				return plan, fmt.Errorf("已有序列 %s.%s 的%s不兼容：目标=%s，需要=%s", w.schema, n.Name, attribute.label, attribute.got, attribute.want)
			}
		}
		wantOrder := "N"
		if n.Ordered {
			wantOrder = "Y"
		}
		if strings.TrimSpace(cycle) != "N" || strings.TrimSpace(ordered) != wantOrder {
			return plan, fmt.Errorf("已有序列 %s.%s 的循环或排序属性不兼容", w.schema, n.Name)
		}
		current, err = repairInteger(actualCurrent)
		if err != nil || !current.IsInt64() || current.Cmp(lo) < 0 || current.Cmp(hi) > 0 {
			return plan, fmt.Errorf("已有序列 %s.%s 的当前值超出达梦或序列边界: %s", w.schema, n.Name, actualCurrent)
		}
		plan.reused = append(plan.reused, generated[0])
	}
	var missing []dialect.Statement
	for i, b := range n.Bindings {
		name := dialect.OracleDamengTriggerName(seq, b)
		exists, inspectErr := w.oracleDamengSequenceObject(ctx, name, "TRIGGER")
		if inspectErr != nil {
			return plan, inspectErr
		}
		if !exists {
			plan.missingTriggers = append(plan.missingTriggers, name)
			missing = append(missing, generated[i+1])
			continue
		}
		if !plan.sequenceExists {
			return plan, fmt.Errorf("目标触发器 %s.%s 已存在，但关联序列 %s 不存在，需先核对孤立对象", w.schema, name, n.Name)
		}
		if err = w.inspectOracleDamengSequenceTrigger(ctx, b, generated[i+1].SQL); err != nil {
			return plan, err
		}
		plan.reused = append(plan.reused, generated[i+1])
	}
	bound := farther(current, start, step)
	seen := map[string]bool{}
	for _, b := range n.Bindings {
		key := strings.ToUpper(b.TableName) + "\x00" + strings.ToUpper(b.ColumnName)
		if seen[key] {
			continue
		}
		seen[key] = true
		binding := seq
		binding.TableName, binding.ColumnName = b.TableName, b.ColumnName
		extreme, readErr := recoveryExtreme(ctx, w, w.db, w.schema, binding, step)
		if readErr != nil {
			return plan, fmt.Errorf("读取序列 %s 关联列 %s.%s 极值: %w", n.Name, b.TableName, b.ColumnName, readErr)
		}
		if extreme != nil {
			bound = farther(bound, afterExtreme(extreme, step), step)
		}
	}
	// Existing sequences retain their own arithmetic progression. Source cache
	// watermarks bound the next value, but do not require the same phase offset.
	next := advanceProgression(current, bound, step)
	if !next.IsInt64() || next.Cmp(lo) < 0 || next.Cmp(hi) > 0 {
		return plan, fmt.Errorf("序列 %s 续值 %s 超出达梦或有效序列边界 [%s,%s]", n.Name, next, lo, hi)
	}
	plan.current, plan.next, plan.step = new(big.Int).Set(current), new(big.Int).Set(next), new(big.Int).Set(step)
	if !plan.sequenceExists {
		seq.StartValueText = next.String()
		plan.statements = append(plan.statements, w.dia.SequenceStatements(w.schema, seq, "")[0])
	} else if next.Cmp(current) != 0 {
		plan.statements = append(plan.statements, dialect.Statement{SQL: "ALTER SEQUENCE " + w.dia.QualifyTable(w.schema, n.Name) + " CURRENT VALUE " + next.String()})
	}
	plan.statements = append(plan.statements, missing...)
	return plan, nil
}

func (w *DaMengWriter) oracleDamengSequenceObject(ctx context.Context, name, want string) (bool, error) {
	rows, err := w.db.QueryContext(ctx, `SELECT OBJECT_TYPE FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=?`, w.schema, strings.ToUpper(name))
	if err != nil {
		return false, fmt.Errorf("读取目标对象 %s.%s 类型: %w", w.schema, name, err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var kind string
		if err = rows.Scan(&kind); err != nil {
			return false, err
		}
		count++
		if kind != want || count > 1 {
			return false, fmt.Errorf("达梦同名对象 %s.%s 不是唯一的 %s（实际 %s），禁止覆盖", w.schema, name, want, kind)
		}
	}
	if err = rows.Err(); err != nil {
		return false, err
	}
	return count == 1, nil
}

func (w *DaMengWriter) inspectOracleDamengSequenceTrigger(ctx context.Context, binding source.SequenceBinding, expected string) error {
	name := dialect.OracleDamengTriggerName(source.SequenceInfo{}, binding)
	actual, err := scanOracleDamengSequenceTrigger(w.db.QueryRowContext(ctx, `SELECT OWNER,TRIGGER_NAME,TABLE_OWNER,TABLE_NAME,TRIGGERING_TYPE,TRIGGERING_EVENT,STATUS,DESCRIPTION,TRIGGER_BODY FROM ALL_TRIGGERS WHERE OWNER=? AND TRIGGER_NAME=?`, w.schema, name))
	if err != nil {
		return fmt.Errorf("读取已有触发器 %s.%s 元数据: %w", w.schema, name, err)
	}
	return w.validateOracleDamengSequenceTrigger(actual, binding, expected)
}

type oracleDamengSequenceTrigger struct {
	owner, name, tableOwner, table, timing, event, status, body string
	description                                                 sql.NullString
}

func scanOracleDamengSequenceTrigger(row interface{ Scan(...any) error }) (actual oracleDamengSequenceTrigger, err error) {
	err = row.Scan(&actual.owner, &actual.name, &actual.tableOwner, &actual.table, &actual.timing, &actual.event, &actual.status, &actual.description, &actual.body)
	return actual, err
}

func (w *DaMengWriter) validateOracleDamengSequenceTrigger(actual oracleDamengSequenceTrigger, binding source.SequenceBinding, expected string) error {
	name := dialect.OracleDamengTriggerName(source.SequenceInfo{}, binding)
	fail := func() error {
		return fmt.Errorf("已有触发器 %s.%s 的表、事件、启用状态或完整定义不兼容，禁止覆盖", w.schema, name)
	}
	timing, status := strings.TrimSpace(actual.timing), strings.TrimSpace(actual.status)
	if actual.owner != w.schema || actual.name != name || actual.tableOwner != w.schema || actual.table != strings.ToUpper(binding.TableName) || timing != "BEFORE EACH ROW" && timing != "BEFORE ROW" || strings.TrimSpace(actual.event) != "INSERT" || status != "ENABLED" && status != "Y" {
		return fail()
	}
	pos := strings.Index(expected, "\nBEGIN")
	if pos < 0 {
		return fail()
	}
	expectedHeader, expectedBody := expected[:pos], expected[pos:]
	actualTokens := strings.Split(oracleDamengSequenceBodyKey(actual.body), "\x00")
	expectedTokens := strings.Split(oracleDamengSequenceBodyKey(expectedBody), "\x00")
	if len(actualTokens) > 0 && actualTokens[0] != "BEGIN" {
		// DM8 exposes the complete CREATE OR REPLACE statement in TRIGGER_BODY.
		// Verify its entire header before comparing the generated block.
		want, _, ok := oracleDamengSequenceHeader(strings.Split(oracleDamengSequenceBodyKey(expectedHeader), "\x00"), "")
		if !ok {
			return fail()
		}
		got, end, ok := oracleDamengSequenceHeader(actualTokens, want[0])
		if !ok || got != want {
			return fail()
		}
		actualTokens = actualTokens[end:]
	} else if !actual.description.Valid || !oracleDamengSequenceTriggerHeader(actual.description.String, expectedHeader) {
		// A block alone cannot prove that the declaration has no WHEN condition.
		return fail()
	}
	if !slices.Equal(actualTokens, expectedTokens) {
		return fail()
	}
	return nil
}

func oracleDamengSequenceTriggerHeader(description, expected string) bool {
	// DM abbreviates DESCRIPTION to unqualified identifiers. Quoted identifiers
	// keep their exact spelling; ordinary identifiers resolve in the target schema.
	wantTokens := strings.Split(oracleDamengSequenceBodyKey(expected), "\x00")
	want, wantEnd, ok := oracleDamengSequenceHeader(wantTokens, "")
	if !ok || wantEnd != len(wantTokens) {
		return false
	}
	gotTokens := strings.Split(oracleDamengSequenceBodyKey(description), "\x00")
	got, gotEnd, ok := oracleDamengSequenceHeader(gotTokens, want[0])
	return ok && gotEnd == len(gotTokens) && got == want
}

func oracleDamengSequenceHeader(tokens []string, defaultSchema string) (identity [4]string, end int, ok bool) {
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
			return identity, i, false
		}
		if !take("TRIGGER") {
			return identity, i, false
		}
	}
	readIdent := func() (string, bool) {
		if i >= len(tokens) || tokens[i] == "" {
			return "", false
		}
		token := tokens[i]
		i++
		if token[0] == '"' && len(token) >= 2 && token[len(token)-1] == '"' {
			return strings.ReplaceAll(token[1:len(token)-1], `""`, `"`), true
		}
		for _, c := range []byte(token) {
			if !isOracleDamengSequenceWord(c) {
				return "", false
			}
		}
		return strings.ToUpper(token), true
	}
	readName := func() (string, string, bool) {
		first, valid := readIdent()
		if !valid {
			return "", "", false
		}
		if take(".") {
			second, valid := readIdent()
			return first, second, valid
		}
		return defaultSchema, first, defaultSchema != ""
	}
	identity[0], identity[1], ok = readName()
	if !ok || !take("BEFORE") || !take("INSERT") || !take("ON") {
		return identity, i, false
	}
	identity[2], identity[3], ok = readName()
	if !ok || !take("FOR") || !take("EACH") || !take("ROW") {
		return identity, i, false
	}
	return identity, i, true
}

// Ignore formatting only outside quoted SQL. In particular, whitespace inside
// a quoted identifier is part of its name and must never be collapsed.
func oracleDamengSequenceBodyKey(sqlText string) string {
	var tokens []string
	for i := 0; i < len(sqlText); {
		if r, width := utf8.DecodeRuneInString(sqlText[i:]); unicode.IsSpace(r) {
			i += width
			continue
		}
		start := i
		if sqlText[i] == '\'' || sqlText[i] == '"' {
			quote := sqlText[i]
			i++
			closed := false
			for i < len(sqlText) {
				if sqlText[i] == quote {
					i++
					if i < len(sqlText) && sqlText[i] == quote {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return "invalid quoted SQL"
			}
			tokens = append(tokens, sqlText[start:i])
		} else if strings.HasPrefix(sqlText[i:], "/*") {
			end := strings.Index(sqlText[i+2:], "*/")
			if end < 0 {
				return "invalid SQL comment"
			}
			i += end + 4
			tokens = append(tokens, sqlText[start:i])
		} else if isOracleDamengSequenceWord(sqlText[i]) {
			for i < len(sqlText) && isOracleDamengSequenceWord(sqlText[i]) {
				i++
			}
			tokens = append(tokens, strings.ToUpper(sqlText[start:i]))
		} else {
			i++
			tokens = append(tokens, sqlText[start:i])
		}
	}
	return strings.Join(tokens, "\x00")
}

func isOracleDamengSequenceWord(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '$' || c == '#' || c >= utf8.RuneSelf
}
