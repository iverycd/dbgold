package diff

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

func (p *planner) projectKey(t *tableProjection, raw Key) Key {
	key := raw
	key.Name = p.mappedName(raw.Name)
	key.Columns = mappedColumns(t, raw.Columns)
	if key.Kind != "primary" && p.src.Product == "mysql" && p.dst.Product != "mysql" {
		key.Name = p.mysqlKeyName(t.name, key)
	}
	if key.Kind == "foreign" {
		if raw.RefNamespace != "" && raw.RefNamespace != p.src.Namespace {
			key.Unsupported = "跨 Schema 外键需要人工指定引用范围"
			return key
		}
		key.RefNamespace = p.dst.Namespace
		key.RefTable = p.mappedName(raw.RefTable)
		if ref := p.tables[raw.RefTable]; ref != nil {
			key.RefTable = ref.name
			key.RefColumns = mappedColumns(ref, raw.RefColumns)
		}
	}
	return key
}

func mappedColumns(t *tableProjection, columns []string) []string {
	names := make([]string, len(columns))
	for i, name := range columns {
		names[i] = t.columns[name]
	}
	return names
}

func (p *planner) mysqlKeyName(table string, key Key) string {
	base := key.Name
	if p.dst.Product == "dameng" || p.indexCounts[base] > 1 {
		base = table + "_" + base
	}
	if validIdentifier(p.dst, base) {
		return base
	}
	if !utf8.ValidString(base) || p.dst.IdentifierLimit < 16 {
		return base
	}
	sum := sha256.Sum256([]byte(table + "\x00" + key.Name))
	head := base[:min(len(base), p.dst.IdentifierLimit-13)]
	for !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	return fmt.Sprintf("%s_%x", head, sum[:6])
}

func (p *planner) keyIssue(k Key) string {
	switch {
	case k.Unsupported != "":
		return k.Unsupported
	case k.Kind != "primary" && !validIdentifier(p.dst, k.Name):
		return "目标键或索引名称不符合数据库限制"
	case len(k.Columns) == 0:
		return "键列元数据不完整"
	case k.Method != "" && !strings.EqualFold(k.Method, "btree"):
		return "只支持普通 B-tree 键和索引"
	case slices.ContainsFunc(k.PrefixLengths, func(n int) bool { return n > 0 }):
		return "前缀键或索引需人工处理"
	case (k.Kind == "unique" || k.Kind == "primary") && slices.Contains(k.Descending, true):
		return "降序约束键需要人工核对"
	}
	for _, name := range k.Columns {
		if !validIdentifier(p.dst, name) {
			return "键列无法映射"
		}
	}
	if k.Kind == "foreign" {
		if len(k.Columns) != len(k.RefColumns) || !validIdentifier(p.dst, k.RefTable) {
			return "外键引用信息不完整"
		}
		for _, name := range k.RefColumns {
			if !validIdentifier(p.dst, name) {
				return "外键引用列无法映射"
			}
		}
		for _, action := range []string{k.OnDelete, k.OnUpdate} {
			switch referentialAction(action) {
			case "NO ACTION", "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT":
			default:
				return "外键动作需要人工确认"
			}
		}
		if p.dst.Product == "dameng" && (normalizeAction("mysql", k.OnUpdate) != "NO ACTION" || normalizeAction("mysql", k.OnDelete) == "SET DEFAULT") ||
			p.dst.Product == "mysql" && (referentialAction(k.OnUpdate) == "SET DEFAULT" || referentialAction(k.OnDelete) == "SET DEFAULT") {
			return "目标库无法保留外键动作"
		}
	}
	return ""
}

func quotedColumns(product string, names []string, descending []bool) string {
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = quote(product, name)
		if i < len(descending) && descending[i] {
			parts[i] += " DESC"
		}
	}
	return strings.Join(parts, ", ")
}

