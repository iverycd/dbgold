package sqlserverview

import (
	"fmt"
	"sort"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
)

// ConvertOraclePostgres renders an explicit SELECT subset as T-SQL. Dependency
// names retain their source spelling. Cross-schema relations require manual
// adaptation because replacing their schema can bind to an unrelated object.
func ConvertOraclePostgres(body, sourceType, sourceSchema, targetSchema string, _ []string, name dialect.NameFunc, tables map[string]*source.TableDDLInfo) (string, []string, error) {
	if sourceType != "oracle" && !source.IsPGFamily(sourceType) {
		return "", nil, fmt.Errorf("不支持的 SQL Server 视图源库 %s", sourceType)
	}
	tokens, err := sqlsyntax.Lex(body)
	if err != nil {
		return "", nil, err
	}
	if len(tokens) > 0 && tokens[len(tokens)-1].Is(";") {
		tokens = tokens[:len(tokens)-1]
	}
	for _, t := range tokens {
		if t.Is(";") || t.Kind == 'q' && strings.HasPrefix(t.Text, "`") {
			return "", nil, fmt.Errorf("只支持单条源方言 SELECT 语句")
		}
	}
	if name == nil {
		name = func(s string) string { return s }
	}
	q := &foreignView{sourceType: sourceType, schema: sourceSchema, target: targetSchema, name: name, tables: tables, deps: map[string]bool{}}
	sql, err := q.selectSQL(viewTokens{tokens: tokens})
	deps := make([]string, 0, len(q.deps))
	for dependency := range q.deps {
		deps = append(deps, dependency)
	}
	sort.Strings(deps)
	if err != nil {
		return "", deps, err
	}
	return sql, deps, nil
}

type viewTokens struct {
	tokens []sqlsyntax.Token
	pos    int
}

func (p *viewTokens) peek() sqlsyntax.Token {
	if p.pos == len(p.tokens) {
		return sqlsyntax.Token{}
	}
	return p.tokens[p.pos]
}

func (p *viewTokens) take(s string) bool {
	if !p.peek().Is(s) {
		return false
	}
	p.pos++
	return true
}

func (p *viewTokens) expect(s string) error {
	if p.take(s) {
		return nil
	}
	return fmt.Errorf("需要 %s，实际为 %q", s, p.peek().Text)
}

// until leaves the next clause token unconsumed and ignores nested clauses.
func (p *viewTokens) until(end func(sqlsyntax.Token) bool) []sqlsyntax.Token {
	start, depth := p.pos, 0
	for p.pos < len(p.tokens) {
		t := p.peek()
		if depth == 0 && end(t) {
			break
		}
		if t.Is("(") {
			depth++
		} else if t.Is(")") {
			depth--
		}
		p.pos++
	}
	return p.tokens[start:p.pos]
}

type foreignView struct {
	sourceType, schema, target string
	name                       dialect.NameFunc
	tables                     map[string]*source.TableDDLInfo
	deps                       map[string]bool
	relations                  []viewRelation
}

type viewRelation struct{ name, alias, sqlAlias string }

type fromPart struct {
	sql   string
	on    []sqlsyntax.Token
	scope int
}

var viewDialect = dialect.NewSQLServer()

func (q *foreignView) identifier(p *viewTokens) (string, error) {
	t := p.peek()
	if !viewIdentifier(t) {
		return "", fmt.Errorf("需要源标识符，实际为 %q", t.Text)
	}
	p.pos++
	if t.Kind == 'i' {
		if q.sourceType == "oracle" {
			return strings.ToUpper(t.Text), nil
		}
		// PG folds ASCII letters in unquoted identifiers in UTF-8 databases.
		return strings.Map(func(r rune) rune {
			if r >= 'A' && r <= 'Z' {
				return r + 'a' - 'A'
			}
			return r
		}, t.Text), nil
	}
	return t.Value, nil
}

func keyword(t sqlsyntax.Token, words ...string) bool {
	for _, word := range words {
		if t.Is(word) {
			return true
		}
	}
	return false
}

