package diff

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"dbgold/datamigrate/damengnames"
	"dbgold/datamigrate/source"
)

type tableProjection struct {
	source      *Table
	name        string
	target      *Table
	columns     map[string]string
	definitions []projectedColumn
	reason      string
	createID    string
}

type planner struct {
	src, dst    Catalog
	options     Options
	tables      map[string]*tableProjection
	indexCounts map[string]int
	reserved    map[string]bool
	changes     []Change
}

func Build(src, dst Catalog, options Options) []Change {
	p := planner{src: src, dst: dst, options: options, tables: map[string]*tableProjection{}, indexCounts: map[string]int{}, reserved: map[string]bool{}, changes: []Change{}}
	p.preflight()
	matched := map[string]bool{}
	requested := map[string]bool{}
	scoped := len(options.Tables) > 0
	for _, name := range options.Tables {
		requested[name] = false
	}
	names := sortedNames(p.tables)
	for _, name := range names {
		t := p.tables[name]
		if t.target != nil {
			matched[t.target.Name] = true
		}
		_, selectedSource := requested[name]
		_, selectedTarget := requested[t.name]
		if selectedSource {
			requested[name] = true
		}
		if selectedTarget {
			requested[t.name] = true
		}
		if scoped && !selectedSource && !selectedTarget {
			continue
		}
		if t.target == nil && t.reason == "" {
			p.prepareCreate(t)
		}
	}
	for _, name := range names {
		t := p.tables[name]
		if !scoped || requested[name] || requested[t.name] {
			p.compareTable(t)
		}
	}
	for _, name := range sortedNames(dst.Tables) {
		if matched[name] {
			continue
		}
		if scoped {
			if _, selected := requested[name]; !selected {
				continue
			}
			requested[name] = true
		}
		p.add(Change{Table: name, Kind: "table_extra", Target: dst.Tables[name], Reason: "表仅存在于目标库"})
	}
	for _, name := range sortedNames(requested) {
		if !requested[name] {
			p.add(Change{Table: name, Kind: "table_unsupported", Reason: "两端目录均未找到所选表"})
		}
	}
	slices.SortStableFunc(p.changes, func(a, b Change) int {
		if a.Order != b.Order {
			return a.Order - b.Order
		}
		return strings.Compare(changeKey(a), changeKey(b))
	})
	return p.changes
}

func sortedNames[V any](values map[string]V) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func changeKey(c Change) string { return c.Table + "\x00" + c.Kind + "\x00" + c.Object }

func changeID(table, kind, object string) string {
	sum := sha256.Sum256([]byte(changeKey(Change{Table: table, Kind: kind, Object: object})))
	return fmt.Sprintf("%x", sum[:16])
}

func (p *planner) add(c Change) string {
	c.ID = changeID(c.Table, c.Kind, c.Object)
	c.Status = Manual
	if c.Reason == "" && len(c.SQL) != 0 {
		c.Status = Applicable
	}
	c.DependsOn = slices.Compact(c.DependsOn)
	p.changes = append(p.changes, c)
	return c.ID
}

func (p *planner) mappedName(name string) string {
	if p.dst.Product == "dameng" {
		return strings.ToUpper(name)
	}
	if p.src.Product == "mysql" && source.IsPGFamily(p.dst.Product) && !p.options.PreserveCase {
		return strings.ToLower(name)
	}
	return name
}

func (p *planner) tableKey(value string) string {
	if p.dst.Product == "mysql" && p.dst.CaseMode != 0 {
		return strings.ToLower(value)
	}
	return value
}

func (p *planner) columnKey(value string) string {
	if p.dst.Product == "mysql" {
		return strings.ToLower(value)
	}
	return value
}

