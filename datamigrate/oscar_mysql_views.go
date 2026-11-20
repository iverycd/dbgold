package datamigrate

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"dbgold/datamigrate/source"
)

type oscarViewRelation struct {
	name, alias string
	columns     []source.ColumnInfo
}

type oscarMySQLViewParser struct {
	tokens    []viewToken
	pos       int
	relations []oscarViewRelation
	quote     func(string) string
	column    func(string, string) string
}

func (p *oscarMySQLViewParser) take(value string) bool {
	if p.pos < len(p.tokens) && p.tokens[p.pos].kind != 'q' && p.tokens[p.pos].kind != 's' && strings.EqualFold(p.tokens[p.pos].text, value) {
		p.pos++
		return true
	}
	return false
}

func (p *oscarMySQLViewParser) ident() (viewToken, error) {
	if p.pos < len(p.tokens) {
		t := p.tokens[p.pos]
		if t.kind == 'q' && strings.HasPrefix(t.text, `"`) || t.kind == 'i' && !oscarViewKeywords[strings.ToUpper(t.name)] {
			p.pos++
			return t, nil
		}
	}
	return viewToken{}, fmt.Errorf("预期原生神通标识符")
}

func oscarIdentifierMatches(t viewToken, name string) bool {
	return t.name == name || t.kind == 'i' && strings.EqualFold(t.name, name)
}

func (p *oscarMySQLViewParser) reference() (string, source.ColumnInfo, error) {
	name, err := p.ident()
	if err != nil {
		return "", source.ColumnInfo{}, err
	}
	var qualifier viewToken
	if p.take(".") {
		qualifier = name
		name, err = p.ident()
		if err != nil {
			return "", source.ColumnInfo{}, err
		}
	}
	var found source.ColumnInfo
	var sql string
	for _, r := range p.relations {
		if qualifier.name != "" && !oscarIdentifierMatches(qualifier, r.alias) {
			continue
		}
		for _, col := range r.columns {
			if !oscarIdentifierMatches(name, col.Name) {
				continue
			}
			if sql != "" {
				return "", source.ColumnInfo{}, fmt.Errorf("视图列 %q 的源身份不唯一", name.name)
			}
			found, sql = col, p.quote(r.alias)+"."+p.quote(p.column(r.name, col.Name))
		}
	}
	if sql == "" {
		return "", source.ColumnInfo{}, fmt.Errorf("未找到视图引用列 %q", name.name)
	}
	return sql, found, nil
}

func (p *oscarMySQLViewParser) operand() (string, bool, error) {
	start := p.pos
	sign := ""
	if p.take("-") {
		sign = "-"
	}
	if p.pos < len(p.tokens) && p.tokens[p.pos].kind == 'p' {
		s := sign + p.tokens[p.pos].text
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			p.pos++
			return strconv.FormatInt(n, 10), true, nil
		}
	}
	p.pos = start
	sql, col, err := p.reference()
	switch strings.ToLower(strings.TrimSpace(col.DataType)) {
	case "int2", "smallint", "int4", "int", "integer", "int8", "bigint":
		return sql, true, err
	default:
		return sql, false, err
	}
}

func (p *oscarMySQLViewParser) predicate(depth int) (string, error) {
	if depth > 32 {
		return "", fmt.Errorf("视图条件嵌套超过可验证范围")
	}
	var parts []string
	for {
		var condition string
		if p.take("(") {
			inner, err := p.predicate(depth + 1)
			if err != nil || !p.take(")") {
				return "", fmt.Errorf("不支持的神通视图括号条件: %v", err)
			}
			condition = "(" + inner + ")"
		} else {
			left, integral, err := p.operand()
			if err != nil {
				return "", err
			}
			if p.take("IS") {
				op := " IS "
				if p.take("NOT") {
					op += "NOT "
				}
				if !p.take("NULL") {
					return "", fmt.Errorf("视图仅支持 IS [NOT] NULL 条件")
				}
				condition = left + op + "NULL"
			} else {
				op := ""
				for _, candidate := range []string{"=", "<", ">", "!"} {
					if p.take(candidate) {
						op = candidate
						break
					}
				}
				if (op == "<" || op == ">" || op == "!") && p.take("=") {
					op += "="
				} else if op == "<" && p.take(">") {
					op = "<>"
				}
				if op == "" || op == "!" || !integral {
					return "", fmt.Errorf("视图仅支持类型等价的整数比较及 NULL 条件")
				}
				right, integer, err := p.operand()
				if err != nil || !integer {
					return "", fmt.Errorf("无法证明视图比较条件的两侧都是整数: %v", err)
				}
				condition = left + " " + op + " " + right
			}
		}
		parts = append(parts, condition)
		if p.take("AND") {
			parts = append(parts, "AND")
		} else if p.take("OR") {
			parts = append(parts, "OR")
		} else {
			return strings.Join(parts, " "), nil
		}
	}
}

