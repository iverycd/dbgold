package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/typemap"
)

type PGOraclePreflight struct {
	Kind                           string                 `json:"kind"`
	Ready                          bool                   `json:"ready"`
	CanStart                       bool                   `json:"can_start"`
	Source                         source.PGProfile       `json:"source"`
	Target                         target.PGOracleProfile `json:"target"`
	Issues                         []CompatibilityIssue   `json:"issues"`
	Excluded                       []ObjectRef            `json:"excluded"`
	Objects                        []PlannedObject        `json:"objects"`
	RowsChecked                    int64                  `json:"rows_checked"`
	CheckScope                     string                 `json:"check_scope"`
	PartitionPreflight             *PartitionPreflight    `json:"partition_preflight,omitempty"`
	tables                         []source.PGTable
	blocked                        map[ObjectRef]error
	createdTables                  map[string]bool
	dependencies                   map[ObjectRef][]ObjectRef
	sourceDDL                      map[ObjectRef]string
	catalogViews                   map[string]bool
	sequences                      map[ObjectRef]*pgoracle.SequencePlan
	indexes                        map[ObjectRef]source.IndexInfo
	foreignKeys                    map[ObjectRef]source.FKInfo
	names                          map[string]ObjectRef
	inlineConstraints              map[string][]string
	globalError                    bool
	dataOnly, objectOnly, viewOnly bool
}

type pgOracleReader interface {
	ReadPGOracleSnapshot(context.Context, []string) (*source.PGSnapshot, error)
}
type pgOracleInspector interface {
	oracleInspector
	InspectPGOracle(context.Context) (target.PGOracleProfile, error)
	ValidatePGOracleDataTable(context.Context, string, []source.ColumnInfo, map[string]string) error
	InspectPGOracleSequence(context.Context, pgoracle.SequencePlan) error
	PGOracleRelationExists(context.Context, string) (bool, error)
}

func (m *Migrator) isPGOracle() bool {
	return source.IsPGFamily(m.reader.DBType()) && m.writer.DBType() == "oracle"
}

func (p *PGOraclePreflight) add(ref ObjectRef, err error) {
	if err == nil || p.blocked[ref] != nil {
		return
	}
	p.blocked[ref] = err
	p.Issues = append(p.Issues, CompatibilityIssue{Object: ref, Severity: "error", Reason: err.Error(), Definition: p.sourceDDL[ref], Excludable: ref.Kind != "metadata" && ref.Kind != "target", Suggestion: "受影响对象及依赖对象跳过，其他对象继续；请修复后重新预检"})
	if ref.Kind == "metadata" || ref.Kind == "target" {
		p.globalError = true
	}
}
func (p *PGOraclePreflight) warn(ref ObjectRef, reason string) {
	p.Issues = append(p.Issues, CompatibilityIssue{Object: ref, Severity: "warning", Reason: reason, Suggestion: "迁移期间源库及目标迁移范围停写；按已确认映射适配应用"})
}
func (p *PGOraclePreflight) excluded(ref ObjectRef) bool {
	for _, e := range p.Excluded {
		if e == ref || e.Kind == ref.Kind && e.Name == ref.Name && (e.Table == "" || e.Table == ref.Table) || e.Kind == "table" && ref.Table == e.Name {
			return true
		}
	}
	return false
}
func (p *PGOraclePreflight) dependencyError(ref ObjectRef, seen map[ObjectRef]bool) error {
	if e := p.blocked[ref]; e != nil {
		return e
	}
	if seen[ref] {
		return nil
	}
	seen[ref] = true
	if p.excluded(ref) {
		return fmt.Errorf("依赖对象 %s 已排除", ref.Name)
	}
	// A data error may block the owner's table ref after its CREATE succeeded.
	owner := tableRef(ref.Table)
	commentOnCreatedTable := ref.Kind == "comment" && p.createdTables[ref.Table]
	if ref.Table != "" && !commentOnCreatedTable {
		if e := p.blocked[owner]; e != nil {
			return e
		}
	}
	for _, dep := range p.dependencies[ref] {
		if commentOnCreatedTable && dep == owner {
			continue
		}
		if e := p.dependencyError(dep, seen); e != nil {
			return fmt.Errorf("依赖 %s: %w", dep.Name, e)
		}
	}
	return nil
}
func (p *PGOraclePreflight) finish() {
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
	if !p.objectOnly && !p.viewOnly {
		for _, t := range p.tables {
			if p.dependencyError(tableRef(t.Info.TableName), map[ObjectRef]bool{}) == nil {
				p.CanStart = true
			}
		}
	}
	for _, o := range p.Objects {
		if len(o.Statements) > 0 && p.dependencyError(o.Object, map[ObjectRef]bool{}) == nil {
			p.CanStart = true
		}
	}
	if pp := p.PartitionPreflight; pp != nil && !pp.Ready {
		p.Ready = false
	}
}
func (p *PGOraclePreflight) register(ref ObjectRef, name, namespace string) {
	if e := dialect.ValidatePGOracleName(name); e != nil {
		p.add(ref, e)
		return
	}
	key := namespace + "\x00" + name
	if old, ok := p.names[key]; ok && old != ref {
		err := fmt.Errorf("Oracle 大写名称冲突: %s 与 %s → %s", old.Name, ref.Name, name)
		p.add(old, err)
		p.add(ref, err)
		return
	}
	p.names[key] = ref
}
func (m *Migrator) wantPGOracle(p *PGOraclePreflight, kind string) bool {
	if p.viewOnly {
		return kind == "view"
	}
	if p.dataOnly {
		return false
	}
	if !p.objectOnly {
		return kind != "trigger"
	}
	key := map[string]string{"primary_key": "primary_keys", "index": "indexes", "foreign_key": "foreign_keys", "check": "check_constraints", "sequence": "sequences", "comment": "comments"}[kind]
	return key != "" && slices.Contains(m.cfg.Objects, key)
}

