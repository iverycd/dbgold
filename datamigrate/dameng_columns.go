package datamigrate

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnames"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/tsqldameng"
)

type damengTablePlan struct {
	source  *source.TableDDLInfo
	target  *source.TableDDLInfo
	columns map[string]string
	renamed bool
}

// Plans are built before data workers start. Source metadata remains untouched.
func (m *Migrator) planDamengTable(ctx context.Context, table string) (*damengTablePlan, error) {
	if p := m.dmPlans[table]; p != nil {
		return p, nil
	}
	var info *source.TableDDLInfo
	var err error
	if snapshot, ok := m.reader.(*sqlServerDamengSnapshot); ok {
		// Selected tables are already cached above. Object-only foreign keys
		// may additionally reference an existing table outside that snapshot.
		info, err = snapshot.Reader.GetTableDDLInfo(ctx, table)
	} else {
		info, err = m.sourceTableDDLInfo(ctx, table)
	}
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, fmt.Errorf("表 %s 缺少列元数据", table)
	}
	names := make([]string, len(info.Columns))
	for i, c := range info.Columns {
		names[i] = c.Name
	}
	targets, err := damengnames.Plan(names)
	if err != nil {
		return nil, err
	}
	copyInfo := *info
	copyInfo.Columns = append([]source.ColumnInfo(nil), info.Columns...)
	p := &damengTablePlan{source: info, target: &copyInfo, columns: map[string]string{}}
	for i, c := range info.Columns {
		if err := tsqldameng.ValidName(targets[i]); err != nil {
			return nil, err
		}
		copyInfo.Columns[i].Name = targets[i]
		p.columns[c.Name] = targets[i]
		p.renamed = p.renamed || damengnames.Forbidden(c.Name)
	}
	if m.dmPlans == nil {
		m.dmPlans = map[string]*damengTablePlan{}
	}
	m.dmPlans[table] = p
	return p, nil
}

func (m *Migrator) targetColumn(table, column string) string {
	if p := m.dmPlans[table]; p != nil {
		if name, ok := p.columns[column]; ok {
			return name
		}
		// Some drivers vary metadata identifier case. Ambiguous folded source
		// names have already been rejected by Plan.
		for original, target := range p.columns {
			if strings.EqualFold(original, column) {
				return target
			}
		}
	}
	return m.objName(column)
}

func (m *Migrator) damengTargetInfo(info *source.TableDDLInfo) *source.TableDDLInfo {
	if p := m.dmPlans[info.TableName]; p != nil {
		return p.target
	}
	return info
}

func (m *Migrator) validateDamengMapping(ctx context.Context, p *damengTablePlan) error {
	if !p.renamed {
		return nil
	}
	v, ok := m.writer.(interface {
		ValidateDamengColumns(context.Context, *source.TableDDLInfo) error
	})
	if !ok {
		return fmt.Errorf("目标写入器不支持校验已有列名映射")
	}
	return v.ValidateDamengColumns(ctx, p.target)
}

func (m *Migrator) prepareDamengColumns(ctx context.Context, tables []string, report *MigrationReport) map[string]bool {
	failed := map[string]bool{}
	if len(tables) == 0 || m.writer.DBType() != "dameng" {
		return failed
	}
	m.dmBlocked = map[string]bool{}
	m.dmUsable = map[string]bool{}
	origins := map[string][]string{}
	for _, table := range tables {
		origins[strings.ToUpper(table)] = append(origins[strings.ToUpper(table)], table)
	}
	for _, table := range tables {
		err := tsqldameng.ValidName(table)
		if len(origins[strings.ToUpper(table)]) > 1 {
			err = fmt.Errorf("目标表名冲突: %s", strings.Join(origins[strings.ToUpper(table)], ", "))
		}
		var p *damengTablePlan
		if err == nil {
			p, err = m.planDamengTable(ctx, table)
		}
		if err == nil && (m.cfg.Content == "data_only" || m.cfg.objectMode()) {
			err = m.validateDamengMapping(ctx, p)
		}
		if err != nil {
			if m.stopOracleDamengTask(ctx, err) {
				return failed
			}
			failed[table], m.dmBlocked[table] = true, true
			report.Tables.Failed++
			result := objectResultForError(table, "", err)
			if m.isOracleDameng() {
				result.Stage = "preflight"
			}
			report.Tables.Items = append(report.Tables.Items, result)
			m.log.Errorf("达梦列名预检 [%s]: %v", table, err)
		}
	}
	return failed
}

func (m *Migrator) recordDamengRepairs(report *MigrationReport, table, status string) {
	p := m.dmPlans[table]
	if p != nil {
		m.dmUsable[table] = true
	}
	if p == nil || !p.renamed {
		return
	}
	for _, c := range p.source.Columns {
		if !damengnames.Forbidden(c.Name) {
			continue
		}
		r := AutoRepair{Type: "column_rename", Table: table, TargetTable: strings.ToUpper(table), Source: c.Name,
			Target: p.columns[c.Name], Reason: "达梦禁用列名", Status: status}
		if m.cfg.TargetSchema != "" {
			r.TargetTable = strings.ToUpper(m.cfg.TargetSchema) + "." + r.TargetTable
		}
		report.AutoRepairs = append(report.AutoRepairs, r)
		label := "自动修复"
		if status == "reused" {
			label = "复用已有映射"
		}
		m.log.Warnf("[%s] %s：%s → %s，原因：%s", label, table, r.Source, r.Target, r.Reason)
	}
}

// External FK references are never renamed speculatively: check their actual
// destination columns first. This runs in the serial post-DDL phase.
func (m *Migrator) ensureDamengReference(ctx context.Context, table string) error {
	if m.writer.DBType() != "dameng" {
		return nil
	}
	if m.dmBlocked[table] {
		return fmt.Errorf("引用表 %s 建表或列名预检失败，未执行关联对象", table)
	}
	if m.dmUsable[table] {
		return nil
	}
	p, err := m.planDamengTable(ctx, table)
	if err != nil {
		return err
	}
	if err := m.validateDamengMapping(ctx, p); err != nil {
		return err
	}
	if m.dmUsable == nil {
		m.dmUsable = map[string]bool{}
	}
	m.dmUsable[table] = true
	return nil
}
