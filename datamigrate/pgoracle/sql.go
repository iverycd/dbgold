package pgoracle

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/typemap"
)

// Expressions retain their SQL value type separately from Oracle conditions.
// Oracle 11g has no SQL BOOLEAN: a predicate cannot be emitted as a column value.
type sqlValue struct {
	sql       string
	kind      string
	predicate bool
	aggregate bool
	literal   bool
	empty     bool
	column    *source.ColumnInfo
}

func (v sqlValue) condition() (string, error) {
	if v.kind != "bool" && v.kind != "null" {
		return "", fmt.Errorf("条件必须是布尔表达式，不能把 %s 隐式转换为布尔值", v.kind)
	}
	if v.predicate {
		return v.sql, nil
	}
	return "(" + v.sql + " = 1)", nil
}

func (v sqlValue) value() string {
	if !v.predicate {
		return v.sql
	}
	// Both branches are necessary: ELSE 0 would turn SQL UNKNOWN into false.
	return "CASE WHEN " + v.sql + " THEN 1 WHEN NOT (" + v.sql + ") THEN 0 ELSE NULL END"
}

type sqlParser struct {
	tokens    []sqlsyntax.Token
	pos       int
	name      func(string) string
	resolve   func([]string) (sqlValue, error)
	aggregate bool
}