func (p *planner) keyClause(k Key) (string, string) {
	if reason := p.keyIssue(k); reason != "" {
		return "", reason
	}
	product := p.dst.Product
	columns := quotedColumns(product, k.Columns, nil)
	switch k.Kind {
	case "primary":
		// Primary names are engine-generated in MySQL. A cross-engine key
		// uses the target's primary-key convention.
		prefix := ""
		if p.src.Product != "mysql" && product != "mysql" && validIdentifier(p.dst, k.Name) {
			prefix = "CONSTRAINT " + quote(product, k.Name) + " "
		}
		return prefix + "PRIMARY KEY (" + columns + ")", ""
	case "unique":
		return "CONSTRAINT " + quote(product, k.Name) + " UNIQUE (" + columns + ")", ""
	case "foreign":
		out := "CONSTRAINT " + quote(product, k.Name) + " FOREIGN KEY (" + columns + ") REFERENCES " +
			p.qualified(k.RefTable) + " (" + quotedColumns(product, k.RefColumns, nil) + ")"
		if action := normalizeAction(product, k.OnDelete); action != "NO ACTION" {
			out += " ON DELETE " + action
		}
		if action := normalizeAction(product, k.OnUpdate); action != "NO ACTION" {
			out += " ON UPDATE " + action
		}
		return out, ""
	default:
		return "", "当前键类型需要人工处理"
	}
}

func referentialAction(action string) string {
	action = strings.ToUpper(strings.TrimSpace(action))
	if action == "" {
		return "NO ACTION"
	}
	return action
}

func normalizeAction(product, action string) string {
	action = referentialAction(action)
	if (product == "mysql" || product == "dameng") && action == "RESTRICT" {
		return "NO ACTION"
	}
	return action
}

func (p *planner) sameKey(a, b Key) bool {
	if a.Kind != b.Kind || a.Unsupported != "" || b.Unsupported != "" ||
		!slices.EqualFunc(a.Columns, b.Columns, func(a, b string) bool { return p.columnKey(a) == p.columnKey(b) }) ||
		!slices.EqualFunc(a.RefColumns, b.RefColumns, func(a, b string) bool { return p.columnKey(a) == p.columnKey(b) }) ||
		p.tableKey(a.RefTable) != p.tableKey(b.RefTable) || a.Kind == "foreign" && a.RefNamespace != b.RefNamespace ||
		normalizeAction(p.dst.Product, a.OnDelete) != normalizeAction(p.dst.Product, b.OnDelete) ||
		normalizeAction(p.dst.Product, a.OnUpdate) != normalizeAction(p.dst.Product, b.OnUpdate) ||
		!strings.EqualFold(firstReason(a.Method, "btree"), firstReason(b.Method, "btree")) {
		return false
	}
	for i := range a.Columns {
		aDesc := i < len(a.Descending) && a.Descending[i]
		bDesc := i < len(b.Descending) && b.Descending[i]
		aPrefix, bPrefix := 0, 0
		if i < len(a.PrefixLengths) {
			aPrefix = a.PrefixLengths[i]
		}
		if i < len(b.PrefixLengths) {
			bPrefix = b.PrefixLengths[i]
		}
		if aDesc != bDesc || aPrefix != bPrefix {
			return false
		}
	}
	return true
}

func (p *planner) keyNameMatches(a, b Key) bool {
	if a.Kind == "primary" && b.Kind == "primary" {
		return true
	}
	return a.Name != "" && p.columnKey(a.Name) == p.columnKey(b.Name)
}

func (p *planner) compareKeys(t *tableProjection, available map[string]columnAvailability) {
	var actual []Key
	if t.target != nil {
		actual = append(slices.Clone(t.target.Keys), t.target.Indexes...)
	}
	consumed := make([]bool, len(actual))
	expected := append(slices.Clone(t.source.Keys), t.source.Indexes...)
	// Match exact identifiers first, so an equivalent renamed key cannot hide
	// a same-name key whose definition changed.
	found := make([]int, len(expected))
	for i := range found {
		found[i] = -1
	}
	mapped := make([]Key, len(expected))
	for i, raw := range expected {
		mapped[i] = p.projectKey(t, raw)
		for j, current := range actual {
			if !consumed[j] && current.Kind == raw.Kind && p.keyNameMatches(mapped[i], current) {
				found[i], consumed[j] = j, true
				break
			}
		}
	}
	if p.src.Product != p.dst.Product {
		for i, wanted := range mapped {
			if found[i] >= 0 {
				continue
			}
			for j, current := range actual {
				if !consumed[j] && p.sameKey(wanted, current) {
					found[i], consumed[j] = j, true
					break
				}
			}
		}
	}

	for i, raw := range expected {
		key := mapped[i]
		if t.target == nil && (key.Kind == "primary" || key.Kind == "unique") {
			continue // Included in CREATE TABLE.
		}
		row := Change{Table: t.name, Object: firstReason(key.Name, raw.Name), Kind: key.Kind, Source: raw, Expected: key, Order: 30}
		if key.Kind == "foreign" {
			row.Order = 50
		}
		if match := found[i]; match >= 0 {
			current := actual[match]
			if p.sameKey(key, current) {
				continue
			}
			row.Target = current
			row.Reason = firstReason(key.Unsupported, current.Unsupported, "同名键或索引的定义不同")
		} else {
			p.planMissingKey(t, &row, key, available)
		}
		p.add(row)
	}
	for i, key := range actual {
		if !consumed[i] {
			p.add(Change{Table: t.name, Object: key.Name, Kind: key.Kind, Target: key, Reason: "键或索引仅存在于目标库"})
		}
	}
}

