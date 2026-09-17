package datamigrate

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
)

type pgViewParser struct {
	tokens           []sqlsyntax.Token
	pos              int
	schema, database string
	relations        map[string][]source.ColumnInfo
	aliases          map[string]string
	resolved         map[string]source.ColumnInfo
	dependencies     []string
	name             func(string) string
}

func (p *pgViewParser) take(s string) bool {
	if p.pos < len(p.tokens) && p.tokens[p.pos].Is(s) {
		p.pos++
		return true
	}
	return false
}
func (p *pgViewParser) ident() (string, error) {
	if p.pos >= len(p.tokens) {
		return "", fmt.Errorf("缺少对象名")
	}
	t := p.tokens[p.pos]
	if t.Kind != 'i' && t.Kind != 'q' {
		return "", fmt.Errorf("无效对象名")
	}
	p.pos++
	return t.Value, nil
}
func (p *pgViewParser) resolve(parts []string) (string, error) {
	col := parts[len(parts)-1]
	alias := ""
	if len(parts) == 2 {
		alias = parts[0]
	} else if len(parts) > 2 {
		return "", fmt.Errorf("不支持多层表达式引用")
	}
	var matches []string
	for a, relation := range p.aliases {
		if alias != "" && a != alias {
			continue
		}
		for _, c := range p.relations[relation] {
			if c.Name == col {
				matches = append(matches, a)
			}
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("列 %s 不存在或存在歧义", strings.Join(parts, "."))
	}
	rendered := sqlsyntax.QuoteIdent(p.name(matches[0])) + "." + sqlsyntax.QuoteIdent(p.name(col))
	if p.resolved == nil {
		p.resolved = map[string]source.ColumnInfo{}
	}
	for _, c := range p.relations[p.aliases[matches[0]]] {
		if c.Name == col {
			p.resolved[rendered] = c
			break
		}
	}
	return rendered, nil
}

func (p *pgViewParser) castReference(reference, target string) (string, error) {
	c, ok := p.resolved[reference]
	if !ok || target != "text" {
		return "", fmt.Errorf("仅支持字符列到 text 的安全转换，当前目标类型 %s", target)
	}
	switch strings.ToLower(c.DataType) {
	case "text", "varchar", "character varying", "uuid":
		c.DataType = "text"
		p.resolved[reference] = c
		return reference, nil
	case "bpchar", "char", "character":
		// PG strips padding when converting CHAR to text.
		rendered := "RTRIM(" + reference + ")"
		c.DataType = "text"
		p.resolved[rendered] = c
		return rendered, nil
	default:
		return "", fmt.Errorf("列 %s 的 %s → text 转换需人工适配，不能直接移除类型转换", c.Name, c.DataType)
	}
}

func (p *pgViewParser) bitLiteral(bits string) (string, error) {
	if p.resolved == nil {
		p.resolved = map[string]source.ColumnInfo{}
	}
	rendered := "b'" + bits + "'"
	p.resolved[rendered] = source.ColumnInfo{DataType: "bit", Length: int64(len(bits))}
	return rendered, nil
}

func (p *pgViewParser) compare(left, operator, right string) error {
	a, b := p.resolved[left], p.resolved[right]
	if a.DataType == "bit" || b.DataType == "bit" {
		if a.DataType != "bit" || b.DataType != "bit" || a.Length != b.Length {
			return fmt.Errorf("位串 %s 比较只支持等长的 BIT 列或位串常量，避免数值比较改变源语义", operator)
		}
	}
	return nil
}

// Text COALESCE preserves argument order and NULL handling. Avoid MySQL's
// implicit mixing of strings, numbers and dates by checking each argument.
func (p *pgViewParser) coalesceProjection(ts []sqlsyntax.Token, result source.ColumnInfo) (string, bool, error) {
	if len(ts) < 2 || !ts[0].Is("COALESCE") || !ts[1].Is("(") {
		return "", false, nil
	}
	if !pgTextExpression(result) {
		return "", true, fmt.Errorf("COALESCE 目前仅支持文本结果，当前为 %s", result.DataType)
	}
	var args [][]sqlsyntax.Token
	start, depth := 2, 1
	for i := 2; i < len(ts); i++ {
		if ts[i].Is("(") {
			depth++
		}
		if ts[i].Is(")") {
			depth--
		}
		if depth == 0 {
			if i != len(ts)-1 {
				return "", true, fmt.Errorf("COALESCE 后存在尚不支持的表达式")
			}
			args = append(args, ts[start:i])
			break
		}
		if depth == 1 && ts[i].Is(",") {
			args = append(args, ts[start:i])
			start = i + 1
		}
	}
	if depth != 0 || len(args) == 0 {
		return "", true, fmt.Errorf("COALESCE 参数或括号不完整")
	}
	var rendered []string
	for _, arg := range args {
		if nested, found, err := p.coalesceProjection(arg, result); found {
			if err != nil {
				return "", true, err
			}
			rendered = append(rendered, nested)
			continue
		}
		refs := 0
		ex := sqlsyntax.Expr{Tokens: arg, TypedLiteral: true, CastReference: p.castReference, Resolve: func(parts []string) (string, error) { refs++; return p.resolve(parts) }}
		s, err := ex.Parse()
		if err != nil {
			return "", true, err
		}
		if refs > 0 {
			c, ok := p.resolved[s]
			if !ok || refs != 1 || !pgTextExpression(c) {
				return "", true, fmt.Errorf("COALESCE 参数必须是文本列、文本常量或 NULL")
			}
		} else {
			lit, err := sqlsyntax.Lex(s)
			if err != nil || len(lit) != 1 || !(lit[0].Kind == 's' || lit[0].Is("NULL")) {
				return "", true, fmt.Errorf("COALESCE 不允许隐式混合文本与其他类型")
			}
		}
		rendered = append(rendered, s)
	}
	// A one-argument COALESCE is exactly its argument, including NULL.
	if len(rendered) == 1 {
		return rendered[0], true, nil
	}
	return "COALESCE(" + strings.Join(rendered, ", ") + ")", true, nil
}

func pgTextExpression(c source.ColumnInfo) bool {
	switch strings.ToLower(c.DataType) {
	case "text", "varchar", "character varying":
		return true
	default:
		return false
	}
}

// integerOffset handles the integer-column + integer-literal projection used
// by UNION views. It deliberately excludes floating point and string coercion.
func (p *pgViewParser) integerOffset(tokens []sqlsyntax.Token, result source.ColumnInfo) (string, bool, error) {
	for len(tokens) > 1 && tokens[0].Is("(") && tokens[len(tokens)-1].Is(")") {
		depth, wraps := 0, true
		for i, t := range tokens {
			if t.Is("(") {
				depth++
			}
			if t.Is(")") {
				depth--
			}
			if depth == 0 && i != len(tokens)-1 {
				wraps = false
				break
			}
		}
		if !wraps {
			break
		}
		tokens = tokens[1 : len(tokens)-1]
	}
	plus, depth := -1, 0
	for i, t := range tokens {
		if t.Is("(") {
			depth++
		}
		if t.Is(")") {
			depth--
		}
		if t.Is("+") && depth == 0 && i > 0 {
			if plus >= 0 {
				return "", true, fmt.Errorf("仅支持整数列与单个整数常量相加")
			}
			plus = i
		}
	}
	if plus < 0 {
		return "", false, nil
	}
	fail := func() (string, bool, error) {
		return "", true, fmt.Errorf("仅支持整数列与整数常量相加，且视图结果须为整数类型")
	}
	if !pgIntegerColumn(result) {
		return fail()
	}
	integer := func(ts []sqlsyntax.Token) (string, bool) {
		sign := ""
		if len(ts) == 2 && (ts[0].Is("-") || ts[0].Is("+")) {
			sign = ts[0].Text
			ts = ts[1:]
		}
		if len(ts) != 1 || ts[0].Kind != 'n' {
			return "", false
		}
		n, err := strconv.ParseInt(sign+ts[0].Text, 10, 64)
		return strconv.FormatInt(n, 10), err == nil
	}
	left, right := tokens[:plus], tokens[plus+1:]
	literal, ok := integer(left)
	column := right
	if !ok {
		literal, ok = integer(right)
		column = left
	}
	if !ok {
		return fail()
	}
	refs := 0
	ex := sqlsyntax.Expr{Tokens: column, Resolve: func(parts []string) (string, error) { refs++; return p.resolve(parts) }}
	s, err := ex.Parse()
	if err != nil {
		return "", true, err
	}
	c, ok := p.resolved[s]
	if !ok || refs != 1 || !pgIntegerColumn(c) {
		return fail()
	}
	return "CAST((" + s + " + " + literal + ") AS SIGNED)", true, nil
}

func pgIntegerColumn(c source.ColumnInfo) bool {
	switch strings.ToLower(c.DataType) {
	case "int2", "int4", "int8", "smallint", "integer", "int", "bigint":
		return true
	default:
		return false
	}
}

func (p *pgViewParser) relation() (string, error) {
	if p.take("(") {
		s, e := p.from()
		if e != nil {
			return "", e
		}
		if !p.take(")") {
			return "", fmt.Errorf("FROM 括号不匹配")
		}
		return "(" + s + ")", nil
	}
	first, e := p.ident()
	if e != nil {
		return "", e
	}
	table := first
	if p.take(".") {
		if first != p.schema {
			return "", fmt.Errorf("视图引用跨 schema 对象 %s", first)
		}
		table, e = p.ident()
		if e != nil {
			return "", e
		}
	}
	if _, ok := p.relations[table]; !ok {
		return "", fmt.Errorf("视图依赖 %s 不在迁移范围内", table)
	}
	alias := table
	if p.take("AS") {
		alias, e = p.ident()
		if e != nil {
			return "", e
		}
	} else if p.pos < len(p.tokens) {
		t := p.tokens[p.pos]
		if (t.Kind == 'i' || t.Kind == 'q') && !t.Is("INNER") && !t.Is("LEFT") && !t.Is("JOIN") && !t.Is("ON") && !t.Is("WHERE") && !t.Is("CROSS") && !t.Is("RIGHT") && !t.Is("FULL") && !t.Is("NATURAL") && !t.Is("USING") {
			alias, e = p.ident()
			if e != nil {
				return "", e
			}
		}
	}
	for existing := range p.aliases {
		if strings.EqualFold(p.name(existing), p.name(alias)) {
			return "", fmt.Errorf("视图别名映射后冲突: %s", alias)
		}
	}
	p.aliases[alias] = table
	p.dependencies = append(p.dependencies, table)
	return sqlsyntax.QuoteIdent(p.database) + "." + sqlsyntax.QuoteIdent(p.name(table)) + " AS " + sqlsyntax.QuoteIdent(p.name(alias)), nil
}
func (p *pgViewParser) from() (string, error) {
	left, e := p.relation()
	if e != nil {
		return "", e
	}
	for p.pos < len(p.tokens) {
		join := ""
		if p.take("INNER") {
			join = "INNER"
			if !p.take("JOIN") {
				return "", fmt.Errorf("缺少 JOIN")
			}
		} else if p.take("LEFT") {
			join = "LEFT"
			p.take("OUTER")
			if !p.take("JOIN") {
				return "", fmt.Errorf("缺少 JOIN")
			}
		} else if p.take("JOIN") {
			join = "INNER"
		} else if p.take("CROSS") {
			join = "CROSS"
			if !p.take("JOIN") {
				return "", fmt.Errorf("缺少 JOIN")
			}
		} else {
			break
		}
		right, e := p.relation()
		if e != nil {
			return "", e
		}
		if join == "CROSS" {
			// Parenthesize left-to-right joins explicitly. A later ON may
			// reference either side of the preceding CROSS JOIN.
			left = "(" + left + " CROSS JOIN " + right + ")"
			continue
		}
		if !p.take("ON") {
			return "", fmt.Errorf("仅支持显式 JOIN ON")
		}
		start := p.pos
		depth := 0
		for p.pos < len(p.tokens) {
			t := p.tokens[p.pos]
			if depth == 0 && (t.Is(")") || t.Is("JOIN") || t.Is("INNER") || t.Is("LEFT") || t.Is("CROSS")) {
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
		ex := sqlsyntax.Expr{Tokens: p.tokens[start:p.pos], Resolve: p.resolve, CastReference: p.castReference, BitLiteral: p.bitLiteral, Compare: p.compare}
		on, e := ex.Parse()
		if e != nil {
			return "", e
		}
		left = "(" + left + " " + join + " JOIN " + right + " ON " + on + ")"
	}
	return left, nil
}
func translatePGView(v source.ViewInfo, schema, database string, relations map[string][]source.ColumnInfo, name func(string) string) (string, []string, error) {
	ts, e := sqlsyntax.Lex(v.Definition)
	if e != nil {
		return "", nil, e
	}
	if len(ts) > 0 && ts[len(ts)-1].Is(";") {
		ts = ts[:len(ts)-1]
	}
	var branches [][]sqlsyntax.Token
	var operators []string
	start, depth := 0, 0
	for i := 0; i < len(ts); i++ {
		t := ts[i]
		if t.Is(";") {
			return "", nil, fmt.Errorf("视图定义不能包含多条语句")
		}
		if t.Is("(") {
			depth++
		}
		if t.Is(")") {
			depth--
			if depth < 0 {
				return "", nil, fmt.Errorf("括号不匹配")
			}
		}
		if depth == 0 && t.Is("UNION") {
			branches = append(branches, ts[start:i])
			op := "UNION"
			if i+1 < len(ts) && (ts[i+1].Is("ALL") || ts[i+1].Is("DISTINCT")) {
				i++
				op += " " + strings.ToUpper(ts[i].Text)
			}
			operators = append(operators, op)
			start = i + 1
		}
	}
	if depth != 0 {
		return "", nil, fmt.Errorf("括号不匹配")
	}
	branches = append(branches, ts[start:])
	var result string
	var deps []string
	for i, branch := range branches {
		sql, refs, err := translatePGSelect(v, branch, schema, database, relations, name)
		if err != nil {
			return "", nil, err
		}
		if i > 0 {
			result += " " + operators[i-1] + " "
		}
		result += sql
		deps = append(deps, refs...)
	}
	return result, deps, nil
}

func translatePGSelect(v source.ViewInfo, ts []sqlsyntax.Token, schema, database string, relations map[string][]source.ColumnInfo, name func(string) string) (string, []string, error) {
	if len(ts) < 4 || !ts[0].Is("SELECT") {
		return "", nil, fmt.Errorf("仅支持简单 SELECT 视图")
	}
	projectionStart, modifier := 1, ""
	if ts[1].Is("DISTINCT") {
		projectionStart, modifier = 2, "DISTINCT "
		if ts[2].Is("ON") {
			return "", nil, fmt.Errorf("DISTINCT ON 需按分组取行规则人工适配，不能替换为普通 DISTINCT")
		}
	} else if ts[1].Is("ALL") {
		projectionStart, modifier = 2, "ALL "
	}
	from, where := -1, -1
	depth := 0
	for i, t := range ts {
		if t.Is("(") {
			depth++
		}
		if t.Is(")") {
			depth--
		}
		if depth == 0 && t.Is("FROM") {
			if from != -1 {
				return "", nil, fmt.Errorf("多个 FROM 不支持")
			}
			from = i
		}
		if depth == 0 && t.Is("WHERE") {
			where = i
		}
	}
	if from < 0 {
		return "", nil, fmt.Errorf("视图缺少 FROM")
	}
	end := len(ts)
	if where >= 0 {
		end = where
	}
	p := pgViewParser{tokens: ts[from+1 : end], schema: schema, database: database, relations: relations, aliases: map[string]string{}, name: name}
	fromSQL, e := p.from()
	if e != nil {
		return "", nil, e
	}
	if p.pos != len(p.tokens) {
		return "", nil, fmt.Errorf("FROM 包含不支持语法")
	}
	segments := [][]sqlsyntax.Token{}
	start := projectionStart
	depth = 0
	for i := projectionStart; i < from; i++ {
		t := ts[i]
		if t.Is("(") {
			depth++
		}
		if t.Is(")") {
			depth--
		}
		if t.Is(",") && depth == 0 {
			segments = append(segments, ts[start:i])
			start = i + 1
		}
	}
	segments = append(segments, ts[start:from])
	if len(segments) != len(v.Columns) {
		return "", nil, fmt.Errorf("投影列数量与源视图不匹配（不支持通配符）")
	}
	projections := []string{}
	columnNames := map[string]bool{}
	for i, segment := range segments {
		columnName := name(v.Columns[i].Name)
		key := strings.ToLower(columnName)
		if columnNames[key] || columnName == "" || utf8.RuneCountInString(columnName) > 64 || strings.ContainsRune(columnName, 0) {
			return "", nil, fmt.Errorf("视图列名映射后冲突或超出 MySQL 限制")
		}
		for _, r := range columnName {
			if r > 0xffff {
				return "", nil, fmt.Errorf("视图列名含 MySQL 不支持的字符")
			}
		}
		columnNames[key] = true
		if len(segment) >= 2 && segment[len(segment)-2].Is("AS") {
			segment = segment[:len(segment)-2]
		}
		if s, found, err := p.coalesceProjection(segment, v.Columns[i]); found {
			if err != nil {
				return "", nil, err
			}
			projections = append(projections, s+" AS "+sqlsyntax.QuoteIdent(columnName))
			continue
		}
		if s, found, e := p.integerOffset(segment, v.Columns[i]); found {
			if e != nil {
				return "", nil, e
			}
			projections = append(projections, s+" AS "+sqlsyntax.QuoteIdent(columnName))
			continue
		}
		refs := 0
		ex := sqlsyntax.Expr{Tokens: segment, TypedLiteral: true, CastReference: p.castReference, Resolve: func(parts []string) (string, error) { refs++; return p.resolve(parts) }}
		s, e := ex.Parse()
		if e != nil {
			return "", nil, e
		}
		_, columnProjection := p.resolved[s]
		if refs > 0 && (refs != 1 || !columnProjection) {
			return "", nil, fmt.Errorf("仅支持列投影或安全常量投影")
		}
		if refs == 0 {
			s, e = pgViewConstant(s, v.Columns[i])
			if e != nil {
				return "", nil, e
			}
		}
		projections = append(projections, s+" AS "+sqlsyntax.QuoteIdent(name(v.Columns[i].Name)))
	}
	result := "SELECT " + modifier + strings.Join(projections, ", ") + " FROM " + fromSQL
	if where >= 0 {
		ex := sqlsyntax.Expr{Tokens: ts[where+1:], Resolve: p.resolve, CastReference: p.castReference, BitLiteral: p.bitLiteral, Compare: p.compare}
		s, e := ex.Parse()
		if e != nil {
			return "", nil, e
		}
		result += " WHERE " + s
	}
	return result, p.dependencies, nil
}

func (m *Migrator) planPGViews(p *PGMySQLPreflight, selected map[string]bool, excluded map[ObjectRef]bool, register func(string, string, string)) {
	relations := map[string][]source.ColumnInfo{}
	viewSet := map[string]source.ViewInfo{}
	want := map[string]bool{}
	for _, v := range m.cfg.ViewNames {
		want[v] = true
	}
	for _, t := range p.snapshot.Tables {
		if selected[t.Info.TableName] || len(m.cfg.ViewNames) > 0 {
			relations[t.Info.TableName] = t.Info.Columns
		}
	}
	for _, v := range p.snapshot.Views {
		if len(want) > 0 && !want[v.ViewName] {
			continue
		}
		if excluded[ObjectRef{Kind: "view", Name: v.ViewName}] {
			continue
		}
		relations[v.ViewName] = v.Columns
		viewSet[v.ViewName] = v
	}
	for n := range want {
		if _, ok := viewSet[n]; !ok && !excluded[ObjectRef{Kind: "view", Name: n}] {
			p.add(ObjectRef{Kind: "view", Name: n}, "所选视图不存在", false)
		}
	}
	type translated struct {
		sql  string
		deps []string
	}
	compiled := map[string]translated{}
	for _, v := range p.snapshot.Views {
		if _, ok := viewSet[v.ViewName]; !ok {
			continue
		}
		ref := ObjectRef{Kind: "view", Name: v.ViewName}
		register("relation", m.objName(v.ViewName), v.ViewName)
		sql, deps, e := translatePGView(v, p.Source.Schema, p.Target.Database, relations, m.objName)
		if e != nil {
			p.add(ref, e.Error(), true)
			if len(p.Issues) > 0 {
				p.Issues[len(p.Issues)-1].Definition = v.Definition
			}
			continue
		}
		compiled[v.ViewName] = translated{sql, deps}
	}
	state := map[string]int{}
	baseDeps := map[string]bool{}
	var visit func(string) error
	visit = func(n string) error {
		if state[n] == 2 {
			return nil
		}
		if state[n] == 1 {
			return fmt.Errorf("视图依赖循环 %s", n)
		}
		c, ok := compiled[n]
		if !ok {
			return fmt.Errorf("依赖视图 %s 未通过预检", n)
		}
		state[n] = 1
		for _, d := range c.deps {
			if _, isView := viewSet[d]; isView {
				if e := visit(d); e != nil {
					return e
				}
			} else {
				baseDeps[d] = true
			}
		}
		state[n] = 2
		p.Objects = append(p.Objects, PlannedObject{ObjectRef{Kind: "view", Name: n}, m.objName(n), "view", []dialect.Statement{{SQL: "CREATE OR REPLACE VIEW " + sqlsyntax.QuoteIdent(p.Target.Database) + "." + sqlsyntax.QuoteIdent(m.objName(n)) + " AS " + c.sql}}})
		return nil
	}
	for _, v := range p.snapshot.Views {
		if _, ok := compiled[v.ViewName]; ok {
			if e := visit(v.ViewName); e != nil {
				p.add(ObjectRef{Kind: "view", Name: v.ViewName}, e.Error(), true)
			}
		}
	}
	if len(m.cfg.ViewNames) > 0 {
		for _, t := range p.snapshot.Tables {
			if baseDeps[t.Info.TableName] {
				p.tables = append(p.tables, t)
			}
		}
	}
}

// Constant casts in a view must retain a target type, not just strip ::type and
// accidentally expose a timestamp/UUID expression as an arbitrary string.
func pgViewConstant(rendered string, c source.ColumnInfo) (string, error) {
	typ, e := typemap.PGToMySQLChecked(c)
	if e != nil {
		return "", e
	}
	ts, e := sqlsyntax.Lex(rendered)
	if e != nil {
		return "", e
	}
	var value any
	if len(ts) == 1 {
		if ts[0].Is("NULL") {
			return "NULL", nil
		}
		if ts[0].Kind == 's' || ts[0].Kind == 'n' {
			value = ts[0].Value
		}
	} else if len(ts) == 2 && (ts[0].Is("-") || ts[0].Is("+")) && ts[1].Kind == 'n' {
		value = ts[0].Text + ts[1].Text
	}
	if value == nil {
		return "", fmt.Errorf("常量投影只支持安全字面量转换")
	}
	converted, e := valueconv.PGMySQLValue(value, c)
	if e != nil {
		return "", e
	}
	literal := fmt.Sprint(converted)
	cast := "SIGNED"
	switch {
	case typ == "DATE" || strings.HasPrefix(typ, "DATETIME(") || strings.HasPrefix(typ, "TIME("):
		literal = sqlsyntax.QuoteString(literal)
		cast = typ
	case strings.Contains(typ, "CHAR") || typ == "LONGTEXT":
		literal = sqlsyntax.QuoteString(literal)
		cast = "CHAR CHARACTER SET utf8mb4"
	case strings.HasPrefix(typ, "DECIMAL"):
		cast = typ
	case typ == "FLOAT" || typ == "DOUBLE":
		return "", fmt.Errorf("浮点常量投影首版不自动转换")
	case typ == "JSON" || typ == "LONGBLOB":
		return "", fmt.Errorf("大对象常量投影首版不自动转换")
	case strings.HasPrefix(typ, "BIT("):
		return "", fmt.Errorf("位串常量投影尚不支持自动转换")
	}
	return "CAST(" + literal + " AS " + cast + ")", nil
}
