package oracledameng

import (
	"fmt"
	"strings"
)

const maxViewDerivedNesting = 60

// Query nodes refer to the shared token stream. Parsing discovers all physical
// dependencies without binding columns or emitting edits. A derived query then
// binds in isolation and exposes only its output columns to its parent.
type viewQuery struct {
	branches []*viewBranch
	order    int
	end      int
}

type viewBranch struct {
	start, end, from, fromEnd int
	nesting                   int
	relations                 []viewRelation
	declarations              map[int]bool
}

func (c *viewConverter) parseQuery(start, end, nesting int) (*viewQuery, error) {
	if nesting > maxViewDerivedNesting {
		return nil, fmt.Errorf("视图 FROM 子查询嵌套不能超过 %d 层", maxViewDerivedNesting)
	}
	ts := c.ts
	if start >= end || !ts[start].is("SELECT") {
		return nil, fmt.Errorf("视图查询及 UNION 分支必须是 SELECT")
	}
	var unions []int
	order, depth := end, 0
	for i := start; i < end; i++ {
		switch ts[i].text {
		case "(":
			depth++
		case ")":
			depth--
		default:
			if depth == 0 && ts[i].is("UNION") {
				if order != end {
					return nil, fmt.Errorf("UNION 的 ORDER BY 必须位于全部分支之后")
				}
				unions = append(unions, i)
			}
			if depth == 0 && ts[i].is("ORDER") && i+1 < end && ts[i+1].is("BY") {
				order = i
			}
		}
	}
	q := &viewQuery{order: end, end: end}
	bodyEnd := end
	if len(unions) > 0 {
		q.order, bodyEnd = order, order
	}
	spans := make([][2]int, 0, len(unions)+1)
	first := start
	for _, union := range unions {
		spans = append(spans, [2]int{first, union})
		first = union + 1
		if first < bodyEnd && (ts[first].is("ALL") || ts[first].is("DISTINCT")) {
			first++
		}
	}
	spans = append(spans, [2]int{first, bodyEnd})
	for _, span := range spans {
		b, err := c.parseBranch(span[0], span[1], nesting)
		if err != nil {
			return nil, err
		}
		q.branches = append(q.branches, b)
	}
	return q, nil
}

func (c *viewConverter) parseBranch(start, end, nesting int) (*viewBranch, error) {
	ts := c.ts
	if start >= end || !ts[start].is("SELECT") {
		return nil, fmt.Errorf("UNION 分支必须是 SELECT")
	}
	from, depth := -1, 0
	for i := start + 1; i < end; i++ {
		if ts[i].text == "(" {
			depth++
		} else if ts[i].text == ")" {
			depth--
		} else if depth == 0 && ts[i].is("FROM") {
			from = i
			break
		}
	}
	if from < 0 {
		return nil, fmt.Errorf("Oracle 视图 SELECT 缺少 FROM")
	}
	b := &viewBranch{start: start, end: end, from: from, nesting: nesting, declarations: map[int]bool{}}
	fromEnd, err := c.parseRelationGroup(from+1, end, b)
	if err != nil {
		return nil, err
	}
	b.fromEnd = fromEnd
	return b, nil
}

