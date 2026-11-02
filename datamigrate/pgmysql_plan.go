package datamigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/typemap"
)

type ObjectRef struct {
	Kind  string `json:"kind"`
	Table string `json:"table,omitempty"`
	Name  string `json:"name"`
}
type pgObjectError struct {
	Object ObjectRef
	Err    error
}

func (e *pgObjectError) Error() string { return e.Err.Error() }
func (e *pgObjectError) Unwrap() error { return e.Err }

type CompatibilityIssue struct {
	Object     ObjectRef `json:"object"`
	Severity   string    `json:"severity"`
	Reason     string    `json:"reason"`
	Suggestion string    `json:"suggestion"`
	Excludable bool      `json:"excludable"`
	Definition string    `json:"definition,omitempty"`
}
type PlannedObject struct {
	Object     ObjectRef           `json:"object"`
	Target     string              `json:"target"`
	Stage      string              `json:"stage"`
	Statements []dialect.Statement `json:"statements"`
}
type PGMySQLPreflight struct {
	PartitionPreflight *PartitionPreflight  `json:"partition_preflight,omitempty"`
	Ready              bool                 `json:"ready"`
	CanStart           bool                 `json:"can_start"`
	Source             source.PGProfile     `json:"source"`
	Target             target.MySQLProfile  `json:"target"`
	Issues             []CompatibilityIssue `json:"issues"`
	Excluded           []ObjectRef          `json:"excluded"`
	Objects            []PlannedObject      `json:"objects"`
	RowsChecked        int64                `json:"rows_checked"`
	CheckScope         string               `json:"check_scope,omitempty"`
	textRules          map[string]map[string]target.MySQLTextRules
	tables             []source.PGTable
	snapshot           *source.PGSnapshot
	validationFKs      []source.FKInfo
	blocked            map[ObjectRef]error
	dependencies       map[ObjectRef][]ObjectRef
	globalError        bool
	viewOnly           bool
	objectOnly         bool
	targetDependencies []target.MySQLForeignKey
	targetSnapshot     []target.MySQLForeignKey
}
type pgSnapshotReader interface {
	ReadPGSnapshot(context.Context) (*source.PGSnapshot, error)
}
type pgScopedSnapshotReader interface {
	ReadPGSnapshotForTables(context.Context, []string) (*source.PGSnapshot, error)
}
type mysqlInspector interface {
	InspectMySQL(context.Context) (target.MySQLProfile, error)
	InspectionDB() *sql.DB
}

func (m *Migrator) isPGMySQL() bool {
	return source.IsPGFamily(m.reader.DBType()) && m.writer.DBType() == "mysql"
}
func (p *PGMySQLPreflight) add(ref ObjectRef, reason string, excludable bool) {
	p.Issues = append(p.Issues, CompatibilityIssue{Object: ref, Severity: "error", Reason: reason, Excludable: excludable, Suggestion: "受影响表及依赖对象将跳过；可调整定义后重新预检"})
	p.blockIssue(ref, fmt.Errorf("%s", reason))
}
func (p *PGMySQLPreflight) warn(ref ObjectRef, reason string) {
	p.Issues = append(p.Issues, CompatibilityIssue{Object: ref, Severity: "warning", Reason: reason, Suggestion: "迁移期间保持源库停写，并按此映射调整应用"})
}
func (p *PGMySQLPreflight) finish() {
	p.Ready = !p.globalError
	for _, issue := range p.Issues {
		if issue.Severity == "error" {
			p.Ready = false
		}
	}
	p.CanStart = false
	if p.globalError {
		return
	}
	if p.viewOnly || p.objectOnly {
		for _, obj := range p.Objects {
			if obj.Stage != "drop_dependency" && !p.objectBlocked(obj.Object) {
				p.CanStart = true
				break
			}
		}
	} else {
		for _, t := range p.tables {
			if !p.objectBlocked(tableRef(t.Info.TableName)) {
				p.CanStart = true
				break
			}
		}
	}
}

func tableRef(t string) ObjectRef { return ObjectRef{Kind: "table", Name: t} }
func idxRef(idx source.IndexInfo) ObjectRef {
	k := "index"
	if idx.IsPrimary {
		k = "primary_key"
	}
	return ObjectRef{k, idx.TableName, idx.IndexName}
}
func pgRef(o source.PGObject) ObjectRef { return ObjectRef{o.Kind, o.Table, o.Name} }
func (m *Migrator) wantPGObject(kind string) bool {
	if len(m.cfg.ViewNames) > 0 {
		return kind == "view"
	}
	if !m.cfg.objectMode() {
		return true
	}
	key := map[string]string{"primary_key": "primary_keys", "index": "indexes", "sequence": "sequences", "foreign_key": "foreign_keys", "comment": "comments"}[kind]
	for _, o := range m.cfg.Objects {
		if o == key {
			return true
		}
	}
	return false
}

