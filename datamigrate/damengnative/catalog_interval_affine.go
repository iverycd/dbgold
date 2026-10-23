package damengnative

import (
	"fmt"
	"math/big"
	"strings"
)

type intervalAffine struct {
	constant *big.Rat
	coeff    map[string]*big.Rat
}

type intervalAffineParser struct {
	tokens  []sqlToken
	text    string
	pos     int
	columns map[string]Column
	used    map[string]Column
}

func intervalAffineConstant(v *big.Rat) intervalAffine {
	return intervalAffine{new(big.Rat).Set(v), map[string]*big.Rat{}}
}

// This grammar never accepts a call, qualification, string, query, or generated
// dependency. It classifies only arithmetic affine in real writable numeric
// columns. Native casts and actual partition routing subsequently prove each
// solution, including DM's intermediate arithmetic and declared result type.
func parseIntervalAffine(text string, columns map[string]Column) (intervalAffine, *intervalAffineParser, error) {
	ts, err := nativeTokens(text)
	if err != nil {
		return intervalAffine{}, nil, err
	}
	if len(ts) == 0 || len(ts) > 512 {
		return intervalAffine{}, nil, fmt.Errorf("生成分区表达式为空或超出已验证的仿射语法范围")
	}
	p := &intervalAffineParser{tokens: ts, text: text, columns: columns, used: map[string]Column{}}
	a, err := p.sum()
	if err == nil && p.pos != len(ts) {
		err = fmt.Errorf("生成分区表达式不是纯数值仿射表达式")
	}
	return a, p, err
}

func (p *intervalAffineParser) sum() (intervalAffine, error) {
	a, err := p.product()
	for err == nil && p.pos < len(p.tokens) && (p.tokens[p.pos].value == "+" || p.tokens[p.pos].value == "-") {
		op := p.tokens[p.pos].value
		p.pos++
		b, e := p.product()
		if e != nil {
			return a, e
		}
		if op == "-" {
			b = scaleIntervalAffine(b, big.NewRat(-1, 1))
		}
		a.constant.Add(a.constant, b.constant)
		for name, v := range b.coeff {
			if a.coeff[name] == nil {
				a.coeff[name] = new(big.Rat)
			}
			a.coeff[name].Add(a.coeff[name], v)
			if a.coeff[name].Sign() == 0 {
				delete(a.coeff, name)
			}
		}
	}
	return a, err
}

func scaleIntervalAffine(a intervalAffine, scale *big.Rat) intervalAffine {
	a.constant.Mul(a.constant, scale)
	for name, v := range a.coeff {
		v.Mul(v, scale)
		if v.Sign() == 0 {
			delete(a.coeff, name)
		}
	}
	return a
}

func (p *intervalAffineParser) product() (intervalAffine, error) {
	a, err := p.primary()
	for err == nil && p.pos < len(p.tokens) && (p.tokens[p.pos].value == "*" || p.tokens[p.pos].value == "/") {
		op := p.tokens[p.pos].value
		p.pos++
		b, e := p.primary()
		if e != nil {
			return a, e
		}
		if op == "/" {
			if len(b.coeff) != 0 || b.constant.Sign() == 0 {
				return a, fmt.Errorf("仿射除数必须是非零纯数值常量")
			}
			a = scaleIntervalAffine(a, new(big.Rat).Inv(b.constant))
		} else if len(a.coeff) == 0 {
			a = scaleIntervalAffine(b, a.constant)
		} else if len(b.coeff) == 0 {
			a = scaleIntervalAffine(a, b.constant)
		} else {
			return a, fmt.Errorf("生成分区表达式包含非线性乘法")
		}
	}
	return a, err
}

