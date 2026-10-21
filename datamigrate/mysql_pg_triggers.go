package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

type preparedMySQLPGTrigger struct {
	source source.TriggerInfo
	plan   mysqltrigger.Plan
	err    error
	stage  string
}

func (m *Migrator) mysqlPGTriggerRequested() bool {
	if len(m.cfg.ViewNames) > 0 || m.cfg.Content == "data_only" {
		return false
	}
	if m.cfg.objectMode() {
		return slices.Contains(m.cfg.Objects, "triggers")
	}
	return m.cfg.MigrateTriggers
}

func (m *Migrator) mysqlPGTargetTables(tables []string) []string {
	out := make([]string, len(tables))
	for i, table := range tables {
		out[i] = m.objName(table)
	}
	return out
}

// Preparation is read-only and precedes compatibility initialization and table DDL.
// The frozen plan is shared by execution, reports, and later read-only retries.
func (m *Migrator) prepareMySQLPGTriggers(ctx context.Context) {
	if !m.mysqlPGTriggerRequested() {
		return
	}
	if m.reader.DBType() != "mysql" || !source.IsPGFamily(m.writer.DBType()) {
		m.mysqlPGTriggerError = fmt.Errorf("触发器迁移仅支持 MySQL → PG 内核目标")
		return
	}
	r, ok := m.reader.(source.TriggerReader)
	if !ok {
		m.mysqlPGTriggerError = fmt.Errorf("源库未实现触发器元数据读取")
		return
	}
	if m.cfg.TargetSchema == "" {
		var err error
		m.cfg.TargetSchema, err = target.ResolveRepairSchema(ctx, m.writer)
		if err != nil {
			m.mysqlPGTriggerError = fmt.Errorf("确定触发器目标 schema: %w", err)
			return
		}
	}
	all, err := m.reader.ListTables(ctx)
	if err != nil {
		m.mysqlPGTriggerError = err
		return
	}
	selected := FilterTables(all, m.cfg.Mode, m.cfg.Filter)
	if m.cfg.objectMode() && len(m.cfg.TableNames) > 0 {
		selected = nil
		for _, table := range m.cfg.TableNames {
			if slices.Contains(all, table) && !slices.Contains(selected, table) {
				selected = append(selected, table)
			}
		}
	}
	values, err := r.GetTriggers(ctx)
	if err != nil {
		m.mysqlPGTriggerError = err
		return
	}
	var characters *source.TriggerCharacterMetadata
	var characterErr error
	if reader, ok := m.reader.(source.TriggerCharacterReader); ok && len(values) > 0 {
		characters, characterErr = reader.GetTriggerCharacterMetadata(ctx)
		if characterErr != nil {
			characters = nil
			if ctx.Err() != nil {
				m.mysqlPGTriggerError = ctx.Err()
				return
			}
		}
	}
	groups, names := map[string]int{}, map[string]int{}
	for _, v := range values {
		groups[v.TableName+"\x00"+strings.ToUpper(v.Timing)+"\x00"+strings.ToUpper(v.Event)]++
		names[m.objName(v.TableName)+"\x00"+m.objName(v.Name)]++
	}
	metadata := map[string]*source.TableDDLInfo{}
	metadataErrors := map[string]error{}
	// Referenced tables may already exist on the target without being selected.
	// Reading their source columns allows the converter to validate every reference.
	for _, table := range all {
		if ctx.Err() != nil {
			m.mysqlPGTriggerError = ctx.Err()
			return
		}
		info, e := m.sourceTableDDLInfo(ctx, table)
		if e != nil || info == nil {
			if e == nil {
				e = fmt.Errorf("源表 %s 的列元数据为空", table)
			}
			metadataErrors[table] = e
			continue
		}
		metadata[table] = info
	}
	for _, v := range values {
		if !slices.Contains(selected, v.TableName) || m.excludedMySQLPGTrigger(v) {
			continue
		}
		p := preparedMySQLPGTrigger{source: v, stage: "unsupported"}
		switch {
		case v.MetadataError != "":
			p.err, p.stage = fmt.Errorf("%s", v.MetadataError), "metadata"
		case metadataErrors[v.TableName] != nil:
			p.err, p.stage = metadataErrors[v.TableName], "metadata"
		case groups[v.TableName+"\x00"+strings.ToUpper(v.Timing)+"\x00"+strings.ToUpper(v.Event)] > 1:
			p.err = fmt.Errorf("同表、同事件及同时机存在多个触发器，执行顺序需人工适配")
		case names[m.objName(v.TableName)+"\x00"+m.objName(v.Name)] > 1:
			p.err = fmt.Errorf("目标触发器名称在大小写转换后冲突")
		default:
			p.plan, p.err = mysqltrigger.Convert(v, metadata, mysqltrigger.Options{Schema: m.cfg.TargetSchema, Name: m.objName, CharacterMetadata: characters})
			if p.err != nil && characterErr != nil {
				p.err = fmt.Errorf("%v；读取源字符排序元数据失败: %w", p.err, characterErr)
			}
			if p.err == nil {
				m.freezeMySQLPGTriggerTables(&p.plan, metadata)
			}
		}
		m.mysqlPGTriggers = append(m.mysqlPGTriggers, p)
	}
	if len(m.mysqlPGTriggers) > 0 {
		m.log.Info("触发器使用 SECURITY INVOKER，不映射源 DEFINER；时间函数使用目标业务会话时区，请保持两端一致")
	}
}

