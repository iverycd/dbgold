package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

// The SQL Server reader keeps the original module text. Only this target-aware
// preparation step may change identifiers or expression semantics.
type sqlServerViewColumns interface {
	GetViewRelationColumns(context.Context, string, string) (*source.TableDDLInfo, error)
}

type sqlServerPGViewError struct {
	message, source, suggested string
}

func (e *sqlServerPGViewError) Error() string { return e.message }

type pgViewToken struct {
	viewToken
	start, end int
	out        string
}

type pgViewType struct {
	kind byte // ?: unknown, s: character, i: integer, n: numeric, b: boolean, 0: NULL
	cast string
	name string // original projection name, for derived relations
}

var pgUnknown = pgViewType{kind: '?'}

func sqlServerViewType(col source.ColumnInfo) pgViewType {
	if col.TypeKind == "user_defined" {
		return pgUnknown
	}
	switch strings.ToLower(col.DataType) {
	case "char", "varchar", "nchar", "nvarchar", "text", "ntext":
		return pgViewType{kind: 's'}
	case "bigint":
		return pgViewType{kind: 'i', cast: "bigint"}
	case "int", "integer", "smallint", "tinyint", "bit":
		return pgViewType{kind: 'i', cast: "integer"}
	case "decimal", "numeric", "money", "smallmoney", "float", "real":
		return pgViewType{kind: 'n'}
	case "date", "datetime", "datetime2", "smalldatetime", "datetimeoffset", "time":
		return pgViewType{kind: 'd'}
	case "uniqueidentifier":
		return pgViewType{kind: 'u'}
	case "binary", "varbinary", "image", "timestamp", "rowversion":
		return pgViewType{kind: 'x'}
	}
	return pgUnknown
}

type pgViewRelation struct {
	name, alias string
	columns     map[string]pgViewType
	columnNames map[string]string
	order       []string
}
type pgViewScope struct {
	parent    *pgViewScope
	relations map[string]*pgViewRelation
	order     []*pgViewRelation
}

func (s *pgViewScope) column(parts []string) (pgViewType, string, string) {
	if len(parts) > 1 {
		key := strings.ToLower(parts[len(parts)-2])
		for scope := s; scope != nil; scope = scope.parent {
			if relation := scope.relations[key]; relation != nil {
				col := strings.ToLower(parts[len(parts)-1])
				if typ, ok := relation.columns[col]; ok {
					return typ, relation.alias, relation.columnNames[col]
				}
				return pgUnknown, "", ""
			}
		}
		return pgUnknown, "", ""
	}
	for scope := s; scope != nil; scope = scope.parent {
		var found *pgViewRelation
		seen := map[*pgViewRelation]bool{}
		for _, relation := range scope.relations {
			if seen[relation] {
				continue
			}
			seen[relation] = true
			if _, ok := relation.columns[strings.ToLower(parts[0])]; ok {
				if found != nil {
					return pgUnknown, "", "" // Ambiguous unqualified column.
				}
				found = relation
			}
		}
		if found != nil {
			key := strings.ToLower(parts[0])
			return found.columns[key], "", found.columnNames[key]
		}
	}
	return pgUnknown, "", ""
}

type pgViewExpr struct {
	sql string
	typ pgViewType
}

type sqlServerPGView struct {
	ctx       context.Context
	m         *Migrator
	raw       string
	tokens    []pgViewToken
	close     map[int]int
	columns   sqlServerViewColumns
	metadata  map[string]*source.TableDDLInfo
	subquery  map[int]pgViewType
	queryDone map[int]bool
	outputs   map[int][]pgViewType
}

func (m *Migrator) isSQLServerPG() bool {
	return m.reader.DBType() == "sqlserver" && source.IsPGFamily(m.writer.DBType())
}

func (m *Migrator) sqlServerViewCandidate(name, body string) string {
	return "CREATE OR REPLACE VIEW " + m.writer.Dialect().QualifyTable(m.cfg.TargetSchema, name) + " AS\n" + body + ";"
}

func (m *Migrator) attachSQLServerViewIssue(result *ObjectResult, view source.ViewInfo, err error) {
	if !m.isSQLServerPG() {
		return
	}
	result.SourceDDL = view.Definition
	var issue *sqlServerPGViewError
	if errors.As(err, &issue) {
		result.Stage = "preflight"
		result.DDL = "" // Never present unconverted source SQL as an executable target.
		result.SourceDDL = issue.source
		if issue.suggested != "" {
			result.SuggestedDDL = m.sqlServerViewCandidate(m.objName(view.ViewName), issue.suggested)
			result.Suggestion = "候选 SQL 仅供人工适配；请核对列名业务值和关联规则，保存并确认后再执行。"
		}
	}
}