func (m *Migrator) prepareOscarMySQLView(ctx context.Context, view source.ViewInfo) (source.ViewInfo, error) {
	if view.Unsupported != "" {
		return view, fmt.Errorf("%s", view.Unsupported)
	}
	raw, err := lexOscarView(view.Definition)
	if err != nil {
		return view, err
	}
	var tokens []viewToken
	for _, token := range raw {
		if token.kind != 'w' {
			tokens = append(tokens, token)
		}
	}
	if len(tokens) > 0 && tokens[len(tokens)-1].text == ";" {
		tokens = tokens[:len(tokens)-1]
	}
	if len(tokens) >= 2 && tokens[0].text == "(" && tokens[len(tokens)-1].text == ")" {
		tokens = tokens[1 : len(tokens)-1]
	}
	d := m.writer.Dialect()
	p := oscarMySQLViewParser{tokens: tokens, quote: d.QuoteIdent, column: m.targetColumn}
	if !p.take("SELECT") {
		return view, fmt.Errorf("仅支持神通基础 SELECT 视图")
	}
	for p.pos < len(tokens) && !p.take("FROM") {
		p.pos++
	}
	if p.pos >= len(tokens) || p.pos <= 2 {
		return view, fmt.Errorf("视图必须包含明确的 SELECT 列和 FROM")
	}
	projection := tokens[1 : p.pos-1]
	schema := ""
	if r, ok := m.reader.(interface{ MigrationSchema() string }); ok {
		schema = r.MigrationSchema()
	}
	tables, err := m.reader.ListTables(ctx)
	if err != nil {
		return view, err
	}
	views, err := m.readViews(ctx)
	if err != nil {
		return view, err
	}
	readRelation := func() (string, error) {
		name, err := p.ident()
		if err != nil {
			return "", err
		}
		if p.take(".") {
			if !oscarIdentifierMatches(name, schema) {
				return "", fmt.Errorf("视图引用了源 Schema 之外的关系: %s", name.name)
			}
			name, err = p.ident()
			if err != nil {
				return "", err
			}
		}
		r := oscarViewRelation{}
		for _, table := range tables {
			if !oscarIdentifierMatches(name, table) {
				continue
			}
			if r.name != "" {
				return "", fmt.Errorf("源关系 %s 的身份不唯一", name.name)
			}
			info, e := m.sourceTableDDLInfo(ctx, table)
			if e != nil {
				return "", e
			}
			if info == nil {
				return "", fmt.Errorf("源关系 %s 缺少列信息", table)
			}
			r.name, r.columns = table, info.Columns
		}
		for _, dependency := range views {
			if !oscarIdentifierMatches(name, dependency.ViewName) {
				continue
			}
			if r.name != "" {
				return "", fmt.Errorf("源关系 %s 的身份不唯一", name.name)
			}
			r.name, r.columns = dependency.ViewName, dependency.Columns
		}
		if r.name == "" || len(r.columns) == 0 {
			return "", fmt.Errorf("找不到源关系 %s 的列定义", name.name)
		}
		if e := m.migrationTargetTableError(m.objName(r.name)); e != nil {
			return "", fmt.Errorf("依赖关系 %s 迁移失败: %w", r.name, e)
		}
		r.alias = r.name
		if p.take("AS") {
			alias, err := p.ident()
			if err != nil {
				return "", err
			}
			r.alias = alias.name
		} else if alias, err := p.ident(); err == nil {
			r.alias = alias.name
		}
		for _, previous := range p.relations {
			if strings.EqualFold(previous.alias, r.alias) {
				return "", fmt.Errorf("视图关系别名 %s 冲突", r.alias)
			}
		}
		p.relations = append(p.relations, r)
		return d.QualifyTable(m.cfg.TargetSchema, m.objName(r.name)) + " AS " + d.QuoteIdent(r.alias), nil
	}
	from, err := readRelation()
	if err != nil {
		return view, err
	}
	for p.pos < len(tokens) {
		join := " INNER JOIN "
		if p.take("LEFT") {
			p.take("OUTER")
			join = " LEFT JOIN "
		} else if p.take("WHERE") {
			condition, e := p.predicate(0)
			if e != nil || p.pos != len(tokens) {
				return view, fmt.Errorf("不支持的神通视图过滤条件: %v", e)
			}
			from += " WHERE " + condition
			break
		} else {
			p.take("INNER")
		}
		if !p.take("JOIN") {
			return view, fmt.Errorf("视图仅支持 INNER JOIN 和 LEFT JOIN")
		}
		relation, e := readRelation()
		if e != nil {
			return view, e
		}
		if !p.take("ON") {
			return view, fmt.Errorf("视图 JOIN 必须包含 ON 条件")
		}
		condition, e := p.predicate(0)
		if e != nil {
			return view, e
		}
		from += join + relation + " ON " + condition
	}
	p.pos, p.tokens = 0, projection
	var selectColumns []string
	var aliases = map[string]bool{}
	for p.pos < len(projection) {
		ref, col, err := p.reference()
		if err != nil {
			return view, err
		}
		alias := col.Name
		if p.take("AS") {
			as, err := p.ident()
			if err != nil {
				return view, err
			}
			alias = as.name
		} else if as, err := p.ident(); err == nil {
			alias = as.name
		}
		if i := len(selectColumns); i < len(view.Columns) {
			alias = view.Columns[i].Name
		}
		alias = m.objName(alias)
		if aliases[strings.ToLower(alias)] || len([]rune(alias)) > 64 {
			return view, fmt.Errorf("目标视图列名冲突或超过 64 字符: %s", alias)
		}
		aliases[strings.ToLower(alias)] = true
		selectColumns = append(selectColumns, ref+" AS "+d.QuoteIdent(alias))
		if p.pos < len(projection) && (!p.take(",") || p.pos == len(projection)) {
			return view, fmt.Errorf("视图仅支持直接列及列别名")
		}
	}
	if len(view.Columns) > 0 && len(selectColumns) != len(view.Columns) {
		return view, fmt.Errorf("视图投影与源列目录不一致")
	}
	view.ViewName = m.objName(view.ViewName)
	view.Definition = "SELECT " + strings.Join(selectColumns, ", ") + " FROM " + from
	return view, nil
}