// PreflightPGMySQL performs structure checks and SQL planning without scanning business rows.
// It never calls Writer mutation methods. Run always builds a fresh plan.
func (m *Migrator) PreflightPGMySQL(ctx context.Context) *PGMySQLPreflight {
	p := &PGMySQLPreflight{blocked: map[ObjectRef]error{}, dependencies: map[ObjectRef][]ObjectRef{}, viewOnly: len(m.cfg.ViewNames) > 0, objectOnly: m.cfg.objectMode(), CheckScope: "structure", Issues: []CompatibilityIssue{}, Objects: []PlannedObject{}, Excluded: append([]ObjectRef{}, m.cfg.ExcludedObjects...)}
	if _, ok := m.reader.(source.PartitionReader); ok {
		p.PartitionPreflight = m.preparePartitions(ctx)
	}
	defer func() {
		if pp := p.PartitionPreflight; pp != nil {
			pp.CanStart = pp.CanStart && p.CanStart
			p.CanStart = pp.CanStart
			if !pp.Ready {
				p.Ready = false
				p.Issues = append(p.Issues, pp.Issues...)
			}
		}
	}()
	fail := func(err error) *PGMySQLPreflight {
		p.add(ObjectRef{Kind: "metadata", Name: "兼容预检"}, err.Error(), false)
		return p
	}
	if !m.isPGMySQL() {
		return fail(fmt.Errorf("此预检仅支持 PG 系 → MySQL"))
	}
	if pp := p.PartitionPreflight; pp != nil && (pp.FatalError != "" || !pp.CanStart && len(pp.Tables) > 0) {
		return p // The partition diagnostics already explain a wholly blocked scope.
	}
	r, ok := m.reader.(pgSnapshotReader)
	if !ok {
		return fail(fmt.Errorf("源 Reader 不具备完整元数据能力"))
	}
	w, ok := m.writer.(mysqlInspector)
	if !ok {
		return fail(fmt.Errorf("目标 Writer 不具备版本探测能力"))
	}
	var err error
	p.Target, err = w.InspectMySQL(ctx)
	if err != nil {
		return fail(err)
	}
	var snap *source.PGSnapshot
	if tolerant, enabled := r.(interface {
		ReadPGMigrationSnapshot(context.Context, []string) (*source.PGSnapshot, error)
	}); enabled {
		var chosen []string
		if m.partitionState != nil && p.PartitionPreflight != nil {
			chosen = m.pgMySQLSnapshotTables()
		}
		snap, err = tolerant.ReadPGMigrationSnapshot(ctx, chosen)
	} else if scoped, enabled := r.(pgScopedSnapshotReader); enabled && m.partitionState != nil && p.PartitionPreflight != nil {
		chosen := m.pgMySQLSnapshotTables()
		if len(chosen) == 0 && len(m.partitionState.blocked) > 0 && len(m.cfg.ViewNames) == 0 {
			p.PartitionPreflight.CanStart = false
			return p
		}
		snap, err = scoped.ReadPGSnapshotForTables(ctx, chosen)
	} else {
		snap, err = r.ReadPGSnapshot(ctx)
	}
	if err != nil {
		return fail(err)
	}
	p.Source = snap.Profile
	p.snapshot = snap
	m.cfg.TargetSchema = p.Target.Database
	if m.cfg.Mode != "" && m.cfg.Mode != "all" && m.cfg.Mode != "include" && m.cfg.Mode != "exclude" {
		return fail(fmt.Errorf("非法表过滤模式"))
	}
	if m.cfg.Content != "" && m.cfg.Content != "both" && m.cfg.Content != "schema_only" && m.cfg.Content != "data_only" {
		return fail(fmt.Errorf("非法迁移内容"))
	}
	if m.cfg.Distributed || m.cfg.ChangeOwner {
		return fail(fmt.Errorf("MySQL 不支持分布列或修改对象 owner"))
	}
	available := map[ObjectRef]bool{}
	tablesByName := map[string]source.PGTable{}
	var all []string
	knownTables := map[string]bool{}
	if s := m.partitionState; s != nil && p.PartitionPreflight != nil {
		for _, name := range s.all {
			all = append(all, name)
			knownTables[name] = true
			available[tableRef(name)] = true
		}
	}
	for _, t := range snap.Tables {
		if !knownTables[t.Info.TableName] {
			all = append(all, t.Info.TableName)
			knownTables[t.Info.TableName] = true
		}
		tablesByName[t.Info.TableName] = t
		available[tableRef(t.Info.TableName)] = true
		for _, idx := range t.Indexes {
			available[idxRef(idx)] = true
		}
		for _, fk := range t.ForeignKeys {
			available[ObjectRef{"foreign_key", fk.TableName, fk.ConstraintName}] = true
		}
		for _, c := range t.Checks {
			available[pgRef(c)] = true
		}
		for _, o := range t.Unsupported {
			if o.Kind != "table" {
				available[pgRef(o)] = true
			}
		}
	}
	for _, v := range snap.Views {
		available[ObjectRef{Kind: "view", Name: v.ViewName}] = true
	}
	for _, o := range snap.Objects {
		available[pgRef(o)] = true
	}
	excluded := map[ObjectRef]bool{}
	for _, ref := range m.cfg.ExcludedObjects {
		if !available[ref] {
			p.add(ref, "排除项不存在或不允许排除（不能排除列或从属自增属性）", false)
			p.globalError = true
		} else {
			excluded[ref] = true
		}
	}
	chosen := FilterTables(all, m.cfg.Mode, m.cfg.Filter)
	if m.cfg.Mode == "" {
		chosen = all
	}
	if m.cfg.TableNames != nil {
		chosen = m.cfg.TableNames
		for _, t := range chosen {
			if !knownTables[t] {
				p.add(tableRef(t), "选择的源表不存在", false)
			}
		}
	}
	selected := map[string]bool{}
	for _, name := range chosen {
		if !excluded[tableRef(name)] {
			selected[name] = true
		}
	}
	viewOnly := len(m.cfg.ViewNames) > 0
	if viewOnly {
		selected = map[string]bool{}
	}
	if s := m.partitionState; s != nil {
		for name := range selected {
			if s.blocked[name] || s.roots[name] != "" && s.roots[name] != name {
				delete(selected, name)
			}
		}
	}
	if !viewOnly && len(selected) == 0 {
		if s := m.partitionState; s == nil || len(s.blocked) == 0 {
			p.add(ObjectRef{Kind: "selection", Name: "表范围"}, "没有可迁移的表", false)
		} else if p.PartitionPreflight != nil {
			p.PartitionPreflight.CanStart = false
		}
	}
	skippedForeignKeys := map[ObjectRef]bool{}
	for _, table := range snap.Tables {
		if p.PartitionPreflight == nil {
			break
		} // Preserve legacy strict planning.
		if !selected[table.Info.TableName] {
			continue
		}
		for _, fk := range table.ForeignKeys {
			ref := ObjectRef{"foreign_key", fk.TableName, fk.ConstraintName}
			blockedReference := false
			if s := m.partitionState; s != nil {
				blockedReference = s.blocked[fk.RefTable]
			}
			if (fk.RefSchema == snap.Profile.Schema || fk.RefSchema == "") && (blockedReference || excluded[tableRef(fk.RefTable)]) && !excluded[ref] {
				skippedForeignKeys[ref] = true
				p.warn(ref, "外键依赖未迁移的表 "+fk.RefTable+"，本次跳过该外键；引用方表结构及数据继续执行")
			}
		}
	}
	isExisting := m.cfg.Content == "data_only" || m.cfg.objectMode() || viewOnly
	names := map[string]string{}
	nameOwners := map[string]ObjectRef{}
	registerName := func(space, name, origin string) {
		owner := tableRef(origin)
		if strings.HasPrefix(space, "column:") || strings.HasPrefix(space, "index:") {
			owner = tableRef(strings.SplitN(space, ":", 2)[1])
		}
		if space == "constraint" {
			for _, table := range snap.Tables {
				if strings.HasPrefix(origin, table.Info.TableName+".") {
					owner = tableRef(table.Info.TableName)
					break
				}
			}
		}
		if space == "relation" {
			for _, view := range snap.Views {
				if view.ViewName == origin {
					owner = ObjectRef{Kind: "view", Name: origin}
					break
				}
			}
		}
		if err := validateMySQLObjectName(name, space == "relation"); err != nil {
			p.add(owner, fmt.Sprintf("%s: %v", origin, err), false)
			return
		}

		key := name
		if space != "relation" || p.Target.LowerCaseTableNames != 0 {
			key = strings.ToLower(key)
		}
		key = space + ":" + key
		if old, ok := names[key]; ok && old != origin {
			p.add(owner, fmt.Sprintf("目标名称与 %s 冲突", old), false)
			p.add(nameOwners[key], fmt.Sprintf("目标名称与 %s 冲突", origin), false)
		} else {
			names[key] = origin
			nameOwners[key] = owner
		}
	}
	dia := dialect.NewMySQL()
	for _, original := range snap.Tables {
		if !selected[original.Info.TableName] {
			continue
		}
		t := original
		// Keep source metadata intact. DDL, defaults, preflight and copy must all
		// consume the same target precision in this plan's private column slice.
		t.Info.Columns = append([]source.ColumnInfo(nil), original.Info.Columns...)
		if !isExisting {
			m.fitPGMySQLRow(p, &t)
		}
		ref := tableRef(t.Info.TableName)
		registerName("relation", m.objName(t.Info.TableName), t.Info.TableName)
		for _, u := range t.Unsupported {
			if !excluded[pgRef(u)] {
				issueRef := ref
				if u.Kind != "table" {
					issueRef = pgRef(u)
				}
				p.add(issueRef, u.Reason, true)
			}
		}
		t.Indexes = nil
		for _, idx := range original.Indexes {
			if excluded[idxRef(idx)] {
				continue
			}
			if idx.Unsupported != "" && m.wantPGObject(idxRef(idx).Kind) {
				p.add(idxRef(idx), idx.Unsupported, true)
			}
			t.Indexes = append(t.Indexes, idx)
		}
		var auto []source.ColumnInfo
		validColumns := true
		for i, c := range t.Info.Columns {
			registerName("column:"+t.Info.TableName, m.objName(c.Name), t.Info.TableName+"."+c.Name)
			if (strings.EqualFold(c.DataType, "numeric") || strings.EqualFold(c.DataType, "decimal")) && c.Precision > 65 {
				mapped := c
				mapped.Precision = 65
				if _, err := typemap.PGToMySQLChecked(mapped); err == nil {
					p.Issues = append(p.Issues, CompatibilityIssue{
						Object: ref, Severity: "warning",
						Reason:     fmt.Sprintf("列 %s: numeric(%d,%d) 将映射为 DECIMAL(65,%d)，保留小数位；实际数据在复制时逐值校验", c.Name, c.Precision, c.Scale, c.Scale),
						Suggestion: fmt.Sprintf("目标最多容纳 %d 位整数、%d 位小数；默认值超限将阻止预检；实际数据超限会在复制时停止，已提交批次可能保留，后续业务写入也须遵守此范围", 65-c.Scale, c.Scale),
					})
					c = mapped
					t.Info.Columns[i] = mapped
				}
			}
			typ, e := typemap.PGToMySQLChecked(c)
			if e != nil {
				p.add(ref, "列 "+c.Name+": "+e.Error(), true)
				validColumns = false
				continue
			}
			if c.Extra == "auto_increment" {
				auto = append(auto, c)
			}
			if c.DataType == "timestamptz" {
				p.warn(ref, "列 "+c.Name+" 转为 UTC DATETIME，不再按会话时区自动显示")
			}
			if c.DataType == "json" || c.DataType == "jsonb" {
				p.warn(ref, "列 "+c.Name+" 转为 MySQL JSON，空白、键顺序和重复键可能规范化")
			}
			if !isExisting && c.Default != nil && c.Extra != "auto_increment" {
				if _, e = dialect.PGColumnDefault(c, typ, p.Target.Parsed); e != nil {
					p.add(ref, "列 "+c.Name+" 默认值: "+e.Error()+"；原始默认表达式: "+*c.Default, true)
					validColumns = false
				}
			}
			if utf8.RuneCountInString(c.Comment) > 1024 {
				p.add(ref, "列 "+c.Name+" 注释超过 MySQL 长度限制", true)
			}
		}
		if utf8.RuneCountInString(t.Info.Comment) > 2048 {
			p.add(ref, "表注释超过 MySQL 长度限制", true)
		}
		if len(auto) > 0 {
			for _, idx := range t.Indexes {
				if strings.EqualFold(m.objName(idx.IndexName), "__migration_auto") {
					p.add(idxRef(idx), "索引名称与自增支持索引保留名冲突", true)
				}
			}
		}
		if len(auto) > 1 {
			p.add(ref, "每表最多一个 MySQL 自增列", true)
		}
		for _, seq := range t.Sequences {
			var col *source.ColumnInfo
			for i := range t.Info.Columns {
				if t.Info.Columns[i].Name == seq.ColumnName {
					col = &t.Info.Columns[i]
				}
			}
			if col == nil || col.Extra != "auto_increment" {
				p.add(ref, "序列 "+seq.Name+" 通过复杂默认表达式使用，不能转换为 AUTO_INCREMENT", true)
				continue
			}
			typ, _ := typemap.PGToMySQLChecked(*col)
			maxValue := int64(0)
			switch typ {
			case "SMALLINT":
				maxValue = 32767
			case "INT":
				maxValue = 2147483647
			case "BIGINT":
				maxValue = 9223372036854775807
			}
			if maxValue == 0 || seq.Max < maxValue || seq.StartValue > maxValue {
				p.add(ref, "序列 "+seq.Name+" 的范围或下一值超出目标整数自增语义", true)
			}
			if col.Identity == "a" {
				p.warn(ref, "列 "+col.Name+" 的 GENERATED ALWAYS 将映射为允许显式赋值的 AUTO_INCREMENT")
			}
		}
		for _, c := range auto {
			found := false
			for _, seq := range t.Sequences {
				if seq.ColumnName == c.Name {
					found = true
					if seq.Shared || seq.Cycle || seq.Increment != 1 || seq.Min < 1 || seq.StartValue < 1 || seq.StartValue > seq.Max {
						p.add(ref, "序列 "+seq.Name+" 为共享、循环、非正或非单位步长，不能等价迁移", true)
					}
				}
			}
			if !found {
				p.add(ref, "自增列 "+c.Name+" 无完整序列依赖，不能推测种子", true)
			}
		}
		if len(auto) > 0 && p.Target.Parsed.Major == 5 {
			p.warn(ref, "MySQL 5.7 重启可按表内最大值重建自增计数器，不能保持 PG 序列空洞语义")
		}
		secondary := 0
		var indexColumns [][]string
		for _, idx := range t.Indexes {
			indexColumns = append(indexColumns, idx.Columns)
			if !idx.IsPrimary {
				secondary++
			}
		}
		hasPrefix := func(columns []string) bool {
			for _, key := range indexColumns {
				if len(key) >= len(columns) && strings.Join(key[:len(columns)], "\x00") == strings.Join(columns, "\x00") {
					return true
				}
			}
			return false
		}
		if len(auto) > 0 && !hasPrefix([]string{auto[0].Name}) {
			secondary++
			indexColumns = append(indexColumns, []string{auto[0].Name})
		}
		if m.wantPGObject("foreign_key") {
			for _, fk := range t.ForeignKeys {
				if !excluded[ObjectRef{"foreign_key", fk.TableName, fk.ConstraintName}] && !skippedForeignKeys[ObjectRef{"foreign_key", fk.TableName, fk.ConstraintName}] && !hasPrefix(fk.Columns) {
					secondary++
					indexColumns = append(indexColumns, fk.Columns)
				}
			}
		}
		if secondary > 64 {
			p.add(ref, "二级索引数量超过 InnoDB 的 64 个限制", true)
		}
		for _, idx := range t.Indexes {
			if !m.wantPGObject(idxRef(idx).Kind) && m.cfg.objectMode() {
				continue
			}
			registerName("index:"+t.Info.TableName, m.objName(idx.IndexName), t.Info.TableName+"."+idx.IndexName)
		}
		// Preserve selected objects for cached-plan execution; exclusions do not
		// change the immutable source snapshot used for diagnostics.
		t.Checks = nil
		for _, c := range original.Checks {
			if excluded[pgRef(c)] {
				continue
			}
			if !isExisting {
				if !p.Target.Parsed.Checks() {
					p.add(pgRef(c), "CHECK 需要 MySQL 8.0.16+ 才会执行", true)
				} else {
					expr, e := translatePGCheck(c.Definition, t.Info, m.objName)
					if e != nil {
						p.add(pgRef(c), e.Error(), true)
					} else {
						registerName("constraint", m.objName(c.Name), t.Info.TableName+"."+c.Name)
						sql := "ALTER TABLE " + dia.QualifyTable(p.Target.Database, m.objName(t.Info.TableName)) + " ADD CONSTRAINT " + dia.QuoteIdent(m.objName(c.Name)) + " CHECK (" + expr + ")"
						p.Objects = append(p.Objects, PlannedObject{pgRef(c), m.objName(c.Name), "check", []dialect.Statement{{SQL: sql}}})
					}
				}
			}
			t.Checks = append(t.Checks, c)
		}

		if !isExisting && validColumns {
			stmts, inline, e := dia.PGTableStatements(p.Target.Database, t, p.Target.Parsed, m.objName)
			if e == nil {
				stmts, e = m.partitionStatements(t.Info.TableName, stmts)
			}
			if e != nil {
				p.add(ref, e.Error(), true)
			} else {
				p.Objects = append(p.Objects, PlannedObject{ref, m.objName(t.Info.TableName), "table", stmts})
				for _, idx := range t.Indexes {
					skip := false
					for _, n := range inline {
						if n == idx.IndexName {
							skip = true
						}
					}
					if skip {
						continue
					}
					copy := mappedPGIndex(idx, m.objName)
					p.Objects = append(p.Objects, PlannedObject{idxRef(idx), copy.IndexName, "index", dia.IndexStatements(p.Target.Database, copy)})
				}
			}
		} else if m.cfg.objectMode() {
			for _, idx := range t.Indexes {
				if m.wantPGObject(idxRef(idx).Kind) {
					copy := mappedPGIndex(idx, m.objName)
					p.Objects = append(p.Objects, PlannedObject{idxRef(idx), copy.IndexName, "index", dia.IndexStatements(p.Target.Database, copy)})
				}
			}
		}
		if !isExisting || m.cfg.Content == "data_only" || m.cfg.objectMode() && m.wantPGObject("sequence") {
			for _, seq := range t.Sequences {
				copy := seq.SequenceInfo
				copy.TableName = m.objName(copy.TableName)
				copy.ColumnName = m.objName(copy.ColumnName)
				p.Objects = append(p.Objects, PlannedObject{ObjectRef{"sequence", seq.TableName, seq.Name}, copy.TableName, "sequence", dia.SequenceStatements(p.Target.Database, copy, "")})
			}
		}
		p.tables = append(p.tables, t)
	}
	for _, t := range p.tables {
		if !m.wantPGObject("foreign_key") {
			continue
		}
		for _, fk := range t.ForeignKeys {
			ref := ObjectRef{"foreign_key", fk.TableName, fk.ConstraintName}
			if excluded[ref] || skippedForeignKeys[ref] {
				continue
			}
			if p.objectBlocked(tableRef(fk.TableName)) || p.objectBlocked(tableRef(fk.RefTable)) {
				p.dependencies[ref] = append(p.dependencies[ref], tableRef(fk.TableName), tableRef(fk.RefTable))
				if m.cfg.Content != "data_only" {
					copy := fk
					copy.TableName = m.objName(fk.TableName)
					copy.RefTable = m.objName(fk.RefTable)
					copy.ConstraintName = m.objName(fk.ConstraintName)
					copy.Columns = mapNames(fk.Columns, m.objName)
					copy.RefColumns = mapNames(fk.RefColumns, m.objName)
					p.Objects = append(p.Objects, PlannedObject{ref, copy.ConstraintName, "foreign_key", dia.ForeignKeyStatements(p.Target.Database, copy)})
				}
				continue
			}
			if fk.Unsupported != "" || fk.RefSchema != snap.Profile.Schema || fk.OnDelete == "SET DEFAULT" || fk.OnUpdate == "SET DEFAULT" {
				p.add(ref, "外键含不支持的动作、延迟语义或跨 schema 引用", true)
				continue
			}
			if !selected[fk.RefTable] && !m.cfg.objectMode() {
				p.add(ref, "引用表未纳入迁移范围，请纳入引用表或排除此外键", true)
				continue
			}
			registerName("constraint", m.objName(fk.ConstraintName), fk.TableName+"."+fk.ConstraintName)
			referenced, ok := tablesByName[fk.RefTable]
			for _, candidate := range p.tables {
				if candidate.Info.TableName == fk.RefTable {
					referenced = candidate
				}
			}
			if !ok {
				p.add(ref, "引用表元数据缺失", true)
				continue
			}
			if e := checkPGForeignKey(t, referenced, fk); e != nil {
				p.add(ref, e.Error(), true)
				continue
			}
			copy := fk
			copy.TableName = m.objName(fk.TableName)
			copy.RefTable = m.objName(fk.RefTable)
			copy.ConstraintName = m.objName(fk.ConstraintName)
			copy.Columns = mapNames(fk.Columns, m.objName)
			copy.RefColumns = mapNames(fk.RefColumns, m.objName)
			p.validationFKs = append(p.validationFKs, fk)
			if m.cfg.Content != "data_only" {
				p.Objects = append(p.Objects, PlannedObject{ref, copy.ConstraintName, "foreign_key", dia.ForeignKeyStatements(p.Target.Database, copy)})
			}
		}
	}
	if !m.cfg.objectMode() && m.cfg.Content != "data_only" && !viewOnly {
		for _, o := range snap.Objects {
			if o.Table != "" && !selected[o.Table] {
				continue
			}
			if !excluded[pgRef(o)] {
				p.add(pgRef(o), o.Reason, true)
			}
		}
	}
	if m.wantPGObject("view") && m.cfg.Content != "data_only" {
		m.planPGViews(p, selected, excluded, registerName)
	}
	if err = m.inspectPGMySQLTarget(ctx, p, w.InspectionDB(), isExisting); err != nil {
		p.add(ObjectRef{Kind: "target", Name: p.Target.Database}, err.Error(), false)
	}
	m.checkPGMySQLTextStructure(p)
	if m.cfg.Content == "data_only" {
		orderPGDataTables(p)
	}
	p.finish()
	if p.CanStart {
		if e := m.checkPGMySQLPrivileges(ctx, p, w.InspectionDB()); e != nil {
			p.add(ObjectRef{Kind: "permission", Name: p.Target.Database}, e.Error(), false)
			p.finish()
		}
	}
	if p.CanStart && !isExisting {
		for {
			err := validatePGTextDefaults(ctx, p, w.InspectionDB())
			if err == nil {
				break
			}
			var objectErr *pgObjectError
			if !m.stopMigrationTask(ctx, err) && errors.As(err, &objectErr) {
				p.add(objectErr.Object, err.Error(), true)
			} else {
				p.add(ObjectRef{Kind: "target", Name: p.Target.Database}, err.Error(), false)
				break
			}
			p.finish()
			if !p.CanStart {
				break
			}
		}
	}
	p.linkTableDependencies()
	p.protectTargetDependencies(m)

	// Stable execution order; dependency order among views is already topological.
	order := map[string]int{"drop_dependency": 0, "table": 1, "index": 2, "sequence": 3, "check": 4, "foreign_key": 5, "comment": 6, "view": 7}
	sort.SliceStable(p.Objects, func(i, j int) bool { return order[p.Objects[i].Stage] < order[p.Objects[j].Stage] })
	p.warn(ObjectRef{Kind: "migration", Name: "迁移前提"}, fmt.Sprintf("停写及停止结构变更期间执行；忽略源排序规则，采用目标配置。新表继承目标数据库 %s / %s，已有表使用各列实际配置", p.Target.TargetCharset, p.Target.TargetCollation))
	p.finish()
	return p
}

