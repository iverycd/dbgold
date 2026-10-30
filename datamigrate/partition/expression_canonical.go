package partition

import (
	"dbgold/datamigrate/sqlsyntax"
	"fmt"
	"github.com/shopspring/decimal"
	"strconv"
	"strings"
)

// Preserve numeric casts and built-in identity. Unknown syntax falls back to
// exact token comparison, never to a cast-stripping approximation.
type routeExpressionParser struct {
	tokens []sqlsyntax.Token
	pos    int
}

func canonicalRoutingExpression(raw string) (string, bool) {
	tokens, e := sqlsyntax.Lex(raw)
	if e != nil {
		return "", false
	}
	p := routeExpressionParser{tokens: tokens}
	v, e := p.binary(1)
	return "route:" + v, e == nil && p.pos == len(tokens)
}
func (p *routeExpressionParser) binary(min int) (string, error) {
	left, err := p.atom()
	if err != nil {
		return "", err
	}
	for p.pos < len(p.tokens) {
		t := p.tokens[p.pos]
		prec := integerRoutePrecedence(t)
		if prec > 0 {
			prec++
		}
		if t.Is("IS") {
			prec = 1
		}
		if prec < min {
			break
		}
		p.pos++
		if t.Is("IS") {
			not := p.take("NOT")
			if !p.take("NULL") {
				return "", fmt.Errorf("unsupported null test")
			}
			left = fmt.Sprintf("isnull:%t(%s)", not, left)
			continue
		}
		right, err := p.binary(prec + 1)
		if err != nil {
			return "", err
		}
		left = "operator:" + strings.ToLower(t.Text) + "(" + left + "," + right + ")"
	}
	return left, nil
}
func (p *routeExpressionParser) take(s string) bool {
	if p.pos < len(p.tokens) && p.tokens[p.pos].Is(s) {
		p.pos++
		return true
	}
	return false
}
func (p *routeExpressionParser) atom() (string, error) {
	if p.pos >= len(p.tokens) {
		return "", fmt.Errorf("empty expression")
	}
	var value string
	if p.take("NULL") {
		value = "literal:null"
	} else if p.take("TRUE") {
		value = "literal:true"
	} else if p.take("FALSE") {
		value = "literal:false"
	} else if p.take("(") {
		var e error
		value, e = p.binary(1)
		if e != nil || !p.take(")") {
			return "", fmt.Errorf("unclosed expression")
		}
	} else if p.take("CASE") {
		var branches []string
		for p.take("WHEN") {
			condition, e := p.binary(1)
			if e != nil || !p.take("THEN") {
				return "", fmt.Errorf("invalid CASE condition")
			}
			result, e := p.binary(1)
			if e != nil {
				return "", e
			}
			branches = append(branches, "when("+condition+","+result+")")
		}
		if len(branches) == 0 || !p.take("ELSE") {
			return "", fmt.Errorf("incomplete CASE")
		}
		fallback, e := p.binary(1)
		if e != nil || !p.take("END") {
			return "", fmt.Errorf("invalid CASE end")
		}
		value = "case(" + strings.Join(branches, ",") + ",else(" + fallback + "))"
	} else if p.take("CAST") {
		if !p.take("(") {
			return "", fmt.Errorf("invalid CAST")
		}
		v, e := p.binary(1)
		if e != nil || !p.take("AS") {
			return "", fmt.Errorf("invalid CAST")
		}
		typ, ok := p.castType()
		if !ok || !p.take(")") {
			return "", fmt.Errorf("unsupported cast")
		}
		value = routeTypedCast(v, typ)
	} else {
		sign := ""
		if p.take("-") {
			sign = "-"
		} else {
			p.take("+")
		}
		if p.pos >= len(p.tokens) {
			return "", fmt.Errorf("missing atom")
		}
		t := p.tokens[p.pos]
		p.pos++
		switch t.Kind {
		case 's':
			if sign != "" {
				return "", fmt.Errorf("signed string")
			}
			value = "string:" + strconv.Quote(t.Value)
		case 'n':
			n, e := decimal.NewFromString(sign + t.Text)
			if e != nil {
				return "", e
			}
			value = "number:" + n.String()
		case 'i', 'q':
			if sign != "" {
				return "", fmt.Errorf("signed reference")
			}
			name := t.Value
			plainFunction := t.Kind == 'i'
			if t.Kind == 'i' {
				name = strings.ToLower(name)
			}
			if p.take(".") {
				plainFunction = false
				if name != "pg_catalog" || p.pos >= len(p.tokens) {
					return "", fmt.Errorf("unknown function schema")
				}
				next := p.tokens[p.pos]
				if next.Kind != 'i' && next.Kind != 'q' {
					return "", fmt.Errorf("invalid qualified function")
				}
				name = next.Value
				if next.Kind == 'i' {
					name = strings.ToLower(name)
				}
				p.pos++
			}
			if p.take("(") {
				if !routeNativeFunction(name) {
					return "", fmt.Errorf("unknown routing function")
				}
				if name == "coalesce" && !plainFunction {
					return "", fmt.Errorf("COALESCE must be SQL syntax, not a named function")
				}
				var args []string
				for {
					v, e := p.binary(1)
					if e != nil {
						return "", e
					}
					args = append(args, v)
					if !p.take(",") {
						break
					}
				}
				if !p.take(")") {
					return "", fmt.Errorf("unclosed function")
				}
				value = "function:" + name + "(" + strings.Join(args, ",") + ")"
			} else {
				value = "column:" + strconv.Quote(name)
			}
		default:
			return "", fmt.Errorf("unsupported atom")
		}
	}
	for p.take("::") {
		typ, ok := p.castType()
		if !ok {
			return "", fmt.Errorf("unsupported postfix cast")
		}
		value = routeTypedCast(value, typ)
	}
	return value, nil
}