func viewIdentifier(t sqlsyntax.Token) bool {
	return t.Kind == 'q' || t.Kind == 'i' && !keyword(t,
		"SELECT", "ALL", "DISTINCT", "FROM", "WHERE", "GROUP", "BY", "HAVING", "ORDER", "LIMIT", "OFFSET", "FETCH",
		"JOIN", "INNER", "LEFT", "RIGHT", "FULL", "OUTER", "CROSS", "NATURAL", "ON", "USING", "AS", "ONLY",
		"UNION", "INTERSECT", "EXCEPT", "MINUS", "WINDOW", "FILTER", "OVER", "FOR", "INTO", "WITH", "AND", "OR",
		"NOT", "IS", "IN", "BETWEEN", "LIKE", "TRUE", "FALSE", "NULL", "CONNECT", "START", "MODEL", "PIVOT")
}

func (q *foreignView) quote(s string) (string, error) {
	s = q.name(s)
	if err := dialect.ValidateSQLServerName(s); err != nil {
		return "", err
	}
	return viewDialect.QuoteIdent(s), nil
}

func selectClause(t sqlsyntax.Token) bool {
	return keyword(t, "WHERE", "GROUP", "HAVING", "ORDER", "LIMIT", "OFFSET", "FETCH", "UNION", "INTERSECT", "EXCEPT", "MINUS", "WINDOW", "FOR", "CONNECT", "START", "MODEL")
}

func (q *foreignView) selectSQL(p viewTokens) (string, error) {
	if err := p.expect("SELECT"); err != nil {
		return "", err
	}
	distinct := p.take("DISTINCT")
	if !distinct {
		p.take("ALL")
	}
	fields := viewTokens{tokens: p.until(func(t sqlsyntax.Token) bool { return t.Is("FROM") || selectClause(t) })}
	var from []fromPart
	if p.take("FROM") {
		var err error
		from, err = q.from(&p, 0)
		if err != nil {
			return "", err
		}
	}
	for _, table := range q.relations {
		if q.tables[table.name] == nil {
			return "", fmt.Errorf("无法确认视图依赖 %q 属于本次迁移的源 Schema", table.name)
		}
	}
	scope := len(q.relations)
	var projections []string
	for {
		v, err := q.expression(&fields, scope, 0, true, !distinct, 0)
		if err != nil {
			return "", err
		}
		if v.kind == viewCondition || distinct && !v.kind.comparable() {
			return "", fmt.Errorf("视图投影或 DISTINCT 的表达式类型无法安全转换")
		}
		if v.kind == viewNull && v.sql == "NULL" {
			return "", fmt.Errorf("视图投影的 NULL 缺少可安全映射的类型")
		}
		if fields.take("AS") || viewIdentifier(fields.peek()) {
			a, err := q.identifier(&fields)
			if err != nil {
				return "", err
			}
			if v.kind == viewStar {
				return "", fmt.Errorf("不支持通配列的别名")
			}
			alias, err := q.quote(a)
			if err != nil {
				return "", err
			}
			v.sql += " AS " + alias
		}
		projections = append(projections, v.sql)
		if !fields.take(",") {
			break
		}
	}
	if fields.pos != len(fields.tokens) {
		return "", fmt.Errorf("不支持的投影语法 %q", fields.peek().Text)
	}
	var out strings.Builder
	out.WriteString("SELECT ")
	if distinct {
		out.WriteString("DISTINCT ")
	}
	out.WriteString(strings.Join(projections, ", "))
	if len(from) != 0 {
		out.WriteString(" FROM ")
		for _, part := range from {
			out.WriteString(part.sql)
			if part.on != nil {
				expr := viewTokens{tokens: part.on}
				predicate, err := q.predicate(&expr, part.scope, false)
				if err != nil {
					return "", err
				}
				if expr.pos != len(expr.tokens) {
					return "", fmt.Errorf("不支持的 JOIN ON 条件语法 %q", expr.peek().Text)
				}
				out.WriteString(predicate)
			}
		}
	}
	if p.take("WHERE") {
		where, err := q.predicate(&p, scope, false)
		if err != nil {
			return "", err
		}
		out.WriteString(" WHERE " + where)
	}
	if p.take("GROUP") {
		if err := p.expect("BY"); err != nil {
			return "", err
		}
		var items []string
		for {
			v, err := q.reference(&p, scope, false)
			if err != nil {
				return "", err
			}
			if !v.kind.comparable() {
				return "", fmt.Errorf("GROUP BY 暂不支持该列类型或字符排序规则")
			}
			items = append(items, v.sql)
			if !p.take(",") {
				break
			}
		}
		out.WriteString(" GROUP BY " + strings.Join(items, ", "))
	}
	if p.take("HAVING") {
		having, err := q.predicate(&p, scope, true)
		if err != nil {
			return "", err
		}
		out.WriteString(" HAVING " + having)
	}
	if p.pos != len(p.tokens) {
		return "", fmt.Errorf("不支持的视图子句 %q", p.peek().Text)
	}
	return out.String(), nil
}