func (p *sqlParser) take(s string) bool {
	if p.pos < len(p.tokens) && p.tokens[p.pos].Is(s) {
		p.pos++
		return true
	}
	return false
}
func (p *sqlParser) require(s string) error {
	if !p.take(s) {
		return fmt.Errorf("缺少 %s", s)
	}
	return nil
}
func quote(s string) string      { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func sqlLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func columnValue(c source.ColumnInfo, sql string) (sqlValue, error) {
	target, err := typemap.PGToOracleChecked(c, false, 4, 4)
	if err != nil {
		return sqlValue{}, err
	}
	kind := ""
	switch {
	case strings.Contains(target, "LOB"):
		kind = "lob"
	case typemap.PGOracleBaseType(c.DataType) == "bool":
		kind = "bool"
	case strings.HasPrefix(target, "NUMBER"), strings.HasPrefix(target, "BINARY_"):
		kind = "number"
	case strings.Contains(target, "CHAR"):
		kind = "text"
	case strings.Contains(target, "WITH TIME ZONE"):
		kind = "timestamptz"
	case strings.HasPrefix(target, "TIMESTAMP"):
		kind = "timestamp"
	case target == "DATE":
		kind = "date"
	default:
		return sqlValue{}, fmt.Errorf("表达式不支持列 %s 的类型 %s", c.Name, c.DataType)
	}
	return sqlValue{sql: sql, kind: kind, column: &c}, nil
}

func exprPrecedence(t sqlsyntax.Token) int {
	switch {
	case t.Is("OR"):
		return 1
	case t.Is("AND"):
		return 2
	case t.Is("="), t.Is("<>"), t.Is("!="), t.Is("<"), t.Is(">"), t.Is("<="), t.Is(">="), t.Is("IS"):
		return 3
	case t.Is("+"), t.Is("-"):
		return 4
	case t.Is("*"):
		return 5
	}
	return 0
}

func (p *sqlParser) parse() (sqlValue, error) {
	v, err := p.expression(1)
	if err == nil && p.pos != len(p.tokens) {
		err = fmt.Errorf("尚不支持的表达式语法 %s", p.tokens[p.pos].Text)
	}
	return v, err
}
func compatible(a, b sqlValue) error {
	if a.kind == "lob" || b.kind == "lob" {
		return fmt.Errorf("Oracle LOB 不支持此比较、分组或表达式操作")
	}
	if a.empty || b.empty {
		return fmt.Errorf("空字符串在 Oracle 中为 NULL，此表达式无法保留源语义")
	}
	if a.kind != b.kind && a.kind != "null" && b.kind != "null" {
		return fmt.Errorf("不能隐式混合 %s 与 %s 类型", a.kind, b.kind)
	}
	return nil
}

func (p *sqlParser) expression(min int) (sqlValue, error) {
	left, err := p.atom()
	if err != nil {
		return sqlValue{}, err
	}
	for p.pos < len(p.tokens) {
		t := p.tokens[p.pos]
		prec := exprPrecedence(t)
		if prec < min || prec == 0 {
			break
		}
		p.pos++
		if t.Is("IS") {
			not := p.take("NOT")
			if p.take("NULL") {
				op := " IS NULL)"
				if not {
					op = " IS NOT NULL)"
				}
				left = sqlValue{sql: "(" + left.value() + op, kind: "bool", predicate: true, aggregate: left.aggregate}
				continue
			}
			truth := p.take("TRUE")
			if !truth && !p.take("FALSE") {
				return sqlValue{}, fmt.Errorf("仅支持 IS [NOT] NULL/TRUE/FALSE")
			}
			if left.kind != "bool" && left.kind != "null" {
				return sqlValue{}, fmt.Errorf("IS TRUE/FALSE 仅适用于布尔表达式")
			}
			value := "0"
			if truth {
				value = "1"
			}
			// IS TRUE/FALSE is never UNKNOWN in PostgreSQL. Retain that property
			// in projections and CHECK constraints, not only in WHERE filtering.
			s := "(" + left.value() + " = " + value + " AND " + left.value() + " IS NOT NULL)"
			if not {
				s = "(" + left.value() + " <> " + value + " OR " + left.value() + " IS NULL)"
			}
			left = sqlValue{sql: s, kind: "bool", predicate: true, aggregate: left.aggregate}
			continue
		}
		right, err := p.expression(prec + 1)
		if err != nil {
			return sqlValue{}, err
		}
		op := strings.ToUpper(t.Text)
		out := sqlValue{aggregate: left.aggregate || right.aggregate}
		switch prec {
		case 1, 2:
			a, err := left.condition()
			if err != nil {
				return sqlValue{}, err
			}
			b, err := right.condition()
			if err != nil {
				return sqlValue{}, err
			}
			out.sql, out.kind, out.predicate = "("+a+" "+op+" "+b+")", "bool", true
		case 3:
			if err := compatible(left, right); err != nil {
				return sqlValue{}, err
			}
			if left.kind == "bool" && op != "=" && op != "<>" && op != "!=" {
				return sqlValue{}, fmt.Errorf("布尔排序比较尚未实现")
			}
			out.sql, out.kind, out.predicate = "("+left.value()+" "+op+" "+right.value()+")", "bool", true
		default:
			if left.kind != "number" && left.kind != "null" || right.kind != "number" && right.kind != "null" {
				return sqlValue{}, fmt.Errorf("算术仅支持数值类型")
			}
			out.sql, out.kind = "("+left.value()+" "+op+" "+right.value()+")", "number"
		}
		left = out
	}
	return left, nil
}

func (p *sqlParser) atom() (sqlValue, error) {
	if p.take("NOT") {
		v, err := p.expression(3)
		if err != nil {
			return v, err
		}
		s, err := v.condition()
		if err != nil {
			return sqlValue{}, err
		}
		return sqlValue{sql: "(NOT " + s + ")", kind: "bool", predicate: true, aggregate: v.aggregate}, nil
	}
	if p.take("CASE") {
		return p.caseValue()
	}
	if p.take("(") {
		v, err := p.expression(1)
		if err != nil {
			return v, err
		}
		if err := p.require(")"); err != nil {
			return sqlValue{}, err
		}
		return p.cast(v)
	}
	sign := ""
	if p.take("-") {
		sign = "-"
	} else if p.take("+") {
		sign = "+"
	}
	if p.pos >= len(p.tokens) {
		return sqlValue{}, fmt.Errorf("表达式为空")
	}
	t := p.tokens[p.pos]
	p.pos++
	v := sqlValue{literal: true}
	switch {
	case t.Kind == 's':
		v.sql, v.kind, v.empty = sqlLiteral(t.Value), "text", t.Value == ""
	case t.Kind == 'n':
		v.sql, v.kind = t.Text, "number"
	case t.Is("TRUE"), t.Is("FALSE"):
		v.sql, v.kind = "0", "bool"
		if t.Is("TRUE") {
			v.sql = "1"
		}
	case t.Is("NULL"):
		v.sql, v.kind = "NULL", "null"
	case t.Kind == 'i' || t.Kind == 'q':
		if p.take("(") {
			if t.Kind == 'q' {
				return sqlValue{}, fmt.Errorf("不自动转换用户函数 %s", t.Text)
			}
			var err error
			v, err = p.function(strings.ToUpper(t.Text))
			if err != nil {
				return v, err
			}
		} else {
			parts := []string{t.Value}
			for p.take(".") {
				if p.pos >= len(p.tokens) || p.tokens[p.pos].Kind != 'i' && p.tokens[p.pos].Kind != 'q' {
					return sqlValue{}, fmt.Errorf("列引用不完整")
				}
				parts = append(parts, p.tokens[p.pos].Value)
				p.pos++
			}
			var err error
			v, err = p.resolve(parts)
			if err != nil {
				return v, err
			}
		}
	default:
		return sqlValue{}, fmt.Errorf("尚不支持的表达式 %s", t.Text)
	}
	if sign != "" {
		if v.kind != "number" {
			return sqlValue{}, fmt.Errorf("一元符号仅支持数值")
		}
		v.sql = sign + "(" + v.value() + ")"
	}
	return p.cast(v)
}

func (p *sqlParser) cast(v sqlValue) (sqlValue, error) {
	for p.take("::") {
		if p.pos >= len(p.tokens) || p.tokens[p.pos].Kind != 'i' {
			return sqlValue{}, fmt.Errorf("缺少类型转换目标")
		}
		t := p.tokens[p.pos].Value
		p.pos++
		if t == "pg_catalog" || t == "sys_catalog" {
			if !p.take(".") || p.pos >= len(p.tokens) || p.tokens[p.pos].Kind != 'i' {
				return sqlValue{}, fmt.Errorf("类型引用不完整")
			}
			t = p.tokens[p.pos].Value
			p.pos++
		}
		if t == "character" && p.take("VARYING") {
			t = "varchar"
		}
		t = typemap.PGOracleBaseType(t)
		if p.pos < len(p.tokens) && p.tokens[p.pos].Is("(") {
			return sqlValue{}, fmt.Errorf("带精度或长度的类型转换需要人工适配")
		}
		if v.kind == "null" {
			if _, err := columnValue(source.ColumnInfo{DataType: t}, "NULL"); err != nil {
				return sqlValue{}, err
			}
			continue
		}
		switch t {
		case "text", "varchar":
			if v.kind != "text" {
				return sqlValue{}, fmt.Errorf("不安全的 %s → %s 转换", v.kind, t)
			}
			if v.column != nil && typemap.PGOracleBaseType(v.column.DataType) == "bpchar" {
				v.sql = "RTRIM(" + v.value() + ")"
				v.column = nil
			}
		case "numeric", "int2", "int4", "int8", "float4", "float8":
			if v.kind != "number" || !v.literal && (v.column == nil || typemap.PGOracleBaseType(v.column.DataType) != t) {
				return sqlValue{}, fmt.Errorf("数值列类型转换 %s 无法证明保留精度和范围", t)
			}
			if v.literal && t != "numeric" {
				s := strings.ReplaceAll(strings.ReplaceAll(v.sql, "(", ""), ")", "")
				n, ok := new(big.Int).SetString(s, 10)
				bits := map[string]uint{"int2": 15, "int4": 31, "int8": 63}[t]
				if !ok || bits == 0 {
					return sqlValue{}, fmt.Errorf("数值常量到 %s 的舍入转换需要人工适配", t)
				}
				bound := new(big.Int).Lsh(big.NewInt(1), bits)
				if n.Cmp(new(big.Int).Neg(new(big.Int).Set(bound))) < 0 || n.Cmp(bound) >= 0 {
					return sqlValue{}, fmt.Errorf("数值常量超出 %s 范围", t)
				}
			}
		case "bool":
			if v.kind != "bool" {
				return sqlValue{}, fmt.Errorf("不安全的布尔类型转换")
			}
		default:
			if v.column == nil || typemap.PGOracleBaseType(v.column.DataType) != t {
				return sqlValue{}, fmt.Errorf("类型转换 %s 尚未支持", t)
			}
		}
	}
	return v, nil
}

func (p *sqlParser) function(fn string) (sqlValue, error) {
	agg := fn == "COUNT" || fn == "SUM" || fn == "AVG" || fn == "MIN" || fn == "MAX"
	if !agg && fn != "COALESCE" && fn != "NULLIF" {
		return sqlValue{}, fmt.Errorf("函数 %s 不在自动转换范围内", fn)
	}
	if agg && !p.aggregate {
		return sqlValue{}, fmt.Errorf("此处不允许聚合函数")
	}
	if fn == "COUNT" && p.take("*") {
		if err := p.require(")"); err != nil {
			return sqlValue{}, err
		}
		return sqlValue{sql: "COUNT(*)", kind: "number", aggregate: true}, nil
	}
	distinct := agg && p.take("DISTINCT")
	var args []sqlValue
	for {
		v, err := p.expression(1)
		if err != nil {
			return sqlValue{}, err
		}
		if v.kind == "lob" || v.empty {
			return sqlValue{}, fmt.Errorf("函数 %s 的 LOB/空字符串参数不能等价转换", fn)
		}
		if agg && v.aggregate {
			return sqlValue{}, fmt.Errorf("不支持嵌套聚合")
		}
		args = append(args, v)
		if !p.take(",") {
			break
		}
	}
	if err := p.require(")"); err != nil {
		return sqlValue{}, err
	}
	if agg && len(args) != 1 || fn == "NULLIF" && len(args) != 2 {
		return sqlValue{}, fmt.Errorf("函数 %s 参数数量不正确", fn)
	}
	if fn == "NULLIF" && args[0].kind == "null" {
		return sqlValue{}, fmt.Errorf("Oracle NULLIF 不接受无类型 NULL 作为首参数")
	}
	if (fn == "SUM" || fn == "AVG") && args[0].kind != "number" && args[0].kind != "null" {
		return sqlValue{}, fmt.Errorf("%s 仅支持数值列", fn)
	}
	if (fn == "MIN" || fn == "MAX") && args[0].kind == "bool" {
		return sqlValue{}, fmt.Errorf("不支持布尔 MIN/MAX")
	}
	result := sqlValue{kind: "null", aggregate: agg}
	values := make([]string, len(args))
	for i, arg := range args {
		if fn == "COALESCE" || fn == "NULLIF" {
			if err := compatible(result, arg); err != nil {
				return sqlValue{}, err
			}
		}
		if result.kind == "null" {
			result.kind = arg.kind
		}
		result.aggregate = result.aggregate || arg.aggregate
		values[i] = arg.value()
	}
	if fn == "COUNT" {
		result.kind = "number"
	}
	if fn == "COALESCE" && len(args) == 1 {
		return args[0], nil
	}
	prefix := ""
	if distinct {
		prefix = "DISTINCT "
	}
	result.sql = fn + "(" + prefix + strings.Join(values, ", ") + ")"
	return result, nil
}

func (p *sqlParser) caseValue() (sqlValue, error) {
	result := sqlValue{kind: "null"}
	var base *sqlValue
	if p.pos >= len(p.tokens) || !p.tokens[p.pos].Is("WHEN") {
		v, err := p.expression(1)
		if err != nil {
			return result, err
		}
		base = &v
	}
	var clauses []string
	for p.take("WHEN") {
		cond, err := p.expression(1)
		if err != nil {
			return result, err
		}
		if base != nil {
			if err := compatible(*base, cond); err != nil {
				return result, err
			}
			cond = sqlValue{sql: "(" + base.value() + " = " + cond.value() + ")", kind: "bool", predicate: true, aggregate: base.aggregate || cond.aggregate}
		}
		condition, err := cond.condition()
		if err != nil {
			return result, err
		}
		if err := p.require("THEN"); err != nil {
			return result, err
		}
		value, err := p.expression(1)
		if err != nil {
			return result, err
		}
		if err := compatible(result, value); err != nil {
			return result, err
		}
		if result.kind == "null" {
			result.kind = value.kind
		}
		result.aggregate = result.aggregate || cond.aggregate || value.aggregate
		clauses = append(clauses, "WHEN "+condition+" THEN "+value.value())
	}
	if len(clauses) == 0 {
		return result, fmt.Errorf("CASE 缺少 WHEN")
	}
	otherwise := "NULL"
	if p.take("ELSE") {
		value, err := p.expression(1)
		if err != nil {
			return result, err
		}
		if err := compatible(result, value); err != nil {
			return result, err
		}
		if result.kind == "null" {
			result.kind = value.kind
		}
		result.aggregate = result.aggregate || value.aggregate
		otherwise = value.value()
	}
	if err := p.require("END"); err != nil {
		return result, err
	}
	result.sql = "CASE " + strings.Join(clauses, " ") + " ELSE " + otherwise + " END"
	return p.cast(result)
}

// CheckExpression accepts a full CHECK clause or its expression body. It never
// executes or evaluates source SQL, and refuses all unconsumed suffixes.
func CheckExpression(expr string, columns []source.ColumnInfo, name func(string) string) (string, error) {
	if name == nil {
		name = func(s string) string { return s }
	}
	tokens, err := sqlsyntax.Lex(expr)
	if err != nil {
		return "", err
	}
	if len(tokens) > 0 && tokens[0].Is("CHECK") {
		tokens = tokens[1:]
	}
	p := sqlParser{tokens: tokens, name: name, resolve: func(parts []string) (sqlValue, error) {
		if len(parts) != 1 {
			return sqlValue{}, fmt.Errorf("CHECK 仅允许引用当前表的列")
		}
		for _, c := range columns {
			if c.Name == parts[0] {
				return columnValue(c, quote(name(c.Name)))
			}
		}
		return sqlValue{}, fmt.Errorf("CHECK 引用了未知列 %s", parts[0])
	}}
	v, err := p.parse()
	if err != nil {
		return "", err
	}
	return v.condition()
}

type viewContext struct {
	sourceSchema string
	targetSchema string
	relations    map[string][]source.ColumnInfo
	aliases      map[string]string
	deps         map[string]bool
	name         func(string) string
}

func (v *viewContext) resolve(parts []string) (sqlValue, error) {
	if len(parts) < 1 || len(parts) > 2 {
		return sqlValue{}, fmt.Errorf("不支持多层列引用")
	}
	var matched []sqlValue
	for alias, relation := range v.aliases {
		if len(parts) == 2 && alias != parts[0] {
			continue
		}
		for _, c := range v.relations[relation] {
			if c.Name == parts[len(parts)-1] {
				value, err := columnValue(c, quote(v.name(alias))+"."+quote(v.name(c.Name)))
				if err != nil {
					return sqlValue{}, err
				}
				matched = append(matched, value)
			}
		}
	}
	if len(matched) != 1 {
		return sqlValue{}, fmt.Errorf("列 %s 不存在或存在歧义", strings.Join(parts, "."))
	}
	return matched[0], nil
}
func (v *viewContext) expression(tokens []sqlsyntax.Token, aggregate bool) (sqlValue, error) {
	p := sqlParser{tokens: tokens, name: v.name, resolve: v.resolve, aggregate: aggregate}
	return p.parse()
}

type fromParser struct {
	tokens []sqlsyntax.Token
	pos    int
	ctx    *viewContext
}

func (p *fromParser) take(s string) bool {
	if p.pos < len(p.tokens) && p.tokens[p.pos].Is(s) {
		p.pos++
		return true
	}
	return false
}
func (p *fromParser) ident() (string, error) {
	if p.pos >= len(p.tokens) || p.tokens[p.pos].Kind != 'i' && p.tokens[p.pos].Kind != 'q' {
		return "", fmt.Errorf("缺少表或别名")
	}
	s := p.tokens[p.pos].Value
	p.pos++
	return s, nil
}
func (p *fromParser) relation() (string, error) {
	if p.take("(") {
		s, err := p.parse()
		if err != nil {
			return "", err
		}
		if !p.take(")") {
			return "", fmt.Errorf("JOIN 括号不匹配")
		}
		return "(" + s + ")", nil
	}
	table, err := p.ident()
	if err != nil {
		return "", err
	}
	if p.take(".") {
		if table != p.ctx.sourceSchema {
			return "", fmt.Errorf("跨 Schema 视图引用 %s 需要人工映射", table)
		}
		table, err = p.ident()
		if err != nil {
			return "", err
		}
	}
	if _, ok := p.ctx.relations[table]; !ok {
		return "", fmt.Errorf("引用表或视图 %s 不在已知源对象中", table)
	}
	alias := table
	if p.take("AS") {
		alias, err = p.ident()
		if err != nil {
			return "", err
		}
	} else if p.pos < len(p.tokens) && (p.tokens[p.pos].Kind == 'i' || p.tokens[p.pos].Kind == 'q') && !joinWord(p.tokens[p.pos]) && !p.tokens[p.pos].Is("ON") {
		alias, err = p.ident()
		if err != nil {
			return "", err
		}
	}
	if err := dialect.ValidatePGOracleName(p.ctx.name(alias)); err != nil {
		return "", err
	}
	for existing := range p.ctx.aliases {
		if p.ctx.name(existing) == p.ctx.name(alias) {
			return "", fmt.Errorf("表别名映射后冲突: %s", alias)
		}
	}
	p.ctx.aliases[alias] = table
	p.ctx.deps[table] = true
	s := quote(p.ctx.name(table))
	if p.ctx.targetSchema != "" {
		s = quote(p.ctx.targetSchema) + "." + s
	}
	return s + " " + quote(p.ctx.name(alias)), nil
}
func joinWord(t sqlsyntax.Token) bool {
	return t.Is("INNER") || t.Is("LEFT") || t.Is("RIGHT") || t.Is("FULL") || t.Is("CROSS") || t.Is("JOIN") || t.Is("NATURAL") || t.Is("USING")
}
func (p *fromParser) parse() (string, error) {
	left, err := p.relation()
	if err != nil {
		return "", err
	}
	for p.pos < len(p.tokens) && !p.tokens[p.pos].Is(")") {
		kind := "INNER"
		if p.take(",") {
			kind = "CROSS"
		} else if p.take("JOIN") {
		} else {
			found := false
			for _, candidate := range []string{"INNER", "LEFT", "RIGHT", "FULL", "CROSS"} {
				if p.take(candidate) {
					kind = candidate
					found = true
					break
				}
			}
			if !found {
				return "", fmt.Errorf("FROM 包含未支持的连接语法 %s", p.tokens[p.pos].Text)
			}
			if kind == "LEFT" || kind == "RIGHT" || kind == "FULL" {
				p.take("OUTER")
			}
			if !p.take("JOIN") {
				return "", fmt.Errorf("缺少 JOIN")
			}
		}
		right, err := p.relation()
		if err != nil {
			return "", err
		}
		if kind == "CROSS" {
			left = "(" + left + " CROSS JOIN " + right + ")"
			continue
		}
		if !p.take("ON") {
			return "", fmt.Errorf("仅支持显式 JOIN ON")
		}
		start, depth := p.pos, 0
		for p.pos < len(p.tokens) {
			t := p.tokens[p.pos]
			if depth == 0 && (t.Is(")") || joinWord(t) || t.Is(",")) {
				break
			}
			if t.Is("(") {
				depth++
			}
			if t.Is(")") {
				depth--
			}
			p.pos++
		}
		value, err := p.ctx.expression(p.tokens[start:p.pos], false)
		if err != nil {
			return "", err
		}
		cond, err := value.condition()
		if err != nil {
			return "", err
		}
		left = "(" + left + " " + kind + " JOIN " + right + " ON " + cond + ")"
	}
	return left, nil
}

func splitSQL(tokens []sqlsyntax.Token, separator string) [][]sqlsyntax.Token {
	var parts [][]sqlsyntax.Token
	start, depth := 0, 0
	for i, t := range tokens {
		if t.Is("(") {
			depth++
		}
		if t.Is(")") {
			depth--
		}
		if depth == 0 && t.Is(separator) {
			parts = append(parts, tokens[start:i])
			start = i + 1
		}
	}
	return append(parts, tokens[start:])
}
func trimOuter(tokens []sqlsyntax.Token) []sqlsyntax.Token {
	for len(tokens) > 1 && tokens[0].Is("(") && tokens[len(tokens)-1].Is(")") {
		depth, whole := 0, true
		for i, t := range tokens {
			if t.Is("(") {
				depth++
			}
			if t.Is(")") {
				depth--
			}
			if depth == 0 && i < len(tokens)-1 {
				whole = false
				break
			}
		}
		if !whole {
			break
		}
		tokens = tokens[1 : len(tokens)-1]
	}
	return tokens
}

// TranslateView compiles a known SELECT grammar into Oracle SQL. Unsupported
// syntax is an object-level error; source text is never passed through as SQL.
func TranslateView(view source.ViewInfo, sourceSchema, targetSchema string, relations map[string][]source.ColumnInfo, name func(string) string) (string, []string, error) {
	if name == nil {
		name = func(s string) string { return s }
	}
	tokens, err := sqlsyntax.Lex(view.Definition)
	if err != nil {
		return "", nil, err
	}
	if len(tokens) > 0 && tokens[len(tokens)-1].Is(";") {
		tokens = tokens[:len(tokens)-1]
	}
	tokens = trimOuter(tokens)
	if len(view.Columns) == 0 {
		return "", nil, fmt.Errorf("视图缺少结果列元数据")
	}
	var branches [][]sqlsyntax.Token
	var operators []string
	start, depth := 0, 0
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if t.Is("(") {
			depth++
		}
		if t.Is(")") {
			depth--
		}
		if depth == 0 && t.Is("UNION") {
			branches = append(branches, tokens[start:i])
			op := "UNION"
			if i+1 < len(tokens) && tokens[i+1].Is("ALL") {
				op += " ALL"
				i++
			} else if i+1 < len(tokens) && tokens[i+1].Is("DISTINCT") {
				i++
			}
			operators = append(operators, op)
			start = i + 1
		}
	}
	branches = append(branches, tokens[start:])
	deps := map[string]bool{}
	var sqls []string
	for _, branch := range branches {
		ctx := &viewContext{sourceSchema: sourceSchema, targetSchema: targetSchema, relations: relations, aliases: map[string]string{}, deps: deps, name: name}
		s, err := translateSelect(view, trimOuter(branch), ctx, len(branches) > 1)
		if err != nil {
			return "", nil, err
		}
		sqls = append(sqls, s)
	}
	result := sqls[0]
	for i, op := range operators {
		result += " " + op + " " + sqls[i+1]
	}
	dependencies := make([]string, 0, len(deps))
	for dep := range deps {
		dependencies = append(dependencies, dep)
	}
	sort.Strings(dependencies)
	return result, dependencies, nil
}

