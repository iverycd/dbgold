package partition

import (
	"fmt"
	"math/big"
	"strings"

	"dbgold/datamigrate/sqlsyntax"
)

// integerRoute proves each intermediate result over the full source column
// domain. Using numeric on PostgreSQL is safe only after this proof: otherwise
// it would silently accept writes that overflow the MySQL partition function.
type integerRoute struct {
	sql      string
	lo, hi   *big.Int
	nullable bool
}

type integerRouteParser struct {
	c      *compiler
	tokens []sqlsyntax.Token
	pos    int
}

func (c *compiler) integerExpression(raw string) (integerRoute, error) {
	if c.opt.SourceType != "mysql" || (c.opt.TargetType != "mysql" && !c.profile.Features["postgres_declarative"]) {
		return integerRoute{}, fmt.Errorf("该源目标的整数表达式求值规则尚未验证")
	}
	ts, err := sqlsyntax.Lex(raw)
	if err != nil {
		return integerRoute{}, err
	}
	if len(ts) == 0 || len(ts) > 512 {
		return integerRoute{}, fmt.Errorf("表达式为空或超过已验证的 512 个语法单元")
	}
	p := integerRouteParser{c: c, tokens: ts}
	n, err := p.binary(1)
	if err == nil && p.pos != len(ts) {
		err = fmt.Errorf("不支持的运算或函数 %q", ts[p.pos].Text)
	}
	return n, err
}

func (p *integerRouteParser) take(s string) bool {
	if p.pos < len(p.tokens) && p.tokens[p.pos].Is(s) {
		p.pos++
		return true
	}
	return false
}

func integerRoutePrecedence(t sqlsyntax.Token) int {
	if t.Is("+") || t.Is("-") {
		return 1
	}
	if t.Is("*") || t.Is("DIV") || t.Is("MOD") || t.Is("%") {
		return 2
	}
	return 0
}

func (p *integerRouteParser) binary(min int) (integerRoute, error) {
	left, err := p.atom()
	if err != nil {
		return left, err
	}
	for p.pos < len(p.tokens) {
		t := p.tokens[p.pos]
		prec := integerRoutePrecedence(t)
		if prec < min {
			break
		}
		p.pos++
		right, err := p.binary(prec + 1)
		if err != nil {
			return left, err
		}
		left, err = p.combine(left, strings.ToUpper(t.Text), right)
		if err != nil {
			return left, err
		}
	}
	return left, nil
}

func (p *integerRouteParser) atom() (integerRoute, error) {
	if p.take("(") {
		n, err := p.binary(1)
		if err != nil || !p.take(")") {
			if err == nil {
				err = fmt.Errorf("表达式括号不匹配")
			}
			return n, err
		}
		return n, nil
	}
	if p.take("+") {
		return p.atom()
	}
	if p.take("-") {
		n, err := p.atom()
		if err != nil {
			return n, err
		}
		return p.combine(p.literal(big.NewInt(0)), "-", n)
	}
	if p.pos >= len(p.tokens) {
		return integerRoute{}, fmt.Errorf("缺少表达式操作数")
	}
	t := p.tokens[p.pos]
	p.pos++
	if t.Kind == 'n' {
		v, ok := new(big.Int).SetString(t.Text, 10)
		if !ok {
			return integerRoute{}, fmt.Errorf("只允许精确的十进制整数常量")
		}
		n := p.literal(v)
		return n, integerRouteFits(n)
	}
	if t.Kind != 'i' && t.Kind != 'q' {
		return integerRoute{}, fmt.Errorf("不支持的表达式操作数 %q", t.Text)
	}
	if p.take("(") {
		if t.Kind != 'i' {
			return integerRoute{}, fmt.Errorf("不执行带引号或自定义分区函数")
		}
		fn := strings.ToUpper(t.Text)
		if fn != "ABS" && fn != "MOD" && fn != "FLOOR" && fn != "CEILING" && fn != "CEIL" {
			return integerRoute{}, fmt.Errorf("函数 %s 的完整取值域、时区或排序规则尚未验证", fn)
		}
		a, err := p.binary(1)
		if err != nil {
			return a, err
		}
		if fn == "MOD" {
			if !p.take(",") {
				return a, fmt.Errorf("MOD 需要两个参数")
			}
			b, err := p.binary(1)
			if err != nil {
				return a, err
			}
			a, err = p.combine(a, "MOD", b)
			if err != nil {
				return a, err
			}
		} else if fn == "ABS" {
			lo, hi := new(big.Int).Abs(a.lo), new(big.Int).Abs(a.hi)
			if lo.Cmp(hi) > 0 {
				lo, hi = hi, lo
			}
			if a.lo.Sign() <= 0 && a.hi.Sign() >= 0 {
				lo = big.NewInt(0)
			}
			a.lo, a.hi = lo, hi
			a.sql = p.function("abs", a.sql)
			if err := integerRouteFits(a); err != nil {
				return a, err
			}
		}
		// FLOOR/CEIL on a proven integer are exactly the identity.
		if !p.take(")") {
			return a, fmt.Errorf("函数参数数量或括号不匹配")
		}
		return a, nil
	}
	name := t.Text
	if t.Kind == 'q' {
		name = t.Value
	}
	col, ok := p.c.column(name)
	if !ok {
		return integerRoute{}, fmt.Errorf("表达式引用未知列 %q", name)
	}
	typ := strings.ToLower(col.DataType)
	if strings.Contains(strings.ToLower(col.FormattedType+" "+typ), "unsigned") {
		return integerRoute{}, fmt.Errorf("列 %s 的无符号运算依赖 SQL_MODE，完整语义尚未验证", name)
	}
	bits := map[string]uint{"tinyint": 8, "smallint": 16, "mediumint": 24, "int": 32, "integer": 32, "bigint": 64}[typ]
	if bits == 0 {
		return integerRoute{}, fmt.Errorf("列 %s 的类型 %s 不在已验证的有符号整数域", name, col.DataType)
	}
	limit := new(big.Int).Lsh(big.NewInt(1), bits-1)
	key := p.c.quote(p.c.opt.ColumnName(name))
	if p.c.opt.TargetType != "mysql" {
		key = "CAST(" + key + " AS numeric)"
	}
	return integerRoute{sql: key, lo: new(big.Int).Neg(limit), hi: new(big.Int).Sub(limit, big.NewInt(1)), nullable: col.IsNullable}, nil
}