func (q *foreignView) from(p *viewTokens, depth int) ([]fromPart, error) {
	left, err := q.relation(p, depth)
	if err != nil {
		return nil, err
	}
	for keyword(p.peek(), "JOIN", "INNER", "LEFT", "RIGHT", "FULL", "CROSS") {
		join := "INNER"
		if !p.peek().Is("JOIN") {
			join = strings.ToUpper(p.peek().Text)
			p.pos++
			if join == "LEFT" || join == "RIGHT" || join == "FULL" {
				p.take("OUTER")
			}
		}
		if err := p.expect("JOIN"); err != nil {
			return nil, err
		}
		right, err := q.relation(p, depth)
		if err != nil {
			return nil, err
		}
		left = append(left, fromPart{sql: " " + join + " JOIN "})
		left = append(left, right...)
		if join != "CROSS" {
			if err := p.expect("ON"); err != nil {
				return nil, err
			}
			condition := p.until(func(t sqlsyntax.Token) bool {
				return selectClause(t) || keyword(t, ")", "JOIN", "INNER", "LEFT", "RIGHT", "FULL", "CROSS", "NATURAL", ",")
			})
			left = append(left, fromPart{sql: " ON ", on: condition, scope: len(q.relations)})
		}
	}
	return left, nil
}

func (q *foreignView) relation(p *viewTokens, depth int) ([]fromPart, error) {
	if depth >= 64 {
		return nil, fmt.Errorf("视图 JOIN 嵌套过深")
	}
	if p.take("(") {
		parts, err := q.from(p, depth+1)
		if err != nil {
			return nil, err
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
		if len(parts) == 1 {
			return parts, nil
		}
		return append(append([]fromPart{{sql: "("}}, parts...), fromPart{sql: ")"}), nil
	}
	table, err := q.identifier(p)
	if err != nil {
		return nil, err
	}
	if p.take(".") {
		owner := table
		table, err = q.identifier(p)
		if err != nil {
			return nil, err
		}
		if owner != q.schema {
			return nil, fmt.Errorf("视图引用了迁移范围之外的源 Schema %q", owner)
		}
	}
	q.deps[table] = true
	quoted, err := q.quote(table)
	if err != nil {
		return nil, err
	}
	schema := q.target
	if schema == "" {
		schema = "dbo"
	}
	if err := dialect.ValidateSQLServerName(schema); err != nil {
		return nil, err
	}
	sql := viewDialect.QuoteIdent(schema) + "." + quoted
	relation := viewRelation{name: table, alias: table, sqlAlias: quoted}
	if p.take("AS") || viewIdentifier(p.peek()) {
		relation.alias, err = q.identifier(p)
		if err != nil {
			return nil, err
		}
		relation.sqlAlias, err = q.quote(relation.alias)
		if err != nil {
			return nil, err
		}
		sql += " AS " + relation.sqlAlias
	}
	for _, prior := range q.relations {
		if prior.alias == relation.alias || strings.EqualFold(prior.sqlAlias, relation.sqlAlias) {
			return nil, fmt.Errorf("重复或冲突的视图表别名 %q", relation.alias)
		}
	}
	q.relations = append(q.relations, relation)
	return []fromPart{{sql: sql}}, nil
}
