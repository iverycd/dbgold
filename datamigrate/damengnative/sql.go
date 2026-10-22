package damengnative

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type sqlToken struct {
	start, end int
	kind       byte
	value      string
}

// Tokens retain byte offsets: transformations splice the original source rather
// than pretty-printing it. In particular, strings and PL/SQL bodies keep their
// original whitespace, case, quoting and semicolons.
func nativeTokens(s string) ([]sqlToken, error) {
	var tokens []sqlToken
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if unicode.IsSpace(r) {
			i += n
			continue
		}
		start := i
		if strings.HasPrefix(s[i:], "--") {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(s[i:], "/*") {
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
				return nil, fmt.Errorf("原生 DDL 包含未闭合注释")
			}
			continue
		}
		// Oracle-compatible alternative quoted strings, including NQ'[...]'.
		q := i
		if (s[q] == 'n' || s[q] == 'N') && q+1 < len(s) && (s[q+1] == 'q' || s[q+1] == 'Q') {
			q++
		}
		if (s[q] == 'q' || s[q] == 'Q') && q+2 < len(s) && s[q+1] == '\'' {
			open := s[q+2]
			close := open
			switch open {
			case '[':
				close = ']'
			case '(':
				close = ')'
			case '{':
				close = '}'
			case '<':
				close = '>'
			}
			i = q + 3
			end := strings.Index(s[i:], string([]byte{close, '\''}))
			if end < 0 {
				return nil, fmt.Errorf("原生 DDL 包含未闭合 Q 字符串")
			}
			i += end + 2
			tokens = append(tokens, sqlToken{start, i, 's', s[q+3 : i-2]})
			continue
		}
		if r == '\'' || r == '"' {
			quote := byte(r)
			i++
			var b strings.Builder
			closed := false
			for i < len(s) {
				if s[i] == quote {
					i++
					if i < len(s) && s[i] == quote {
						b.WriteByte(quote)
						i++
						continue
					}
					closed = true
					break
				}
				b.WriteByte(s[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("原生 DDL 包含未闭合引号")
			}
			kind := byte('s')
			if quote == '"' {
				kind = 'q'
			}
			tokens = append(tokens, sqlToken{start, i, kind, b.String()})
			continue
		}
		if unicode.IsLetter(r) || r == '_' || r == '$' || r == '#' {
			i += n
			for i < len(s) {
				c, z := utf8.DecodeRuneInString(s[i:])
				if !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_' && c != '$' && c != '#' {
					break
				}
				i += z
			}
			tokens = append(tokens, sqlToken{start, i, 'i', s[start:i]})
			continue
		}
		i += n
		tokens = append(tokens, sqlToken{start, i, 'p', s[start:i]})
	}
	return tokens, nil
}

// RewriteSchema maps qualified identifier tokens only. Literal text (including
// dynamic SQL) is never rewritten; potentially affected strings are reported.
func RewriteSchema(sql, source, target string) (string, []string, error) {
	tokens, err := nativeTokens(sql)
	if err != nil {
		return "", nil, err
	}
	if source == target {
		return StripClientDelimiter(sql), nil, nil
	}
	if shadowedSchema(tokens, source) {
		return "", nil, fmt.Errorf("Schema %s 同时被用作别名或局部变量，无法安全自动映射，请调整定义或使用同名目标模式", source)
	}
	var out strings.Builder
	last := 0
	var warnings []string
	for i, t := range tokens {
		if t.kind == 's' {
			// The literal remains untouched even if the apparent reference occurs
			// in executable dynamic SQL. It is a review item, never guessed SQL.
			v := strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return -1
				}
				return unicode.ToUpper(r)
			}, t.value)
			if strings.EqualFold(strings.TrimSpace(t.value), source) || strings.Contains(v, strings.ToUpper(source)+".") || strings.Contains(v, strings.ToUpper(Quote(source))+".") {
				warnings = append(warnings, "字符串/动态 SQL 含源 Schema 引用，已保留原文，请人工核对")
			}
			continue
		}
		matches := t.kind == 'q' && t.value == source || t.kind == 'i' && strings.ToUpper(t.value) == source
		if !matches || i+2 >= len(tokens) || tokens[i+1].value != "." || (tokens[i+2].kind != 'q' && tokens[i+2].kind != 'i') {
			continue
		}
		out.WriteString(sql[last:t.start])
		out.WriteString(Quote(target))
		last = t.end
	}
	out.WriteString(sql[last:])
	return StripClientDelimiter(out.String()), warnings, nil
}

