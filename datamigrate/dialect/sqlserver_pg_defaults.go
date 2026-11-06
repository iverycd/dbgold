package dialect

import (
	"fmt"
	"strings"
)

// sqlServerPGDefault removes only parentheses enclosing the complete expression.
// A complete SQL string literal keeps its quotes; callers must not quote it again
// or classify its contents as a function/keyword. Other target dialects retain
// their own default conversion paths.
func sqlServerPGDefault(raw string) (string, bool, error) {
	def := strings.TrimSpace(raw)
	for strings.HasPrefix(def, "(") {
		end, err := sqlServerPGDefaultParenEnd(def)
		if err != nil {
			return "", false, err
		}
		if end != len(def)-1 {
			break
		}
		def = strings.TrimSpace(def[1:end])
	}
	if len(def) > 1 && (def[0] == 'N' || def[0] == 'n') && def[1] == '\'' {
		def = def[1:]
	}
	if strings.HasPrefix(def, "'") {
		end, err := sqlServerPGDefaultQuoteEnd(def, 0, '\'')
		if err != nil {
			return "", false, err
		}
		return def, end == len(def)-1, nil
	}
	return def, false, nil
}

func sqlServerPGDefaultQuoteEnd(s string, start int, quote byte) (int, error) {
	for i := start + 1; i < len(s); i++ {
		if s[i] != quote {
			continue
		}
		if i+1 < len(s) && s[i+1] == quote {
			i++
			continue
		}
		return i, nil
	}
	return 0, fmt.Errorf("SQL Server 默认值包含未闭合的引号")
}

func sqlServerPGDefaultParenEnd(s string) (int, error) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'', '"', '[':
			quote := s[i]
			if quote == '[' {
				quote = ']'
			}
			end, err := sqlServerPGDefaultQuoteEnd(s, i, quote)
			if err != nil {
				return 0, err
			}
			i = end
		case '-':
			if i+1 < len(s) && s[i+1] == '-' {
				end := strings.IndexByte(s[i+2:], '\n')
				if end < 0 {
					return 0, fmt.Errorf("SQL Server 默认值包含未闭合的括号")
				}
				i += end + 2
			}
		case '/':
			if i+1 < len(s) && s[i+1] == '*' {
				commentDepth := 1
				i += 2
				for ; i < len(s) && commentDepth > 0; i++ {
					if strings.HasPrefix(s[i:], "/*") {
						commentDepth++
						i++
					} else if strings.HasPrefix(s[i:], "*/") {
						commentDepth--
						i++
					}
				}
				if commentDepth != 0 {
					return 0, fmt.Errorf("SQL Server 默认值包含未闭合的注释")
				}
				i--
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("SQL Server 默认值包含未闭合的括号")
}
