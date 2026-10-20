package oracledameng

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MappedColumn describes the original Oracle name and its target identifier.
// Source names retain their original case; Target names are quoted uppercase.
type MappedColumn struct {
	Source string
	Target string
}

// ViewResult contains a SELECT body, its source relation dependencies, and its
// output column mapping. Dependencies are returned even when binding fails, so
// callers can load view dependencies before trying conversion again.
type ViewResult struct {
	SQL          string
	Dependencies []string
	Columns      []MappedColumn
}

type viewToken struct {
	text, name string
	start, end int
	kind       byte // i: identifier, q: quoted identifier, s: literal, n: number, p: punctuation
}

func (t viewToken) ident() bool { return t.kind == 'i' || t.kind == 'q' }
func (t viewToken) is(s string) bool {
	return t.kind == 'i' && strings.EqualFold(t.name, s)
}
func (t viewToken) matches(name string) bool {
	if t.kind == 'q' {
		return t.name == name
	}
	return strings.ToUpper(t.name) == name
}
func viewQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ToUpper(s), `"`, `""`) + `"`
}

type viewEdit struct {
	start, end int
	value      string
}

type viewRelation struct {
	name, qualifier string
	alias           viewToken
	columns         []MappedColumn
	known, dual     bool
}

type viewConverter struct {
	raw            string
	ts             []viewToken
	source, target string
	catalog        map[string][]MappedColumn
	edits          []viewEdit
	deps           []string
	seenDeps       map[string]bool
	firstErr       error
	options        Options
}

// ConvertView selectively maps a standard Oracle SELECT body to its target
// schema. It preserves literals and comments verbatim and never converts Oracle
// expressions to a different dialect. CTEs, subqueries, database links, cross-owner
// references, user routines and Oracle-specific query extensions fail closed.
//
// A nil catalog performs relation discovery and identifier normalization only;
// wildcard/output binding is deferred. With a non-nil catalog every relation and
// column must resolve, including dependencies which already exist on the target.
func ConvertView(raw, sourceSchema, targetSchema string, catalog map[string][]MappedColumn) (ViewResult, error) {
	return ConvertViewWithOptions(raw, sourceSchema, targetSchema, catalog, Options{})
}

func ConvertViewWithOptions(raw, sourceSchema, targetSchema string, catalog map[string][]MappedColumn, opt Options) (ViewResult, error) {
	c := &viewConverter{raw: raw, source: sourceSchema, target: targetSchema, catalog: catalog, seenDeps: map[string]bool{}, options: opt}
	ts, err := tokenizeView(raw)
	if err != nil {
		return ViewResult{}, err
	}
	c.ts = ts
	if len(ts) > 0 && ts[len(ts)-1].text == ";" {
		c.edit(len(ts)-1, len(ts), "")
		c.ts = ts[:len(ts)-1]
		ts = c.ts
	}
	if len(ts) == 0 || !ts[0].is("SELECT") {
		return ViewResult{}, fmt.Errorf("视图仅支持 Oracle SELECT 查询体")
	}
	depth, start := 0, 0
	branches := [][2]int{}
	for i, t := range ts {
		switch t.text {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return ViewResult{}, fmt.Errorf("视图括号不匹配")
			}
		case ";":
			return ViewResult{}, fmt.Errorf("视图不能包含多条语句")
		}
		if t.kind == 'i' {
			switch strings.ToUpper(t.name) {
			case "CONNECT", "START", "MODEL", "PIVOT", "UNPIVOT", "MATCH_RECOGNIZE", "SAMPLE", "VERSIONS", "FLASHBACK", "INTO", "FOR", "FETCH", "OFFSET", "MINUS", "INTERSECT", "LATERAL", "APPLY":
				return ViewResult{}, fmt.Errorf("暂不支持 Oracle 视图语法 %s", t.text)
			case "SELECT":
				if depth != 0 {
					return ViewResult{}, fmt.Errorf("视图暂不支持子查询")
				}
			case "UNION":
				if depth == 0 {
					branches = append(branches, [2]int{start, i})
					start = i + 1
					if start < len(ts) && (ts[start].is("ALL") || ts[start].is("DISTINCT")) {
						start++
					}
				}
			}
		}
		if i+2 < len(ts) && t.text == "(" && ts[i+1].text == "+" && ts[i+2].text == ")" {
			return ViewResult{}, fmt.Errorf("视图暂不支持 Oracle (+) 外连接，请使用显式 JOIN")
		}
	}
	if depth != 0 {
		return ViewResult{}, fmt.Errorf("视图括号不匹配")
	}
	branches = append(branches, [2]int{start, len(ts)})
	var columns []MappedColumn
	for i, b := range branches {
		cols, e := c.branch(b[0], b[1])
		if e != nil && c.firstErr == nil {
			c.firstErr = e
		}
		if i == 0 {
			columns = cols
		} else if catalog != nil && e == nil && len(cols) != len(columns) && c.firstErr == nil {
			c.firstErr = fmt.Errorf("UNION 各分支输出列数不一致")
		}
	}
	sort.Strings(c.deps)
	result := ViewResult{Dependencies: c.deps, Columns: columns}
	if c.firstErr != nil {
		return result, c.firstErr
	}
	result.SQL, err = c.render()
	return result, err
}

