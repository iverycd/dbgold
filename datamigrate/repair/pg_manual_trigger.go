package repair

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// PGManualTriggerDefinition identifies both objects in a manual trigger program.
type PGManualTriggerDefinition struct {
	Schema       string
	FunctionName string
	TriggerName  string
	Table        string
}

// HasPGManualTriggerOrigin identifies a failed conversion or a frozen MySQL
// trigger plan. Callers separately check the task, binding and execution result.
func (o Object) HasPGManualTriggerOrigin() bool {
	if o.Kind != "trigger" || o.Schema == "" || o.Table == "" || o.TargetName == "" ||
		o.ManualTrigger != nil || o.Native != nil || o.MySQLDamengTrigger != nil {
		return false
	}
	switch o.TriggerFailureStage {
	case "identity_conflict", "metadata", "diagnostic":
		return false
	}
	if p := o.MySQLPGTrigger; p != nil {
		return p.Schema == o.Schema && p.Table == o.Table && p.Name == o.TargetName &&
			(o.SourceName == "" || p.Source.Name == o.SourceName) &&
			(o.SourceTable == "" || p.Source.TableName == o.SourceTable)
	}
	return o.TriggerFailureStage == "unsupported" && o.SourceName != "" && o.SourceTable != "" &&
		strings.TrimSpace(o.SourceDDL) != ""
}

func (o Object) trustedPGManualTrigger() bool {
	p := o.ManualTrigger
	original := p.Original
	if o.Native != nil || o.MySQLPGTrigger != nil || o.MySQLDamengTrigger != nil ||
		p.Owner != "" && p.Owner != o.Schema || !original.HasPGManualTriggerOrigin() ||
		original.ID != o.ID || original.Schema != o.Schema || original.Table != o.Table ||
		original.TargetName != o.TargetName || original.SourceName != o.SourceName || original.SourceTable != o.SourceTable {
		return false
	}
	d, err := ParsePGManualTrigger(p.FunctionSQL, p.SQL)
	if err != nil || d.Schema != o.Schema || d.TriggerName != o.TargetName || d.Table != o.Table ||
		len(o.Units) != 2 || o.Units[0].ID == o.Units[1].ID {
		return false
	}
	for i, sql := range [...]string{p.FunctionSQL, p.SQL} {
		u := o.Units[i]
		if u.ID == "" || u.Destructive || u.SQL != sql || u.OriginalSQL != sql {
			return false
		}
	}
	return true
}

// ParsePGManualTrigger validates the CREATE statements' envelopes and the
// binding between them. PostgreSQL validates the PL/pgSQL body and WHEN
// expression when it compiles the statements.
func ParsePGManualTrigger(functionSQL, triggerSQL string) (PGManualTriggerDefinition, error) {
	var d PGManualTriggerDefinition
	function, err := lexPGManualTrigger(functionSQL)
	if err != nil {
		return d, fmt.Errorf("触发器函数 SQL: %w", err)
	}
	d.Schema, d.FunctionName, err = function.parseFunction()
	if err != nil {
		return d, err
	}
	trigger, err := lexPGManualTrigger(triggerSQL)
	if err != nil {
		return d, fmt.Errorf("创建触发器 SQL: %w", err)
	}
	if !trigger.take("create") || !trigger.take("trigger") {
		return d, fmt.Errorf("创建触发器 SQL 必须使用 CREATE TRIGGER")
	}
	d.TriggerName = trigger.name()
	if d.TriggerName == "" || (!trigger.take("before") && !trigger.take("after")) || !trigger.parseEvents() || !trigger.take("on") {
		return d, fmt.Errorf("CREATE TRIGGER 须指定触发器名称、触发时机及事件")
	}
	tableSchema, table := trigger.qualifiedName()
	if tableSchema != d.Schema {
		return d, fmt.Errorf("触发器所属表和函数须使用同一个显式 schema")
	}
	d.Table = table
	if !trigger.take("for") {
		return d, fmt.Errorf("CREATE TRIGGER 须为 FOR EACH ROW 行触发器")
	}
	trigger.take("each")
	if !trigger.take("row") {
		return d, fmt.Errorf("CREATE TRIGGER 须为 FOR EACH ROW 行触发器")
	}
	if trigger.take("when") && !trigger.parenthesizedExpression() {
		return d, fmt.Errorf("CREATE TRIGGER 的 WHEN 表达式不完整")
	}
	if !trigger.take("execute") || (!trigger.take("function") && !trigger.take("procedure")) {
		return d, fmt.Errorf("CREATE TRIGGER 须使用 EXECUTE FUNCTION 或 EXECUTE PROCEDURE")
	}
	functionSchema, functionName := trigger.qualifiedName()
	if functionSchema != d.Schema || functionName != d.FunctionName {
		return d, fmt.Errorf("CREATE TRIGGER 须引用本次创建的函数")
	}
	if !trigger.arguments() || !trigger.end() {
		return d, fmt.Errorf("创建触发器 SQL 须为一条完整 CREATE TRIGGER 语句")
	}
	return d, nil
}

