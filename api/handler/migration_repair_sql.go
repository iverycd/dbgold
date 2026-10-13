package handler

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"dbgold/datamigrate/repair"
)

var repairDDLHead = regexp.MustCompile(`(?is)^\s*(?:CREATE(?:\s+OR\s+(?:REPLACE|ALTER))?(?:\s+UNIQUE)?(?:\s+(?:CLUSTERED|NONCLUSTERED))?\s+(TABLE|VIEW|INDEX|SEQUENCE|TRIGGER)|ALTER\s+(TABLE|VIEW|SEQUENCE)|COMMENT\s+ON\s+(TABLE|COLUMN)|DROP\s+(INDEX|VIEW|SEQUENCE))\s+(?:IF\s+(?:NOT\s+)?EXISTS\s+)?`)
var repairIndexOn = regexp.MustCompile(`(?is)\bON\s+`)
var repairExecutableComment = regexp.MustCompile(`(?is)/\*\s*!|/\*[mM]!`)
var repairVisibilityComment = regexp.MustCompile(`(?i)/\*![0-9]+\s+(?:INVISIBLE|VISIBLE)\s*\*/`)
var repairLiteralImmediate = regexp.MustCompile(`(?is)^\s*EXECUTE\s+IMMEDIATE\s+'((?:''|[^'])*)'\s*;`)
var repairLiteralExec = regexp.MustCompile(`(?is)^\s*EXEC(?:UTE)?\s*\(\s*N?'((?:''|[^'])*)'\s*\)\s*;?`)

func trimRepairComments(s string) string {
	for {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "--") {
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return ""
			}
			s = s[i+1:]
			continue
		}
		if strings.HasPrefix(s, "/*") {
			i := strings.Index(s[2:], "*/")
			if i < 0 {
				return ""
			}
			s = s[i+4:]
			continue
		}
		return s
	}
}
func repairIdentifier(s string) ([]string, int, error) {
	var parts []string
	pos := 0
	for {
		for pos < len(s) && unicode.IsSpace(rune(s[pos])) {
			pos++
		}
		if pos >= len(s) {
			return nil, 0, fmt.Errorf("DDL缺少对象名")
		}
		start := pos
		quote := s[pos]
		var value string
		if quote == '"' || quote == '`' || quote == '[' {
			end := quote
			if quote == '[' {
				end = ']'
			}
			pos++
			var b strings.Builder
			closed := false
			for pos < len(s) {
				ch := s[pos]
				pos++
				if ch == end {
					if pos < len(s) && s[pos] == end {
						b.WriteByte(ch)
						pos++
						continue
					}
					closed = true
					break
				}
				b.WriteByte(ch)
			}
			if !closed {
				return nil, 0, fmt.Errorf("对象标识符未闭合")
			}
			value = b.String()
		} else {
			for pos < len(s) {
				r, n := utf8.DecodeRuneInString(s[pos:])
				if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' && r != '#' {
					break
				}
				pos += n
			}
			if pos == start {
				return nil, 0, fmt.Errorf("无法识别DDL对象名")
			}
			value = s[start:pos]
		}
		parts = append(parts, value)
		for pos < len(s) && unicode.IsSpace(rune(s[pos])) {
			pos++
		}
		if pos >= len(s) || s[pos] != '.' {
			break
		}
		pos++
	}
	return parts, pos, nil
}
func repairHead(s string) (kind string, parts []string, tail string, err error) {
	s = trimRepairComments(s)
	m := repairDDLHead.FindStringSubmatchIndex(s)
	if m == nil {
		return "", nil, s, nil
	}
	for i := 2; i < len(m); i += 2 {
		if m[i] >= 0 {
			kind = strings.ToLower(s[m[i]:m[i+1]])
			break
		}
	}
	parts, n, e := repairIdentifier(s[m[1]:])
	return kind, parts, s[m[1]+n:], e
}
func qualifiedRepairIdentity(parts []string, defaultSchema string) (string, string, error) {
	if len(parts) == 1 {
		return defaultSchema, parts[0], nil
	}
	if len(parts) == 2 {
		return parts[0], parts[1], nil
	}
	return "", "", fmt.Errorf("DDL不能跨数据库或服务器修改对象")
}