func (c *viewConverter) edit(start, end int, value string) {
	// Replace tokens individually so comments and whitespace between the tokens
	// survive qualified-reference and wildcard rewrites verbatim.
	for i := start; i < end; i++ {
		replacement := ""
		if i == start {
			replacement = value
		}
		c.edits = append(c.edits, viewEdit{c.ts[i].start, c.ts[i].end, replacement})
	}
}

func (c *viewConverter) render() (string, error) {
	sort.SliceStable(c.edits, func(i, j int) bool { return c.edits[i].start < c.edits[j].start })
	var b strings.Builder
	end := 0
	for _, e := range c.edits {
		if e.start < end {
			return "", fmt.Errorf("视图引用映射发生重叠")
		}
		b.WriteString(c.raw[end:e.start])
		b.WriteString(e.value)
		end = e.end
	}
	b.WriteString(c.raw[end:])
	return strings.TrimSpace(b.String()), nil
}

func (c *viewConverter) branch(start, end int) ([]MappedColumn, error) {
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
	relations, relationTokens, fromEnd, err := c.relations(from+1, end)
	if err != nil {
		return nil, err
	}
	projectionStart := start + 1
	if projectionStart < from && (ts[projectionStart].is("DISTINCT") || ts[projectionStart].is("ALL")) {
		projectionStart++
	}
	items := splitViewTokens(ts, projectionStart, from, ",")
	if len(items) == 0 {
		return nil, fmt.Errorf("视图缺少输出列")
	}
	aliases := map[string]MappedColumn{}
	var output []MappedColumn
	for _, span := range items {
		a, b := span[0], span[1]
		if a == b {
			return nil, fmt.Errorf("视图输出列为空")
		}
		alias, exprEnd := projectionAlias(ts, a, b)
		if alias >= 0 {
			sourceName := ts[alias].name
			if ts[alias].kind != 'q' {
				sourceName = strings.ToUpper(sourceName)
			}
			col := MappedColumn{Source: sourceName, Target: strings.ToUpper(sourceName)}
			if _, exists := aliases[col.Target]; exists {
				return nil, fmt.Errorf("视图输出列大写命名冲突: %s", col.Source)
			}
			aliases[col.Target] = col
			c.edit(alias, alias+1, viewQuote(col.Target))
			output = append(output, col)
		}
		wildcard, isWildcard, e := c.wildcard(a, exprEnd, relations)
		if e != nil {
			return nil, e
		}
		if isWildcard {
			if alias >= 0 {
				return nil, fmt.Errorf("通配符不能使用列别名")
			}
			output = append(output, wildcard...)
			continue
		}
		if e := c.expression(a, exprEnd, relations, nil); e != nil {
			return nil, e
		}
		if alias < 0 {
			col, direct, e := c.directColumn(a, exprEnd, relations)
			if e != nil {
				return nil, e
			}
			if !direct {
				return nil, fmt.Errorf("视图表达式必须显式指定输出列别名")
			}
			output = append(output, col)
		}
	}
	if c.catalog != nil {
		names := map[string]bool{}
		for _, col := range output {
			key := strings.ToUpper(col.Target)
			if names[key] {
				return nil, fmt.Errorf("视图输出列大写命名冲突: %s", col.Source)
			}
			names[key] = true
		}
	}
	// FROM contains ON/USING expressions as well as relation declarations.
	for i := from + 1; i < fromEnd; {
		if relationTokens[i] {
			i++
			continue
		}
		j := i + 1
		for j < fromEnd && !relationTokens[j] {
			j++
		}
		if err := c.expression(i, j, relations, nil); err != nil {
			return nil, err
		}
		i = j
	}
	order, depth := end, 0
	for i := fromEnd; i+1 < end; i++ {
		if ts[i].text == "(" {
			depth++
		} else if ts[i].text == ")" {
			depth--
		} else if depth == 0 && ts[i].is("ORDER") && ts[i+1].is("BY") {
			order = i
			break
		}
	}
	if err := c.expression(fromEnd, order, relations, nil); err != nil {
		return nil, err
	}
	if err := c.expression(order, end, relations, aliases); err != nil {
		return nil, err
	}
	return output, nil
}