// BuildMySQLPGTriggerRepairPlan only reads metadata. The caller owns auth,
// original-report provenance, preview hashes, and the eventual ledger CAS.
func BuildMySQLPGTriggerRepairPlan(ctx context.Context, r source.Reader, w target.Writer, cfg Config, original repair.Object, existing []repair.Object) (repair.Object, error) {
	if original.Kind != "trigger" || original.SourceName == "" || original.SourceTable == "" || original.SourceDDL == "" {
		return repair.Object{}, fmt.Errorf("触发器缺少可核对的原始身份或源 DDL")
	}
	if r.DBType() != "mysql" || !source.IsPGFamily(w.DBType()) {
		return repair.Object{}, fmt.Errorf("触发器重新规划仅支持 MySQL → PG 内核目标")
	}
	if original.Schema == "" || cfg.TargetSchema != "" && cfg.TargetSchema != original.Schema {
		return repair.Object{}, fmt.Errorf("触发器目标 schema 已偏离原记录")
	}
	cfg.TargetSchema = original.Schema
	cfg.TargetDBType = w.DBType()
	cfg.CaptureRepair = false
	cfg.RepairSink = nil
	cfg.RepairObjectSink = nil
	cfg.Content = "schema_only"
	cfg.Objects = []string{"triggers"}
	cfg.TableNames = []string{original.SourceTable}
	cfg.ViewNames = nil
	cfg.MigrateTriggers = true
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 1024)}, cfg)
	if m.objName(original.SourceTable) != original.Table || m.objName(original.SourceName) != original.TargetName {
		return repair.Object{}, fmt.Errorf("触发器名称映射已偏离原记录")
	}
	m.prepareMySQLPGTriggers(ctx)
	if m.mysqlPGTriggerError != nil {
		return repair.Object{}, m.mysqlPGTriggerError
	}
	var found *preparedMySQLPGTrigger
	for i := range m.mysqlPGTriggers {
		p := &m.mysqlPGTriggers[i]
		if p.source.Name == original.SourceName && p.source.TableName == original.SourceTable {
			if found != nil {
				return repair.Object{}, fmt.Errorf("源触发器身份不唯一")
			}
			found = p
		}
	}
	if found == nil {
		return repair.Object{}, fmt.Errorf("原源触发器已不存在或当前账号不可见")
	}
	if found.source.SourceDDL != original.SourceDDL {
		return repair.Object{}, fmt.Errorf("源触发器 DDL 已改变，不能替换原迁移的冻结计划")
	}
	if found.err != nil {
		return repair.Object{}, found.err
	}
	if found.plan.Table != original.Table || found.plan.Name != original.TargetName || found.plan.Schema != original.Schema {
		return repair.Object{}, fmt.Errorf("重新规划后的触发器身份与原对象不一致")
	}
	candidate := original
	plan := found.plan
	candidate.MySQLPGTrigger = &plan
	candidate.DiagnosticOnly = false
	candidate.RequiresReview = false
	candidate.Status = "pending"
	candidate.Error = ""
	candidate.SuggestedDDL = ""
	candidate.DDL = dialect.JoinSQL(triggerStatements(plan))
	candidate.Units = unitsFor(triggerStatements(plan))
	candidate.Dependencies = []string{}
	for _, name := range plan.Dependencies {
		var match *repair.Object
		for _, item := range existing {
			targetName := item.Table
			if targetName == "" {
				targetName = item.TargetName
			}
			if item.Kind != "table" || targetName != name {
				continue
			}
			if item.Schema != plan.Schema {
				return repair.Object{}, fmt.Errorf("依赖表 %s 的 ledger schema 与原触发器不一致", name)
			}
			if match != nil {
				return repair.Object{}, fmt.Errorf("依赖表 %s 的 ledger 身份不唯一", name)
			}
			var sourceName string
			for _, table := range plan.SourceTables {
				if m.objName(table.TableName) == name {
					sourceName = table.TableName
					break
				}
			}
			identity := item.SourceTable
			if identity == "" {
				identity = item.SourceName
			}
			if sourceName == "" || identity != sourceName || item.TargetName != name || item.ID == "" {
				return repair.Object{}, fmt.Errorf("依赖表 %s 的 ledger 身份已偏离原迁移", name)
			}
			copied := item
			match = &copied
		}
		if match != nil {
			candidate.Dependencies = append(candidate.Dependencies, match.ID)
		}
	}
	return candidate, nil
}