// Procedural languages are not a general SQL sandbox. Only unchanged trusted
// generated blocks (with optional numeric-literal edits), or blocks consisting
// entirely of statically scoped literal DDL, can pass this boundary.
func validateRepairObjectScope(obj repair.Object, units []repair.Unit, dbType string) error {
	for _, u := range units {
		if !u.Selected || u.Status == "succeeded" {
			continue
		}
		if unsafeRepairComment(u.SQL) {
			return fmt.Errorf("对象修复不允许可执行SQL注释")
		}
		tokens, count, e := scanQueryTokens(u.SQL)
		if e != nil {
			return e
		}
		if len(tokens) == 0 {
			return fmt.Errorf("SQL不能为空")
		}
		triggerBody := false
		for _, t := range tokens[:min(len(tokens), 5)] {
			if t == "TRIGGER" {
				triggerBody = true
			}
		}
		native := triggerBody || tokens[0] == "DECLARE" || tokens[0] == "BEGIN" || tokens[0] == "IF" || tokens[0] == "DO" || tokens[0] == "EXEC" || tokens[0] == "EXECUTE" || tokens[0] == "DBCC"
		if native && !trustedRepairBlock(u) {
			if e := validateLiteralRepairBlock(obj, u.SQL, dbType); e != nil {
				return e
			}
		}
		if count > 1 && !native {
			return fmt.Errorf("普通DDL每个执行单元只能包含一条语句，请明确拆分")
		}
		for i, t := range tokens {
			prev := ""
			if i > 0 {
				prev = tokens[i-1]
			}
			if (t == "DELETE" || t == "UPDATE") && prev != "ON" {
				return fmt.Errorf("对象DDL修复不允许数据增删改")
			}
			if t == "INSERT" && prev != "BEFORE" && prev != "AFTER" && prev != "OF" {
				return fmt.Errorf("对象DDL修复不允许插入数据")
			}
			if t == "GRANT" || t == "REVOKE" || t == "COPY" || t == "ATTACH" || t == "DETACH" {
				return fmt.Errorf("此操作不属于对象DDL修复")
			}
		}
		kind, parts, tail, e := repairHead(u.SQL)
		if e != nil {
			return e
		}
		if kind == "" {
			if native {
				continue
			}
			return fmt.Errorf("无法确认DDL对象身份，请使用受支持的对象DDL")
		}
		if kind == "column" {
			if len(parts) < 2 {
				return fmt.Errorf("列注释须限定所属表")
			}
			parts = parts[:len(parts)-1]
		}
		if kind == "table" && tokens[0] == "ALTER" {
			for i, token := range tokens {
				if token == "RENAME" && (i+1 == len(tokens) || tokens[i+1] != "COLUMN") {
					return fmt.Errorf("表名必须固定；仅允许显式 RENAME COLUMN 并同步列映射")
				}
				if token == "SET" && i+1 < len(tokens) && tokens[i+1] == "SCHEMA" {
					return fmt.Errorf("表所属Schema必须固定")
				}
			}
		}
		schema, name, e := qualifiedRepairIdentity(parts, obj.Schema)
		if e != nil {
			return e
		}
		if schema != obj.Schema {
			return fmt.Errorf("DDL目标Schema必须为 %s", obj.Schema)
		}
		if dbType == "sqlserver" && kind != "index" && len(parts) < 2 {
			return fmt.Errorf("SQL Server DDL必须使用Schema限定对象名")
		}
		allowed := obj.TargetName
		if kind == "table" || kind == "column" {
			if obj.Table != "" {
				allowed = obj.Table
			}
		}
		// Auxiliary generated objects (Oracle identity triggers) keep their original identity.
		_, oldParts, _, oldErr := repairHead(u.OriginalSQL)
		if oldErr == nil && len(oldParts) > 0 {
			_, oldName, oldErr := qualifiedRepairIdentity(oldParts, obj.Schema)
			if oldErr == nil {
				allowed = oldName
			}
		}
		if name != allowed && !originalAuxiliaryTarget(obj, kind, schema, name) {
			return fmt.Errorf("DDL对象名必须保持为 %s", allowed)
		}
		if kind == "index" && (strings.EqualFold(tokens[0], "CREATE") || repairIndexOn.MatchString(tail)) {
			loc := repairIndexOn.FindStringIndex(tail)
			if loc == nil {
				return fmt.Errorf("索引DDL缺少所属表")
			}
			tableParts, _, e := repairIdentifier(tail[loc[1]:])
			if e != nil {
				return e
			}
			if dbType == "sqlserver" && len(tableParts) != 2 {
				return fmt.Errorf("SQL Server索引所属表必须带Schema")
			}
			s, t, e := qualifiedRepairIdentity(tableParts, obj.Schema)
			expectedTable := obj.Table
			if expectedTable == "" && obj.Legacy {
				_, _, oldTail, parseErr := repairHead(u.OriginalSQL)
				if parseErr == nil {
					oldOn := repairIndexOn.FindStringIndex(oldTail)
					if oldOn != nil {
						oldTable, _, parseErr := repairIdentifier(oldTail[oldOn[1]:])
						if parseErr == nil {
							oldSchema, oldName, parseErr := qualifiedRepairIdentity(oldTable, obj.Schema)
							if parseErr == nil && oldSchema == obj.Schema {
								expectedTable = oldName
							}
						}
					}
				}
			}
			if e != nil || s != obj.Schema || t != expectedTable {
				return fmt.Errorf("索引所属表不能改变")
			}
		}
	}
	return nil
}

// OriginalSQL is retained by the server, never accepted from a draft request.
// Preserve strings, quoted identifiers, identifiers and operators verbatim. Only
// standalone numeric literals can change, which permits sequence starting-value
// fixes without letting a dynamic SQL string or target identity be rewritten.
func trustedRepairBlock(u repair.Unit) bool {
	if strings.TrimSpace(u.OriginalSQL) == "" || unsafeRepairComment(u.OriginalSQL) {
		return false
	}
	if strings.TrimSpace(u.SQL) == strings.TrimSpace(u.OriginalSQL) {
		return true
	}
	a, ok := repairBlockShape(u.SQL)
	b, oldOK := repairBlockShape(u.OriginalSQL)
	return ok && oldOK && a == b
}

