// Package tsqldameng implements the explicitly supported, lossless T-SQL subset.
package tsqldameng

import (
	"fmt"
	"regexp"
	"strings"
)

var number = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// Default keeps quoted literals distinct from functions and keywords.
func Default(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	for strings.HasPrefix(s, "(") {
		end, err := outerEnd(s)
		if err != nil {
			return "", err
		}
		if end != len(s)-1 {
			break
		}
		s = strings.TrimSpace(s[1:end])
	}
	if len(s) > 1 && (s[0] == 'N' || s[0] == 'n') && s[1] == '\'' {
		s = s[1:]
	}
	if strings.HasPrefix(s, "'") {
		end, err := quotedEnd(s, 0, '\'')
		if err != nil || end != len(s)-1 {
			return "", fmt.Errorf("不支持默认表达式 %q", raw)
		}
		return " DEFAULT " + s, nil
	}
	if number.MatchString(s) {
		return " DEFAULT " + s, nil
	}
	switch strings.ToUpper(strings.ReplaceAll(s, " ", "")) {
	case "NULL":
		return " DEFAULT NULL", nil
	case "CURRENT_TIMESTAMP", "GETDATE()", "SYSDATETIME()":
		return " DEFAULT LOCALTIMESTAMP", nil
	case "CURRENT_DATE":
		return " DEFAULT CURRENT_DATE", nil
	case "CURRENT_TIME":
		return " DEFAULT CURRENT_TIME", nil
	}
	return "", fmt.Errorf("不支持默认表达式 %q", raw)
}

func quotedEnd(s string, start int, quote byte) (int, error) {
	for i := start + 1; i < len(s); i++ {
		if s[i] == quote {
			if i+1 < len(s) && s[i+1] == quote {
				i++
				continue
			}
			return i, nil
		}
	}
	return 0, fmt.Errorf("未闭合的 SQL 引号")
}
func outerEnd(s string) (int, error) {
	depth := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			end, err := quotedEnd(s, i, '\'')
			if err != nil {
				return 0, err
			}
			i = end
			continue
		}
		if s[i] == '(' {
			depth++
		}
		if s[i] == ')' {
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("未闭合的 SQL 括号")
}