func (m *Migrator) migrateOscarMySQLViews(ctx context.Context, names []string) []ObjectResult {
	if _, err := configureOscarMySQL(ctx, m.writer); err != nil {
		return []ObjectResult{{Name: "MySQL 预检", Stage: "preflight", Error: err.Error()}}
	}
	if m.repairs == nil {
		m.resetSourceTableDDLCache()
		defer m.clearSourceTableDDLCache()
		m.resetOscarMySQLViews()
	}
	m.tableState.mu.Lock()
	m.tableState.objects, m.tableState.failures = nil, nil
	m.tableState.mu.Unlock()
	views, err := m.readViews(ctx)
	if err != nil {
		return []ObjectResult{{Name: "神通视图元数据", Stage: "metadata", Error: err.Error()}}
	}
	byName := map[string]source.ViewInfo{}
	for _, v := range views {
		byName[v.ViewName] = v
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ctx = context.WithValue(ctx, oracleDamengTaskKey{}, cancel)
	var selected []source.ViewInfo
	var results []ObjectResult
	for _, name := range names {
		if v, ok := byName[name]; ok {
			selected = append(selected, v)
			delete(byName, name)
		} else {
			results = append(results, ObjectResult{Name: name, Stage: "preflight", Error: "视图不存在或重复选择"})
		}
	}
	category := newCategoryReport()
	results = append(results, m.migrateIndependentViews(ctx, selected, &category)...)
	if ctx.Err() != nil {
		results = append(results, ObjectResult{Name: "视图迁移", Stage: "task", Error: context.Cause(ctx).Error()})
	}
	return results
}

func (m *Migrator) resetOscarMySQLViews() {
	m.oscarMySQLViews, m.oscarMySQLViewsError, m.oscarMySQLViewsRead = nil, nil, false
}