func (p *planner) preflight() {
	targetTables := map[string]*Table{}
	targetFolded := map[string]string{}
	for _, name := range sortedNames(p.dst.Tables) {
		targetTables[p.tableKey(name)] = p.dst.Tables[name]
	}
	for name := range p.dst.Occupied {
		p.reserved[p.tableKey(name)] = true
		targetFolded[strings.ToLower(name)] = name
	}
	for name := range p.dst.Tables {
		p.reserved[p.tableKey(name)] = true
		targetFolded[strings.ToLower(name)] = name
		table := p.dst.Tables[name]
		for _, index := range append(slices.Clone(table.Keys), table.Indexes...) {
			p.reserved[p.indexScope(name, index.Name)] = true
			if index.Kind == "foreign" && p.dst.Product == "mysql" {
				p.reserved[foreignScope(index.Name)] = true
			}
		}
	}
	byTarget := map[string]*tableProjection{}
	for _, name := range sortedNames(p.src.Tables) {
		table := p.src.Tables[name]
		mapped := p.mappedName(name)
		if p.dst.Product == "mysql" && p.dst.CaseMode == 1 {
			mapped = strings.ToLower(mapped)
		}
		key := p.tableKey(mapped)
		t := &tableProjection{source: table, name: mapped, target: targetTables[key], reason: table.Unsupported}
		p.tables[name] = t
		if t.target != nil {
			t.name = t.target.Name
			if t.target.Unsupported != "" {
				t.reason = t.target.Unsupported
			}
		} else if p.reserved[key] || targetFolded[strings.ToLower(mapped)] != "" {
			t.reason = "目标名称已被占用或存在大小写冲突"
		}
		if !validIdentifier(p.dst, mapped) {
			t.reason = "目标表名不符合数据库限制"
		}
		if prior := byTarget[key]; prior != nil {
			t.reason = "多个源表映射到同一目标名称"
			prior.reason = t.reason
		}
		byTarget[key] = t
		p.projectColumns(t)
		for _, index := range append(slices.Clone(table.Keys), table.Indexes...) {
			p.indexCounts[p.mappedName(index.Name)]++
		}
	}
	for _, name := range sortedNames(byTarget) {
		p.reserved[name] = true
	}
}

func (p *planner) projectColumns(t *tableProjection) {
	names := make([]string, 0, len(t.source.Columns))
	for _, c := range t.source.Columns {
		names = append(names, p.mappedName(c.Name))
	}
	if p.dst.Product == "dameng" {
		var err error
		names, err = damengnames.Plan(names)
		if err != nil {
			t.reason = err.Error()
			return
		}
	}
	seen := map[string]bool{}
	t.columns = map[string]string{}
	for i, c := range t.source.Columns {
		name := names[i]
		if t.target != nil && p.dst.Product == "mysql" {
			for _, current := range t.target.Columns {
				if strings.EqualFold(current.Name, name) {
					name = current.Name
					break
				}
			}
		}
		key := p.columnKey(name)
		if seen[key] {
			t.reason = "多个源列映射到同一目标名称"
			return
		}
		seen[key] = true
		t.columns[c.Name] = name
		t.definitions = append(t.definitions, projectColumn(c, name, p.src, p.dst, p.options))
	}
}

func (p *planner) compareTable(t *tableProjection) {
	if t.reason != "" {
		kind := "table_unsupported"
		if t.target == nil {
			kind = "table_missing"
		}
		object := ""
		if t.source.Name != t.name {
			object = t.source.Name
		}
		p.add(Change{Table: t.name, Object: object, Kind: kind, Source: t.source, Target: optionalTable(t.target), Reason: t.reason})
		return
	}
	if t.target == nil && t.createID == "" {
		return // The manual CREATE row already describes the blocker.
	}
	if t.target != nil && p.src.Product == "mysql" && p.dst.Product == "mysql" &&
		(t.source.Engine != t.target.Engine || t.source.Collation != t.target.Collation) {
		p.add(Change{
			Table: t.name, Kind: "table_definition", Source: tableOptions(t.source),
			Expected: tableOptions(t.source), Target: tableOptions(t.target), Reason: "已有表的引擎或默认排序规则需要人工处理",
		})
	}
	available := p.compareColumns(t)
	p.compareKeys(t, available)
	p.compareComments(t, available)
}

func tableOptions(t *Table) map[string]string {
	return map[string]string{"engine": t.Engine, "charset": t.Charset, "collation": t.Collation}
}

func optionalTable(t *Table) any {
	if t == nil {
		return nil
	}
	return t
}