func (c *viewConverter) relations(start, end int) ([]viewRelation, map[int]bool, int, error) {
	ts := c.ts
	relations := []viewRelation{}
	declarations := map[int]bool{}
	wantRelation, inUsing, depth := true, false, 0
	fromEnd := end
	for i := start; i < end; {
		t := ts[i]
		if depth == 0 && (t.is("WHERE") || t.is("GROUP") || t.is("HAVING") || t.is("ORDER")) {
			fromEnd = i
			break
		}
		if wantRelation {
			if !t.ident() || viewKeyword(t) {
				return relations, declarations, fromEnd, fmt.Errorf("视图 FROM 仅支持普通表或视图引用")
			}
			first, table, relationEnd := t, t, i+1
			if relationEnd < end && ts[relationEnd].text == "." {
				if relationEnd+1 >= end || !ts[relationEnd+1].ident() || !first.matches(c.source) {
					return relations, declarations, fromEnd, fmt.Errorf("视图不支持跨 owner 引用 %s", first.text)
				}
				table = ts[relationEnd+1]
				relationEnd += 2
			}
			if relationEnd < end && (ts[relationEnd].text == "." || ts[relationEnd].text == "(") {
				return relations, declarations, fromEnd, fmt.Errorf("视图不支持多级对象或表函数")
			}
			name, cols, known, e := c.lookupRelation(table)
			if e != nil {
				return relations, declarations, fromEnd, e
			}
			r := viewRelation{name: name, qualifier: name, columns: cols, known: known, dual: name == "DUAL" && !known && relationEnd == i+1}
			if r.dual {
				r.columns = []MappedColumn{{Source: "DUMMY", Target: "DUMMY"}}
			}
			// Discovery cannot distinguish SYS.DUAL from an owner-local table
			// named DUAL. Report the potential dependency; the caller resolves
			// it against ListTables before binding with a non-nil catalog.
			if (!r.dual || c.catalog == nil) && !c.seenDeps[name] {
				c.deps = append(c.deps, name)
				c.seenDeps[name] = true
			}
			qualified := viewQuote(name)
			if c.target != "" && !r.dual {
				qualified = viewQuote(c.target) + "." + qualified
			}
			c.edit(i, relationEnd, qualified)
			for j := i; j < relationEnd; j++ {
				declarations[j] = true
			}
			i = relationEnd
			if i < end && ts[i].is("AS") {
				return relations, declarations, fromEnd, fmt.Errorf("Oracle 表别名不支持 AS")
			}
			if i < end && ts[i].ident() && !viewKeyword(ts[i]) {
				r.alias, r.qualifier = ts[i], ts[i].name
				c.edit(i, i+1, viewQuote(r.qualifier))
				declarations[i] = true
				i++
			}
			for _, prev := range relations {
				if strings.EqualFold(prev.qualifier, r.qualifier) {
					return relations, declarations, fromEnd, fmt.Errorf("视图关系别名大写命名冲突: %s", r.qualifier)
				}
			}
			relations = append(relations, r)
			wantRelation = false
			continue
		}
		if depth == 0 && (t.text == "," || t.is("JOIN")) {
			wantRelation = true
		}
		if t.is("USING") {
			// USING can change the output of SELECT *. Reject it rather than
			// silently expanding two copies of a coalesced join column.
			inUsing = true
		}
		if t.is("NATURAL") {
			return relations, declarations, fromEnd, fmt.Errorf("视图暂不支持 NATURAL JOIN，请显式使用 ON")
		}
		if t.text == "(" {
			depth++
		} else if t.text == ")" {
			depth--
		}
		i++
	}
	if wantRelation || len(relations) == 0 {
		return relations, declarations, fromEnd, fmt.Errorf("视图缺少 FROM/JOIN 对象")
	}
	if inUsing {
		return relations, declarations, fromEnd, fmt.Errorf("视图暂不支持 JOIN USING，请显式使用 ON")
	}
	// Discover all dependencies before reporting an unloaded catalog entry.
	if c.catalog != nil {
		for _, r := range relations {
			if !r.known && !r.dual {
				return relations, declarations, fromEnd, fmt.Errorf("缺少视图依赖对象 %s 的列映射", r.name)
			}
		}
	}
	return relations, declarations, fromEnd, nil
}