type pgManualToken struct {
	kind byte   // i: keyword or bare identifier, q: quoted identifier, d: dollar string, s: string, p: punctuation
	text string // Only identifiers and punctuation need values.
}

type pgManualTokens []pgManualToken

func (ts *pgManualTokens) take(text string) bool {
	if len(*ts) == 0 || (*ts)[0].text != text || (*ts)[0].kind == 'q' {
		return false
	}
	*ts = (*ts)[1:]
	return true
}

func (ts *pgManualTokens) takeKind(kind byte) bool {
	if len(*ts) == 0 || (*ts)[0].kind != kind {
		return false
	}
	*ts = (*ts)[1:]
	return true
}

func (ts *pgManualTokens) name() string {
	if len(*ts) == 0 {
		return ""
	}
	token := (*ts)[0]
	if token.kind != 'q' && token.kind != 'i' {
		return ""
	}
	*ts = (*ts)[1:]
	return token.text
}

func (ts *pgManualTokens) qualifiedName() (schema, name string) {
	schema = ts.name()
	if schema == "" || !ts.take(".") {
		return "", ""
	}
	name = ts.name()
	if name == "" {
		return "", ""
	}
	return schema, name
}

func (ts *pgManualTokens) end() bool {
	ts.take(";")
	return len(*ts) == 0
}

func (ts *pgManualTokens) parseFunction() (schema, name string, err error) {
	if !ts.take("create") || !ts.take("function") {
		return "", "", fmt.Errorf("触发器函数 SQL 必须使用 CREATE FUNCTION")
	}
	schema, name = ts.qualifiedName()
	if schema == "" || !ts.take("(") || !ts.take(")") || !ts.take("returns") {
		return "", "", fmt.Errorf("触发器函数须显式指定 schema、使用空参数并声明 RETURNS trigger")
	}
	returnType := ts.name()
	if returnType == "pg_catalog" && ts.take(".") {
		returnType = ts.name()
	}
	if returnType != "trigger" {
		return "", "", fmt.Errorf("触发器函数须声明 RETURNS trigger")
	}
	var language, body, invoker, volatile bool
	for {
		switch {
		case ts.take("language"):
			if language || ts.name() != "plpgsql" {
				return "", "", fmt.Errorf("触发器函数须使用 LANGUAGE plpgsql")
			}
			language = true
		case ts.take("as"):
			if body || !ts.takeKind('d') {
				return "", "", fmt.Errorf("触发器函数须用 AS 引入一个 dollar-quoted 函数体")
			}
			body = true
		case ts.take("security"):
			if invoker || !ts.take("invoker") {
				return "", "", fmt.Errorf("触发器函数安全模式仅支持 SECURITY INVOKER")
			}
			invoker = true
		case ts.take("volatile"):
			if volatile {
				return "", "", fmt.Errorf("触发器函数的 VOLATILE 重复")
			}
			volatile = true
		default:
			if !language || !body || !ts.end() {
				return "", "", fmt.Errorf("请填写一条完整 CREATE FUNCTION 语句，包含 LANGUAGE plpgsql 和函数体")
			}
			return schema, name, nil
		}
	}
}

func (ts *pgManualTokens) parseEvents() bool {
	for {
		switch {
		case ts.take("update"):
			if ts.take("of") {
				for {
					if ts.name() == "" {
						return false
					}
					if !ts.take(",") {
						break
					}
				}
			}
		case ts.take("insert"), ts.take("delete"):
		default:
			return false
		}
		if !ts.take("or") {
			return true
		}
	}
}

func (ts *pgManualTokens) parenthesizedExpression() bool {
	if !ts.take("(") || ts.take(")") {
		return false
	}
	depth := 1
	for len(*ts) > 0 {
		switch {
		case ts.take("("):
			depth++
		case ts.take(")"):
			depth--
			if depth == 0 {
				return true
			}
		case ts.take(";"):
			return false
		default:
			*ts = (*ts)[1:]
		}
	}
	return false
}

