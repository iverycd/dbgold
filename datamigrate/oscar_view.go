package datamigrate

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type viewToken struct {
	text string
	name string // decoded identifier; empty for non-identifiers
	kind byte   // i: identifier, q: quoted identifier, s: literal, w: trivia, p: punctuation
}

func viewIdentStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }
func viewIdentPart(r rune) bool {
	return viewIdentStart(r) || unicode.IsDigit(r) || r == '$' || r == '#'
}

// lexOscarView preserves every byte of literals, comments and whitespace. It is
// intentionally a lexer, not a cross-database expression translator.
func lexOscarView(sql string) ([]viewToken, error) {
	var tokens []viewToken
	for i := 0; i < len(sql); {
		start := i
		kind := byte('p')
		name := ""
		r, size := utf8.DecodeRuneInString(sql[i:])
		arrayBracket := false
		if r == '[' && len(tokens) > 0 {
			previous := tokens[len(tokens)-1]
			// Adjacent expression[index], ARRAY[...] and empty type[] suffixes.
			arrayBracket = previous.kind == 'q' || previous.text == ")" || previous.text == "]" || previous.kind == 'i' && (!oscarViewKeywords[strings.ToUpper(previous.name)] || strings.EqualFold(previous.name, "ARRAY"))
		}
		switch {
		case unicode.IsSpace(r):
			i += size
			kind = 'w'
		case strings.HasPrefix(sql[i:], "--"):
			i += 2
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			kind = 'w'
		case strings.HasPrefix(sql[i:], "/*"):
			i += 2
			depth := 1
			for i < len(sql) && depth > 0 {
				if strings.HasPrefix(sql[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(sql[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if depth != 0 {
				return nil, fmt.Errorf("Oscar 视图包含未闭合的注释")
			}
			kind = 'w'
		case (r == 'q' || r == 'Q') && i+2 < len(sql) && sql[i+1] == '\'':
			end := sql[i+2]
			switch end {
			case '[':
				end = ']'
			case '(':
				end = ')'
			case '{':
				end = '}'
			case '<':
				end = '>'
			}
			j := strings.Index(sql[i+3:], string([]byte{end, '\''}))
			if j < 0 {
				return nil, fmt.Errorf("Oscar 视图包含未闭合的 q 字符串")
			}
			i += 3 + j + 2
			kind = 's'
		case r == '\'' || (strings.ContainsRune("eEnNxXbB", r) && i+1 < len(sql) && sql[i+1] == '\''):
			escaped := r == 'e' || r == 'E'
			if r != '\'' {
				i++
			}
			i++
			closed := false
			for i < len(sql) {
				if escaped && sql[i] == '\\' && i+1 < len(sql) {
					i += 2
					continue
				}
				if sql[i] == '\'' {
					i++
					if i < len(sql) && sql[i] == '\'' {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, fmt.Errorf("Oscar 视图包含未闭合的字符串")
			}
			kind = 's'
		case r == '$':
			j := i + 1
			for j < len(sql) && (sql[j] == '_' || sql[j] >= 'a' && sql[j] <= 'z' || sql[j] >= 'A' && sql[j] <= 'Z' || sql[j] >= '0' && sql[j] <= '9') {
				j++
			}
			if j >= len(sql) || sql[j] != '$' {
				return nil, fmt.Errorf("Oscar 视图包含不支持的 $ 表达式")
			}
			delimiter := sql[i : j+1]
			end := strings.Index(sql[j+1:], delimiter)
			if end < 0 {
				return nil, fmt.Errorf("Oscar 视图包含未闭合的 dollar 字符串")
			}
			i = j + 1 + end + len(delimiter)
			kind = 's'
		case r == '"' || r == '`' || r == '[' && !arrayBracket:
			end := byte(r)
			if r == '[' {
				end = ']'
			}
			i++
			var decoded strings.Builder
			closed := false
			for i < len(sql) {
				if sql[i] == end {
					i++
					if i < len(sql) && sql[i] == end {
						decoded.WriteByte(end)
						i++
						continue
					}
					closed = true
					break
				}
				decoded.WriteByte(sql[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("Oscar 视图包含未闭合的标识符")
			}
			kind, name = 'q', decoded.String()
		case r >= '0' && r <= '9':
			i++
			if r == '0' && i < len(sql) && (sql[i] == 'x' || sql[i] == 'X' || sql[i] == 'b' || sql[i] == 'B') {
				i++
				for i < len(sql) && (sql[i] >= '0' && sql[i] <= '9' || sql[i] >= 'a' && sql[i] <= 'f' || sql[i] >= 'A' && sql[i] <= 'F') {
					i++
				}
				break
			}
			for i < len(sql) && (sql[i] >= '0' && sql[i] <= '9' || sql[i] == '.') {
				i++
			}
			if i < len(sql) && (sql[i] == 'e' || sql[i] == 'E') {
				i++
				if i < len(sql) && (sql[i] == '+' || sql[i] == '-') {
					i++
				}
				for i < len(sql) && sql[i] >= '0' && sql[i] <= '9' {
					i++
				}
			}
		case viewIdentStart(r):
			i += size
			for i < len(sql) {
				r, size = utf8.DecodeRuneInString(sql[i:])
				if !viewIdentPart(r) {
					break
				}
				i += size
			}
			kind, name = 'i', sql[start:i]
		default:
			i += size
			if r == ':' && i < len(sql) && sql[i] == ':' {
				i++
			}
		}
		tokens = append(tokens, viewToken{text: sql[start:i], name: name, kind: kind})
	}
	return tokens, nil
}

var oscarViewKeywords = func() map[string]bool {
	m := make(map[string]bool)
	for _, s := range strings.Fields(`SELECT FROM WHERE AS DISTINCT ALL ON JOIN INNER LEFT RIGHT FULL OUTER CROSS NATURAL LATERAL ONLY USING AND OR NOT IS NULL TRUE FALSE UNKNOWN IN EXISTS BETWEEN LIKE ILIKE ESCAPE CASE WHEN THEN ELSE END GROUP BY HAVING ORDER ASC DESC NULLS FIRST LAST LIMIT OFFSET FETCH NEXT ROW ROWS ONLY UNION INTERSECT EXCEPT MINUS WITH RECURSIVE MATERIALIZED OVER PARTITION RANGE GROUPS UNBOUNDED PRECEDING FOLLOWING CURRENT ROW EXCLUDE TIES FILTER WITHIN WINDOW COLLATE AT TIME ZONE FOR UPDATE OF SHARE NOWAIT SKIP LOCKED ANY SOME BOTH LEADING TRAILING INTERVAL CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP LOCALTIME LOCALTIMESTAMP CURRENT_USER SESSION_USER SYSTEM_USER USER CURRENT_SCHEMA CAST EXTRACT POSITION SUBSTRING TRIM ARRAY VARIADIC`) {
		m[s] = true
	}
	return m
}()

// normalizeOscarView quotes uppercased relation/column/alias identifiers. SQL
// keywords, function names, cast types and external schema names are retained.
// stripSchemas applies only to actual qualified references, never string text.
func normalizeOscarView(sql string, stripSchemas []string) (string, error) {
	tokens, err := lexOscarView(sql)
	if err != nil {
		return sql, err
	}
	var sig []int
	for i, t := range tokens {
		if t.kind != 'w' {
			sig = append(sig, i)
		}
	}
	text := func(i int) string {
		if i < 0 || i >= len(sig) {
			return ""
		}
		return tokens[sig[i]].text
	}
	word := func(i int) string {
		if i < 0 || i >= len(sig) || tokens[sig[i]].kind != 'i' {
			return ""
		}
		return strings.ToUpper(tokens[sig[i]].name)
	}
	ident := func(i int) bool {
		return i >= 0 && i < len(sig) && (tokens[sig[i]].kind == 'i' || tokens[sig[i]].kind == 'q')
	}
	// Pair parentheses once, so CAST type clauses and CTE column lists are not
	// mistaken for aliases/functions. Malformed SQL fails before CREATE VIEW.
	closeAt := make(map[int]int)
	expressionFrom := make(map[int]bool)
	var stack []int
	for i := range sig {
		if word(i) == "FROM" && len(stack) > 0 {
			switch word(stack[len(stack)-1] - 1) {
			case "EXTRACT", "SUBSTRING", "TRIM", "OVERLAY":
				expressionFrom[i] = true
			}
		}
		if text(i) == "(" {
			stack = append(stack, i)
		}
		if text(i) == ")" {
			if len(stack) == 0 {
				return sql, fmt.Errorf("Oscar 视图括号不匹配")
			}
			open := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			closeAt[open] = i
		}
	}
	if len(stack) != 0 {
		return sql, fmt.Errorf("Oscar 视图括号不匹配")
	}
	preserve := make(map[int]bool)
	for i := range sig {
		if word(i) == "EXTRACT" && text(i+1) == "(" {
			for j := i + 2; j < closeAt[i+1] && word(j) != "FROM"; j++ {
				preserve[j] = true
			}
		}
		if word(i) == "INTERVAL" && i+1 < len(sig) && tokens[sig[i+1]].kind == 's' {
			for j := i + 2; j < len(sig); j++ {
				if word(j) == "" || !strings.Contains("|YEAR|MONTH|DAY|HOUR|MINUTE|SECOND|TO|", "|"+word(j)+"|") {
					break
				}
				preserve[j] = true
			}
		}
		if word(i) == "COLLATE" {
			for j := i + 1; ident(j); j += 2 {
				preserve[j] = true
				if text(j+1) != "." {
					break
				}
			}
		}
		if word(i) == "CAST" && text(i+1) == "(" {
			end := closeAt[i+1]
			for j := i + 2; j < end; j++ {
				if text(j) == "(" {
					j = closeAt[j]
					continue
				}
				if word(j) == "AS" {
					for k := j + 1; k < end; k++ {
						preserve[k] = true
					}
					break
				}
			}
		}
		if text(i) == "::" {
			j := i + 1
			for ident(j) {
				preserve[j] = true
				if text(j+1) != "." {
					break
				}
				j += 2
			}
			if text(j+1) == "(" {
				j = closeAt[j+1]
			}
			// Multiword built-in types after ::, optionally after precision.
			for k := j + 1; k < len(sig); k++ {
				if !strings.Contains("|PRECISION|VARYING|WITH|WITHOUT|TIME|ZONE|", "|"+word(k)+"|") || word(k) == "" {
					break
				}
				preserve[k] = true
			}
		}
	}
	from := false
	var fromStack []bool
	for i := 0; i < len(sig); i++ {
		t := &tokens[sig[i]]
		if t.text == "(" {
			fromStack = append(fromStack, from)
			from = false // commas inside functions/expressions are not FROM items
			continue
		}
		if t.text == ")" {
			from = fromStack[len(fromStack)-1]
			fromStack = fromStack[:len(fromStack)-1]
			continue
		}
		switch word(i) {
		case "SELECT", "WHERE", "GROUP", "HAVING", "ORDER", "UNION", "LIMIT", "OFFSET":
			from = false
		case "FROM", "JOIN":
			if !expressionFrom[i] {
				from = true
			}
		}
		if !ident(i) || preserve[i] {
			continue
		}
		// Process dotted chains together, preserving only their schema prefix.
		if text(i-1) == "." {
			continue
		}
		end := i
		for text(end+1) == "." && ident(end+2) {
			end += 2
		}
		function := text(end+1) == "("
		if function && from && (word(i-1) == "AS" || text(i-1) == ")" || ident(i-1) && !oscarViewKeywords[word(i-1)]) {
			function = false // FROM item alias(column, ...) is not a function
		}
		if function && word(closeAt[end+1]+1) == "AS" && (text(closeAt[end+1]+2) == "(" || word(closeAt[end+1]+2) == "MATERIALIZED" || word(closeAt[end+1]+2) == "NOT") {
			function = false
		} // CTE name(columns) AS (...)
		if function {
			continue
		}
		if t.kind == 'i' && oscarViewKeywords[word(i)] && end == i {
			continue
		}
		// Typed literals, e.g. DATE '2026-01-01', keep their type spelling.
		if end+1 < len(sig) && tokens[sig[end+1]].kind == 's' {
			continue
		}
		relation := word(i-1) == "FROM" && !expressionFrom[i-1] || word(i-1) == "JOIN" || word(i-1) == "ONLY" || word(i-1) == "LATERAL" || text(i-1) == "," && from
		schemaEnd := i - 2
		if end-i >= 4 {
			schemaEnd = end - 4
		} else if end > i && (relation || text(end+1) == "." && text(end+2) == "*") {
			schemaEnd = end - 2
		}
		for j := i; j <= end; j += 2 {
			part := &tokens[sig[j]]
			if j <= schemaEnd {
				for _, schema := range stripSchemas {
					if strings.EqualFold(strings.TrimSpace(schema), part.name) {
						part.text = ""
						tokens[sig[j+1]].text = ""
						break
					}
				}
				continue
			}
			part.text = `"` + strings.ReplaceAll(strings.ToUpper(part.name), `"`, `""`) + `"`
		}
	}
	var out strings.Builder
	for _, t := range tokens {
		out.WriteString(t.text)
	}
	return out.String(), nil
}