func (p *intervalAffineParser) primary() (intervalAffine, error) {
	if p.pos >= len(p.tokens) {
		return intervalAffine{}, fmt.Errorf("仿射表达式不完整")
	}
	t := p.tokens[p.pos]
	p.pos++
	if t.kind == 'p' && (t.value == "+" || t.value == "-") {
		a, e := p.primary()
		if e == nil && t.value == "-" {
			a = scaleIntervalAffine(a, big.NewRat(-1, 1))
		}
		return a, e
	}
	if t.kind == 'p' && t.value == "(" {
		a, e := p.sum()
		if e != nil {
			return a, e
		}
		if p.pos >= len(p.tokens) || p.tokens[p.pos].value != ")" {
			return a, fmt.Errorf("仿射括号不完整")
		}
		p.pos++
		return a, nil
	}
	if t.kind == 'p' && (t.value >= "0" && t.value <= "9" || t.value == ".") {
		end := t.end
		for p.pos < len(p.tokens) && p.tokens[p.pos].start == end {
			n := p.tokens[p.pos]
			if n.kind != 'p' || !(n.value >= "0" && n.value <= "9" || n.value == ".") {
				break
			}
			end = n.end
			p.pos++
		}
		literal := p.text[t.start:end]
		if len(literal) > 128 {
			return intervalAffine{}, fmt.Errorf("仿射数值常量过长")
		}
		r, ok := new(big.Rat).SetString(literal)
		if !ok {
			return intervalAffine{}, fmt.Errorf("仿射数值常量无效")
		}
		return intervalAffineConstant(r), nil
	}
	if t.kind == 'q' || t.kind == 'i' {
		name := t.value
		if t.kind == 'i' {
			name = strings.ToUpper(name)
		}
		c, ok := p.columns[name]
		if !ok || !c.Writable {
			return intervalAffine{}, fmt.Errorf("生成分区表达式引用未验证的可写数值列 %s", name)
		}
		if _, err := intervalColumnQuantum(c); err != nil {
			return intervalAffine{}, err
		}
		p.used[name] = c
		a := intervalAffineConstant(new(big.Rat))
		a.coeff[name] = big.NewRat(1, 1)
		return a, nil
	}
	return intervalAffine{}, fmt.Errorf("生成分区表达式超出纯数值仿射语法")
}

func intervalColumnQuantum(c Column) (*big.Rat, error) {
	if c.TypeSchema != "" || c.TypeName != "" {
		return nil, fmt.Errorf("仿射输入列 %s 需要用户定义类型转换，不能用于源端纯原生数值探测", c.Name)
	}
	scale := 0
	switch strings.ToUpper(c.DataType) {
	case "TINYINT", "SMALLINT", "INT", "INTEGER", "BIGINT", "BYTE":
	case "DEC", "DECIMAL", "NUMERIC", "NUMBER":
		scale = int(c.Scale)
	default:
		return nil, fmt.Errorf("仿射输入列 %s 不是已验证的精确数值类型", c.Name)
	}
	if scale < -38 || scale > 38 {
		return nil, fmt.Errorf("仿射输入列 %s 的刻度超出精确求逆范围", c.Name)
	}
	power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absIntervalScale(scale))), nil)
	if scale >= 0 {
		return new(big.Rat).SetFrac(big.NewInt(1), power), nil
	}
	return new(big.Rat).SetInt(power), nil
}