func (ts *pgManualTokens) arguments() bool {
	if !ts.take("(") {
		return false
	}
	if ts.take(")") {
		return true
	}
	for {
		if !ts.takeKind('s') && !ts.takeKind('d') {
			return false
		}
		if !ts.take(",") {
			return ts.take(")")
		}
	}
}

func lexPGManualTrigger(sql string) (pgManualTokens, error) {
	if len(sql) == 0 || len(sql) > 1<<20 || !utf8.ValidString(sql) || strings.ContainsRune(sql, 0) {
		return nil, fmt.Errorf("每条语句须为不超过 1 MiB 的有效 UTF-8 SQL")
	}
	var out pgManualTokens
	for len(sql) > 0 {
		c := sql[0]
		switch {
		case strings.ContainsRune(" \t\n\r\f\v", rune(c)):
			sql = sql[1:]
		case strings.HasPrefix(sql, "--"):
			end := strings.IndexAny(sql, "\r\n")
			if end < 0 {
				return out, nil
			}
			sql = sql[end:]
		case strings.HasPrefix(sql, "/*"):
			end := pgCommentEnd(sql)
			if end == 0 {
				return nil, fmt.Errorf("未闭合的 SQL 注释")
			}
			sql = sql[end:]
		case c == '$' && pgDollarTag(sql) != "":
			tag := pgDollarTag(sql)
			end := strings.Index(sql[len(tag):], tag)
			if end < 0 {
				return nil, fmt.Errorf("未闭合的 dollar-quoted 字符串")
			}
			out = append(out, pgManualToken{kind: 'd'})
			sql = sql[end+2*len(tag):]
		case c == '"' || c == '\'' || (c == 'e' || c == 'E') && len(sql) > 1 && sql[1] == '\'':
			escape := c == 'e' || c == 'E'
			if escape {
				sql = sql[1:]
			}
			token, end, err := pgManualQuoted(sql, escape)
			if err != nil {
				return nil, err
			}
			out = append(out, token)
			sql = sql[end:]
		case pgIdentStart(c):
			end := 1
			for end < len(sql) && (pgIdentPart(sql[end]) || sql[end] == '$') {
				end++
			}
			// PostgreSQL folds unquoted ASCII letters. UTF-8 bytes are preserved.
			word := strings.Map(func(r rune) rune {
				if r >= 'A' && r <= 'Z' {
					return r + ('a' - 'A')
				}
				return r
			}, sql[:end])
			out = append(out, pgManualToken{kind: 'i', text: word})
			sql = sql[end:]
		default:
			_, size := utf8.DecodeRuneInString(sql)
			out = append(out, pgManualToken{kind: 'p', text: sql[:size]})
			sql = sql[size:]
		}
	}
	return out, nil
}

func pgIdentStart(c byte) bool {
	return c == '_' || c >= 0x80 || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func pgIdentPart(c byte) bool { return pgIdentStart(c) || c >= '0' && c <= '9' }

func pgDollarTag(sql string) string {
	if len(sql) < 2 {
		return ""
	}
	if sql[1] == '$' {
		return "$$"
	}
	if !pgIdentStart(sql[1]) {
		return ""
	}
	i := 2
	for i < len(sql) && pgIdentPart(sql[i]) {
		i++
	}
	if i < len(sql) && sql[i] == '$' {
		return sql[:i+1]
	}
	return ""
}

func pgCommentEnd(sql string) int {
	depth := 1
	for i := 2; i+1 < len(sql); {
		switch {
		case strings.HasPrefix(sql[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(sql[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return 0
}

func pgManualQuoted(sql string, escape bool) (pgManualToken, int, error) {
	quote := sql[0]
	for i := 1; i < len(sql); i++ {
		if sql[i] == '\\' && quote == '\'' {
			if !escape {
				return pgManualToken{}, 0, fmt.Errorf("带反斜杠的字符串须使用 E 或 dollar-quoted 语法")
			}
			i++
			continue
		}
		if sql[i] != quote {
			continue
		}
		if i+1 < len(sql) && sql[i+1] == quote {
			i++
			continue
		}
		if quote == '"' {
			return pgManualToken{kind: 'q', text: strings.ReplaceAll(sql[1:i], `""`, `"`)}, i + 1, nil
		}
		return pgManualToken{kind: 's'}, i + 1, nil
	}
	return pgManualToken{}, 0, fmt.Errorf("未闭合的引号")
}