func (m *Migrator) excludedMySQLPGTrigger(v source.TriggerInfo) bool {
	for _, e := range m.cfg.ExcludedObjects {
		if e.Kind == "table" && e.Name == v.TableName || (e.Kind == "trigger" || e.Kind == "triggers") && e.Name == v.Name && (e.Table == "" || e.Table == v.TableName) {
			return true
		}
	}
	return false
}

func (m *Migrator) freezeMySQLPGTriggerTables(p *mysqltrigger.Plan, tables map[string]*source.TableDDLInfo) {
	p.Tables, p.SourceTables = nil, nil
	p.LowerCaseNames = m.cfg.LowerCaseNames
	if m.cfg.ChangeOwner {
		p.Owner = m.cfg.TargetSchema
	}
	for _, targetTable := range p.Dependencies {
		for name, info := range tables {
			if m.objName(name) != targetTable {
				continue
			}
			p.SourceTables = append(p.SourceTables, *cloneSourceTableDDLInfo(info))
			mapped := cloneSourceTableDDLInfo(info)
			mapped.TableName = targetTable
			for i, col := range mapped.Columns {
				mapped.Columns[i].Name = m.objName(col.Name)
				mapped.Columns[i].DataType = m.writer.Dialect().MapType(col, "mysql", dialect.TypeOpt{CharInLength: m.cfg.CharInLength, UseNvarchar2: m.cfg.UseNvarchar2})
			}
			p.Tables = append(p.Tables, *mapped)
			break
		}
	}
}

func triggerStatements(p mysqltrigger.Plan) []dialect.Statement {
	return []dialect.Statement{{SQL: p.FunctionSQL}, {SQL: p.TriggerSQL}}
}

func triggerSourceDDL(v source.TriggerInfo) string {
	if v.SourceDDL != "" {
		return v.SourceDDL
	}
	if v.Body != "" {
		return "-- 未能读取完整 CREATE TRIGGER；以下是原始触发器正文，仅供人工适配参考\n" + v.Body
	}
	return ""
}

