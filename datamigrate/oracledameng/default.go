// Package oracledameng implements the explicitly supported Oracle-to-DM SQL subset.
package oracledameng

import (
	"fmt"
	"regexp"
	"strings"

	"dbgold/datamigrate/valueconv"
)

var numericDefault = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// Default returns a complete DEFAULT clause, preserving SQL literals as literals.
// Sequence defaults are deliberately rejected: DM8 version support differs and
// NULL-only triggers cannot preserve Oracle's omitted-versus-explicit-NULL distinction.
func Default(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		depth, end, quoted := 0, -1, false
		for i := 0; i < len(s); i++ {
			if s[i] == '\'' {
				if quoted && i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				quoted = !quoted
				continue
			}
			if quoted {
				continue
			}
			if s[i] == '(' {
				depth++
			}
			if s[i] == ')' {
				depth--
				if depth == 0 {
					end = i
					break
				}
			}
		}
		if end != len(s)-1 {
			break
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if numericDefault.MatchString(s) {
		if _, err := valueconv.OracleDameng(s, "NUMBER"); err != nil {
			return "", err
		}
		return " DEFAULT " + s, nil
	}
	if validLiteral(s) {
		return " DEFAULT " + s, nil
	}
	switch strings.ToUpper(s) {
	case "NULL":
		return " DEFAULT NULL", nil
	case "SYSDATE", "CURRENT_DATE":
		return " DEFAULT LOCALTIMESTAMP(0)", nil
	case "SYSTIMESTAMP", "CURRENT_TIMESTAMP":
		return " DEFAULT CURRENT_TIMESTAMP", nil
	case "LOCALTIMESTAMP":
		return " DEFAULT LOCALTIMESTAMP", nil
	}
	upper := strings.ToUpper(s)
	for _, prefix := range []string{"DATE ", "TIMESTAMP "} {
		if strings.HasPrefix(upper, prefix) && validLiteral(strings.TrimSpace(s[len(prefix):])) {
			return " DEFAULT " + s, nil
		}
	}
	for _, fn := range []string{"TO_DATE", "TO_TIMESTAMP", "TO_TIMESTAMP_TZ"} {
		if !strings.HasPrefix(upper, fn+"(") || !strings.HasSuffix(s, ")") {
			continue
		}
		args, ok := literalArguments(s[len(fn)+1 : len(s)-1])
		if ok && len(args) == 2 {
			if fn == "TO_DATE" {
				fn = "TO_TIMESTAMP"
			}
			return " DEFAULT " + fn + "(" + strings.Join(args, ", ") + ")", nil
		}
	}
	return "", fmt.Errorf("不支持 Oracle 默认表达式 %q（序列/自定义函数须人工适配）", raw)
}

func validLiteral(s string) bool {
	if len(s) > 1 && (s[0] == 'N' || s[0] == 'n') {
		s = s[1:]
	}
	if len(s) < 2 || s[0] != '\'' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] == '\'' {
			if i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			return i == len(s)-1
		}
	}
	return false
}

func literalArguments(s string) ([]string, bool) {
	var result []string
	start := 0
	quoted := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			if quoted && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			quoted = !quoted
		}
		if !quoted && s[i] == ',' {
			result = append(result, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	result = append(result, strings.TrimSpace(s[start:]))
	if quoted {
		return nil, false
	}
	for _, a := range result {
		if !validLiteral(a) {
			return nil, false
		}
	}
	return result, true
}