func (c *viewConverter) lookupRelation(t viewToken) (string, []MappedColumn, bool, error) {
	name := t.name
	if t.kind != 'q' {
		name = strings.ToUpper(name)
	}
	if c.catalog == nil {
		return name, nil, false, nil
	}
	cols, exists := c.catalog[name]
	if !exists {
		return name, nil, false, nil
	}
	seen := map[string]bool{}
	for _, col := range cols {
		key := strings.ToUpper(col.Target)
		if col.Source == "" || col.Target == "" || seen[key] {
			return name, nil, false, fmt.Errorf("依赖对象 %s 的列映射为空或存在大写命名冲突", name)
		}
		seen[key] = true
	}
	return name, cols, true, nil
}

func projectionAlias(ts []viewToken, start, end int) (int, int) {
	depth := 0
	for i := start; i < end; i++ {
		if ts[i].text == "(" {
			depth++
		} else if ts[i].text == ")" {
			depth--
		} else if depth == 0 && ts[i].is("AS") {
			if i+2 == end && ts[i+1].ident() {
				return i + 1, i
			}
			return -1, end
		}
	}
	if end-start >= 2 && ts[end-1].ident() && !viewKeyword(ts[end-1]) {
		prev := ts[end-2]
		if prev.text == ")" || prev.kind == 's' || prev.kind == 'n' || prev.ident() && (!viewKeyword(prev) || prev.is("END") || viewValueKeyword(prev)) {
			return end - 1, end - 1
		}
	}
	return -1, end
}

func splitViewTokens(ts []viewToken, start, end int, sep string) [][2]int {
	spans := [][2]int{}
	depth, first := 0, start
	for i := start; i < end; i++ {
		if ts[i].text == "(" {
			depth++
		} else if ts[i].text == ")" {
			depth--
		} else if depth == 0 && ts[i].text == sep {
			spans = append(spans, [2]int{first, i})
			first = i + 1
		}
	}
	if first < end {
		spans = append(spans, [2]int{first, end})
	}
	return spans
}