func (m *Migrator) triggerRepairObject(p preparedMySQLPGTrigger) repair.Object {
	o := repair.Object{Kind: "trigger", Name: m.objName(p.source.Name), SourceName: p.source.Name,
		SourceTable: p.source.TableName, Table: m.objName(p.source.TableName), TargetName: m.objName(p.source.Name),
		SourceDDL: triggerSourceDDL(p.source), Schema: m.cfg.TargetSchema, Dependencies: []string{}}
	if p.err != nil {
		o.Error, o.DiagnosticOnly = p.err.Error(), true
		return o
	}
	plan := p.plan
	o.MySQLPGTrigger = &plan
	for _, dep := range plan.Dependencies {
		if m.repairs == nil {
			continue
		}
		for _, existing := range m.repairs.objects {
			if existing.Kind == "table" && existing.Table == dep {
				o.Dependencies = append(o.Dependencies, existing.ID)
				break
			}
		}
	}
	return o
}

func (m *Migrator) initializeMySQLPGTriggerReport(report *MigrationReport) {
	report.TriggerMigrationEnabled = true
	if m.mysqlPGTriggerError != nil {
		recordMetadataFailure(&report.Triggers, m.mysqlPGTriggerError)
		return
	}
	report.Triggers.Total = len(m.mysqlPGTriggers)
	for _, p := range m.mysqlPGTriggers {
		if p.err != nil {
			m.recordMySQLPGTriggerFailure(report, p, p.stage, p.err)
		}
	}
}

func (m *Migrator) recordMySQLPGTriggerFailure(report *MigrationReport, p preparedMySQLPGTrigger, stage string, err error) {
	report.Triggers.Failed++
	ddl := ""
	if p.err == nil {
		ddl = dialect.JoinSQL(triggerStatements(p.plan))
	}
	report.Triggers.Items = append(report.Triggers.Items, ObjectResult{
		Name: m.objName(p.source.Name), SourceName: p.source.Name, TargetName: m.objName(p.source.Name),
		Table: p.source.TableName, TargetTable: m.objName(p.source.TableName), TargetSchema: m.cfg.TargetSchema,
		Stage: stage, DDL: ddl, SourceDDL: triggerSourceDDL(p.source), Error: err.Error(),
	})
	m.log.Errorf("触发器 [%s.%s] %s: %v", p.source.TableName, p.source.Name, stage, err)
}

func (m *Migrator) createMySQLPGTriggers(ctx context.Context, report *MigrationReport) {
	if !m.mysqlPGTriggerRequested() || m.mysqlPGTriggerError != nil {
		return
	}
	failed := map[string]bool{}
	for _, category := range []CategoryReport{report.Tables, report.Data} {
		for _, result := range category.Items {
			if result.Error != "" {
				failed[m.objName(result.Name)] = true
			}
		}
	}
	for _, count := range report.RowCounts {
		if !count.Match {
			failed[count.Table] = true
		}
	}
	for _, prepared := range m.mysqlPGTriggers {
		if ctx.Err() != nil {
			return
		}
		if prepared.err != nil {
			continue
		}
		blocked := ""
		for _, dep := range prepared.plan.Dependencies {
			if failed[dep] {
				blocked = dep
				break
			}
		}
		if blocked != "" {
			m.recordMySQLPGTriggerFailure(report, prepared, "dependency", fmt.Errorf("依赖表 %s 的结构或数据迁移失败，未安装触发器", blocked))
			continue
		}
		object := m.triggerRepairObject(prepared)
		err := m.captureRepairCall(ctx, object, triggerStatements(prepared.plan), func(ctx context.Context) error {
			return target.CreateMySQLPGTrigger(ctx, m.writer, prepared.plan, prepared.plan.Owner)
		})
		if err != nil {
			m.recordMySQLPGTriggerFailure(report, prepared, "execute", err)
		} else {
			report.Triggers.Success++
			m.log.DDLf("创建触发器 %s ... OK", prepared.plan.Name)
		}
	}
}
