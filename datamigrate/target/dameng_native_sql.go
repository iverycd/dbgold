package target

import (
	"fmt"
	"strings"
	"unicode"
)

// nativeSQLTokens retains identifiers and literals as indivisible tokens. It is
// used only to bind an immutable statement's mutating head to the target schema;
// procedural bodies remain one driver statement and are never semicolon-split.
func nativeSQLTokens(s string) ([]string, error) {
	var tokens []string
	for i := 0; i < len(s); {
		if unicode.IsSpace(rune(s[i])) {
			i++
			continue
		}
		if i+1 < len(s) && s[i:i+2] == "--" {
			end := strings.IndexByte(s[i+2:], '\n')
			if end < 0 {
				break
			}
			i += end + 3
			continue
		}
		if i+1 < len(s) && s[i:i+2] == "/*" {
			i += 2
			depth := 1
			for i < len(s) && depth > 0 {
				if strings.HasPrefix(s[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(s[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if depth != 0 {
				return nil, fmt.Errorf("SQL 注释未闭合")
			}
			continue
		}
		start := i
		q := i
		if (s[q] == 'n' || s[q] == 'N') && q+1 < len(s) && (s[q+1] == 'q' || s[q+1] == 'Q') {
			q++
		}
		if (s[q] == 'q' || s[q] == 'Q') && q+2 < len(s) && s[q+1] == '\'' {
			close := s[q+2]
			switch close {
			case '[':
				close = ']'
			case '(':
				close = ')'
			case '{':
				close = '}'
			case '<':
				close = '>'
			}
			end := strings.Index(s[q+3:], string([]byte{close, '\''}))
			if end < 0 {
				return nil, fmt.Errorf("SQL Q 字符串未闭合")
			}
			i = q + 3 + end + 2
			tokens = append(tokens, s[start:i])
			continue
		}
		if s[i] == '\'' || s[i] == '"' {
			quote := s[i]
			i++
			closed := false
			for i < len(s) {
				if s[i] == quote {
					if i+1 < len(s) && s[i+1] == quote {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, fmt.Errorf("SQL 引号未闭合")
			}
			tokens = append(tokens, s[start:i])
			continue
		}
		if strings.ContainsRune(".;(),=:+-*/<>%", rune(s[i])) {
			tokens = append(tokens, s[i:i+1])
			i++
			continue
		}
		for i < len(s) && !unicode.IsSpace(rune(s[i])) && !strings.ContainsRune(".;(),=:+-*/<>%\"'", rune(s[i])) {
			i++
		}
		if start == i {
			return nil, fmt.Errorf("无效 SQL 字符")
		}
		tokens = append(tokens, s[start:i])
	}
	return tokens, nil
}
func nativeTokenName(token string) string {
	if len(token) >= 2 && token[0] == '"' && token[len(token)-1] == '"' {
		return strings.ReplaceAll(token[1:len(token)-1], `""`, `"`)
	}
	return strings.ToUpper(token)
}

func nativeIdentifierToken(token string) bool {
	if len(token) >= 2 && token[0] == '"' && token[len(token)-1] == '"' {
		return len(nativeTokenName(token)) > 0 && !strings.ContainsRune(token, 0)
	}
	for i, r := range token {
		if !unicode.IsLetter(r) && r != '_' && r != '$' && r != '#' && !(i > 0 && unicode.IsDigit(r)) {
			return false
		}
	}
	return token != ""
}

func nativeHeadIdentity(tokens []string, index int, schema string) (int, error) {
	if index >= len(tokens) {
		return index, fmt.Errorf("原生 DDL 缺少对象名")
	}
	if !nativeIdentifierToken(tokens[index]) {
		return index, fmt.Errorf("无效原生 DDL 对象名")
	}
	if index+1 < len(tokens) && tokens[index+1] == "." {
		if nativeTokenName(tokens[index]) != schema {
			return index, fmt.Errorf("原生 DDL 不能修改目标 schema %s 以外的对象", schema)
		}
		index += 2
		if index >= len(tokens) || !nativeIdentifierToken(tokens[index]) {
			return index, fmt.Errorf("原生 DDL 缺少对象名")
		}
		if index+1 < len(tokens) && tokens[index+1] == "." {
			return index, fmt.Errorf("原生 DDL 不允许跨库对象名")
		}
	}
	return index + 1, nil
}
func validateDamengNativeSQL(schema, statement string) error {
	if schema == "" || strings.ContainsRune(schema, 0) {
		return fmt.Errorf("原生 DDL 缺少目标 schema")
	}
	tokens, err := nativeSQLTokens(statement)
	if err != nil {
		return err
	}
	if len(tokens) < 3 {
		return fmt.Errorf("原生 DDL 不完整")
	}
	upper := func(i int) string {
		if i >= len(tokens) {
			return ""
		}
		return strings.ToUpper(tokens[i])
	}
	i := 1
	kind := ""
	procedural := false
	identityChecked := false
	switch upper(0) {
	case "REFRESH":
		if upper(1) != "MATERIALIZED" || upper(2) != "VIEW" {
			return fmt.Errorf("原生基线仅允许物化视图 COMPLETE 刷新")
		}
		next, e := nativeHeadIdentity(tokens, 3, schema)
		if e != nil {
			return e
		}
		if upper(next) != "COMPLETE" {
			return fmt.Errorf("原生基线刷新必须显式使用 COMPLETE")
		}
		next++
		if next < len(tokens) && tokens[next] == ";" {
			next++
		}
		if next != len(tokens) {
			return fmt.Errorf("原生基线刷新只能包含一条 COMPLETE 语句")
		}
		return nil
	case "CREATE", "ALTER", "DROP", "TRUNCATE":
		if upper(i) == "OR" && upper(i+1) == "REPLACE" {
			i += 2
		}
		for upper(i) == "FORCE" || upper(i) == "UNIQUE" || upper(i) == "BITMAP" || upper(i) == "CLUSTERED" || upper(i) == "NONCLUSTERED" || upper(i) == "GLOBAL" || upper(i) == "LOCAL" || upper(i) == "SPATIAL" {
			i++
		}
		// DM's native index metadata spells these modifiers CLUSTER and
		// NOT CLUSTER / NOT PARTIAL. Bind their INDEX identity through the
		// same schema and ON-table checks as every other immutable index.
		if upper(0) == "CREATE" {
			if upper(i) == "NOT" && (upper(i+1) == "CLUSTER" || upper(i+1) == "PARTIAL") && upper(i+2) == "INDEX" {
				i += 2
			} else if upper(i) == "CLUSTER" && upper(i+1) == "UNIQUE" && upper(i+2) == "INDEX" {
				i += 2
			} else if upper(i) == "CLUSTER" && upper(i+1) == "INDEX" {
				i++
			}
		}
		kind = upper(i)
		i++
		if upper(0) == "TRUNCATE" && kind != "TABLE" {
			return fmt.Errorf("原生清空仅限目标表")
		}
		if kind == "MATERIALIZED" && upper(i) == "VIEW" {
			kind = "MATERIALIZED VIEW"
			i++
			if upper(i) == "LOG" && upper(i+1) == "ON" {
				i += 2
			}
		}
		if kind == "CONTEXT" && upper(i) == "INDEX" {
			kind = "INDEX"
			i++
		}
		if (kind == "PACKAGE" || kind == "TYPE" || kind == "CLASS") && upper(i) == "BODY" {
			i++
		}
		if kind == "DATABASE" && upper(i) == "LINK" {
			kind = "DATABASE LINK"
			i++
		}
		if kind == "LINK" {
			kind = "DATABASE LINK"
		}
		if kind == "JAVA" {
			if upper(i) != "SOURCE" && upper(i) != "CLASS" && upper(i) != "RESOURCE" {
				return fmt.Errorf("JAVA 原生定义缺少对象类型")
			}
			i++
			if upper(i) == "NAMED" {
				i++
			}
		}
		switch kind {
		case "TABLE", "INDEX", "VIEW", "MATERIALIZED VIEW", "SEQUENCE", "PROCEDURE", "FUNCTION", "PACKAGE", "TYPE", "CLASS", "JAVA", "TRIGGER", "SYNONYM", "DOMAIN", "CONTEXT", "DATABASE LINK":
		default:
			return fmt.Errorf("原生 DDL 对象类型不支持: %s", kind)
		}
		procedural = kind == "PROCEDURE" || kind == "FUNCTION" || kind == "PACKAGE" || kind == "TYPE" || kind == "CLASS" || kind == "JAVA" || kind == "TRIGGER"
		if upper(i) == "IF" {
			if upper(i+1) == "NOT" && upper(i+2) == "EXISTS" {
				i += 3
			} else if upper(i+1) == "EXISTS" {
				i += 2
			} else {
				return fmt.Errorf("无效原生 DDL IF 子句")
			}
		}
	case "COMMENT":
		if upper(1) != "ON" || (upper(2) != "TABLE" && upper(2) != "VIEW" && upper(2) != "COLUMN" && upper(2) != "SEQUENCE") {
			return fmt.Errorf("只允许目标表、视图、列或序列注释")
		}
		i = 3
		// A column identity has a third part after its schema/table.
		if upper(2) == "COLUMN" {
			if i+2 >= len(tokens) || tokens[i+1] != "." {
				return fmt.Errorf("列注释缺少表名")
			}
			if i+4 < len(tokens) && tokens[i+3] == "." {
				if nativeTokenName(tokens[i]) != schema {
					return fmt.Errorf("列注释目标 schema 不一致")
				}
				if i+5 < len(tokens) && tokens[i+5] == "." {
					return fmt.Errorf("列注释不能跨库")
				}
			}
			identityChecked = true
		}
	case "GRANT", "REVOKE":
		i = -1
		for j := 1; j < len(tokens); j++ {
			if upper(j) == "ON" {
				i = j + 1
				break
			}
		}
		if i < 0 {
			return fmt.Errorf("原生迁移只允许目标对象授权")
		}
	default:
		return fmt.Errorf("原生迁移只允许已绑定对象的 DDL")
	}
	if !identityChecked {
		_, err = nativeHeadIdentity(tokens, i, schema)
		if err != nil {
			return err
		}
	}
	if !procedural {
		for j, token := range tokens {
			if token == ";" && j != len(tokens)-1 {
				return fmt.Errorf("原生执行单元不能包含多条 SQL")
			}
		}
	}
	// CREATE INDEX also mutates the table named by ON. Its table must stay in
	// the same schema; referenced FK/view objects are validated by preflight.
	if kind == "INDEX" && upper(0) == "CREATE" {
		for j := i + 1; j < len(tokens); j++ {
			if upper(j) == "ON" {
				_, err = nativeHeadIdentity(tokens, j+1, schema)
				return err
			}
		}
		return fmt.Errorf("原生索引定义缺少所属表")
	}
	return nil
}