func routeNativeFunction(name string) bool {
	switch name {
	case "abs", "mod", "div", "coalesce", "int8and", "int8xor", "int8or", "int8pl", "int8mul", "int8shl", "int8shr", "int8eq", "int8lt", "int8gt", "int8mod", "get_byte", "decode":
		return true
	}
	return false
}

func (p *routeExpressionParser) castType() (string, bool) {
	if p.take("pg_catalog") && !p.take(".") {
		return "", false
	}
	for _, pair := range [][2]string{{"numeric", "numeric"}, {"bigint", "bigint"}, {"int8", "bigint"}, {"integer", "integer"}, {"int4", "integer"}, {"text", "text"}} {
		if p.take(pair[0]) {
			return pair[1], true
		}
	}
	return "", false
}

func routeTypedCast(v, typ string) string {
	if typ == "numeric" {
		return routeNumericCast(v)
	}
	if typ == "text" && strings.HasPrefix(v, "string:") {
		return v
	}
	if typ == "bigint" || typ == "integer" {
		// Normalize only exact, in-domain integer constants. Keep all other
		// casts visible; stripping a narrowing cast would hide changed routing.
		raw := strings.TrimPrefix(strings.TrimPrefix(v, "string:"), "number:")
		if strings.HasPrefix(v, "string:") {
			raw, _ = strconv.Unquote(raw)
		}
		bits := 64
		if typ == "integer" {
			bits = 32
		}
		if strings.HasPrefix(v, "string:") || strings.HasPrefix(v, "number:") {
			if n, err := strconv.ParseInt(raw, 10, bits); err == nil {
				return typ + ":" + strconv.FormatInt(n, 10)
			}
		}
	}
	return "cast:" + typ + "(" + v + ")"
}
func routeNumericCast(v string) string {
	if strings.HasPrefix(v, "string:") || strings.HasPrefix(v, "number:") {
		x := strings.TrimPrefix(strings.TrimPrefix(v, "string:"), "number:")
		if strings.HasPrefix(v, "string:") {
			var err error
			x, err = strconv.Unquote(x)
			if err != nil {
				return "cast:numeric(" + v + ")"
			}
		}
		if n, e := decimal.NewFromString(x); e == nil {
			return "numeric:" + n.String()
		}
	}
	return "cast:numeric(" + v + ")"
}