func (p *planner) prepareCreate(t *tableProjection) {
	c := Change{Table: t.name, Kind: "table_missing", Source: t.source, Order: 10}
	definitions := make([]columnDefinition, 0, len(t.definitions))
	for _, col := range t.definitions {
		definitions = append(definitions, col.definition)
	}
	keys := make([]Key, 0)
	var reserved []string
	items := make([]string, 0, len(t.definitions)+len(t.source.Keys))
	for _, col := range t.definitions {
		items = append(items, col.body)
		if c.Reason == "" && col.reason != "" {
			c.Reason = col.definition.Name + "：" + col.reason
		}
	}
	for _, raw := range t.source.Keys {
		if raw.Kind == "foreign" {
			continue
		}
		key := p.projectKey(t, raw)
		keys = append(keys, key)
		clause, reason := p.keyClause(key)
		if reason == "" {
			reason = key.Unsupported
		}
		if reason == "" && p.dst.Product != "mysql" &&
			(key.Kind == "unique" || key.Kind == "primary" && p.src.Product != "mysql" && key.Name != "") {
			if p.reserved[key.Name] || slices.Contains(reserved, key.Name) {
				reason = "键的索引名称已被其他对象占用"
			} else {
				reserved = append(reserved, key.Name)
			}
		}
		if reason != "" && c.Reason == "" {
			c.Reason = key.Name + "：" + reason
		}
		if reason == "" {
			items = append(items, clause)
		}
	}
	c.Expected = struct {
		Name    string             `json:"name"`
		Columns []columnDefinition `json:"columns"`
		Keys    []Key              `json:"keys"`
	}{t.name, definitions, keys}
	if len(t.definitions) == 0 {
		c.Reason = "表的列定义不可见"
	}
	if p.dst.Product == "gaussdb" && strings.Contains(strings.ToLower(p.dst.Mode), "distribut") {
		c.Reason = "分布式目标需要单独确认分布键和约束"
	}
	suffix := ""
	if p.dst.Product == "mysql" {
		if !sqlWord.MatchString(t.source.Engine) || !sqlWord.MatchString(t.source.Charset) || !sqlWord.MatchString(t.source.Collation) {
			c.Reason = "缺少可验证的 MySQL 表引擎或字符集定义"
		} else {
			suffix = " ENGINE=" + t.source.Engine + " DEFAULT CHARACTER SET " + t.source.Charset + " COLLATE " + t.source.Collation
		}
		if t.source.Comment != "" {
			if reason := commentIssue(t.source.Comment, 2048); reason != "" {
				c.Reason = reason
			}
			suffix += " COMMENT=" + sqlString(t.source.Comment)
		}
	}
	if c.Reason == "" {
		c.SQL = []string{"CREATE TABLE " + p.qualified(t.name) + " (\n  " + strings.Join(items, ",\n  ") + "\n)" + suffix}
		for _, name := range reserved {
			p.reserved[name] = true
		}
		t.createID = p.add(c)
		return
	}
	p.add(c)
}

type columnAvailability struct {
	id       string
	eligible bool
}

func (p *planner) compareColumns(t *tableProjection) map[string]columnAvailability {
	existing := map[string]Column{}
	folded := map[string]bool{}
	available := map[string]columnAvailability{}
	if t.target != nil {
		for _, col := range t.target.Columns {
			existing[p.columnKey(col.Name)] = col
			folded[strings.ToLower(col.Name)] = true
		}
	}
	for i, src := range t.source.Columns {
		expected := t.definitions[i]
		name := expected.definition.Name
		if t.target == nil {
			available[name] = columnAvailability{t.createID, true}
			continue
		}
		key := p.columnKey(name)
		current, exists := existing[key]
		row := Change{Table: t.name, Object: name, Source: displayColumn(src), Expected: expected.definition, Order: 20}
		if !exists {
			row.Kind = "column_missing"
			row.Reason = expected.reason
			switch {
			case folded[strings.ToLower(name)]:
				row.Reason = "目标存在仅大小写不同的列"
			case row.Reason != "":
			case !expected.definition.Nullable || expected.definition.Default != nil || expected.definition.Identity != "":
				row.Reason = "已有表只支持新增可空、无默认值的普通列"
			default:
				row.SQL = []string{"ALTER TABLE " + p.qualified(t.name) + " ADD COLUMN " + expected.body}
			}
			available[name] = columnAvailability{p.add(row), row.Reason == ""}
			continue
		}
		delete(existing, key)
		row.Target = displayColumn(current)
		matches := columnMatches(p.dst.Product, expected.definition, displayColumn(current), p.src.Product == "mysql")
		available[name] = columnAvailability{eligible: matches && expected.reason == "" && current.Unsupported == ""}
		if !matches || src.Unsupported != "" || current.Unsupported != "" || p.src.Product != p.dst.Product && expected.reason != "" {
			row.Kind = "column_definition"
			row.Reason = firstReason(expected.reason, current.Unsupported, "已有列的结构变更需人工处理")
			p.add(row)
		}
	}
	for _, name := range sortedNames(existing) {
		col := existing[name]
		p.add(Change{Table: t.name, Object: col.Name, Kind: "column_extra", Target: displayColumn(col), Reason: "列仅存在于目标库"})
	}
	return available
}

