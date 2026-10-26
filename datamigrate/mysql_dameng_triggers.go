package datamigrate

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/mysqldmtrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
)

type preparedMySQLDamengTrigger struct {
	source      source.TriggerInfo
	plan        mysqldmtrigger.Plan
	predecessor string
	err         error
	stage       string
}

func (m *Migrator) isMySQLDameng() bool {
	return m.reader.DBType() == "mysql" && m.writer.DBType() == "dameng"
}

func (m *Migrator) mysqlDMTriggerRequested() bool {
	if !m.isMySQLDameng() || len(m.cfg.ViewNames) > 0 || m.cfg.Content == "data_only" {
		return false
	}
	if m.cfg.objectMode() {
		return slices.Contains(m.cfg.Objects, "triggers")
	}
	return m.cfg.MigrateTriggers
}

// Preparation is read-only. The same frozen mapping is used by table DDL,
// trigger execution, and subsequent repair; source identifiers are untouched.
func (m *Migrator) prepareMySQLDamengTriggers(ctx context.Context) {
	if !m.mysqlDMTriggerRequested() {
		return
	}
	r, ok := m.reader.(source.TriggerReader)
	if !ok {
		m.mysqlDMTriggerError = fmt.Errorf("源库未实现触发器元数据读取")
		return
	}
	if m.cfg.TargetSchema == "" {
		var err error
		m.cfg.TargetSchema, err = target.ResolveRepairSchema(ctx, m.writer)
		if err != nil {
			m.mysqlDMTriggerError = err
			return
		}
	}
	m.cfg.TargetSchema = strings.ToUpper(m.cfg.TargetSchema)
	all, err := m.reader.ListTables(ctx)
	if err != nil {
		m.mysqlDMTriggerError = err
		return
	}
	selected := FilterTables(all, m.cfg.Mode, m.cfg.Filter)
	if m.cfg.objectMode() && len(m.cfg.TableNames) > 0 {
		selected = nil
		for _, t := range m.cfg.TableNames {
			if slices.Contains(all, t) && !slices.Contains(selected, t) {
				selected = append(selected, t)
			}
		}
	}
	values, err := r.GetTriggers(ctx)
	if err != nil {
		m.mysqlDMTriggerError = err
		return
	}
	values = slices.DeleteFunc(slices.Clone(values), func(v source.TriggerInfo) bool {
		return !slices.Contains(selected, v.TableName) || m.excludedMySQLPGTrigger(v)
	})
	if len(values) == 0 {
		return
	}
	caps, capErr := target.DetectMySQLDamengTriggerCapabilities(ctx, m.writer)
	var characters *source.TriggerCharacterMetadata
	if cr, ok := m.reader.(source.TriggerCharacterReader); ok {
		characters, err = cr.GetTriggerCharacterMetadata(ctx)
		if err != nil {
			characters = nil
		}
	}
	metadata := map[string]*source.TableDDLInfo{}
	metadataErrors := map[string]error{}
	opt := mysqldmtrigger.Options{Schema: m.cfg.TargetSchema, Capabilities: caps, CharacterMetadata: characters,
		TableNames: map[string]string{}, ColumnNames: map[string]map[string]string{}}
	for _, table := range all {
		if ctx.Err() != nil {
			m.mysqlDMTriggerError = ctx.Err()
			return
		}
		p, e := m.planDamengTable(ctx, table)
		if e != nil {
			metadataErrors[table] = e
			continue
		}
		metadata[table] = p.source
		opt.TableNames[table] = strings.ToUpper(m.objName(table))
		opt.ColumnNames[table] = map[string]string{}
		for _, c := range p.source.Columns {
			opt.ColumnNames[table][c.Name] = strings.ToUpper(m.targetColumn(table, c.Name))
		}
	}
	sort.SliceStable(values, func(i, j int) bool {
		a, b := values[i], values[j]
		ga, gb := mysqlDMTriggerGroup(a), mysqlDMTriggerGroup(b)
		if ga != gb {
			return ga < gb
		}
		if a.ActionOrder != b.ActionOrder {
			return a.ActionOrder < b.ActionOrder
		}
		return a.Name < b.Name
	})
	names, orders, groups := map[string]int{}, map[string]int{}, map[string]int{}
	for _, v := range values {
		names[strings.ToUpper(v.Name)]++
		groups[mysqlDMTriggerGroup(v)]++
		orders[fmt.Sprintf("%s\x00%d", mysqlDMTriggerGroup(v), v.ActionOrder)]++
	}
	previous := map[string]string{}
	previousSQLs := map[string]map[string]string{}
	for _, v := range values {
		p := preparedMySQLDamengTrigger{source: v, stage: "unsupported", predecessor: previous[mysqlDMTriggerGroup(v)]}
		previous[mysqlDMTriggerGroup(v)] = strings.ToUpper(v.Name)
		switch {
		case v.MetadataError != "":
			p.err, p.stage = fmt.Errorf("%s", v.MetadataError), "metadata"
		case capErr != nil:
			p.err, p.stage = fmt.Errorf("读取目标触发器能力: %w", capErr), "preflight"
		case metadataErrors[v.TableName] != nil:
			p.err, p.stage = metadataErrors[v.TableName], "metadata"
		case names[strings.ToUpper(v.Name)] > 1:
			p.err = fmt.Errorf("目标 schema 中触发器名称转大写后冲突: %s", v.Name)
		case groups[mysqlDMTriggerGroup(v)] > 1 && (v.ActionOrder <= 0 || orders[fmt.Sprintf("%s\x00%d", mysqlDMTriggerGroup(v), v.ActionOrder)] > 1):
			p.err = fmt.Errorf("同组触发器 ACTION_ORDER 缺失或重复，无法确认执行顺序")
		default:
			o := opt
			o.Predecessor = p.predecessor
			o.PredecessorSQLs = maps.Clone(previousSQLs[mysqlDMTriggerGroup(v)])
			p.plan, p.err = mysqldmtrigger.Convert(v, metadata, o)
		}
		if p.err == nil {
			if previousSQLs[mysqlDMTriggerGroup(v)] == nil {
				previousSQLs[mysqlDMTriggerGroup(v)] = map[string]string{}
			}
			previousSQLs[mysqlDMTriggerGroup(v)][p.plan.Name] = p.plan.TriggerSQL
		}
		m.mysqlDMTriggers = append(m.mysqlDMTriggers, p)
	}
	m.log.Info("MySQL → 达梦触发器将在数据迁移与校验完成后创建；按目标所属用户权限执行，不映射源 DEFINER，请保持业务会话时区一致")
}

