package sqlsyntax

import (
	"fmt"
	"strings"
)

// LexTSQL normalizes bracket identifiers without changing literals/comments.
func LexTSQL(raw string) ([]Token, error) {
	var b strings.Builder
	for i := 0; i < len(raw); {
		if strings.HasPrefix(raw[i:], "--") {
			j := strings.IndexByte(raw[i:], '\n')
			if j < 0 {
				b.WriteString(raw[i:])
				break
			}
			b.WriteString(raw[i : i+j+1])
			i += j + 1
			continue
		}
		if strings.HasPrefix(raw[i:], "/*") {
			start, depth := i, 1
			i += 2
			for i < len(raw) && depth > 0 {
				if strings.HasPrefix(raw[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(raw[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if depth != 0 {
				return nil, fmt.Errorf("未闭合注释")
			}
			b.WriteString(raw[start:i])
			continue
		}
		if raw[i] == '\'' || raw[i] == '"' {
			q := raw[i]
			b.WriteByte(q)
			i++
			closed := false
			for i < len(raw) {
				c := raw[i]
				b.WriteByte(c)
				i++
				if c == q {
					if i < len(raw) && raw[i] == q {
						b.WriteByte(q)
						i++
						continue
					}
					closed = true
					break
				}
			}
			if !closed {
				return nil, fmt.Errorf("未闭合引号")
			}
			continue
		}
		if raw[i] == '[' {
			i++
			var name strings.Builder
			closed := false
			for i < len(raw) {
				c := raw[i]
				i++
				if c == ']' {
					if i < len(raw) && raw[i] == ']' {
						name.WriteByte(']')
						i++
						continue
					}
					closed = true
					break
				}
				name.WriteByte(c)
			}
			if !closed {
				return nil, fmt.Errorf("未闭合标识符")
			}
			b.WriteString(`"` + strings.ReplaceAll(name.String(), `"`, `""`) + `"`)
			continue
		}
		b.WriteByte(raw[i])
		i++
	}
	return Lex(b.String())
}