func (c *viewConverter) wildcard(start, end int, relations []viewRelation) ([]MappedColumn, bool, error) {
	ts := c.ts
	if end-start != 1 || ts[start].text != "*" {
		if end-start != 3 || !ts[start].ident() || ts[start+1].text != "." || ts[start+2].text != "*" {
			return nil, false, nil
		}
	}
	selected := relations
	if end-start == 3 {
		r, e := lookupViewQualifier(ts[start], relations)
		if e != nil {
			return nil, true, e
		}
		selected = []viewRelation{r}
	}
	if c.catalog == nil {
		return nil, true, nil
	}
	columns, projections := []MappedColumn{}, []string{}
	for _, r := range selected {
		if r.dual {
			return nil, true, fmt.Errorf("视图暂不支持 DUAL.*")
		}
		for _, col := range r.columns {
			columns = append(columns, col)
			projections = append(projections, viewQuote(r.qualifier)+"."+viewQuote(col.Target))
		}
	}
	if len(columns) == 0 {
		return nil, true, fmt.Errorf("通配符缺少依赖列映射")
	}
	c.edit(start, end, strings.Join(projections, ", "))
	return columns, true, nil
}

func lookupViewQualifier(t viewToken, relations []viewRelation) (viewRelation, error) {
	for _, r := range relations {
		name := r.name
		if r.alias.ident() {
			name = r.alias.name
			if r.alias.kind != 'q' {
				name = strings.ToUpper(name)
			}
		}
		if t.matches(name) {
			return r, nil
		}
	}
	return viewRelation{}, fmt.Errorf("未知或不可用的视图关系别名 %s", t.text)
}

func lookupViewColumn(t viewToken, relations []viewRelation, discovery bool) (MappedColumn, error) {
	var result MappedColumn
	found := 0
	for _, r := range relations {
		for _, col := range r.columns {
			if t.matches(col.Source) {
				result = col
				found++
			}
		}
	}
	if found == 1 {
		return result, nil
	}
	if found > 1 {
		return result, fmt.Errorf("视图列 %s 存在歧义，请加表别名", t.text)
	}
	if discovery {
		return MappedColumn{Source: t.name, Target: strings.ToUpper(t.name)}, nil
	}
	return result, fmt.Errorf("未知的视图列 %s", t.text)
}

func (c *viewConverter) directColumn(start, end int, relations []viewRelation) (MappedColumn, bool, error) {
	ts := c.ts
	if end-start == 1 && ts[start].ident() && !viewKeyword(ts[start]) {
		col, err := lookupViewColumn(ts[start], relations, c.catalog == nil)
		return col, true, err
	}
	if end-start == 3 && ts[start].ident() && ts[start+1].text == "." && ts[start+2].ident() {
		r, err := lookupViewQualifier(ts[start], relations)
		if err != nil {
			return MappedColumn{}, true, err
		}
		col, err := lookupViewColumn(ts[start+2], []viewRelation{r}, c.catalog == nil)
		return col, true, err
	}
	if end-start == 5 && ts[start].ident() && ts[start+1].text == "." && ts[start+2].ident() && ts[start+3].text == "." && ts[start+4].ident() {
		if !ts[start].matches(c.source) {
			return MappedColumn{}, true, fmt.Errorf("视图不支持跨 owner 列引用")
		}
		r, err := lookupViewQualifier(ts[start+2], relations)
		if err != nil {
			return MappedColumn{}, true, err
		}
		col, err := lookupViewColumn(ts[start+4], []viewRelation{r}, c.catalog == nil)
		return col, true, err
	}
	return MappedColumn{}, false, nil
}