func mysqlDMTriggerGroup(v source.TriggerInfo) string {
	return v.TableName + "\x00" + strings.ToUpper(v.Timing) + "\x00" + strings.ToUpper(v.Event)
}

func mysqlDMTriggerStatements(p mysqldmtrigger.Plan) []dialect.Statement {
	return []dialect.Statement{{SQL: p.TriggerSQL}}
}

func (m *Migrator) mysqlDMTriggerRepairObject(p preparedMySQLDamengTrigger) repair.Object {
	o := repair.Object{Kind: "trigger", Name: strings.ToUpper(p.source.Name), SourceName: p.source.Name,
		SourceTable: p.source.TableName, Table: strings.ToUpper(p.source.TableName), TargetName: strings.ToUpper(p.source.Name),
		SourceDDL: triggerSourceDDL(p.source), Schema: m.cfg.TargetSchema, Dependencies: []string{}}
	if p.err != nil {
		o.Error, o.DiagnosticOnly = p.err.Error(), true
		return o
	}
	plan := p.plan
	o.MySQLDamengTrigger = &plan
	if m.repairs != nil {
		for _, dep := range plan.Dependencies {
			for _, existing := range m.repairs.objects {
				if existing.Kind == "table" && strings.ToUpper(existing.Table) == dep {
					o.Dependencies = append(o.Dependencies, existing.ID)
					break
				}
			}
		}
	}
	if p.predecessor != "" {
		o.Dependencies = append(o.Dependencies, repairID("trigger", o.Table, p.predecessor))
	}
	return o
}

