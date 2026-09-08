// Package sqlsyntax implements a deliberately small SQL grammar. It is not a
// best-effort SQL rewriter: unconsumed tokens always produce an error.
package sqlsyntax

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Token struct {
	Kind  byte
	Text  string
	Value string
}

func (t Token) Is(s string) bool {
	return t.Kind != 's' && t.Kind != 'q' && strings.EqualFold(t.Text, s)
}
func Lex(s string) ([]Token, error) {
	var out []Token
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if unicode.IsSpace(r) {
			i += n
			continue
		}
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
				return nil, fmt.Errorf("未闭合注释")
			}
			continue
		}
		start := i
		escaped := (r == 'e' || r == 'E') && i+1 < len(s) && s[i+1] == '\''
		if r == '\'' || r == '"' || r == '`' || escaped {
			if escaped {
				i++
				r = '\''
			}
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
				if escaped && s[i] == '\\' {
					i++
					if i >= len(s) {
						return nil, fmt.Errorf("未闭合转义")
					}
					switch s[i] {
					case 'n':
						b.WriteByte('\n')
					case 'r':
						b.WriteByte('\r')
					case 't':
						b.WriteByte('\t')
					case 'b':
						b.WriteByte('\b')
					case 'f':
						b.WriteByte('\f')
					case '\\', '\'':
						b.WriteByte(s[i])
					default:
						return nil, fmt.Errorf("不支持的 E 字符串转义")
					}
					i++
					continue
				}
				b.WriteByte(s[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("未闭合引号")
			}
			k := byte('s')
			if quote == '"' || quote == '`' {
				k = 'q'
			}
			out = append(out, Token{k, s[start:i], b.String()})
			continue
		}
		if unicode.IsLetter(r) || r == '_' {
			i += n
			for i < len(s) {
				c, z := utf8.DecodeRuneInString(s[i:])
				if !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_' && c != '$' {
					break
				}
				i += z
			}
			v := s[start:i]
			out = append(out, Token{'i', v, strings.ToLower(v)})
			continue
		}
		if r >= '0' && r <= '9' || r == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
			i++
			for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
				i++
			}
			if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
				i++
				if i < len(s) && (s[i] == '+' || s[i] == '-') {
					i++
				}
				for i < len(s) && s[i] >= '0' && s[i] <= '9' {
					i++
				}
			}
			v := s[start:i]
			if _, e := strconv.ParseFloat(v, 64); e != nil {
				return nil, fmt.Errorf("无效数字 %s", v)
			}
			out = append(out, Token{'n', v, v})
			continue
		}
		op := s[i : i+n]
		if i+1 < len(s) {
			switch s[i : i+2] {
			case "::", "<=", ">=", "<>", "!=":
				op = s[i : i+2]
			}
		}
		if !strings.Contains("(),.=<>!+-*;:", string(r)) {
			return nil, fmt.Errorf("不支持的 SQL 符号 %q", r)
		}
		i += len(op)
		out = append(out, Token{'p', op, op})
	}
	return out, nil
}
func QuoteString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" } // requires NO_BACKSLASH_ESCAPES session
func QuoteIdent(s string) string  { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// Expr supports comparisons, boolean logic, constants, and column references.
// Casts are accepted only for literals; they are checked, never stripped blind.
type Expr struct {
	Tokens  []Token
	Pos     int
	Resolve func([]string) (string, error)
	// TypedLiteral is only enabled when the caller subsequently validates and
	// converts the literal using its result type (defaults/view projections).
	TypedLiteral bool
}

func (p *Expr) take(s string) bool {
	if p.Pos < len(p.Tokens) && p.Tokens[p.Pos].Is(s) {
		p.Pos++
		return true
	}
	return false
}
func (p *Expr) Parse() (string, error) {
	s, e := p.binary(0)
	if e == nil && p.Pos != len(p.Tokens) {
		e = fmt.Errorf("不支持的表达式 %s", p.Tokens[p.Pos].Text)
	}
	return s, e
}
func precedence(t Token) int {
	switch {
	case t.Is("OR"):
		return 1
	case t.Is("AND"):
		return 2
	case t.Is("="), t.Is("<>"), t.Is("!="), t.Is("<"), t.Is(">"), t.Is("<="), t.Is(">="), t.Is("IS"):
		return 3
	}
	return 0
}
func (p *Expr) binary(min int) (string, error) {
	left, e := p.atom()
	if e != nil {
		return "", e
	}
	for p.Pos < len(p.Tokens) {
		t := p.Tokens[p.Pos]
		prec := precedence(t)
		if prec == 0 || prec < min {
			break
		}
		p.Pos++
		if t.Is("IS") {
			not := ""
			if p.take("NOT") {
				not = "NOT "
			}
			if !p.take("NULL") {
				return "", fmt.Errorf("仅支持 IS [NOT] NULL")
			}
			left = "(" + left + " IS " + not + "NULL)"
			continue
		}
		right, e := p.binary(prec + 1)
		if e != nil {
			return "", e
		}
		left = "(" + left + " " + strings.ToUpper(t.Text) + " " + right + ")"
	}
	return left, nil
}
func (p *Expr) atom() (string, error) {
	if p.take("NOT") {
		s, e := p.binary(3)
		return "(NOT " + s + ")", e
	}
	if p.take("(") {
		s, e := p.binary(0)
		if e != nil {
			return "", e
		}
		if !p.take(")") {
			return "", fmt.Errorf("括号不匹配")
		}
		ts, e := Lex(s)
		literal := false
		tok := Token{}
		if e == nil && len(ts) == 1 && (ts[0].Kind == 's' || ts[0].Kind == 'n' || ts[0].Is("NULL")) {
			literal = true
			tok = ts[0]
		}
		if e == nil && len(ts) == 2 && (ts[0].Is("-") || ts[0].Is("+")) && ts[1].Kind == 'n' {
			literal = true
			tok = ts[1]
			tok.Text = ts[0].Text + tok.Text
			tok.Value = tok.Text
		}
		return p.cast(s, tok, literal, "")
	}
	sign := ""
	if p.take("-") {
		sign = "-"
	} else if p.take("+") {
		sign = "+"
	}
	if p.Pos >= len(p.Tokens) {
		return "", fmt.Errorf("表达式为空")
	}
	t := p.Tokens[p.Pos]
	p.Pos++
	literal := t.Kind == 's' || t.Kind == 'n' || t.Is("NULL") || t.Is("TRUE") || t.Is("FALSE")
	var text string
	if literal {
		switch {
		case t.Kind == 's':
			text = QuoteString(t.Value)
		case t.Is("TRUE"):
			text = "1"
		case t.Is("FALSE"):
			text = "0"
		case t.Is("NULL"):
			text = "NULL"
		default:
			text = t.Text
		}
		if sign != "" && t.Kind != 'n' {
			return "", fmt.Errorf("符号只能用于数字")
		}
		text = sign + text
	} else {
		if sign != "" {
			return "", fmt.Errorf("不支持列上的一元运算")
		}
		if t.Kind != 'i' && t.Kind != 'q' {
			return "", fmt.Errorf("不支持的表达式 %s", t.Text)
		}
		parts := []string{t.Value}
		for p.take(".") {
			if p.Pos >= len(p.Tokens) {
				return "", fmt.Errorf("标识符不完整")
			}
			n := p.Tokens[p.Pos]
			if n.Kind != 'i' && n.Kind != 'q' {
				return "", fmt.Errorf("无效标识符")
			}
			parts = append(parts, n.Value)
			p.Pos++
		}
		if p.Resolve == nil {
			return "", fmt.Errorf("不支持默认值函数/引用 %s", t.Text)
		}
		var e error
		text, e = p.Resolve(parts)
		if e != nil {
			return "", e
		}
	}
	return p.cast(text, t, literal, sign)
}
func (p *Expr) cast(text string, t Token, literal bool, sign string) (string, error) {
	for p.take("::") {
		if !literal {
			return "", fmt.Errorf("只支持常量类型转换")
		}
		if p.Pos >= len(p.Tokens) {
			return "", fmt.Errorf("缺少转换类型")
		}
		cast := p.Tokens[p.Pos].Value
		p.Pos++
		if cast == "character" && p.take("varying") {
			cast = "varchar"
		}
		if cast == "double" && p.take("precision") {
			cast = "float8"
		}
		if cast == "timestamp" || cast == "time" {
			if p.take("without") {
				if !p.take("time") || !p.take("zone") {
					return "", fmt.Errorf("无效时间类型")
				}
			} else if p.take("with") {
				if !p.take("time") || !p.take("zone") {
					return "", fmt.Errorf("无效时间类型")
				}
				if cast == "timestamp" {
					cast = "timestamptz"
				} else {
					cast = "timetz"
				}
			}
		}
		if t.Is("NULL") {
			switch cast {
			case "text", "varchar", "bpchar", "char", "character", "int2", "int4", "int8", "smallint", "integer", "bigint", "numeric", "decimal", "float4", "float8", "real", "bool", "boolean", "date", "time", "timestamp", "timestamptz", "uuid", "json", "jsonb", "bytea":
				continue
			default:
				return "", fmt.Errorf("不支持 NULL 的目标类型 %s", cast)
			}
		}
		switch cast {
		case "text", "varchar", "bpchar", "char", "character":
			if t.Kind != 's' {
				return "", fmt.Errorf("仅字符串允许转字符类型")
			}
		case "int2", "int4", "int8", "smallint", "integer", "bigint", "numeric", "decimal", "float4", "float8", "real":
			if t.Kind == 's' {
				v := t.Value
				if sign != "" {
					v = sign + v
				}
				if _, e := strconv.ParseFloat(v, 64); e != nil {
					return "", fmt.Errorf("非法数值常量")
				}
				text = v
			} else if t.Kind != 'n' {
				return "", fmt.Errorf("非法数值类型转换")
			}
			if strings.Contains(text, "NaN") || strings.Contains(strings.ToLower(text), "inf") {
				return "", fmt.Errorf("非有限数值常量")
			}
			switch cast {
			case "int2", "smallint", "int4", "integer", "int8", "bigint":
				bits := 64
				if cast == "int2" || cast == "smallint" {
					bits = 16
				}
				if cast == "int4" || cast == "integer" {
					bits = 32
				}
				if _, e := strconv.ParseInt(text, 10, bits); e != nil {
					return "", fmt.Errorf("整数类型转换会溢出或改变数值")
				}
			case "float4", "real":
				x, e := strconv.ParseFloat(text, 64)
				if e != nil || math.IsInf(x, 0) || float64(float32(x)) != x {
					return "", fmt.Errorf("单精度转换会改变常量数值")
				}
			}
		case "boolean", "bool":
			if t.Kind == 'n' && text != "0" && text != "1" {
				return "", fmt.Errorf("布尔转换仅支持 0/1 常量")
			}
			if t.Kind == 's' {
				switch strings.ToLower(t.Value) {
				case "true", "t", "1":
					text = "1"
				case "false", "f", "0":
					text = "0"
				default:
					return "", fmt.Errorf("非法布尔常量")
				}
			}
		case "date", "time", "timestamp", "timestamptz", "uuid", "json", "jsonb", "bytea":
			if !p.TypedLiteral {
				return "", fmt.Errorf("谓词中的 %s 类型转换尚不支持安全改写", cast)
			}
			if t.Kind != 's' {
				return "", fmt.Errorf("类型转换需要字符串常量")
			}
		default:
			return "", fmt.Errorf("不支持常量转换类型 %s", cast)
		}
		if ts, e := Lex(text); e == nil && len(ts) == 1 {
			t = ts[0]
			sign = ""
		}
		// Length / precision casts can truncate, and therefore are not erased.
		if p.Pos < len(p.Tokens) && p.Tokens[p.Pos].Is("(") {
			return "", fmt.Errorf("带长度/精度的类型转换需人工确认")
		}
	}
	return text, nil
}