func translateSelect(view source.ViewInfo, tokens []sqlsyntax.Token, ctx *viewContext, setOperation bool) (string, error) {
	if len(tokens) == 0 || !tokens[0].Is("SELECT") {
		return "", fmt.Errorf("仅支持普通 SELECT，不支持 CTE、窗口或递归视图")
	}
	start := 1
	distinct := false
	if start < len(tokens) && tokens[start].Is("DISTINCT") {
		distinct = true
		start++
		if start < len(tokens) && tokens[start].Is("ON") {
			return "", fmt.Errorf("DISTINCT ON 需要人工适配")
		}
	} else if start < len(tokens) && tokens[start].Is("ALL") {
		start++
	}
	clauses := map[string][]sqlsyntax.Token{}
	current := "SELECT"
	begin := start
	depth := 0
	order := 0
	for i := start; i < len(tokens); i++ {
		t := tokens[i]
		if t.Is("(") {
			depth++
		}
		if t.Is(")") {
			depth--
			if depth < 0 {
				return "", fmt.Errorf("括号不匹配")
			}
		}
		if depth != 0 {
			continue
		}
		kind := ""
		rank := 0
		switch {
		case t.Is("FROM"):
			kind, rank = "FROM", 1
		case t.Is("WHERE"):
			kind, rank = "WHERE", 2
		case t.Is("GROUP"):
			kind, rank = "GROUP", 3
		case t.Is("HAVING"):
			kind, rank = "HAVING", 4
		case t.Is("ORDER"), t.Is("LIMIT"), t.Is("OFFSET"), t.Is("FETCH"), t.Is("WINDOW"), t.Is("INTERSECT"), t.Is("EXCEPT"):
			return "", fmt.Errorf("视图语法 %s 尚未支持", t.Text)
		}
		if kind != "" {
			if rank <= order {
				return "", fmt.Errorf("SELECT 子句顺序或数量不合法")
			}
			clauses[current] = tokens[begin:i]
			if kind == "GROUP" {
				i++
				if i >= len(tokens) || !tokens[i].Is("BY") {
					return "", fmt.Errorf("缺少 GROUP BY")
				}
			}
			current, begin, order = kind, i+1, rank
		}
	}
	if depth != 0 {
		return "", fmt.Errorf("括号不匹配")
	}
	clauses[current] = tokens[begin:]
	from := "DUAL"
	if ts, ok := clauses["FROM"]; ok {
		p := fromParser{tokens: ts, ctx: ctx}
		var err error
		from, err = p.parse()
		if err != nil {
			return "", err
		}
		if p.pos != len(ts) {
			return "", fmt.Errorf("FROM 包含未消费语法")
		}
	}
	parts := splitSQL(clauses["SELECT"], ",")
	if len(parts) != len(view.Columns) {
		return "", fmt.Errorf("投影列数量与视图元数据不一致，不支持通配符")
	}
	var projections []string
	seen := map[string]bool{}
	for i, ts := range parts {
		p := sqlParser{tokens: ts, name: ctx.name, resolve: ctx.resolve, aggregate: true}
		value, err := p.expression(1)
		if err != nil {
			return "", err
		}
		if p.take("AS") {
			if p.pos >= len(ts) || ts[p.pos].Kind != 'i' && ts[p.pos].Kind != 'q' {
				return "", fmt.Errorf("缺少投影别名")
			}
			p.pos++
		} else if p.pos+1 == len(ts) && (ts[p.pos].Kind == 'i' || ts[p.pos].Kind == 'q') {
			p.pos++
		}
		if p.pos != len(ts) {
			return "", fmt.Errorf("投影包含未支持语法 %s", ts[p.pos].Text)
		}
		column := view.Columns[i]
		mapped, err := columnValue(column, "")
		if err != nil {
			return "", err
		}
		if (distinct || setOperation) && (value.kind == "lob" || mapped.kind == "lob") {
			return "", fmt.Errorf("Oracle DISTINCT/UNION 不支持 LOB 结果列 %s", column.Name)
		}
		if value.kind != "null" && value.kind != mapped.kind && !(mapped.kind == "lob" && value.kind == "text") {
			return "", fmt.Errorf("视图列 %s 的结果类型 %s 与源元数据 %s 不符", column.Name, value.kind, mapped.kind)
		}
		n := ctx.name(column.Name)
		if err := dialect.ValidatePGOracleName(n); err != nil {
			return "", err
		}
		if seen[n] {
			return "", fmt.Errorf("视图列名映射后冲突: %s", n)
		}
		seen[n] = true
		projection := value.value()
		if value.kind == "null" {
			// Oracle must receive a concrete type for an all-NULL view column;
			// otherwise its inferred zero-length VARCHAR2 cannot preserve the
			// source view's declared result type for dependent views.
			targetType, err := typemap.PGToOracleChecked(column, false, 4, 4)
			if err != nil {
				return "", err
			}
			projection = "CAST(" + projection + " AS " + targetType + ")"
		}
		projections = append(projections, projection+" AS "+quote(n))
	}
	modifier := ""
	if distinct {
		modifier = "DISTINCT "
	}
	sql := "SELECT " + modifier + strings.Join(projections, ", ") + " FROM " + from
	if ts, ok := clauses["WHERE"]; ok {
		v, err := ctx.expression(ts, false)
		if err != nil {
			return "", err
		}
		cond, err := v.condition()
		if err != nil {
			return "", err
		}
		sql += " WHERE " + cond
	}
	if ts, ok := clauses["GROUP"]; ok {
		var values []string
		for _, part := range splitSQL(ts, ",") {
			ordinal := trimOuter(part)
			if len(ordinal) == 1 && ordinal[0].Kind == 'n' {
				return "", fmt.Errorf("GROUP BY 位置编号需要显式源表达式，不能转换为 Oracle 常量分组")
			}
			v, err := ctx.expression(part, false)
			if err != nil {
				return "", err
			}
			if v.kind == "lob" || v.empty {
				return "", fmt.Errorf("Oracle GROUP BY 不支持 LOB/空字符串表达式")
			}
			values = append(values, v.value())
		}
		sql += " GROUP BY " + strings.Join(values, ", ")
	}
	if ts, ok := clauses["HAVING"]; ok {
		v, err := ctx.expression(ts, true)
		if err != nil {
			return "", err
		}
		cond, err := v.condition()
		if err != nil {
			return "", err
		}
		sql += " HAVING " + cond
	}
	return sql, nil
}