func (c *viewConverter) expression(start, end int, relations []viewRelation, aliases map[string]MappedColumn) error {
	ts := c.ts
	for i := start; i < end; i++ {
		t := ts[i]
		if t.kind == 's' {
			literal, err := renderOracleLiteral(t.text)
			if err != nil {
				return err
			}
			if literal != t.text {
				c.edit(i, i+1, literal)
			}
			continue
		}
		if !t.ident() {
			continue
		}
		if (t.is("DATE") || t.is("TIMESTAMP")) && i+1 < end && ts[i+1].kind == 's' {
			literal, err := renderOracleLiteral(ts[i+1].text)
			if err != nil {
				return err
			}
			if literal == "NULL" {
				c.edit(i, i+2, "NULL")
			} else {
				if literal != ts[i+1].text {
					c.edit(i+1, i+2, literal)
				}
				if t.is("DATE") && !c.options.UseDate {
					c.edit(i, i+1, "CAST(DATE")
					c.edits = append(c.edits, viewEdit{ts[i+1].end, ts[i+1].end, " AS TIMESTAMP(0))"})
				}
			}
			i++
			continue
		}
		if (t.is("CAST") || t.is("TO_DATE")) && i+1 < end && ts[i+1].text == "(" {
			close := viewClosingParen(ts, i+1, end)
			if close < 0 {
				return fmt.Errorf("视图 %s 括号不匹配", t.text)
			}
			if t.is("CAST") {
				as, depth := -1, 0
				for j := i + 2; j < close; j++ {
					if ts[j].text == "(" {
						depth++
					} else if ts[j].text == ")" {
						depth--
					} else if depth == 0 && ts[j].is("AS") {
						if as >= 0 {
							return fmt.Errorf("不支持 Oracle CAST 语法")
						}
						as = j
					}
				}
				if as <= i+2 || as+1 >= close {
					return fmt.Errorf("不支持 Oracle CAST 语法")
				}
				typeParts := make([]string, 0, close-as-1)
				for _, token := range ts[as+1 : close] {
					typeParts = append(typeParts, token.text)
				}
				mapped, err := oracleDMCastType(strings.Join(typeParts, " "), c.options)
				if err != nil {
					return err
				}
				if err = c.expression(i+2, as, relations, aliases); err != nil {
					return err
				}
				c.edit(as+1, close, mapped)
			} else {
				args := splitViewTokens(ts, i+2, close, ",")
				if len(args) != 2 || args[0][0] == args[0][1] || args[1][0] == args[1][1] || ts[close-1].text == "," {
					return fmt.Errorf("视图 TO_DATE 仅支持两个参数")
				}
				if err := c.expression(i+2, close, relations, aliases); err != nil {
					return err
				}
				c.edit(i, i+1, "CAST(TO_TIMESTAMP")
				c.edits = append(c.edits, viewEdit{ts[close].end, ts[close].end, " AS TIMESTAMP(0))"})
			}
			i = close
			continue
		}
		if i+1 < end && ts[i+1].text == "." {
			last := i + 2
			if last >= end || !ts[last].ident() {
				return fmt.Errorf("不完整的视图限定列引用")
			}
			qualifier := t
			if last+1 < end && ts[last+1].text == "." {
				if !t.matches(c.source) || last+2 >= end || !ts[last+2].ident() {
					return fmt.Errorf("视图不支持跨 owner 或多级列引用")
				}
				qualifier = ts[last]
				last += 2
			}
			if last+1 < end && (ts[last+1].text == "." || ts[last+1].text == "(") {
				return fmt.Errorf("视图不支持对象属性或 schema 函数调用")
			}
			r, err := lookupViewQualifier(qualifier, relations)
			if err != nil {
				return err
			}
			col, err := lookupViewColumn(ts[last], []viewRelation{r}, c.catalog == nil)
			if err != nil {
				return err
			}
			c.edit(i, last+1, viewQuote(r.qualifier)+"."+viewQuote(col.Target))
			i = last
			continue
		}
		if i+1 < end && ts[i+1].text == "(" {
			if t.kind != 'i' || !viewFunctions[strings.ToUpper(t.name)] && !viewKeyword(t) {
				return fmt.Errorf("视图函数 %s 未经达梦兼容支持", t.text)
			}
			continue
		}
		if viewKeyword(t) {
			continue
		}
		if t.kind == 'i' && (t.is("ROWID") || t.is("ROWNUM") || t.is("ORA_ROWSCN")) {
			return fmt.Errorf("视图暂不支持 Oracle 伪列 %s", t.text)
		}
		if alias, exists := aliases[strings.ToUpper(t.name)]; exists && t.matches(alias.Source) {
			c.edit(i, i+1, viewQuote(alias.Target))
			continue
		}
		col, err := lookupViewColumn(t, relations, c.catalog == nil)
		if err != nil {
			return err
		}
		c.edit(i, i+1, viewQuote(col.Target))
	}
	return nil
}