func mapNames(in []string, name func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = name(s)
	}
	return out
}

func (m *Migrator) pgMySQLSnapshotTables() []string {
	s := m.partitionState
	if s == nil {
		return nil
	}
	chosen := FilterTables(s.all, m.cfg.Mode, m.cfg.Filter)
	if m.cfg.Mode == "" {
		chosen = append([]string(nil), s.all...)
	}
	if m.cfg.TableNames != nil {
		chosen = append([]string(nil), m.cfg.TableNames...)
	}
	excluded := map[string]bool{}
	for _, ref := range m.cfg.ExcludedObjects {
		if ref.Kind == "table" {
			excluded[ref.Name] = true
		}
	}
	known := map[string]bool{}
	for _, name := range s.all {
		known[name] = true
	}
	seen := map[string]bool{}
	result := []string{}
	for _, name := range chosen {
		if known[name] && !excluded[name] && !s.blocked[name] && !seen[name] {
			result = append(result, name)
			seen[name] = true
		}
	}
	return result
}
func mappedPGIndex(i source.IndexInfo, name func(string) string) source.IndexInfo {
	i.TableName = name(i.TableName)
	i.IndexName = name(i.IndexName)
	i.Columns = mapNames(i.Columns, name)
	return i
}
func checkPGIndex(t source.PGTable, idx source.IndexInfo, p target.MySQLProfile, rules map[string]target.MySQLTextRules) error {
	if len(idx.Columns) == 0 || len(idx.Columns) > 16 {
		return fmt.Errorf("索引列数量超出 MySQL 范围")
	}
	limit := 3072
	if p.PageSize < 16384 {
		limit = p.PageSize * 3 / 16
	}
	if !p.LargePrefix {
		limit = 767
	}
	bytes := int64(0)
	for _, name := range idx.Columns {
		found := false
		for _, c := range t.Info.Columns {
			if c.Name != name {
				continue
			}
			found = true
			typ, e := typemap.PGToMySQLChecked(c)
			if e != nil {
				return e
			}
			switch {
			case strings.Contains(typ, "TEXT"), strings.Contains(typ, "BLOB"), typ == "JSON":
				return fmt.Errorf("%s 索引不能无损自动加前缀", typ)
			case strings.Contains(typ, "CHAR"):
				if c.DataType == "uuid" {
					bytes += 36 * int64(rules[c.Name].MaxBytes)
				} else {
					bytes += max(c.Length, 1) * int64(rules[c.Name].MaxBytes)
				}
			default:
				bytes += 32
			}
			if idx.IsPrimary && c.IsNullable {
				return fmt.Errorf("主键包含可空列")
			}
		}
		if !found {
			return fmt.Errorf("索引引用未知列 %s", name)
		}
	}
	if bytes > int64(limit) {
		return fmt.Errorf("索引预计 %d 字节，超过目标 %d 字节上限", bytes, limit)
	}
	return nil
}
func checkPGForeignKey(t, ref source.PGTable, fk source.FKInfo) error {
	if len(fk.Columns) != len(fk.RefColumns) || len(fk.Columns) == 0 {
		return fmt.Errorf("外键列对应关系无效")
	}
	for i, n := range fk.Columns {
		var a, b *source.ColumnInfo
		for j := range t.Info.Columns {
			if t.Info.Columns[j].Name == n {
				a = &t.Info.Columns[j]
			}
		}
		for j := range ref.Info.Columns {
			if ref.Info.Columns[j].Name == fk.RefColumns[i] {
				b = &ref.Info.Columns[j]
			}
		}
		if a == nil || b == nil {
			return fmt.Errorf("外键列不存在")
		}
		ta, e := typemap.PGToMySQLChecked(*a)
		if e != nil {
			return e
		}
		tb, e := typemap.PGToMySQLChecked(*b)
		if e != nil {
			return e
		}
		if ta != tb {
			return fmt.Errorf("外键列目标类型不匹配")
		}
		if (fk.OnDelete == "SET NULL" || fk.OnUpdate == "SET NULL") && !a.IsNullable {
			return fmt.Errorf("SET NULL 作用于 NOT NULL 列")
		}
	}
	for _, idx := range ref.Indexes {
		if idx.Unsupported == "" && idx.IsUnique && strings.Join(idx.Columns, "\x00") == strings.Join(fk.RefColumns, "\x00") {
			return nil
		}
	}
	return fmt.Errorf("引用列缺少完整有效唯一键")
}
func translatePGCheck(raw string, t source.TableDDLInfo, name func(string) string) (string, error) {
	ts, e := sqlsyntax.Lex(raw)
	if e != nil {
		return "", e
	}
	if len(ts) < 3 || !ts[0].Is("CHECK") {
		return "", fmt.Errorf("不支持的 CHECK 定义")
	}
	p := sqlsyntax.Expr{Tokens: ts[1:], Resolve: func(parts []string) (string, error) {
		if len(parts) != 1 {
			return "", fmt.Errorf("CHECK 不支持跨对象引用")
		}
		for _, c := range t.Columns {
			if c.Name == parts[0] {
				return sqlsyntax.QuoteIdent(name(c.Name)), nil
			}
		}
		return "", fmt.Errorf("CHECK 引用未知列")
	}}
	return p.Parse()
}