func (p *integerRouteParser) literal(v *big.Int) integerRoute {
	s := v.String()
	if p.c.opt.TargetType != "mysql" {
		s = "CAST('" + s + "' AS numeric)"
	}
	return integerRoute{sql: s, lo: new(big.Int).Set(v), hi: new(big.Int).Set(v)}
}

func (p *integerRouteParser) function(fn string, args ...string) string {
	if p.c.opt.TargetType != "mysql" {
		fn = "pg_catalog." + fn
	}
	return fn + "(" + strings.Join(args, ",") + ")"
}

func (p *integerRouteParser) combine(a integerRoute, op string, b integerRoute) (integerRoute, error) {
	n := integerRoute{nullable: a.nullable || b.nullable}
	switch op {
	case "+":
		n.lo, n.hi = new(big.Int).Add(a.lo, b.lo), new(big.Int).Add(a.hi, b.hi)
	case "-":
		n.lo, n.hi = new(big.Int).Sub(a.lo, b.hi), new(big.Int).Sub(a.hi, b.lo)
	case "*", "DIV":
		if op == "DIV" && b.lo.Sign() <= 0 && b.hi.Sign() >= 0 {
			return n, fmt.Errorf("DIV 除数可能为零，源 SQL_MODE 下的错误/NULL 行为不能等价保证")
		}
		for _, x := range []*big.Int{a.lo, a.hi} {
			for _, y := range []*big.Int{b.lo, b.hi} {
				v := new(big.Int)
				if op == "*" {
					v.Mul(x, y)
				} else {
					v.Quo(x, y)
				}
				if n.lo == nil || v.Cmp(n.lo) < 0 {
					n.lo = new(big.Int).Set(v)
				}
				if n.hi == nil || v.Cmp(n.hi) > 0 {
					n.hi = new(big.Int).Set(v)
				}
			}
		}
		// MySQL integer DIV checks a negative quotient's absolute magnitude
		// against LLONG_MAX before restoring its sign. Even LLONG_MIN DIV 1
		// therefore raises 1690, while PostgreSQL numeric div accepts it. A
		// signed-int64 result bound alone does not prove equivalent semantics.
		if op == "DIV" && n.lo.Cmp(big.NewInt(-1<<63)) == 0 {
			return n, fmt.Errorf("DIV 负商可能达到 BIGINT 最小值，MySQL 会溢出而目标精确除法接受，不能改变未来写入语义")
		}
	case "MOD", "%":
		if b.lo.Sign() <= 0 && b.hi.Sign() >= 0 {
			return n, fmt.Errorf("MOD 除数可能为零，源 SQL_MODE 下的错误/NULL 行为不能等价保证")
		}
		bound := new(big.Int).Abs(b.lo)
		if z := new(big.Int).Abs(b.hi); z.Cmp(bound) > 0 {
			bound = z
		}
		bound.Sub(bound, big.NewInt(1))
		n.lo, n.hi = big.NewInt(0), big.NewInt(0)
		if a.lo.Sign() < 0 {
			n.lo.Neg(bound)
			if n.lo.Cmp(a.lo) < 0 {
				n.lo.Set(a.lo)
			}
		}
		if a.hi.Sign() > 0 {
			n.hi.Set(bound)
			if n.hi.Cmp(a.hi) > 0 {
				n.hi.Set(a.hi)
			}
		}
	default:
		return n, fmt.Errorf("未验证的运算 %s", op)
	}
	if op == "MOD" || op == "%" {
		n.sql = p.function("mod", a.sql, b.sql)
	} else if op == "DIV" && p.c.opt.TargetType != "mysql" {
		// PostgreSQL div(numeric,numeric) truncates exactly toward zero; using
		// numeric division followed by truncation could first round the quotient.
		n.sql = p.function("div", a.sql, b.sql)
	} else {
		n.sql = "(" + a.sql + " " + op + " " + b.sql + ")"
	}
	return n, integerRouteFits(n)
}

func integerRouteFits(n integerRoute) error {
	if !n.lo.IsInt64() || !n.hi.IsInt64() {
		return fmt.Errorf("表达式中间结果可能超出源有符号 BIGINT 范围，不能改变未来写入的溢出行为")
	}
	return nil
}