func viewClosingParen(ts []viewToken, open, end int) int {
	depth := 0
	for i := open; i < end; i++ {
		if ts[i].text == "(" {
			depth++
		} else if ts[i].text == ")" {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

var viewFunctions = map[string]bool{}
var viewKeywords = map[string]bool{}

func init() {
	for _, name := range strings.Fields(`ABS ACOS ASIN ATAN ATAN2 CEIL COS COSH EXP FLOOR LN LOG MOD POWER ROUND SIGN SIN SINH SQRT TAN TANH TRUNC
NVL NVL2 COALESCE NULLIF DECODE GREATEST LEAST
COUNT SUM AVG MIN MAX STDDEV STDDEV_POP STDDEV_SAMP VARIANCE VAR_POP VAR_SAMP
TO_CHAR TO_DATE TO_TIMESTAMP TO_TIMESTAMP_TZ TO_NUMBER TO_NCHAR CAST EXTRACT
ASCII CHR NCHR CONCAT INITCAP INSTR INSTRB LENGTH LENGTHB LENGTHC LOWER UPPER LPAD RPAD LTRIM RTRIM TRIM REPLACE TRANSLATE SUBSTR SUBSTRB SOUNDEX
ADD_MONTHS LAST_DAY MONTHS_BETWEEN NEXT_DAY NUMTODSINTERVAL NUMTOYMINTERVAL TO_DSINTERVAL TO_YMINTERVAL
HEXTORAW RAWTOHEX REGEXP_LIKE REGEXP_REPLACE REGEXP_SUBSTR REGEXP_INSTR REGEXP_COUNT`) {
		viewFunctions[name] = true
	}
	for _, name := range strings.Fields(`SELECT DISTINCT ALL FROM WHERE GROUP BY HAVING ORDER ASC DESC NULLS FIRST LAST JOIN INNER LEFT RIGHT FULL OUTER CROSS NATURAL USING ON AND OR NOT IS NULL LIKE ESCAPE BETWEEN IN EXISTS CASE WHEN THEN ELSE END AS UNION
DATE TIMESTAMP INTERVAL YEAR MONTH DAY HOUR MINUTE SECOND TO TIME ZONE LOCAL WITH WITHOUT CHAR NCHAR VARCHAR VARCHAR2 NVARCHAR2 NUMBER NUMERIC DECIMAL INTEGER INT SMALLINT FLOAT BINARY_FLOAT BINARY_DOUBLE CLOB NCLOB BLOB RAW
SYSDATE SYSTIMESTAMP CURRENT_DATE CURRENT_TIMESTAMP LOCALTIMESTAMP CURRENT_USER SESSION_USER USER
OVER PARTITION RANGE ROWS UNBOUNDED PRECEDING FOLLOWING CURRENT ROW BOTH LEADING TRAILING`) {
		viewKeywords[name] = true
	}
}
func viewValueKeyword(t viewToken) bool {
	if t.kind != 'i' {
		return false
	}
	switch strings.ToUpper(t.name) {
	case "NULL", "SYSDATE", "SYSTIMESTAMP", "CURRENT_DATE", "CURRENT_TIMESTAMP", "LOCALTIMESTAMP", "CURRENT_USER", "SESSION_USER", "USER":
		return true
	}
	return false
}
func viewKeyword(t viewToken) bool {
	return t.kind == 'i' && viewKeywords[strings.ToUpper(t.name)]
}

func tokenizeView(raw string) ([]viewToken, error) {
	tokens := []viewToken{}
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRuneInString(raw[i:])
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		if strings.HasPrefix(raw[i:], "--") {
			j := strings.IndexByte(raw[i:], '\n')
			if j < 0 {
				break
			}
			i += j + 1
			continue
		}
		if strings.HasPrefix(raw[i:], "/*") {
			j := strings.Index(raw[i+2:], "*/")
			if j < 0 {
				return nil, fmt.Errorf("视图块注释未闭合")
			}
			i += j + 4
			continue
		}
		start, prefix := i, 0
		if (raw[i] == 'n' || raw[i] == 'N') && i+1 < len(raw) {
			if raw[i+1] == '\'' || (raw[i+1] == 'q' || raw[i+1] == 'Q') && i+2 < len(raw) && raw[i+2] == '\'' {
				prefix = 1
			}
		}
		qpos := i + prefix
		if qpos+2 < len(raw) && (raw[qpos] == 'q' || raw[qpos] == 'Q') && raw[qpos+1] == '\'' {
			delim, dsize := utf8.DecodeRuneInString(raw[qpos+2:])
			if unicode.IsSpace(delim) || delim == '\'' {
				return nil, fmt.Errorf("视图 q 引号分隔符无效")
			}
			closing := delim
			switch delim {
			case '[':
				closing = ']'
			case '{':
				closing = '}'
			case '(':
				closing = ')'
			case '<':
				closing = '>'
			}
			j := strings.Index(raw[qpos+2+dsize:], string(closing)+"'")
			if j < 0 {
				return nil, fmt.Errorf("视图 q 引号字符串未闭合")
			}
			i = qpos + 2 + dsize + j + len(string(closing)) + 1
			tokens = append(tokens, viewToken{text: raw[start:i], start: start, end: i, kind: 's'})
			continue
		}
		if raw[qpos] == '\'' || raw[i] == '"' {
			quote, j := raw[qpos], qpos+1
			if raw[i] == '"' {
				quote, j = '"', i+1
			}
			closed := false
			for j < len(raw) {
				if raw[j] == quote {
					j++
					if j < len(raw) && raw[j] == quote {
						j++
						continue
					}
					closed = true
					break
				}
				j++
			}
			if !closed {
				return nil, fmt.Errorf("视图字符串或标识符未闭合")
			}
			kind, name := byte('s'), ""
			if quote == '"' {
				kind = 'q'
				name = strings.ReplaceAll(raw[i+1:j-1], `""`, `"`)
			}
			tokens = append(tokens, viewToken{text: raw[start:j], name: name, start: start, end: j, kind: kind})
			i = j
			continue
		}
		if unicode.IsLetter(r) || r == '_' {
			i += size
			for i < len(raw) {
				next, nsize := utf8.DecodeRuneInString(raw[i:])
				if !unicode.IsLetter(next) && !unicode.IsDigit(next) && !strings.ContainsRune("_$#", next) {
					break
				}
				i += nsize
			}
			tokens = append(tokens, viewToken{text: raw[start:i], name: raw[start:i], start: start, end: i, kind: 'i'})
			continue
		}
		if r >= '0' && r <= '9' || r == '.' && i+1 < len(raw) && raw[i+1] >= '0' && raw[i+1] <= '9' {
			i++
			for i < len(raw) && (raw[i] >= '0' && raw[i] <= '9' || raw[i] == '.') {
				i++
			}
			if i < len(raw) && (raw[i] == 'e' || raw[i] == 'E') {
				i++
				if i < len(raw) && (raw[i] == '+' || raw[i] == '-') {
					i++
				}
				for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
					i++
				}
			}
			tokens = append(tokens, viewToken{text: raw[start:i], start: start, end: i, kind: 'n'})
			continue
		}
		if strings.ContainsRune("(),.*+-/=<>!|%;", r) {
			i += size
			tokens = append(tokens, viewToken{text: raw[start:i], start: start, end: i, kind: 'p'})
			continue
		}
		return nil, fmt.Errorf("视图不支持字符 %q（数据库链接、绑定参数及过程调用不参与自动迁移）", r)
	}
	return tokens, nil
}