func (m *Migrator) initializeMySQLDamengTriggerReport(report *MigrationReport) {
	report.TriggerMigrationEnabled = true
	if m.mysqlDMTriggerError != nil {
		recordMetadataFailure(&report.Triggers, m.mysqlDMTriggerError)
		return
	}
	report.Triggers.Total = len(m.mysqlDMTriggers)
	for _, p := range m.mysqlDMTriggers {
		if p.err != nil {
			m.recordMySQLDMTriggerFailure(report, p, p.stage, p.err)
		}
	}
}

func (m *Migrator) recordMySQLDMTriggerFailure(report *MigrationReport, p preparedMySQLDamengTrigger, stage string, err error) {
	report.Triggers.Failed++
	report.Triggers.Items = append(report.Triggers.Items, ObjectResult{Name: strings.ToUpper(p.source.Name),
		SourceName: p.source.Name, TargetName: strings.ToUpper(p.source.Name), Table: p.source.TableName,
		TargetTable: strings.ToUpper(p.source.TableName), TargetSchema: m.cfg.TargetSchema, Stage: stage,
		DDL: p.plan.TriggerSQL, SourceDDL: triggerSourceDDL(p.source), Error: err.Error()})
	m.log.Errorf("触发器 [%s.%s] %s: %v", p.source.TableName, p.source.Name, stage, err)
}

func (m *Migrator) createMySQLDamengTriggers(ctx context.Context, report *MigrationReport) {
	if !m.mysqlDMTriggerRequested() || m.mysqlDMTriggerError != nil || ctx.Err() != nil {
		return
	}
	failed := map[string]bool{}
	for _, c := range []CategoryReport{report.Tables, report.Data} {
		for _, r := range c.Items {
			if r.Error != "" {
				failed[strings.ToUpper(r.Name)] = true
			}
		}
	}
	for _, rc := range report.RowCounts {
		if !rc.Match {
			failed[strings.ToUpper(rc.Table)] = true
		}
	}
	if report.DataValidation != nil {
		for _, t := range report.DataValidation.Tables {
			if t.Status != validation.StatusPassed {
				failed[strings.ToUpper(t.SourceTable)] = true
			}
		}
	}
	installed := map[string]bool{}
	for _, p := range m.mysqlDMTriggers {
		if ctx.Err() != nil {
			return
		}
		if p.err != nil {
			continue
		}
		blocked := ""
		if !m.dmUsable[p.source.TableName] {
			blocked = "所属表的结构准备未成功"
		}
		for _, dep := range p.plan.Dependencies {
			if failed[dep] {
				blocked = "依赖表 " + dep + " 的结构、数据或校验未成功"
				break
			}
		}
		if blocked == "" && p.predecessor != "" && !installed[p.predecessor] {
			blocked = "前序触发器 " + p.predecessor + " 未成功安装"
		}
		if blocked != "" {
			m.recordMySQLDMTriggerFailure(report, p, "dependency", fmt.Errorf("%s，未创建触发器", blocked))
			continue
		}
		err := m.captureRepairCall(ctx, m.mysqlDMTriggerRepairObject(p), mysqlDMTriggerStatements(p.plan), func(ctx context.Context) error {
			return target.CreateMySQLDamengTrigger(ctx, m.writer, p.plan)
		})
		if err != nil {
			m.recordMySQLDMTriggerFailure(report, p, "execute", err)
			continue
		}
		installed[p.plan.Name] = true
		report.Triggers.Success++
		m.log.DDLf("创建达梦触发器 %s ... OK", p.plan.Name)
		if strings.Contains(strings.ToUpper(p.source.Body), "SIGNAL") {
			m.log.Warnf("触发器 %s 的业务异常已映射为达梦应用错误，SQLSTATE/错误号与 MySQL 不同", p.plan.Name)
		}
	}
}