func (p *planner) planMissingKey(t *tableProjection, row *Change, key Key, available map[string]columnAvailability) {
	if row.Reason = p.keyIssue(key); row.Reason != "" {
		return
	}
	if key.Kind != "index" && t.target != nil {
		row.Reason = "已有表的键约束需人工处理"
		return
	}
	for _, col := range key.Columns {
		availability := available[col]
		if !availability.eligible {
			row.Reason = "索引或键依赖列的定义需要先处理"
			return
		}
		if availability.id != "" {
			row.DependsOn = append(row.DependsOn, availability.id)
		}
	}
	if key.Kind == "index" {
		scopeKey := key.Name
		if p.dst.Product == "mysql" {
			scopeKey = p.indexScope(t.name, key.Name)
		}
		if p.reserved[scopeKey] {
			row.Reason = "目标索引名称已被占用"
			return
		}
		p.reserved[scopeKey] = true
		index := quote(p.dst.Product, key.Name)
		if p.dst.Product == "dameng" {
			index = p.qualified(key.Name)
		}
		row.SQL = []string{"CREATE INDEX " + index + " ON " + p.qualified(t.name) +
			" (" + quotedColumns(p.dst.Product, key.Columns, key.Descending) + ")"}
		return
	}
	if key.Kind == "foreign" {
		if p.dst.Product == "mysql" && p.reserved[foreignScope(key.Name)] {
			row.Reason = "目标外键名称已被占用"
			return
		}
		ref := p.refTable(key.RefTable)
		if ref == nil || ref.reason != "" {
			row.Reason = "未能确认外键的引用表定义"
			return
		}
		if ref.target == nil && ref.createID == "" {
			row.Reason = "需先选择并创建外键的引用表"
			return
		}
		if !p.referenceKeyExists(ref, key.RefColumns) {
			row.Reason = "目标引用列缺少兼容的主键或唯一约束"
			return
		}
		if ref.createID != "" {
			row.DependsOn = append(row.DependsOn, ref.createID)
		}
		clause, reason := p.keyClause(key)
		if reason != "" {
			row.Reason = reason
			return
		}
		row.SQL = []string{"ALTER TABLE " + p.qualified(t.name) + " ADD " + clause}
		if p.dst.Product == "mysql" {
			p.reserved[foreignScope(key.Name)] = true
		}
	}
}

func (p *planner) indexScope(table, index string) string {
	return p.tableKey(table) + "\x00" + strings.ToLower(index)
}

func foreignScope(name string) string { return "foreign\x00" + strings.ToLower(name) }

func (p *planner) refTable(name string) *tableProjection {
	for _, table := range p.tables {
		if table.name == name {
			return table
		}
	}
	return nil
}

func (p *planner) referenceKeyExists(ref *tableProjection, columns []string) bool {
	keys := ref.source.Keys
	if ref.target != nil {
		keys = ref.target.Keys
	}
	for _, key := range keys {
		if ref.target == nil {
			key = p.projectKey(ref, key)
		}
		if (key.Kind == "primary" || key.Kind == "unique") && p.keyIssue(key) == "" &&
			slices.EqualFunc(key.Columns, columns, func(a, b string) bool { return p.columnKey(a) == p.columnKey(b) }) {
			return true
		}
	}
	return false
}