func firstReason(reasons ...string) string {
	for _, reason := range reasons {
		if reason != "" {
			return reason
		}
	}
	return ""
}

func (p *planner) compareComments(t *tableProjection, available map[string]columnAvailability) {
	if t.target == nil && p.dst.Product == "mysql" {
		return // A new MySQL table carries its comments inline.
	}
	oldComment := ""
	if t.target != nil {
		oldComment = t.target.Comment
	}
	p.comment(t, "", t.source.Comment, oldComment, t.createID, "")
	existing := map[string]Column{}
	if t.target != nil {
		for _, col := range t.target.Columns {
			existing[p.columnKey(col.Name)] = col
		}
	}
	for _, col := range t.source.Columns {
		name := t.columns[col.Name]
		current, found := existing[p.columnKey(name)]
		link := available[name]
		if !found && p.dst.Product == "mysql" && link.eligible {
			continue // ADD COLUMN carries the comment inline.
		}
		reason := ""
		if !found && !link.eligible {
			reason = "需先创建对应列"
		}
		p.comment(t, name, col.Comment, current.Comment, link.id, reason)
	}
}

func (p *planner) comment(t *tableProjection, column, text, current, dependency, reason string) {
	if text == current {
		return
	}
	kind := "table_comment"
	if column != "" {
		kind = "column_comment"
	}
	row := Change{Table: t.name, Object: column, Kind: kind, Order: 40, Source: text, Expected: text, Target: current, Reason: reason}
	if t.target == nil {
		row.Target = nil
	}
	if dependency != "" {
		row.DependsOn = []string{dependency}
	}
	if row.Reason == "" {
		stmt, err := p.commentSQL(t.name, column, text)
		if err != nil {
			row.Reason = err.Error()
		} else {
			row.SQL = []string{stmt}
		}
	}
	p.add(row)
}

func (p *planner) commentSQL(table, column, text string) (string, error) {
	product := p.dst.Product
	ref := p.qualified(table)
	limit := 0
	if product == "mysql" {
		limit = 2048
		if column != "" {
			limit = 1024
		}
	}
	if reason := commentIssue(text, limit); reason != "" {
		return "", fmt.Errorf("%s", reason)
	}
	if product == "mysql" {
		if column == "" {
			return "ALTER TABLE " + ref + " COMMENT = " + sqlString(text), nil
		}
		if p.options.MySQLColumnComment == nil {
			return "", fmt.Errorf("缺少 MySQL 目标列的完整定义")
		}
		return p.options.MySQLColumnComment(table, column, text)
	}
	kind := "TABLE"
	if column != "" {
		kind = "COLUMN"
		ref += "." + quote(product, column)
	}
	value := "NULL"
	if text != "" {
		value = sqlString(text)
	}
	return "COMMENT ON " + kind + " " + ref + " IS " + value, nil
}

func validIdentifier(c Catalog, value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
		return false
	}
	length := len(value)
	if c.Product == "mysql" {
		length = utf8.RuneCountInString(value)
	}
	return c.IdentifierLimit <= 0 || length <= c.IdentifierLimit
}

func quote(product, value string) string {
	q := `"`
	if product == "mysql" {
		q = "`"
	}
	return q + strings.ReplaceAll(value, q, q+q) + q
}

func (p *planner) qualified(table string) string {
	return quote(p.dst.Product, p.dst.Namespace) + "." + quote(p.dst.Product, table)
}
