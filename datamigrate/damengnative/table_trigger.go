package damengnative

import (
	"fmt"
	"strings"
	"unicode"
)

// TableTriggerDefinition describes one complete Oracle-compatible table trigger.
// SQL retains the user's program bytes except for a final client '/' delimiter.
type TableTriggerDefinition struct {
	Schema, Name, TableSchema, Table, SQL string
	Replace                               bool
}

// ParseTableTriggerHeader recovers object identity from failed historical DDL
// without lexing its potentially invalid body. It grants no execution authority.
func ParseTableTriggerHeader(sql, defaultSchema string) (TableTriggerDefinition, error) {
	sql = StripClientDelimiter(sql)
	on, end := false, 0
	_, err := nativeTokensUntil(sql, func(t sqlToken) bool {
		if !on {
			if t.kind == 'i' && strings.EqualFold(t.value, "ON") {
				on = true
			}
			return false
		}
		if t.kind != 'i' && t.kind != 'q' {
			return false
		}
		// A following dot means this was the schema component, not the table.
		remaining := trimTriggerHeaderComments(sql[t.end:])
		if strings.HasPrefix(remaining, ".") {
			return false
		}
		end = t.end
		return true
	})
	if err != nil {
		return TableTriggerDefinition{}, err
	}
	if end == 0 {
		return TableTriggerDefinition{}, fmt.Errorf("触发器缺少可识别的 ON 表身份")
	}
	d, err := ParseTableTriggerSQL(sql[:end]+" BEGIN NULL; END;", defaultSchema)
	d.SQL = sql
	return d, err
}

func trimTriggerHeaderComments(s string) string {
	for {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "--") {
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[i+1:]
				continue
			}
			return ""
		}
		if strings.HasPrefix(s, "/*") {
			if i := strings.Index(s[2:], "*/"); i >= 0 {
				s = s[i+4:]
				continue
			}
			return ""
		}
		return s
	}
}

// CreateSQL changes only the CREATE header. In particular, an absent target is
// created without OR REPLACE so a concurrent unrelated object cannot be replaced.
func (d TableTriggerDefinition) CreateSQL(replace bool) string {
	ts, err := nativeTokens(d.SQL)
	if err != nil || len(ts) < 2 || d.Replace == replace {
		return d.SQL
	}
	if d.Replace && len(ts) > 3 {
		return d.SQL[:ts[1].start] + d.SQL[ts[3].start:]
	}
	return d.SQL[:ts[0].end] + " OR REPLACE" + d.SQL[ts[0].end:]
}

