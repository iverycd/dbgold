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
	return DefaultWithOptions(raw, Options{})
}

func DefaultWithOptions(raw string, opt Options) (string, error) {
	s := strings.TrimSpace(raw)
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		ts, err := tokenizeView(s)
		if err != nil {
			return "", err
		}
		depth, end := 0, -1
		for i, token := range ts {
			if token.text == "(" {
				depth++
			}
			if token.text == ")" {
				depth--
				if depth == 0 {
					end = i
					break
				}
			}
		}
		if end != len(ts)-1 {
			break
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if numericDefault.MatchString(s) {
		var err error
		if opt.Float {
			_, err = valueconv.OracleDamengFloat(s, "FLOAT", false, 64)
		} else {
			_, err = valueconv.OracleDameng(s, "NUMBER")
		}
		if err != nil {
			return "", err
		}
		return " DEFAULT " + s, nil
	}
	if validLiteral(s) {
		literal, err := renderOracleLiteral(s)
		if err != nil {
			return "", err
		}
		return " DEFAULT " + literal, nil
	}
	if strings.HasPrefix(strings.ToUpper(s), "CAST(") && strings.HasSuffix(s, ")") {
		return defaultCast(s, opt)
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
			literal, err := renderOracleLiteral(strings.TrimSpace(s[len(prefix):]))
			if err != nil {
				return "", err
			}
			if literal == "NULL" {
				return " DEFAULT NULL", nil
			}
			value := prefix + literal
			if prefix == "DATE " && !opt.UseDate {
				value = "CAST(" + value + " AS TIMESTAMP(0))"
			}
			return " DEFAULT " + value, nil
		}
	}
	for _, fn := range []string{"TO_DATE", "TO_TIMESTAMP", "TO_TIMESTAMP_TZ"} {
		if !strings.HasPrefix(upper, fn+"(") || !strings.HasSuffix(s, ")") {
			continue
		}
		args, ok := literalArguments(s[len(fn)+1 : len(s)-1])
		if ok && len(args) == 2 {
			for i := range args {
				var err error
				args[i], err = renderOracleLiteral(args[i])
				if err != nil {
					return "", err
				}
			}
			if fn == "TO_DATE" {
				return " DEFAULT CAST(TO_TIMESTAMP(" + strings.Join(args, ", ") + ") AS TIMESTAMP(0))", nil
			}
			return " DEFAULT " + fn + "(" + strings.Join(args, ", ") + ")", nil
		}
	}
	return "", fmt.Errorf("不支持 Oracle 默认表达式 %q（序列/自定义函数须人工适配）", raw)
}

func validLiteral(s string) bool {
	_, _, err := decodeOracleLiteral(s)
	return err == nil
}

func literalArguments(s string) ([]string, bool) {
	ts, err := tokenizeView(s)
	if err != nil || len(ts) == 0 || ts[len(ts)-1].text == "," {
		return nil, false
	}
	var result []string
	for _, span := range splitViewTokens(ts, 0, len(ts), ",") {
		if span[1]-span[0] != 1 || ts[span[0]].kind != 's' {
			return nil, false
		}
		result = append(result, ts[span[0]].text)
	}
	return result, true
}

func defaultCast(s string, opt Options) (string, error) {
	ts, err := tokenizeView(s)
	if err != nil {
		return "", err
	}
	if len(ts) < 6 || !ts[0].is("CAST") || ts[1].text != "(" || viewClosingParen(ts, 1, len(ts)) != len(ts)-1 {
		return "", fmt.Errorf("不支持 Oracle CAST 默认表达式")
	}
	as, depth := -1, 0
	for i := 2; i < len(ts)-1; i++ {
		if ts[i].text == "(" {
			depth++
		} else if ts[i].text == ")" {
			depth--
		} else if depth == 0 && ts[i].is("AS") {
			if as >= 0 {
				return "", fmt.Errorf("不支持 Oracle CAST 默认表达式")
			}
			as = i
		}
	}
	if as <= 2 || as+1 >= len(ts)-1 {
		return "", fmt.Errorf("不支持 Oracle CAST 默认表达式")
	}
	parts := make([]string, 0, len(ts)-as-2)
	for _, token := range ts[as+1 : len(ts)-1] {
		parts = append(parts, token.text)
	}
	if strings.EqualFold(strings.Join(parts, " "), "DATE") {
		return "", fmt.Errorf("CAST AS DATE 默认值的时区、格式或小数秒截断语义无法确认，请使用显式 TO_DATE 格式")
	}
	mapped, err := oracleDMCastType(strings.Join(parts, " "), opt)
	if err != nil {
		return "", err
	}
	inner, err := DefaultWithOptions(s[ts[2].start:ts[as-1].end], opt)
	if err != nil {
		return "", err
	}
	return " DEFAULT CAST(" + strings.TrimPrefix(inner, " DEFAULT ") + " AS " + mapped + ")", nil
}
