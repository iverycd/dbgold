package mysqltrigger

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type token struct {
	raw, word  string
	start, end int
	ident      bool
}

// lex only finds program boundaries. SQL expressions and DML are parsed again
// by TiDB, with the trigger's saved SQL mode; strings are never rewritten.
func lex(s string, ansi, noBackslash bool) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		start := i
		if s[i] == 0 {
			return nil, fmt.Errorf("触发器包含 NUL 字符")
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return nil, fmt.Errorf("触发器包含无效 UTF-8")
		}
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		if s[i] == '#' || (i+2 < len(s) && s[i:i+2] == "--" && s[i+2] <= ' ') {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < len(s) && s[i:i+2] == "/*" {
			if i+2 < len(s) && (s[i+2] == '!' || s[i+2] == '+') {
				return nil, fmt.Errorf("不支持可执行注释或优化提示")
			}
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("注释未闭合")
			}
			i += end + 4
			continue
		}
		if s[i] == '\'' || s[i] == '"' || s[i] == '`' {
			q := s[i]
			ident := q == '`' || (q == '"' && ansi)
			i++
			closed := false
			for i < len(s) {
				if s[i] == '\\' && !ident && !noBackslash {
					i += 2
					continue
				}
				if s[i] == q {
					if i+1 < len(s) && s[i+1] == q {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed || i > len(s) {
				return nil, fmt.Errorf("引号未闭合")
			}
			out = append(out, token{raw: s[start:i], start: start, end: i, ident: ident})
			continue
		}
		if unicode.IsLetter(r) || r == '_' || r == '$' {
			i += size
			for i < len(s) {
				r, size = utf8.DecodeRuneInString(s[i:])
				if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' {
					break
				}
				i += size
			}
			out = append(out, token{raw: s[start:i], word: strings.ToUpper(s[start:i]), start: start, end: i})
			continue
		}
		i += size
		out = append(out, token{raw: s[start:i], start: start, end: i})
	}
	return out, nil
}

func identifier(t token) (string, error) {
	if t.word != "" {
		return t.raw, nil
	}
	if t.ident && len(t.raw) >= 2 {
		q := t.raw[:1]
		return strings.ReplaceAll(t.raw[1:len(t.raw)-1], q+q, q), nil
	}
	return "", fmt.Errorf("需要列名，遇到 %s", t.raw)
}