func shadowedSchema(tokens []sqlToken, source string) bool {
	match := func(t sqlToken) bool {
		return t.kind == 'q' && t.value == source || t.kind == 'i' && strings.ToUpper(t.value) == source
	}
	is := func(i int, value string) bool {
		return i >= 0 && i < len(tokens) && tokens[i].kind == 'i' && strings.EqualFold(tokens[i].value, value)
	}
	hasReference := false
	for i, t := range tokens {
		if match(t) && i+1 < len(tokens) && tokens[i+1].value == "." {
			hasReference = true
			break
		}
	}
	if !hasReference {
		return false
	}
	for i, t := range tokens {
		if match(t) {
			if is(i-1, "AS") || is(i+1, "IN") || is(i+1, "OUT") || is(i+1, "INOUT") || is(i+1, "CONSTANT") || is(i+1, "AS") && (is(i-1, "WITH") || i > 0 && tokens[i-1].value == ",") {
				return true
			}
			if i+1 < len(tokens) && tokens[i+1].kind == 'i' && (is(i-1, "DECLARE") || is(i-1, "IS") || i > 0 && tokens[i-1].value == ";") {
				return true
			}
		}
		if t.kind != 'i' || !strings.EqualFold(t.value, "FROM") && !strings.EqualFold(t.value, "JOIN") {
			continue
		}
		j := i + 1
		if j >= len(tokens) {
			continue
		}
		if tokens[j].value == "(" {
			depth := 1
			j++
			for j < len(tokens) && depth > 0 {
				if tokens[j].value == "(" {
					depth++
				}
				if tokens[j].value == ")" {
					depth--
				}
				j++
			}
		} else {
			if tokens[j].kind != 'i' && tokens[j].kind != 'q' {
				continue
			}
			j++
			for j+1 < len(tokens) && tokens[j].value == "." && (tokens[j+1].kind == 'i' || tokens[j+1].kind == 'q') {
				j += 2
			}
		}
		if is(j, "AS") {
			j++
		}
		if j < len(tokens) && match(tokens[j]) {
			return true
		}
	}
	return false
}

func StripClientDelimiter(sql string) string {
	sql = strings.TrimSpace(sql)
	lines := strings.Split(sql, "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "/" {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// Order is deterministic and puts acyclic dependencies first. Remaining SCCs
// retain a declaration-before-body order; the runner retries failed units until
// no progress can be made, then reports the cycle instead of looping forever.
func Order(objects []Object) []Object {
	byID := map[string]Object{}
	remaining := map[string]bool{}
	for _, o := range objects {
		byID[o.ID] = o
		remaining[o.ID] = true
	}
	var out []Object
	for len(remaining) > 0 {
		var ready []Object
		for id := range remaining {
			o := byID[id]
			blocked := false
			for _, dep := range o.Dependencies {
				if remaining[dep] {
					blocked = true
					break
				}
			}
			if !blocked {
				ready = append(ready, o)
			}
		}
		if len(ready) == 0 {
			for id := range remaining {
				ready = append(ready, byID[id])
			}
		}
		sort.Slice(ready, func(i, j int) bool {
			a, b := Rank(ready[i].Kind), Rank(ready[j].Kind)
			if a != b {
				return a < b
			}
			if ready[i].Name != ready[j].Name {
				return ready[i].Name < ready[j].Name
			}
			return ready[i].ID < ready[j].ID
		})
		for _, o := range ready {
			out = append(out, o)
			delete(remaining, o.ID)
		}
	}
	return out
}

func Rank(kind string) int {
	switch kind {
	case "domain", "type", "class", "class_head", "type_spec":
		return 0
	case "sequence", "package", "package_spec":
		return 1
	case "table":
		return 2
	case "primary_key", "unique", "check", "constraint", "index", "context_index":
		return 3
	case "foreign_key":
		return 4
	case "function", "procedure", "package_body", "class_body", "type_body":
		return 5
	case "view", "synonym", "db_link":
		return 6
	case "materialized_view", "materialized_view_log":
		return 7
	case "trigger":
		return 8
	case "comment", "grant", "object_grant":
		return 9
	}
	return 10
}