func absIntervalScale(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func intervalGeneratedExpression(ddl, column string) (string, error) {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return "", err
	}
	depth, start := 0, -1
	for i, t := range ts {
		if t.value == "(" && t.kind == 'p' {
			depth++
			if depth == 1 && start < 0 {
				start = i + 1
			}
			continue
		}
		if t.kind == 'p' && (t.value == "," || t.value == ")") && depth == 1 && start >= 0 {
			if start < i {
				name := ts[start].value
				if ts[start].kind == 'i' {
					name = strings.ToUpper(name)
				}
				if name == column {
					local := 0
					for j := start + 1; j+1 < i; j++ {
						if local == 0 && ts[j].kind == 'i' && strings.EqualFold(ts[j].value, "AS") && ts[j+1].value == "(" {
							end, nested := j+2, 1
							for ; end < i; end++ {
								if ts[end].value == "(" && ts[end].kind == 'p' {
									nested++
								}
								if ts[end].value == ")" && ts[end].kind == 'p' {
									nested--
									if nested == 0 {
										return ddl[ts[j+1].end:ts[end].start], nil
									}
								}
							}
						}
						if ts[j].kind == 'p' && ts[j].value == "(" {
							local++
						}
						if ts[j].kind == 'p' && ts[j].value == ")" {
							local--
						}
					}
					return "", fmt.Errorf("生成分区列 %s 的原生表达式不可提取", column)
				}
			}
			start = i + 1
		}
		if t.kind == 'p' && t.value == ")" {
			depth--
			if depth == 0 && start >= 0 {
				break
			}
		}
	}
	return "", fmt.Errorf("生成分区列 %s 不在原生表定义中", column)
}

func (p *intervalAffineParser) render(values map[string]string) string {
	var b strings.Builder
	last := 0
	for _, t := range p.tokens {
		if t.kind != 'q' && t.kind != 'i' {
			continue
		}
		name := t.value
		if t.kind == 'i' {
			name = strings.ToUpper(name)
		}
		b.WriteString(p.text[last:t.start])
		b.WriteString("(" + values[name] + ")")
		last = t.end
	}
	b.WriteString(p.text[last:])
	return b.String()
}