func unsafeRepairComment(sql string) bool {
	// SHOW CREATE may retain column visibility in this narrowly scoped MySQL
	// version comment; it cannot execute another statement or change namespace.
	return repairExecutableComment.MatchString(repairVisibilityComment.ReplaceAllString(sql, ""))
}

func repairBlockShape(s string) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(s); {
		ch := s[i]
		if unicode.IsSpace(rune(ch)) {
			i++
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' || ch == '[' {
			start, end := i, ch
			if ch == '[' {
				end = ']'
			}
			i++
			closed := false
			for i < len(s) {
				if s[i] == '\\' {
					return "", false
				}
				if s[i] == end {
					i++
					if i < len(s) && s[i] == end {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return "", false
			}
			out.WriteString(s[start:i])
			out.WriteByte(' ')
			continue
		}
		if i+1 < len(s) && (s[i:i+2] == "--" || s[i:i+2] == "/*") {
			// Exact originals remain accepted; edits involving comments require
			// an explicit ordinary DDL unit instead of an ambiguous comparison.
			return "", false
		}
		if unicode.IsLetter(rune(ch)) || ch == '_' || ch == '$' || ch == '#' || ch == '@' || ch >= 128 {
			start := i
			for i < len(s) && (unicode.IsLetter(rune(s[i])) || unicode.IsDigit(rune(s[i])) || strings.ContainsRune("_$#@", rune(s[i])) || s[i] >= 128) {
				i++
			}
			out.WriteString(s[start:i])
			out.WriteByte(' ')
			continue
		}
		if ch >= '0' && ch <= '9' {
			for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
				i++
			}
			out.WriteString("<number> ")
			continue
		}
		out.WriteByte(ch)
		out.WriteByte(' ')
		i++
	}
	return out.String(), true
}

// Recognize complete literal-execution units, without splitting SQL at
// semicolons. Variable execution, concatenation, calls and unparsed control flow
// fail closed; the original generated block path above handles known templates.
func validateLiteralRepairBlock(obj repair.Object, sql, dbType string) error {
	fail := fmt.Errorf("无法证明程序块的对象范围：仅支持原生成块的数值修改，或只含常量DDL的执行块；请将其他修改写为独立对象DDL")
	s := strings.TrimSpace(sql)
	if len(s) >= 5 && strings.EqualFold(s[:5], "BEGIN") {
		end := regexp.MustCompile(`(?is)\bEND\s*;?\s*$`).FindStringIndex(s)
		if end == nil {
			return fail
		}
		s = strings.TrimSpace(s[5:end[0]])
	}
	count := 0
	for s != "" {
		var re *regexp.Regexp
		switch dbType {
		case "oracle", "dameng":
			re = repairLiteralImmediate
		case "sqlserver":
			re = repairLiteralExec
		default:
			return fail
		}
		match := re.FindStringSubmatchIndex(s)
		if match == nil {
			return fail
		}
		inner := strings.ReplaceAll(s[match[2]:match[3]], "''", "'")
		kind, _, _, e := repairHead(inner)
		if e != nil || kind == "" || kind == "trigger" {
			return fail
		}
		units := []repair.Unit{{SQL: inner, Selected: true}}
		if e = validateRepairUnits(units); e != nil {
			return e
		}
		if e = validateRepairObjectScope(obj, units, dbType); e != nil {
			return e
		}
		count++
		s = strings.TrimSpace(s[match[1]:])
	}
	if count == 0 {
		return fail
	}
	return nil
}

func originalAuxiliaryTarget(obj repair.Object, kind, schema, name string) bool {
	if obj.Kind != "sequence" || kind != "trigger" {
		return false
	}
	re := regexp.MustCompile(`(?is)CREATE\s+(?:OR\s+REPLACE\s+)?TRIGGER\s+`)
	for _, loc := range re.FindAllStringIndex(obj.DDL, -1) {
		parts, _, e := repairIdentifier(obj.DDL[loc[1]:])
		if e != nil {
			continue
		}
		s, n, e := qualifiedRepairIdentity(parts, obj.Schema)
		if e == nil && s == schema && n == name {
			return true
		}
	}
	return false
}

func conditionalRepairEffect(units []repair.Unit) bool {
	for _, u := range units {
		if !u.Selected || u.Status == "succeeded" {
			continue
		}
		tokens, _, err := scanQueryTokens(u.SQL)
		if err != nil {
			continue
		}
		for i := 0; i+2 < len(tokens); i++ {
			if tokens[i] == "IF" && tokens[i+1] == "NOT" && tokens[i+2] == "EXISTS" {
				return true
			}
		}
		if len(tokens) > 0 && tokens[0] == "IF" {
			for _, t := range tokens {
				if t == "CREATE" {
					return true
				}
			}
		}
	}
	return false
}