// ParseTableTriggerSQL validates the statement boundary, not the business
// program. DML, calls and control flow inside the trigger remain user-authored
// code and are submitted as one driver statement, never split at semicolons.
func ParseTableTriggerSQL(sql, defaultSchema string) (d TableTriggerDefinition, err error) {
	d.SQL = StripClientDelimiter(sql)
	if len(d.SQL) == 0 || len(d.SQL) > 1<<20 || strings.ContainsRune(d.SQL, 0) {
		return d, fmt.Errorf("触发器 SQL 不能为空且不能超过 1 MiB")
	}
	ts, err := nativeTokens(d.SQL)
	if err != nil {
		return d, err
	}
	last := 0
	for _, t := range ts {
		gap := d.SQL[last:t.start]
		if strings.Contains(gap, "/*!") || strings.Contains(gap, "/*M!") || strings.Contains(gap, "/*m!") {
			return d, fmt.Errorf("触发器 SQL 不允许可执行版本注释")
		}
		last = t.end
	}
	if gap := d.SQL[last:]; strings.Contains(gap, "/*!") || strings.Contains(gap, "/*M!") || strings.Contains(gap, "/*m!") {
		return d, fmt.Errorf("触发器 SQL 不允许可执行版本注释")
	}
	i := 0
	take := func(word string) bool {
		if i < len(ts) && ts[i].kind == 'i' && strings.EqualFold(ts[i].value, word) {
			i++
			return true
		}
		return false
	}
	if !take("CREATE") {
		return d, fmt.Errorf("请提交完整 CREATE TRIGGER 语句")
	}
	if take("OR") {
		if !take("REPLACE") {
			return d, fmt.Errorf("仅支持 CREATE 或 CREATE OR REPLACE TRIGGER")
		}
		d.Replace = true
	}
	if !take("TRIGGER") {
		return d, fmt.Errorf("仅支持表触发器 CREATE TRIGGER")
	}
	name := func() (string, string, error) {
		first, e := tableTriggerName(ts, &i)
		if e != nil {
			return "", "", e
		}
		if i < len(ts) && ts[i].value == "." {
			i++
			second, e := tableTriggerName(ts, &i)
			if e != nil {
				return "", "", e
			}
			if i < len(ts) && (ts[i].value == "." || ts[i].value == "@") {
				return "", "", fmt.Errorf("触发器对象不允许跨库名称")
			}
			return first, second, nil
		}
		if i < len(ts) && ts[i].value == "@" {
			return "", "", fmt.Errorf("触发器对象不允许跨库名称")
		}
		return defaultSchema, first, nil
	}
	d.Schema, d.Name, err = name()
	if err != nil {
		return d, err
	}
	if !take("BEFORE") && !take("AFTER") {
		return d, fmt.Errorf("仅支持普通表的 BEFORE / AFTER 触发器，不支持视图或事件触发器")
	}
	for {
		if take("UPDATE") {
			if take("OF") {
				if _, err = tableTriggerName(ts, &i); err != nil {
					return d, err
				}
				for i < len(ts) && ts[i].value == "," {
					i++
					if _, err = tableTriggerName(ts, &i); err != nil {
						return d, err
					}
				}
			}
		} else if !take("INSERT") && !take("DELETE") {
			return d, fmt.Errorf("仅支持 INSERT / UPDATE / DELETE 表触发器")
		}
		if !take("OR") {
			break
		}
	}
	take("LOCAL")
	if !take("ON") {
		return d, fmt.Errorf("触发器缺少 ON 所属表")
	}
	if i < len(ts) && ts[i].kind == 'i' && (strings.EqualFold(ts[i].value, "SCHEMA") || strings.EqualFold(ts[i].value, "DATABASE")) {
		return d, fmt.Errorf("不支持 Schema 或数据库事件触发器")
	}
	d.TableSchema, d.Table, err = name()
	if err != nil {
		return d, err
	}
	// The remaining header permits row aliases, row/statement granularity and
	// WHEN predicates. Quotes and parentheses keep keyword-like column names out
	// of the body-boundary scan.
	depth := 0
	body := -1
	for ; i < len(ts); i++ {
		t := ts[i]
		if t.value == ";" {
			return d, fmt.Errorf("触发器缺少完整正文")
		}
		if t.value == "(" {
			depth++
			continue
		}
		if t.value == ")" {
			depth--
			if depth < 0 {
				return d, fmt.Errorf("触发器头部括号未闭合")
			}
			continue
		}
		if depth != 0 || t.kind != 'i' {
			continue
		}
		switch strings.ToUpper(t.value) {
		case "DECLARE", "BEGIN", "CALL", "INSERT", "UPDATE", "DELETE", "SELECT", "SET":
			body = i
		case "INSTEAD", "COMPOUND", "CREATE", "ALTER", "DROP", "EXEC", "EXECUTE", "GRANT", "REVOKE":
			return d, fmt.Errorf("不支持该触发器头部或附加 SQL")
		}
		if body >= 0 {
			break
		}
	}
	if depth != 0 || body < 0 {
		return d, fmt.Errorf("触发器缺少完整正文")
	}
	if err = tableTriggerBody(ts[body:]); err != nil {
		return d, err
	}
	return d, nil
}

func tableTriggerName(ts []sqlToken, pos *int) (string, error) {
	if *pos >= len(ts) || ts[*pos].kind != 'i' && ts[*pos].kind != 'q' || ts[*pos].value == "" {
		return "", fmt.Errorf("触发器缺少有效对象名")
	}
	t := ts[*pos]
	*pos++
	if t.kind == 'q' {
		return t.value, nil
	}
	return strings.ToUpper(t.value), nil
}