// Existing MySQL FKs are immediate; data-only copies parent tables first.
// Cycles/self references require a separate row ordering strategy and block.
func orderPGDataTables(p *PGMySQLPreflight) {
	byName := map[string]source.PGTable{}
	for _, t := range p.tables {
		byName[t.Info.TableName] = t
	}
	state := map[string]int{}
	var out []source.PGTable
	var visit func(string) error
	visit = func(n string) error {
		if state[n] == 2 {
			return nil
		}
		if state[n] == 1 {
			return fmt.Errorf("data_only 外键存在循环/自引用，无法保证逐页插入顺序")
		}
		state[n] = 1
		for _, fk := range p.validationFKs {
			if fk.TableName == n {
				if _, ok := byName[fk.RefTable]; !ok {
					return fmt.Errorf("引用表 %s 未纳入预检", fk.RefTable)
				}
				if e := visit(fk.RefTable); e != nil {
					return e
				}
			}
		}
		state[n] = 2
		out = append(out, byName[n])
		return nil
	}
	for _, t := range p.tables {
		if e := visit(t.Info.TableName); e != nil {
			p.add(tableRef(t.Info.TableName), e.Error(), false)
			continue
		}
	}
	for _, t := range p.tables {
		if p.objectBlocked(tableRef(t.Info.TableName)) {
			out = append(out, t)
		}
	}
	p.tables = out
}