// PreflightPGOracle performs only metadata reads. Runtime rebuilds this plan;
// no client-provided SQL or old preview can authorize target changes.
func (m *Migrator) PreflightPGOracle(ctx context.Context) *PGOraclePreflight {
	p := &PGOraclePreflight{Kind: "pg_oracle", CheckScope: "structure", Issues: []CompatibilityIssue{}, Excluded: append([]ObjectRef{}, m.cfg.ExcludedObjects...), Objects: []PlannedObject{}, blocked: map[ObjectRef]error{}, dependencies: map[ObjectRef][]ObjectRef{}, sourceDDL: map[ObjectRef]string{}, sequences: map[ObjectRef]*pgoracle.SequencePlan{}, indexes: map[ObjectRef]source.IndexInfo{}, foreignKeys: map[ObjectRef]source.FKInfo{}, names: map[string]ObjectRef{}, dataOnly: m.cfg.Content == "data_only", objectOnly: m.cfg.objectMode(), viewOnly: len(m.cfg.ViewNames) > 0}
	defer p.finish()
	p.inlineConstraints = map[string][]string{}
	fatal := func(err error) *PGOraclePreflight {
		p.add(ObjectRef{Kind: "metadata", Name: "PG → Oracle 预检"}, err)
		m.stopMigrationTask(ctx, err)
		return p
	}
	if !m.isPGOracle() {
		return fatal(fmt.Errorf("此预检仅支持 PG 内核源库 → Oracle"))
	}
	r, ok := m.reader.(pgOracleReader)
	if !ok {
		return fatal(fmt.Errorf("源 Reader 缺少 PG Oracle 原始快照能力"))
	}
	w, ok := m.writer.(pgOracleInspector)
	if !ok {
		return fatal(fmt.Errorf("Oracle Writer 缺少 PG 迁移预检能力"))
	}
	d, ok := m.writer.Dialect().(*dialect.OracleDialect)
	if !ok {
		return fatal(fmt.Errorf("Oracle 方言未配置"))
	}
	var err error
	p.Target, err = w.InspectPGOracle(ctx)
	if err != nil {
		return fatal(err)
	}
	m.cfg.TargetSchema = p.Target.Schema
	if p.viewOnly {
		p.PartitionPreflight = &PartitionPreflight{Ready: true, CanStart: true, SourceType: m.reader.DBType(), TargetType: "oracle", Tables: []*PartitionTableResult{}, Issues: []CompatibilityIssue{}}
	} else {
		p.PartitionPreflight = m.preparePartitions(ctx)
	}
	if p.PartitionPreflight.FatalError != "" {
		return fatal(fmt.Errorf("%s", p.PartitionPreflight.FatalError))
	}
	all, err := m.migrationTables(ctx)
	if err != nil {
		return fatal(err)
	}
	chosen := FilterTables(slices.Clone(all), m.cfg.Mode, m.cfg.Filter)
	if m.cfg.Mode == "" {
		chosen = slices.Clone(all)
	}
	if p.objectOnly && len(m.cfg.TableNames) > 0 {
		chosen = slices.Clone(m.cfg.TableNames)
	}
	sort.Strings(chosen)
	queryTables := chosen
	if p.viewOnly {
		queryTables = nil
	}
	snap, err := r.ReadPGOracleSnapshot(ctx, queryTables)
	if err != nil {
		return fatal(err)
	}
	if snap == nil {
		return fatal(fmt.Errorf("源快照为空"))
	}
	p.Source = snap.Profile
	p.Source.Verification = "未完成实库验证（能力探测不等于实库认证）"
	relations := map[string][]source.ColumnInfo{}
	for _, t := range snap.Tables {
		relations[t.Info.TableName] = t.Info.Columns
	}
	for _, v := range snap.Views {
		relations[v.ViewName] = v.Columns
	}
	for _, t := range snap.Tables {
		if p.viewOnly || !slices.Contains(chosen, t.Info.TableName) || t.PartitionChild || p.excluded(tableRef(t.Info.TableName)) {
			continue
		}
		ref := tableRef(t.Info.TableName)
		name := m.objName(ref.Name)
		if s := m.partitionState; s != nil {
			if part := s.tables[ref.Name]; part != nil {
				t.Info.Partition = source.ClonePartitionSpec(part.Source)
			}
		}
		p.tables = append(p.tables, t)
		if metadata, err := json.MarshalIndent(t.Info, "", "  "); err == nil {
			p.sourceDDL[ref] = "-- 源表原始元数据（诊断信息，非目标可执行 SQL）\n" + string(metadata)
		}
		p.register(ref, name, "RELATION")
		if len(t.Info.Columns) == 0 {
			p.add(ref, fmt.Errorf("表 %s 缺少列元数据", ref.Name))
		}
		if t.Inherits && t.Info.Partition == nil {
			p.add(ref, fmt.Errorf("传统继承表不自动转换"))
		}
		if m.partitionState != nil && m.partitionState.blocked[ref.Name] {
			p.add(ref, fmt.Errorf("分区定义不兼容"))
		}
		seen := map[string]bool{}
		for _, c := range t.Info.Columns {
			cn := m.objName(c.Name)
			if e := dialect.ValidatePGOracleName(cn); e != nil {
				p.add(ref, e)
			}
			if seen[cn] {
				p.add(ref, fmt.Errorf("列名转换为大写后冲突: %s", cn))
			}
			seen[cn] = true
			if !p.objectOnly {
				if _, e := typemap.PGToOracleChecked(c, m.cfg.UseNvarchar2, d.CharBytes, d.NCharBytes); e != nil {
					p.add(ref, fmt.Errorf("列 %s: %w", c.Name, e))
				}
				if !p.dataOnly && m.cfg.UseNvarchar2 && typemap.PGOracleBaseType(c.DataType) == "varchar" && c.Length > 0 {
					check := dialect.PGOracleLengthCheckName(name, cn)
					p.inlineConstraints[name] = append(p.inlineConstraints[name], check)
					p.register(ref, check, "CONSTRAINT")
				}
			}
			if !p.objectOnly {
				for _, warning := range dialect.PGOracleDefaultWarnings(c) {
					p.warn(ref, c.Name+": "+warning)
				}
				switch strings.ToLower(c.DataType) {
				case "json", "jsonb", "xml", "uuid", "inet", "cidr", "macaddr", "macaddr8", "bit", "varbit":
					p.warn(ref, c.Name+": 保存为文本表示，原类型运算需要应用适配")
				}
			}
		}
		for _, o := range t.Unsupported {
			if o.Kind == "trigger" || o.Kind == "routine" || o.Kind == "function" || o.Kind == "procedure" {
				p.warn(ObjectRef{Kind: o.Kind, Table: ref.Name, Name: o.Name}, "仅原样导出，需人工适配")
				continue
			}
			if o.Kind == "table" || o.Kind == "metadata" {
				p.add(ref, fmt.Errorf("%s", o.Reason))
			}
		}
		if !p.objectOnly && !p.dataOnly {
			ss, e := d.CreateTableStatements(p.Target.Schema, &t.Info, "postgres", dialect.TypeOpt{CharInLength: true, UseNvarchar2: m.cfg.UseNvarchar2}, m.objName)
			if e == nil {
				ss, e = m.partitionStatements(ref.Name, ss)
			}
			p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: name, Stage: "table", Statements: ss})
			if e != nil {
				p.add(ref, e)
			}
		}
	}
	for _, name := range chosen {
		if p.excluded(tableRef(name)) || p.viewOnly {
			continue
		}
		if _, ok := relations[name]; !ok {
			p.tables = append(p.tables, source.PGTable{Info: source.TableDDLInfo{TableName: name}})
			p.add(tableRef(name), fmt.Errorf("表元数据缺失或读取失败"))
			if !p.objectOnly && !p.dataOnly {
				p.Objects = append(p.Objects, PlannedObject{Object: tableRef(name), Target: m.objName(name), Stage: "table", Statements: []dialect.Statement{}})
			}
		}
	}
	for _, o := range snap.Objects {
		if o.Kind == "table" && slices.Contains(chosen, o.Name) {
			p.add(tableRef(o.Name), fmt.Errorf("%s", o.Reason))
		}
		if o.Kind == "trigger" || o.Kind == "routine" || o.Kind == "function" || o.Kind == "procedure" {
			p.warn(ObjectRef{Kind: o.Kind, Table: o.Table, Name: o.Name}, "业务代码仅原样导出，不自动转换")
		}
		if o.Reason != "" && m.wantPGOracle(p, o.Kind) && slices.Contains([]string{"index", "primary_key", "foreign_key", "check", "sequence", "view", "comment"}, o.Kind) && (o.Table == "" || slices.Contains(chosen, o.Table)) {
			ref := ObjectRef{Kind: o.Kind, Table: o.Table, Name: o.Name}
			if !p.excluded(ref) {
				p.sourceDDL[ref] = o.Definition
				p.add(ref, fmt.Errorf("%s", o.Reason))
				// Named objects are also planned below. Only category-level
				// metadata diagnostics need a synthetic report/repair entry.
				if o.Table == "" && o.Kind != "sequence" {
					p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: m.objName(o.Name), Stage: o.Kind, Statements: []dialect.Statement{}})
				}
			}
		}
	}
	// Create post-table plans even for blocked tables, so dependency failures and
	// the original definitions remain reviewable in reports and repair records.
	for _, t := range p.tables {
		m.planPGOracleTableObjects(p, t, d)
	}
	m.planPGOracleSequences(p, snap.Sequences)
	if m.wantPGOracle(p, "sequence") {
		for _, table := range p.tables {
			for _, column := range table.Info.Columns {
				auto := column.Identity != "" && column.Identity != "0" || column.Default != nil && dialect.PGOracleDirectSequenceDefault(*column.Default)
				if !auto {
					continue
				}
				found := false
				for _, seq := range snap.Sequences {
					for _, binding := range seq.Bindings {
						found = found || binding.Schema == p.Source.Schema && binding.TableName == table.Info.TableName && binding.ColumnName == column.Name
					}
				}
				if !found {
					ref := ObjectRef{Kind: "sequence", Table: table.Info.TableName, Name: table.Info.TableName + "." + column.Name}
					if !p.excluded(ref) {
						p.add(ref, fmt.Errorf("自动编号列缺少可核验的命名序列及绑定元数据，不能安装自动编号触发器"))
					}
				}
			}
		}
	}
	m.bindPGOracleSequenceEvidence(ctx, p)
	m.planPGOracleForeignKeys(p, d)
	if m.wantPGOracle(p, "view") {
		m.planPGOracleViews(p, snap.Views, relations)
	}
	for _, issue := range p.Issues {
		ref := issue.Object
		if issue.Severity != "error" || !m.wantPGOracle(p, ref.Kind) || ref.Kind == "table" {
			continue
		}
		if !slices.ContainsFunc(p.Objects, func(o PlannedObject) bool { return o.Object == ref }) {
			p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: m.objName(ref.Name), Stage: ref.Kind, Statements: []dialect.Statement{}})
		}
	}
	if err = m.inspectPGOraclePlan(ctx, p, w); err != nil {
		return fatal(err)
	}
	return p
}