func intervalCeil(r *big.Rat) *big.Int {
	q, rem := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func intervalFloor(r *big.Rat) *big.Int { return new(big.Int).Neg(intervalCeil(new(big.Rat).Neg(r))) }

func (r *catalogReader) freezeGeneratedIntervalSeed(t Table, c Column, d intervalDefinition, ddl, high string, empty *MaterializedEmptyTouch) (string, []IntervalSeedValue, error) {
	if _, err := intervalColumnQuantum(c); err != nil {
		return "", nil, err
	}
	expression, err := intervalGeneratedExpression(ddl, c.Name)
	if err != nil {
		return "", nil, err
	}
	columns := map[string]Column{}
	for _, column := range t.Columns {
		columns[column.Name] = column
	}
	a, parser, err := parseIntervalAffine(expression, columns)
	if err != nil {
		return "", nil, err
	}
	upper, _, err := parseIntervalAffine(high, nil)
	if err != nil {
		return "", nil, err
	}
	step, _, err := parseIntervalAffine(d.interval, nil)
	if err != nil || step.constant.Sign() <= 0 {
		return "", nil, fmt.Errorf("生成分区键间隔不是正数值常量")
	}
	lower := new(big.Rat).Sub(upper.constant, step.constant)
	var basis []Column
	var coefficients []*big.Rat
	denominator := new(big.Int).Set(a.constant.Denom())
	for _, column := range t.Columns {
		if coefficient := a.coeff[column.Name]; coefficient != nil {
			quantum, e := intervalColumnQuantum(column)
			if e != nil {
				return "", nil, e
			}
			coefficient = new(big.Rat).Mul(coefficient, quantum)
			basis = append(basis, column)
			coefficients = append(coefficients, coefficient)
			g := new(big.Int).GCD(nil, nil, denominator, coefficient.Denom())
			denominator.Mul(denominator, new(big.Int).Quo(coefficient.Denom(), g))
		}
	}
	constant := new(big.Int).Mul(a.constant.Num(), new(big.Int).Quo(denominator, a.constant.Denom()))
	gcd := new(big.Int)
	var bezout []*big.Int
	for _, coefficient := range coefficients {
		integer := new(big.Int).Mul(coefficient.Num(), new(big.Int).Quo(denominator, coefficient.Denom()))
		x, y, next := new(big.Int), new(big.Int), new(big.Int)
		next.GCD(x, y, gcd, integer)
		for _, v := range bezout {
			v.Mul(v, x)
		}
		bezout = append(bezout, y)
		gcd = next
	}
	var ks []*big.Int
	if gcd.Sign() == 0 {
		ks = []*big.Int{big.NewInt(0)}
	} else {
		quotient := func(value *big.Rat) *big.Rat {
			return new(big.Rat).Quo(new(big.Rat).Sub(new(big.Rat).Mul(value, new(big.Rat).SetInt(denominator)), new(big.Rat).SetInt(constant)), new(big.Rat).SetInt(gcd))
		}
		lo, hi := intervalCeil(quotient(lower)), intervalCeil(quotient(upper.constant))
		hi.Sub(hi, big.NewInt(1))
		if d.inclusive {
			origin, _, e := parseIntervalAffine(d.last, nil)
			if e != nil {
				return "", nil, e
			}
			after := intervalFloor(quotient(origin.constant))
			after.Add(after, big.NewInt(1))
			if lo.Cmp(after) < 0 {
				lo = after
			}
		}
		if lo.Cmp(hi) > 0 {
			return "", nil, fmt.Errorf("生成分区范围没有仿射数值格点")
		}
		ks = []*big.Int{lo, hi, new(big.Int).Quo(new(big.Int).Add(lo, hi), big.NewInt(2)), new(big.Int).Add(lo, big.NewInt(1)), new(big.Int).Sub(hi, big.NewInt(1))}
	}
	var lastErr error
	seen := map[string]bool{}
	for _, k := range ks {
		if seen[k.String()] {
			continue
		}
		seen[k.String()] = true
		values := map[string]string{}
		for name, column := range parser.used {
			values[name] = "CAST('0' AS " + column.SQLType + ")"
		}
		for i, column := range basis {
			quantum, _ := intervalColumnQuantum(column)
			value := new(big.Rat).Mul(new(big.Rat).SetInt(new(big.Int).Mul(bezout[i], k)), quantum)
			scale := int(column.Scale)
			if scale < 0 {
				scale = 0
			}
			values[column.Name] = "CAST('" + value.FloatString(scale) + "' AS " + column.SQLType + ")"
		}
		if len(values) == 0 {
			for _, column := range t.Columns {
				if column.Writable {
					if _, e := intervalColumnQuantum(column); e == nil {
						values[column.Name] = "CAST('0' AS " + column.SQLType + ")"
						break
					}
				}
			}
		}
		if len(values) == 0 && empty != nil {
			for _, column := range empty.Columns {
				value := column.ValueSQL
				if value == "" {
					value = "NULL"
				}
				values[column.Name] = value
				break
			}
		}
		if len(values) == 0 {
			return "", nil, fmt.Errorf("常量生成分区键没有已验证的临时可写列")
		}
		actual := "CAST((" + parser.render(values) + ") AS " + c.SQLType + ")"
		cast := func(s string) string { return "CAST((" + s + ") AS " + c.SQLType + ")" }
		predicate := actual + ">=" + cast(high+"-("+d.interval+")") + " AND " + actual + "<" + cast(high)
		if d.inclusive {
			predicate += " AND " + actual + ">" + cast(d.last)
		}
		var count int
		probe := "SELECT CASE WHEN " + predicate + " THEN (SELECT COUNT(*) FROM " + Qualified(r.schema, t.Name) + " PARTITION FOR(" + actual + ")) ELSE -1 END FROM DUAL"
		if e := r.db.QueryRowContext(r.ctx, probe).Scan(&count); e != nil {
			if r.ctx.Err() != nil {
				return "", nil, r.ctx.Err()
			}
			lastErr = e
			continue
		}
		if count < 0 {
			continue
		}
		var result []IntervalSeedValue
		for _, column := range t.Columns {
			if value, ok := values[column.Name]; ok {
				result = append(result, IntervalSeedValue{Column: column.Name, SQL: value})
			}
		}
		return actual, result, nil
	}
	return "", nil, fmt.Errorf("生成分区键未得到通过原生CAST及实际分区路由证明的仿射种子: %v", lastErr)
}