func (c *viewConverter) parseRelationGroup(start, end int, b *viewBranch) (int, error) {
	ts := c.ts
	initialRelations := len(b.relations)
	wantRelation, inUsing, depth := true, false, 0
	fromEnd := end
	for i := start; i < end; {
		t := ts[i]
		if depth == 0 && (t.is("WHERE") || t.is("GROUP") || t.is("HAVING") || t.is("ORDER")) {
			fromEnd = i
			break
		}
		if wantRelation {
			r := viewRelation{start: i, aliasIndex: -1}
			if t.text == "(" {
				close := viewClosingParen(ts, i, end)
				if close < 0 {
					return fromEnd, fmt.Errorf("视图括号不匹配")
				}
				if i+1 < close && ts[i+1].is("SELECT") {
					query, err := c.parseQuery(i+1, close, b.nesting+1)
					if err != nil {
						return fromEnd, err
					}
					r.query, r.end = query, close+1
				} else {
					groupEnd, err := c.parseRelationGroup(i+1, close, b)
					if err != nil {
						return fromEnd, err
					}
					if groupEnd != close {
						return fromEnd, fmt.Errorf("视图关系分组中不支持查询子句")
					}
					i = close + 1
					if i < end && (ts[i].is("AS") || ts[i].ident() && !viewKeyword(ts[i])) {
						return fromEnd, fmt.Errorf("视图暂不支持关系分组别名")
					}
					wantRelation = false
					continue
				}
			} else {
				if !t.ident() || viewKeyword(t) {
					return fromEnd, fmt.Errorf("视图 FROM 仅支持普通表、视图或 SELECT 子查询")
				}
				r.table, r.end = t, i+1
				if r.end < end && ts[r.end].text == "." {
					if r.end+1 >= end || !ts[r.end+1].ident() || !t.matches(c.source) {
						return fromEnd, fmt.Errorf("视图不支持跨 owner 引用 %s", t.text)
					}
					r.table = ts[r.end+1]
					r.end += 2
				}
				if r.end < end && (ts[r.end].text == "." || ts[r.end].text == "(") {
					return fromEnd, fmt.Errorf("视图不支持多级对象或表函数")
				}
				r.name = r.table.name
				if r.table.kind != 'q' {
					r.name = strings.ToUpper(r.name)
				}
				r.qualifier = r.name
				_, exists := c.catalog[r.name]
				r.dual = r.name == "DUAL" && r.end == i+1 && !exists
				// Discovery must report a possible owner-local DUAL; binding
				// distinguishes it from the built-in relation.
				if (!r.dual || c.catalog == nil) && !c.seenDeps[r.name] {
					c.deps = append(c.deps, r.name)
					c.seenDeps[r.name] = true
				}
			}
			// The parent must never bind child tokens a second time.
			for j := r.start; j < r.end; j++ {
				b.declarations[j] = true
			}
			i = r.end
			if i < end && ts[i].is("AS") {
				return fromEnd, fmt.Errorf("Oracle 表别名不支持 AS")
			}
			if i < end && ts[i].ident() && !viewKeyword(ts[i]) {
				r.alias, r.qualifier, r.aliasIndex = ts[i], ts[i].name, i
				b.declarations[i] = true
				i++
			}
			b.relations = append(b.relations, r)
			wantRelation = false
			continue
		}
		if depth == 0 && (t.text == "," || t.is("JOIN")) {
			wantRelation = true
		}
		if t.is("USING") {
			inUsing = true
		}
		if t.is("NATURAL") {
			return fromEnd, fmt.Errorf("视图暂不支持 NATURAL JOIN，请显式使用 ON")
		}
		if t.text == "(" {
			depth++
		} else if t.text == ")" {
			depth--
		}
		i++
	}
	if wantRelation || len(b.relations) == initialRelations {
		return fromEnd, fmt.Errorf("视图缺少 FROM/JOIN 对象")
	}
	if inUsing {
		return fromEnd, fmt.Errorf("视图暂不支持 JOIN USING，请显式使用 ON")
	}
	return fromEnd, nil
}

func (c *viewConverter) bindQuery(q *viewQuery) ([]MappedColumn, error) {
	var columns []MappedColumn
	var firstErr error
	for i, b := range q.branches {
		cols, err := c.branch(b)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if i == 0 {
			columns = cols
		} else if c.catalog != nil && err == nil && firstErr == nil && len(cols) != len(columns) {
			firstErr = fmt.Errorf("UNION 各分支输出列数不一致")
		}
	}
	if firstErr != nil {
		return columns, firstErr
	}
	if q.order < q.end {
		aliases := map[string]MappedColumn{}
		for _, column := range columns {
			aliases[strings.ToUpper(column.Source)] = column
		}
		if err := c.expression(q.order, q.end, nil, aliases); err != nil {
			return columns, err
		}
	}
	return columns, nil
}

func (c *viewConverter) bindRelations(declarations []viewRelation) ([]viewRelation, error) {
	for i, r := range declarations {
		if r.qualifier == "" {
			continue
		}
		for _, prev := range declarations[:i] {
			if strings.EqualFold(prev.qualifier, r.qualifier) {
				return nil, fmt.Errorf("视图关系别名大写命名冲突: %s", r.qualifier)
			}
		}
	}
	relations := append([]viewRelation(nil), declarations...)
	for i := range relations {
		r := &relations[i]
		var err error
		if r.query != nil {
			r.columns, err = c.bindQuery(r.query)
			r.known = true
		} else {
			_, r.columns, r.known, err = c.lookupRelation(r.table)
			if r.dual {
				r.columns = []MappedColumn{{Source: "DUMMY", Target: "DUMMY"}}
			}
			if err == nil && c.catalog != nil && !r.known && !r.dual {
				err = fmt.Errorf("缺少视图依赖对象 %s 的列映射", r.name)
			}
			qualified := viewQuote(r.name)
			if c.target != "" && !r.dual {
				qualified = viewQuote(c.target) + "." + qualified
			}
			c.edit(r.start, r.end, qualified)
		}
		if err != nil {
			return nil, err
		}
		if r.aliasIndex >= 0 {
			c.edit(r.aliasIndex, r.aliasIndex+1, viewQuote(r.qualifier))
		}
	}
	return relations, nil
}