func (m *Migrator) inspectPGOraclePlan(ctx context.Context, p *PGOraclePreflight, w pgOracleInspector) error {
	// SQL encoding/name failures can retain an old target table. Resolve them
	// before calculating which target tables may safely be dropped together.
	for _, o := range p.Objects {
		if checker, ok := m.writer.(interface {
			ValidatePGOracleStatements(context.Context, []dialect.Statement) error
		}); ok && p.dependencyError(o.Object, map[ObjectRef]bool{}) == nil {
			if err := checker.ValidatePGOracleStatements(ctx, o.Statements); err != nil {
				if m.stopMigrationTask(ctx, err) {
					return err
				}
				p.add(o.Object, err)
			}
		}
	}
	active := map[string]*source.TableDDLInfo{}
	for i := range p.tables {
		t := &p.tables[i]
		if p.blocked[tableRef(t.Info.TableName)] == nil {
			info := t.Info
			info.Columns = slices.Clone(t.Info.Columns)
			for j := range info.Columns {
				// Legacy preflight validates source SQL encoding. PG defaults are
				// parsed and emitted (including UNISTR) before checking target SQL.
				info.Columns[j].Default = nil
			}
			active[m.objName(t.Info.TableName)] = &info
		}
	}
	// Recheck the complete retained-table closure whenever a target conflict
	// removes a table, before allowing any DROP statements.
	for {
		objs := []target.OracleObject{}
		for name := range active {
			if !p.objectOnly && !p.dataOnly {
				objs = append(objs, target.OracleObject{Kind: "TABLE", Name: name, Table: name})
				for _, check := range p.inlineConstraints[name] {
					objs = append(objs, target.OracleObject{Kind: "CONSTRAINT", Name: check, Table: name})
				}
			}
		}
		sort.Slice(objs, func(i, j int) bool { return objs[i].Name < objs[j].Name })
		err := w.PreflightOracle(ctx, active, objs, nil, !p.objectOnly && !p.dataOnly, p.dataOnly)
		if err == nil {
			break
		}
		if m.stopMigrationTask(ctx, err) {
			return err
		}
		var local *target.OracleTablePreflightError
		if !errors.As(err, &local) {
			for _, t := range p.tables {
				p.add(tableRef(t.Info.TableName), err)
			}
			break
		}
		if _, exists := active[local.Table]; !exists {
			return err
		}
		delete(active, local.Table)
		for _, t := range p.tables {
			if m.objName(t.Info.TableName) == local.Table {
				p.add(tableRef(t.Info.TableName), err)
			}
		}
	}
	for _, t := range p.tables {
		ref := tableRef(t.Info.TableName)
		if p.blocked[ref] != nil {
			continue
		}
		if p.dataOnly || p.objectOnly {
			// Object-only may inspect columns but never executes existing triggers.
			if p.dataOnly {
				allowed := map[string]string{}
				for _, plan := range p.sequences {
					if plan.Identity == nil {
						continue
					}
					for _, binding := range plan.Bindings {
						if binding.SourceTable == ref.Name {
							allowed[binding.Trigger] = plan.TriggerSQL(binding)
						}
					}
				}
				if e := w.ValidatePGOracleDataTable(ctx, m.objName(ref.Name), t.Info.Columns, allowed); e != nil {
					if m.stopMigrationTask(ctx, e) {
						return e
					}
					p.add(ref, e)
				}
			} else {
				exists, e := w.PGOracleRelationExists(ctx, m.objName(ref.Name))
				if e != nil {
					return e
				}
				if !exists {
					p.add(ref, fmt.Errorf("目标表 %s 不存在", m.objName(ref.Name)))
				}
			}
		}
	}
	for _, o := range p.Objects {
		if o.Object.Kind == "table" || p.dependencyError(o.Object, map[ObjectRef]bool{}) != nil {
			continue
		}
		kind := map[string]string{"index": "INDEX", "primary_key": "CONSTRAINT", "check": "CONSTRAINT", "foreign_key": "CONSTRAINT", "view": "VIEW", "sequence": "SEQUENCE", "trigger": "TRIGGER"}[o.Object.Kind]
		if idx, ok := p.indexes[o.Object]; ok && idx.PG != nil && idx.PG.ConstraintType == "u" {
			kind = "CONSTRAINT"
		}
		if kind != "" {
			err := w.PreflightOracle(ctx, nil, []target.OracleObject{{Kind: kind, Name: o.Target, Table: m.objName(o.Object.Table)}}, nil, !p.objectOnly && !p.dataOnly, false)
			if err != nil {
				if m.stopMigrationTask(ctx, err) {
					return err
				}
				p.add(o.Object, err)
				continue
			}
		}
		if s := p.sequences[o.Object]; s != nil {
			if e := w.InspectPGOracleSequence(ctx, *s); e != nil {
				if m.stopMigrationTask(ctx, e) {
					return e
				}
				p.add(o.Object, e)
			}
		}
		if o.Object.Kind == "view" {
			// CREATE VIEW resolves external references in the actual target session.
			continue
		}
		for _, dep := range p.dependencies[o.Object] {
			if dep.Kind != "table" && dep.Kind != "view" {
				continue
			}
			planned := false
			for _, candidate := range p.Objects {
				if candidate.Object == dep {
					planned = true
					break
				}
			}
			if !planned && p.blocked[dep] == nil {
				exists, e := w.PGOracleRelationExists(ctx, m.objName(dep.Name))
				if e != nil {
					return e
				}
				if !exists {
					p.add(o.Object, fmt.Errorf("目标依赖 %s 不存在", dep.Name))
				}
			}
		}
	}
	return nil
}