func (m *Migrator) prepareSQLServerPGView(ctx context.Context, v source.ViewInfo) (source.ViewInfo, error) {
	out := v
	out.ViewName = m.objName(v.ViewName)
	reader, _ := m.reader.(sqlServerViewColumns)
	p := &sqlServerPGView{ctx: ctx, m: m, raw: v.Definition, columns: reader,
		close: map[int]int{}, metadata: map[string]*source.TableDDLInfo{}, subquery: map[int]pgViewType{}, queryDone: map[int]bool{}, outputs: map[int][]pgViewType{}}
	lexed, err := lexOscarView(v.Definition)
	if err != nil {
		return out, &sqlServerPGViewError{message: "SQL Server 视图解析失败: " + err.Error(), source: v.Definition}
	}
	offset := 0
	for _, t := range lexed {
		if t.kind != 'w' {
			p.tokens = append(p.tokens, pgViewToken{viewToken: t, start: offset, end: offset + len(t.text), out: t.text})
		}
		offset += len(t.text)
	}
	var stack []int
	for i := range p.tokens {
		if p.text(i) == "(" {
			stack = append(stack, i)
		} else if p.text(i) == ")" {
			if len(stack) == 0 {
				return out, p.issue("视图括号不匹配")
			}
			p.close[stack[len(stack)-1]] = i
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) > 0 {
		return out, p.issue("视图括号不匹配")
	}
	start := 0
	var explicitColumns []string
	if p.word(start) == "CREATE" || p.word(start) == "ALTER" {
		for start < len(p.tokens) && p.word(start) != "VIEW" {
			start++
		}
		start++
		if !p.ident(start) {
			return out, p.issue("无法识别视图名称")
		}
		start++
		for p.text(start) == "." && p.ident(start+1) {
			start += 2
		}
		if p.text(start) == "(" {
			close := p.close[start]
			for _, column := range p.split(start+1, close, ",") {
				if column[1] != column[0]+1 || !p.ident(column[0]) {
					return out, p.issue("视图列清单无效")
				}
				explicitColumns = append(explicitColumns, p.quote(p.tokens[column[0]].name))
			}
			start = close + 1
		}
		if p.word(start) == "WITH" {
			for start < len(p.tokens) && p.word(start) != "AS" {
				start++
			}
		}
		if p.word(start) != "AS" {
			return out, p.issue("无法识别视图 AS 子句")
		}
		start++
	}
	if start >= len(p.tokens) {
		return out, p.issue("视图定义为空或不可读取")
	}
	end := len(p.tokens)
	if p.text(end-1) == ";" {
		end--
	}
	// System catalog names are business values in this pattern. A target-name
	// lookup would change both its output and the fieldName join after lowercasing.
	for i := start; i < end; i++ {
		if p.ident(i) && (strings.EqualFold(p.tokens[i].name, "syscolumns") || strings.EqualFold(p.tokens[i].name, "object_id") && p.text(i+1) == "(") {
			candidate := p.catalogCandidate(start, end)
			out.Definition = ""
			return out, &sqlServerPGViewError{message: "需人工适配：该视图使用 SQL Server 系统目录的原始列名作为业务值；目标名称映射可能改变返回值及 fieldName 关联，请核对源列名和排序规则后采用候选 SQL", source: v.Definition, suggested: candidate}
		}
	}
	if _, err = p.query(start, end, nil); err != nil {
		out.Definition = ""
		return out, err
	}
	out.Definition = strings.TrimSpace(p.render(start, end))
	if len(explicitColumns) > 0 {
		out.Definition = "SELECT * FROM (\n" + out.Definition + "\n) AS \"__dbgold_view_columns\" (" + strings.Join(explicitColumns, ", ") + ")"
	}
	return out, nil
}

func (p *sqlServerPGView) issue(message string) error {
	return &sqlServerPGViewError{message: "SQL Server 视图需人工适配: " + message, source: p.raw}
}
func (p *sqlServerPGView) text(i int) string {
	if i < 0 || i >= len(p.tokens) {
		return ""
	}
	return p.tokens[i].text
}
func (p *sqlServerPGView) word(i int) string {
	if !p.ident(i) || p.tokens[i].kind == 'q' {
		return ""
	}
	return strings.ToUpper(p.tokens[i].name)
}
func (p *sqlServerPGView) ident(i int) bool {
	return i >= 0 && i < len(p.tokens) && (p.tokens[i].kind == 'i' || p.tokens[i].kind == 'q')
}
func (p *sqlServerPGView) quote(name string) string {
	return p.m.writer.Dialect().QuoteIdent(p.m.objName(name))
}
func (p *sqlServerPGView) replace(start, end int, sql string) {
	if start >= end {
		return
	}
	p.tokens[start].out = sql
	for i := start + 1; i < end; i++ {
		p.tokens[i].out = ""
	}
}
func (p *sqlServerPGView) render(start, end int) string {
	if start >= end {
		return ""
	}
	var out strings.Builder
	for i := start; i < end; i++ {
		if i > start {
			out.WriteString(p.raw[p.tokens[i-1].end:p.tokens[i].start])
		}
		out.WriteString(p.tokens[i].out)
	}
	return out.String()
}

func (p *sqlServerPGView) query(start, end int, parent *pgViewScope) (map[string]pgViewType, error) {
	for start < end && p.text(start) == "(" && p.close[start] == end-1 {
		start, end = start+1, end-1
	}
	if p.word(start) != "SELECT" {
		return nil, p.issue("暂不支持此查询头（需要 SELECT）")
	}
	var first map[string]pgViewType
	var resultTypes []pgViewType
	block := start
	for i := start; i <= end; i++ {
		if i < end && p.text(i) == "(" {
			i = p.close[i]
			continue
		}
		if i == end || p.word(i) == "UNION" || p.word(i) == "INTERSECT" || p.word(i) == "EXCEPT" {
			cols, err := p.selectBlock(block, i, parent)
			if err != nil {
				return nil, err
			}
			if first == nil {
				first = cols
				resultTypes = append([]pgViewType(nil), p.outputs[block]...)
			} else {
				branch := p.outputs[block]
				if len(resultTypes) != len(branch) {
					return nil, p.issue("UNION 分支输出列数不一致")
				}
				for j, typ := range branch {
					old := resultTypes[j]
					common := pgViewCommonType([]pgViewExpr{{typ: old}, {typ: typ}})
					if old.kind == 's' && typ.kind == 'i' || old.kind == 'i' && typ.kind == 's' {
						return nil, p.issue("UNION 字符与整数分支的隐式转换需人工适配")
					}
					common.name = old.name
					resultTypes[j] = common
				}
			}
			block = i + 1
			if p.word(block) == "ALL" || p.word(block) == "DISTINCT" {
				block++
			}
		}
	}
	for _, typ := range resultTypes {
		if typ.name != "" {
			first[strings.ToLower(typ.name)] = typ
		}
	}
	p.outputs[start] = resultTypes
	return first, nil
}

func (p *sqlServerPGView) selectBlock(start, end int, parent *pgViewScope) (map[string]pgViewType, error) {
	if p.word(start) != "SELECT" {
		return nil, p.issue("UNION 分支缺少 SELECT")
	}
	scope := &pgViewScope{parent: parent, relations: map[string]*pgViewRelation{}}
	from := end
	for i := start + 1; i < end; i++ {
		if p.text(i) == "(" {
			i = p.close[i]
		} else if p.word(i) == "FROM" {
			from = i
			break
		}
	}
	// Bind FROM/JOIN before inspecting projections and predicates. Each UNION
	// branch owns its aliases; correlated subqueries can see their parent scope.
	if err := p.bindRelations(from, end, scope, false); err != nil {
		return nil, err
	}
	// Prepare nested SELECTs before their enclosing expressions.
	for i := start + 1; i < end; i++ {
		if p.text(i) == "(" {
			close := p.close[i]
			if p.word(i+1) == "SELECT" && !p.queryDone[i] {
				cols, err := p.query(i+1, close, scope)
				if err != nil {
					return nil, err
				}
				p.queryDone[i] = true
				if len(cols) == 1 {
					for _, typ := range cols {
						p.subquery[i] = typ
					}
				}
			}
		}
	}
	projectionStart := start + 1
	if p.word(projectionStart) == "DISTINCT" || p.word(projectionStart) == "ALL" {
		projectionStart++
	}
	if p.word(projectionStart) == "TOP" {
		i := projectionStart + 1
		if p.text(i) == "(" && p.close[i] == i+2 && p.text(i+1) == "100" {
			i += 3
		} else if p.text(i) == "100" {
			i++
		} else {
			return nil, p.issue("仅支持移除 TOP 100 PERCENT，其他 TOP 必须人工保留行数语义")
		}
		if p.word(i) != "PERCENT" {
			return nil, p.issue("TOP 行数限制需人工适配")
		}
		p.replace(projectionStart, i+1, "")
		projectionStart = i + 1
	}
	outputs := map[string]pgViewType{}
	for _, segment := range p.split(projectionStart, from, ",") {
		a, b := segment[0], segment[1]
		if b == a+1 && p.text(a) == "*" || b == a+3 && p.ident(a) && p.text(a+1) == "." && p.text(a+2) == "*" {
			for _, relation := range scope.order {
				if b == a+3 && !strings.EqualFold(p.tokens[a].name, relation.alias) {
					continue
				}
				for _, name := range relation.order {
					typ := relation.columns[strings.ToLower(name)]
					typ.name = name
					outputs[strings.ToLower(name)] = typ
					p.outputs[start] = append(p.outputs[start], typ)
				}
			}
			if b == a+3 {
				p.tokens[a].out = p.quote(p.tokens[a].name)
			}
			continue
		}
		alias := ""
		for i := a; i < b; i++ {
			if p.text(i) == "(" {
				i = p.close[i]
			} else if p.word(i) == "AS" && i+2 == b {
				alias = p.tokens[i+1].name
				if p.tokens[i+1].kind == 's' {
					alias = decodeViewLiteral(p.text(i + 1))
				}
				p.tokens[i+1].out = p.quote(alias)
				b = i
				break
			}
		}
		value, next, err := p.expression(a, b, 0, scope)
		if err != nil {
			return nil, err
		}
		if next+1 == b && p.ident(next) { // implicit alias
			alias = p.tokens[next].name
			p.tokens[next].out = p.quote(alias)
			b = next
		}
		if err = p.checkUnparsed(next, b); err != nil {
			return nil, err
		}
		if alias == "" && next == b && b > a && p.ident(b-1) {
			alias = p.tokens[b-1].name
		}
		if alias != "" {
			typ := value.typ
			typ.name = alias
			outputs[strings.ToLower(alias)] = typ
		}
		projectionType := value.typ
		projectionType.name = alias
		p.outputs[start] = append(p.outputs[start], projectionType)
		if next == b {
			p.replace(a, b, value.sql)
		}
	}
	if err := p.rewritePredicates(from, end, scope); err != nil {
		return nil, err
	}

	return outputs, nil
}

func (p *sqlServerPGView) rewritePredicates(start, end int, scope *pgViewScope) error {
	// Predicate and ordering ranges contain no relation declarations. Rewriting
	// only these spans leaves the rest of the SELECT structure intact.
	for i := start; i < end; i++ {
		if p.text(i) == "(" {
			close := p.close[i]
			if !p.queryDone[i] {
				if err := p.rewritePredicates(i+1, close, scope); err != nil {
					return err
				}
			}
			i = close
			continue
		}
		w := p.word(i)
		if w != "ON" && w != "WHERE" && w != "HAVING" && w != "GROUP" && w != "ORDER" {
			continue
		}
		a := i + 1
		if w == "GROUP" || w == "ORDER" {
			if p.word(a) != "BY" {
				continue
			}
			a++
		}
		b := a
		for b < end {
			if p.text(b) == "(" {
				b = p.close[b] + 1
				continue
			}
			x := p.word(b)
			if x == "JOIN" || x == "INNER" || x == "LEFT" || x == "RIGHT" || x == "FULL" || x == "CROSS" || x == "WHERE" || x == "GROUP" || x == "HAVING" || x == "ORDER" {
				break
			}
			b++
		}
		for _, segment := range p.split(a, b, ",") {
			value, next, err := p.expression(segment[0], segment[1], 0, scope)
			if err != nil {
				return err
			}
			if err = p.checkUnparsed(next, segment[1]); err != nil {
				return err
			}
			if next > segment[0] {
				p.replace(segment[0], next, value.sql)
			}
		}
		i = b - 1
	}
	return nil
}

func (p *sqlServerPGView) bindRelations(start, end int, scope *pgViewScope, expectRelation bool) error {
	inFrom := expectRelation
	for i := start; i < end; i++ {
		w := p.word(i)
		if w == "WHERE" || w == "GROUP" || w == "HAVING" || w == "ORDER" {
			inFrom = false
		}
		if w == "FROM" || w == "JOIN" {
			inFrom, expectRelation = true, true
			continue
		}
		if inFrom && p.text(i) == "," {
			expectRelation = true
			continue
		}
		if expectRelation {
			last, err := p.relation(i, end, scope)
			if err != nil {
				return err
			}
			i, expectRelation = last-1, false
			continue
		}
		if p.text(i) == "(" {
			i = p.close[i]
		}
	}
	return nil
}

func (p *sqlServerPGView) split(start, end int, sep string) [][2]int {
	var result [][2]int
	first := start
	for i := start; i < end; i++ {
		if p.text(i) == "(" {
			i = p.close[i]
		} else if p.text(i) == sep {
			result = append(result, [2]int{first, i})
			first = i + 1
		}
	}
	if first < end {
		result = append(result, [2]int{first, end})
	}
	return result
}

func (p *sqlServerPGView) relation(start, end int, scope *pgViewScope) (int, error) {
	i := start
	relation := &pgViewRelation{columns: map[string]pgViewType{}, columnNames: map[string]string{}}
	if p.text(i) == "(" {
		close := p.close[i]
		if p.word(i+1) != "SELECT" {
			if err := p.bindRelations(i+1, close, scope, true); err != nil {
				return i, err
			}
			return close + 1, nil
		}
		cols, err := p.query(i+1, close, scope)
		if err != nil {
			return i, err
		}
		p.queryDone[i] = true
		relation.columns = cols
		for name := range cols {
			relation.columnNames[name] = cols[name].name
		}
		for _, typ := range p.outputs[i+1] {
			relation.order = append(relation.order, typ.name)
		}
		i = close + 1
	} else {
		var parts []string
		for p.ident(i) {
			parts = append(parts, p.tokens[i].name)
			i++
			if p.text(i) != "." {
				break
			}
			i++
		}
		if len(parts) == 0 || len(parts) > 2 {
			return i, p.issue("不支持的表引用或跨数据库引用")
		}
		schema, name := "", parts[len(parts)-1]
		if len(parts) == 2 {
			schema = parts[0]
		}
		if p.columns == nil {
			return i, p.issue("源 Reader 不支持视图列类型元数据")
		}
		key := strings.ToLower(schema + "." + name)
		info := p.metadata[key]
		if info == nil {
			var err error
			info, err = p.columns.GetViewRelationColumns(p.ctx, schema, name)
			if err != nil {
				return i, fmt.Errorf("读取视图引用 %s 的列类型: %w", strings.Join(parts, "."), err)
			}
			if info == nil {
				return i, p.issue("引用对象缺少元数据: " + name)
			}
			p.metadata[key] = info
		}
		relation.name, relation.alias = info.TableName, info.TableName
		for _, column := range info.Columns {
			key := strings.ToLower(column.Name)
			relation.columns[key], relation.columnNames[key] = sqlServerViewType(column), column.Name
			relation.order = append(relation.order, column.Name)
		}
		p.replace(start, i, p.m.writer.Dialect().QualifyTable(p.m.cfg.TargetSchema, p.m.objName(info.TableName)))
	}
	if p.word(i) == "AS" {
		i++
	}
	if i < end && p.ident(i) && !pgViewReserved(p.word(i)) {
		relation.alias = p.tokens[i].name
		p.tokens[i].out = p.quote(relation.alias)
		i++
	}
	if relation.alias == "" {
		return i, p.issue("派生表缺少别名")
	}
	key := strings.ToLower(relation.alias)
	if scope.relations[key] != nil {
		return i, p.issue("表别名冲突: " + relation.alias)
	}
	scope.relations[key] = relation
	scope.order = append(scope.order, relation)
	if relation.name != "" && strings.EqualFold(relation.name, relation.alias) {
		// The source may spell an unaliased table in a different case.
		if p.ident(start) {
			last := start
			for p.text(last+1) == "." && p.ident(last+2) {
				last += 2
			}
			scope.relations[strings.ToLower(p.tokens[last].name)] = relation
		}
	}
	return i, nil
}

func pgViewReserved(w string) bool {
	return oscarViewKeywords[w] || strings.Contains(" TOP PERCENT WITH SCHEMABINDING ENCRYPTION ", " "+w+" ")
}

func (p *sqlServerPGView) checkUnparsed(start, end int) error {
	if start == end || start+1 == end && (p.word(start) == "ASC" || p.word(start) == "DESC") {
		return nil
	}
	return p.issue("无法确定表达式操作数类型或不支持的语法: " + p.render(start, end))
}

func decodeViewLiteral(s string) string {
	if len(s) > 1 && (s[0] == 'N' || s[0] == 'n') {
		s = s[1:]
	}
	if len(s) >= 2 {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}

func (p *sqlServerPGView) operator(i, end int) (string, int, int) {
	t := p.text(i)
	if i+1 < end && (t == "<" || t == ">" || t == "!") && (p.text(i+1) == "=" || t == "<" && p.text(i+1) == ">") {
		t += p.text(i + 1)
		return t, 3, 2
	}
	switch p.word(i) {
	case "OR":
		return "OR", 1, 1
	case "AND":
		return "AND", 2, 1
	case "IS", "LIKE", "IN", "BETWEEN":
		return p.word(i), 3, 1
	}
	switch t {
	case "=", "<", ">":
		return t, 3, 1
	case "+", "-":
		return t, 4, 1
	case "*", "/", "%":
		return t, 5, 1
	}
	return "", 0, 0
}

func (p *sqlServerPGView) expression(start, end, min int, scope *pgViewScope) (pgViewExpr, int, error) {
	left, i, err := p.atom(start, end, scope)
	if err != nil {
		return left, i, err
	}
	for i < end {
		op, priority, width := p.operator(i, end)
		negate := p.word(i) == "NOT" && (p.word(i+1) == "IN" || p.word(i+1) == "LIKE" || p.word(i+1) == "BETWEEN")
		if negate {
			op, priority, width = p.word(i+1), 3, 2
		}
		if priority == 0 || priority < min {
			break
		}
		if op == "IS" {
			j := i + 1
			if p.word(j) == "NOT" {
				j++
			}
			if p.word(j) != "NULL" {
				break
			}
			left = pgViewExpr{"(" + left.sql + " " + p.render(i, j+1) + ")", pgViewType{kind: 'b'}}
			i = j + 1
			continue
		}
		if op == "BETWEEN" {
			low, j, e := p.expression(i+width, end, 4, scope)
			if e != nil {
				return left, j, e
			}
			if p.word(j) != "AND" {
				return left, j, p.issue("不完整的 BETWEEN")
			}
			high, k, e := p.expression(j+1, end, 4, scope)
			if e != nil {
				return left, k, e
			}
			operator := " BETWEEN "
			if negate {
				operator = " NOT BETWEEN "
			}
			left = pgViewExpr{"(" + left.sql + operator + low.sql + " AND " + high.sql + ")", pgViewType{kind: 'b'}}
			i = k
			continue
		}
		if op == "IN" && p.text(i+width) == "(" {
			close := p.close[i+width]
			if !p.queryDone[i+width] {
				for _, arg := range p.split(i+width+1, close, ",") {
					x, next, e := p.expression(arg[0], arg[1], 0, scope)
					if e != nil {
						return left, next, e
					}
					p.replace(arg[0], next, x.sql)
				}
			}
			operator := " IN "
			if negate {
				operator = " NOT IN "
			}
			left = pgViewExpr{"(" + left.sql + operator + p.render(i+width, close+1) + ")", pgViewType{kind: 'b'}}
			i = close + 1
			continue
		}
		right, next, e := p.expression(i+width, end, priority+1, scope)
		if e != nil {
			return left, next, e
		}
		left, e = p.binary(left, op, right)
		if e != nil {
			return left, next, e
		}
		if negate {
			left.sql = "(NOT " + left.sql + ")"
		}
		i = next
	}
	return left, i, nil
}

func (p *sqlServerPGView) atom(i, end int, scope *pgViewScope) (pgViewExpr, int, error) {
	unknown := pgViewExpr{typ: pgUnknown}
	if i >= end {
		return unknown, i, nil
	}
	t := p.tokens[i]
	if t.text == "+" || t.text == "-" || p.word(i) == "NOT" || p.word(i) == "EXISTS" {
		priority := 6
		if p.word(i) == "NOT" {
			priority = 3
		}
		x, next, err := p.expression(i+1, end, priority, scope)
		if p.word(i) == "NOT" || p.word(i) == "EXISTS" {
			x.typ = pgViewType{kind: 'b'}
		}
		x.sql = t.text + " " + x.sql
		return x, next, err
	}
	if t.text == "(" {
		close := p.close[i]
		if p.queryDone[i] {
			typ := pgUnknown
			if found, ok := p.subquery[i]; ok {
				typ = found
			}
			return pgViewExpr{p.render(i, close+1), typ}, close + 1, nil
		}
		x, next, err := p.expression(i+1, close, 0, scope)
		if err == nil && next != close {
			err = p.checkUnparsed(next, close)
			x.sql = p.render(i+1, close)
		}
		x.sql = "(" + x.sql + ")"
		return x, close + 1, err
	}
	if t.kind == 's' {
		literal := t.text
		if len(literal) > 1 && (literal[0] == 'N' || literal[0] == 'n') {
			literal = literal[1:]
		}
		return pgViewExpr{literal, pgViewType{kind: 's'}}, i + 1, nil
	}
	if t.kind == 'p' && len(t.text) > 0 && t.text[0] >= '0' && t.text[0] <= '9' {
		typ := pgViewType{kind: 'i', cast: "integer"}
		if strings.ContainsAny(t.text, ".eE") {
			typ = pgViewType{kind: 'n'}
		}
		return pgViewExpr{t.text, typ}, i + 1, nil
	}
	if p.word(i) == "NULL" {
		return pgViewExpr{t.text, pgViewType{kind: '0'}}, i + 1, nil
	}
	if p.word(i) == "CASE" {
		return p.caseExpression(i, end, scope)
	}
	if p.ident(i) {
		j := i + 1
		parts := []string{t.name}
		for p.text(j) == "." && (p.ident(j+1) || p.text(j+1) == "*") {
			if p.text(j+1) == "*" {
				return pgViewExpr{p.quote(parts[0]) + ".*", pgUnknown}, j + 2, nil
			}
			parts = append(parts, p.tokens[j+1].name)
			j += 2
		}
		if p.text(j) == "(" {
			x, next, err := p.function(i, j, scope)
			if err != nil || p.word(next) != "OVER" {
				return x, next, err
			}
			return p.window(x, next, end, scope)
		}
		if p.word(i) == "CURRENT_TIMESTAMP" {
			return pgViewExpr{t.text, pgUnknown}, j, nil
		}
		typ, alias, column := scope.column(parts)
		if column != "" {
			sql := p.quote(column)
			if alias != "" {
				sql = p.quote(alias) + "." + sql
			}
			return pgViewExpr{sql, typ}, j, nil
		}
		var quoted []string
		for _, part := range parts {
			quoted = append(quoted, p.quote(part))
		}
		return pgViewExpr{strings.Join(quoted, "."), pgUnknown}, j, nil
	}
	return pgViewExpr{t.text, pgUnknown}, i + 1, nil
}

func (p *sqlServerPGView) function(start, open int, scope *pgViewScope) (pgViewExpr, int, error) {
	close := p.close[open]
	name := p.word(start)
	if name == "CAST" {
		as := open + 1
		for as < close {
			if p.text(as) == "(" {
				as = p.close[as] + 1
				continue
			}
			if p.word(as) == "AS" {
				break
			}
			as++
		}
		if as >= close {
			return pgViewExpr{}, close + 1, p.issue("CAST 缺少 AS")
		}
		x, next, err := p.expression(open+1, as, 0, scope)
		if err == nil {
			err = p.checkUnparsed(next, as)
		}
		typeName := strings.ToLower(p.tokens[as+1].name)
		typ := sqlServerViewType(source.ColumnInfo{DataType: typeName})
		switch typeName {
		case "nvarchar":
			typeName = "varchar"
		case "nchar":
			typeName = "char"
		case "ntext":
			typeName = "text"
		}
		target := typeName + p.render(as+2, close)
		if typ.kind == 'i' && x.typ.kind == 'n' {
			x.sql = "trunc(" + x.sql + ")"
		}
		if typ.kind == 'i' && x.typ.kind == 's' {
			return pgViewExpr{sqlServerIntegerText(x.sql, typ.cast), typ}, close + 1, err
		}
		return pgViewExpr{"CAST(" + x.sql + " AS " + target + ")", typ}, close + 1, err
	}
	var args []pgViewExpr
	for _, arg := range p.split(open+1, close, ",") {
		x, next, err := p.expression(arg[0], arg[1], 0, scope)
		if err != nil {
			return x, next, err
		}
		if err = p.checkUnparsed(next, arg[1]); err != nil {
			return x, next, err
		}
		if next != arg[1] {
			x.sql = p.render(arg[0], arg[1])
		}
		args = append(args, x)
	}
	typ := pgUnknown
	target := p.render(start, open)
	switch name {
	case "GETDATE":
		return pgViewExpr{"CURRENT_TIMESTAMP", pgUnknown}, close + 1, nil
	case "LEN":
		target, typ = "length", pgViewType{kind: 'i', cast: "integer"}
		if len(args) == 1 {
			if args[0].typ.kind == 'i' || args[0].typ.kind == 'n' {
				args[0].sql = "CAST(" + args[0].sql + " AS text)"
			} else if args[0].typ.kind != 's' && args[0].typ.kind != '0' {
				return pgViewExpr{}, close + 1, p.issue("LEN 参数的字符转换格式需人工适配")
			}
			args[0].sql = "rtrim(" + args[0].sql + ", ' ')"
		}
	case "ISNULL":
		target = "coalesce"
		if len(args) > 0 {
			typ = args[0].typ
			if typ.kind == '0' && len(args) > 1 {
				typ = args[1].typ
			}
		}
	case "COALESCE", "MIN", "MAX", "SUM", "ABS":
		if len(args) > 0 {
			typ = args[0].typ
		}
		if name == "COALESCE" {
			typ = pgViewCommonType(args)
		}
	case "COUNT":
		typ = pgViewType{kind: 'i', cast: "integer"}
	case "COUNT_BIG":
		target, typ = "count", pgViewType{kind: 'i', cast: "bigint"}
	case "ROW_NUMBER", "RANK", "DENSE_RANK":
		typ = pgViewType{kind: 'i', cast: "bigint"}
	case "LOWER", "UPPER", "LTRIM", "RTRIM", "SUBSTRING", "REPLACE":
		typ = pgViewType{kind: 's'}
	}
	var sql []string
	for _, arg := range args {
		if (name == "COALESCE" || name == "ISNULL") && arg.typ.kind == 's' && typ.kind == 'i' {
			arg.sql = sqlServerIntegerText(arg.sql, typ.cast)
		} else if name == "ISNULL" && typ.kind == 's' && arg.typ.kind == 'i' {
			arg.sql = "CAST(" + arg.sql + " AS varchar)"
		}
		sql = append(sql, arg.sql)
	}
	return pgViewExpr{target + "(" + strings.Join(sql, ", ") + ")", typ}, close + 1, nil
}

func pgViewCommonType(args []pgViewExpr) pgViewType {
	result := pgViewType{kind: '0'}
	for _, arg := range args {
		typ := arg.typ
		if typ.kind == '0' {
			continue
		}
		if result.kind == '0' {
			result = typ
			continue
		}
		if typ.kind == '?' || result.kind == '?' {
			return pgUnknown
		}
		if result.kind == typ.kind {
			if typ.cast == "bigint" {
				result = typ
			}
			continue
		}
		if result.kind == 's' && typ.kind == 'i' {
			result = typ
			continue
		}
		if result.kind == 'i' && typ.kind == 's' {
			continue
		}
		if result.kind == 'i' && typ.kind == 'n' {
			result = typ
			continue
		}
		if result.kind == 'n' && typ.kind == 'i' {
			continue
		}
		return pgUnknown
	}
	return result
}

func (p *sqlServerPGView) caseExpression(start, end int, scope *pgViewScope) (pgViewExpr, int, error) {
	i, first := start+1, start+1
	type part struct {
		keyword string
		value   pgViewExpr
	}
	var parts []part
	var results []pgViewExpr
	keyword := "CASE"
	for i < end {
		if p.text(i) == "(" {
			i = p.close[i] + 1
			continue
		}
		if p.word(i) == "CASE" {
			_, next, err := p.caseExpression(i, end, scope)
			if err != nil {
				return pgViewExpr{}, next, err
			}
			i = next
			continue
		}
		w := p.word(i)
		if w == "WHEN" || w == "THEN" || w == "ELSE" || w == "END" {
			if first < i {
				x, next, err := p.expression(first, i, 0, scope)
				if err != nil {
					return x, next, err
				}
				if err = p.checkUnparsed(next, i); err != nil {
					return x, next, err
				}
				parts = append(parts, part{keyword, x})
				if keyword == "THEN" || keyword == "ELSE" {
					results = append(results, x)
				}
			} else if keyword == "CASE" {
				parts = append(parts, part{keyword: keyword})
			}
			if w == "END" {
				typ := pgViewCommonType(results)
				var out strings.Builder
				for _, part := range parts {
					x := part.value
					if (part.keyword == "THEN" || part.keyword == "ELSE") && typ.kind == 'i' && x.typ.kind == 's' {
						x.sql = sqlServerIntegerText(x.sql, typ.cast)
					}
					out.WriteString(part.keyword + " ")
					if x.sql != "" {
						out.WriteString(x.sql + " ")
					}
				}
				out.WriteString("END")
				return pgViewExpr{out.String(), typ}, i + 1, nil
			}
			keyword = w
			first = i + 1
		}
		i++
	}
	return pgViewExpr{}, i, p.issue("CASE 缺少 END")
}

func (p *sqlServerPGView) binary(left pgViewExpr, op string, right pgViewExpr) (pgViewExpr, error) {
	if op == "+" {
		if left.typ.kind == '0' {
			left.typ = right.typ
		}
		if right.typ.kind == '0' {
			right.typ = left.typ
		}
	}
	comparison := strings.Contains(" = < > <= >= <> != ", " "+op+" ")
	if op == "+" || comparison {
		if left.typ.kind == '?' || right.typ.kind == '?' {
			return pgViewExpr{}, p.issue("无法确定操作数类型: " + left.sql + " " + op + " " + right.sql)
		}
		if left.typ.kind == 's' && right.typ.kind == 'i' {
			left = pgViewExpr{sqlServerIntegerText(left.sql, right.typ.cast), right.typ}
		} else if left.typ.kind == 'i' && right.typ.kind == 's' {
			right = pgViewExpr{sqlServerIntegerText(right.sql, left.typ.cast), left.typ}
		} else if left.typ.kind == 's' && right.typ.kind == 'n' || left.typ.kind == 'n' && right.typ.kind == 's' {
			return pgViewExpr{}, p.issue("字符与非整数数值的隐式转换需人工确认精度")
		}
		if op == "+" && (left.typ.kind != 's' && left.typ.kind != 'i' && left.typ.kind != 'n' && left.typ.kind != '0' || right.typ.kind != 's' && right.typ.kind != 'i' && right.typ.kind != 'n' && right.typ.kind != '0') {
			return pgViewExpr{}, p.issue("暂不支持此类型的加法")
		}
	}
	typ := left.typ
	if op == "+" && left.typ.kind == 's' && right.typ.kind == 's' {
		op = "||"
	} else if comparison || op == "AND" || op == "OR" || op == "LIKE" {
		typ = pgViewType{kind: 'b'}
	} else if typ.kind == '0' {
		typ = right.typ
	} else if right.typ.kind == 'n' {
		typ = right.typ
	} else if right.typ.cast == "bigint" {
		typ = right.typ
	}
	return pgViewExpr{"(" + left.sql + " " + op + " " + right.sql + ")", typ}, nil
}

func (p *sqlServerPGView) window(value pgViewExpr, start, end int, scope *pgViewScope) (pgViewExpr, int, error) {
	open := start + 1
	if p.text(open) != "(" {
		return value, start, p.issue("不支持命名窗口")
	}
	close := p.close[open]
	for i := open + 1; i < close; {
		w := p.word(i)
		if (w != "PARTITION" && w != "ORDER") || p.word(i+1) != "BY" {
			return value, i, p.issue("不支持的窗口定义")
		}
		a, b := i+2, i+2
		for b < close {
			if p.text(b) == "(" {
				b = p.close[b] + 1
				continue
			}
			if p.word(b) == "ORDER" {
				break
			}
			b++
		}
		for _, segment := range p.split(a, b, ",") {
			x, next, err := p.expression(segment[0], segment[1], 0, scope)
			if err != nil {
				return value, next, err
			}
			if err = p.checkUnparsed(next, segment[1]); err != nil {
				return value, next, err
			}
			p.replace(segment[0], next, x.sql)
		}
		i = b
	}
	value.sql += " OVER (" + p.render(open+1, close) + ")"
	return value, close + 1, nil
}

// SQL Server treats empty/space-only character input as zero when converting
// to an integer. NULL stays NULL; malformed and overflowing numbers still fail.
func sqlServerIntegerText(sql, typ string) string {
	return "CAST(CASE WHEN btrim(" + sql + ") = '' THEN '0' ELSE " + sql + " END AS " + typ + ")"
}

func (p *sqlServerPGView) catalogCandidate(start, end int) string {
	for i := start; i+3 < end; i++ {
		if p.word(i) == "OBJECT_ID" && p.text(i+1) == "(" && p.tokens[i+2].kind == 's' && p.text(i+3) == ")" {
			name := decodeViewLiteral(p.text(i + 2))
			parts := strings.Split(name, ".")
			if len(parts) > 2 {
				return ""
			}
			name = strings.Trim(parts[len(parts)-1], "[]\"")
			lookup := "SELECT c.column_name AS name FROM information_schema.columns c WHERE c.table_schema = '" + strings.ReplaceAll(p.m.cfg.TargetSchema, "'", "''") + "' AND c.table_name = '" + strings.ReplaceAll(p.m.objName(name), "'", "''") + "'"
			// Only replace the recognized parenthesized syscolumns lookup. The
			// candidate is reference-only and explicitly requires name/join review.
			for open := i - 1; open >= start; open-- {
				if p.text(open) == "(" && p.close[open] == i+4 && p.word(open+1) == "SELECT" {
					j := open + 2
					if p.text(j) != "*" || p.word(j+1) != "FROM" {
						continue
					}
					j += 2
					if !p.ident(j) {
						continue
					}
					if p.text(j+1) == "." && (strings.EqualFold(p.tokens[j].name, "dbo") || strings.EqualFold(p.tokens[j].name, "sys")) {
						j += 2
					}
					if !p.ident(j) || !strings.EqualFold(p.tokens[j].name, "syscolumns") {
						continue
					}
					j++
					if p.word(j) != "WHERE" || !p.ident(j+1) || !strings.EqualFold(p.tokens[j+1].name, "id") || p.text(j+2) != "=" || j+3 != i {
						continue
					}
					p.replace(open+1, p.close[open], lookup)
					return "-- 仅供人工适配：请确认 name 返回源列名还是目标列名，并核对 table_struct.fieldName 的大小写/排序规则。\n" + p.render(start, end)
				}
			}
		}
	}
	return ""
}