func tableTriggerBody(ts []sqlToken) error {
	fail := func() error {
		return fmt.Errorf("触发器正文必须是单个完整程序块，不能附加其他 SQL")
	}
	is := func(i int, word string) bool {
		return i < len(ts) && ts[i].kind == 'i' && strings.EqualFold(ts[i].value, word)
	}
	if !is(0, "BEGIN") && !is(0, "DECLARE") {
		depth := 0
		for i, t := range ts {
			if t.value == "(" {
				depth++
			}
			if t.value == ")" {
				depth--
			}
			if depth < 0 || t.value == ";" && depth == 0 && i != len(ts)-1 {
				return fail()
			}
		}
		if depth != 0 {
			return fail()
		}
		return nil
	}
	type frame struct {
		kind string
		body bool
	}
	var stack []frame
	paren := 0
	for i := 0; i < len(ts); i++ {
		t := ts[i]
		if t.value == "(" {
			paren++
			continue
		}
		if t.value == ")" {
			paren--
			if paren < 0 {
				return fail()
			}
			continue
		}
		if t.kind != 'i' {
			if t.value == ";" && len(stack) > 0 && stack[len(stack)-1].kind == "SUB" && !stack[len(stack)-1].body {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		word := strings.ToUpper(t.value)
		switch word {
		case "DECLARE":
			stack = append(stack, frame{kind: "DECLARE"})
		case "PROCEDURE", "FUNCTION":
			if len(stack) > 0 && (stack[len(stack)-1].kind == "DECLARE" || stack[len(stack)-1].kind == "SUB") {
				stack = append(stack, frame{kind: "SUB"})
			}
		case "IS", "AS":
			if len(stack) > 0 && stack[len(stack)-1].kind == "SUB" {
				stack[len(stack)-1].body = true
			}
		case "BEGIN", "IF", "LOOP", "CASE":
			stack = append(stack, frame{kind: word})
		case "END":
			if len(stack) == 0 {
				return fail()
			}
			top := stack[len(stack)-1].kind
			if is(i+1, "IF") || is(i+1, "LOOP") || is(i+1, "CASE") {
				if !is(i+1, top) {
					return fail()
				}
				i++
			} else if top != "BEGIN" && top != "CASE" {
				return fail()
			}
			stack = stack[:len(stack)-1]
			if top == "BEGIN" && len(stack) > 0 && (stack[len(stack)-1].kind == "DECLARE" || stack[len(stack)-1].kind == "SUB") {
				stack = stack[:len(stack)-1]
			}
			if len(stack) == 0 {
				if paren != 0 {
					return fail()
				}
				if i+1 < len(ts) && (ts[i+1].kind == 'i' || ts[i+1].kind == 'q') {
					i++
				}
				if i+1 < len(ts) && ts[i+1].value == ";" {
					i++
				}
				if i != len(ts)-1 {
					return fail()
				}
				return nil
			}
		}
	}
	return fail()
}

// TableTriggerDefinitionsEqual compares complete programs while preserving
// literal bytes and the case-sensitive meaning of quoted identifiers.
func TableTriggerDefinitionsEqual(a, b, defaultSchema string) bool {
	x, err := ParseTableTriggerSQL(a, defaultSchema)
	if err != nil {
		return false
	}
	y, err := ParseTableTriggerSQL(b, defaultSchema)
	if err != nil || x.Schema != y.Schema || x.Name != y.Name || x.TableSchema != y.TableSchema || x.Table != y.Table {
		return false
	}
	canonical := func(d TableTriggerDefinition) string {
		sql := d.CreateSQL(false)
		ts, _ := nativeTokens(sql)
		replaceIdentity := func(start int, schema, name string) {
			end := start + 1
			if end+1 < len(ts) && ts[end].value == "." {
				end += 2
			}
			ts = append(ts[:start], append([]sqlToken{{kind: 'x', value: schema + "\x01" + name}}, ts[end:]...)...)
		}
		replaceIdentity(2, d.Schema, d.Name)
		for i := 3; i < len(ts); i++ {
			if ts[i].kind == 'i' && strings.EqualFold(ts[i].value, "ON") {
				replaceIdentity(i+1, d.TableSchema, d.Table)
				break
			}
		}
		for len(ts) > 0 && ts[len(ts)-1].value == ";" {
			ts = ts[:len(ts)-1]
		}
		var out []string
		for _, t := range ts {
			switch t.kind {
			case 'x':
				out = append(out, "X:"+t.value)
			case 'i':
				out = append(out, "I:"+strings.ToUpper(t.value))
			case 'q':
				plain := t.value != "" && t.value == strings.ToUpper(t.value)
				for i, r := range t.value {
					plain = plain && (unicode.IsLetter(r) || r == '_' || r == '$' || r == '#' || i > 0 && unicode.IsDigit(r))
				}
				if plain {
					out = append(out, "I:"+t.value)
				} else {
					out = append(out, "Q:"+t.value)
				}
			case 's':
				out = append(out, "S:"+sql[t.start:t.end])
			default:
				out = append(out, "P:"+t.value)
			}
		}
		return strings.Join(out, "\x00")
	}
	return canonical(x) == canonical(y)
}
