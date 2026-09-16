package datamigrate

import (
	"context"
	"fmt"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
)

func pgCategory(r *MigrationReport, kind string) *CategoryReport {
	switch kind {
	case "table":
		return &r.Tables
	case "primary_key":
		return &r.PrimaryKeys
	case "index":
		return &r.Indexes
	case "foreign_key", "check", "constraint":
		return &r.Constraints
	case "sequence":
		return &r.Sequences
	case "view":
		return &r.Views
	case "comment":
		return &r.Comments
	case "trigger":
		return &r.Triggers
	default:
		return &r.Data
	}
}
func (m *Migrator) runPGMySQL(ctx context.Context) MigrationReport {
	report := newMigrationReport()
	m.log.Info("=== PG → MySQL 完整兼容预检（只读）===")
	plan := m.PreflightPGMySQL(ctx)
	report.Preflight = plan
	if !plan.Ready {
		for _, issue := range plan.Issues {
			if issue.Severity == "error" {
				m.log.Errorf("预检阻止 [%s]: %s", issue.Object.Name, issue.Reason)
			}
		}
		return report
	}
	if err := m.recheckPGMySQLText(ctx, plan, m.writer.(mysqlInspector).InspectionDB()); err != nil {
		plan.add(ObjectRef{Kind: "target", Name: plan.Target.Database}, err.Error(), false)
		plan.finish()
		m.log.Errorf("执行前复核失败: %v", err)
		return report
	}
	for _, issue := range plan.Issues {
		if issue.Severity == "warning" {
			m.log.Warnf("%s", issue.Reason)
		}
	}
	m.log.Infof("预检通过，检查 %d 行，开始执行冻结的 DDL 计划", plan.RowsChecked)
	execute := func(obj PlannedObject) bool {
		cat := pgCategory(&report, obj.Object.Kind)
		if obj.Stage != "drop_dependency" {
			cat.Total++
		}
		if e := ctx.Err(); e != nil {
			cat.Failed++
			cat.Items = append(cat.Items, ObjectResult{Name: obj.Object.Name, Error: e.Error()})
			return false
		}
		err := m.writer.CreateTable(ctx, obj.Statements) // common statement execution; no regeneration
		if err != nil {
			cat.Failed++
			cat.Items = append(cat.Items, ObjectResult{Name: obj.Object.Name, DDL: dialect.JoinSQL(obj.Statements), Error: err.Error()})
			m.log.Errorf("执行失败 [%s]: %v", obj.Object.Name, err)
			return false
		}
		if obj.Stage != "drop_dependency" {
			cat.Success++
			if obj.Object.Kind == "view" {
				cat.Items = append(cat.Items, ObjectResult{Name: obj.Object.Name, DDL: dialect.JoinSQL(obj.Statements)})
			}
		}
		m.log.DDLf("%s %s ... OK", obj.Object.Kind, obj.Object.Name)
		return true
	}
	for _, obj := range plan.Objects {
		if obj.Stage == "drop_dependency" || obj.Stage == "table" {
			if !execute(obj) {
				return report
			}
		}
	}
	if !m.cfg.objectMode() && len(m.cfg.ViewNames) == 0 && m.cfg.Content != "data_only" {
		// Indexes embedded into CREATE TABLE and inline comments are real migrated
		// objects too; they must appear in the totals without duplicate execution.
		for _, t := range plan.tables {
			for _, idx := range t.Indexes {
				inline := true
				for _, o := range plan.Objects {
					if o.Stage == "index" && o.Object == idxRef(idx) {
						inline = false
					}
				}
				if inline {
					c := pgCategory(&report, idxRef(idx).Kind)
					c.Total++
					c.Success++
				}
			}
			if t.Info.Comment != "" {
				report.Comments.Total++
				report.Comments.Success++
			}
			for _, c := range t.Info.Columns {
				if c.Comment != "" {
					report.Comments.Total++
					report.Comments.Success++
				}
			}
		}
	}
	if !m.cfg.objectMode() && len(m.cfg.ViewNames) == 0 && m.cfg.Content != "schema_only" {
		for _, t := range plan.tables {
			if err := ctx.Err(); err != nil {
				report.Data.Failed++
				report.Data.Items = append(report.Data.Items, ObjectResult{Name: t.Info.TableName, Error: err.Error()})
				return report
			}
			report.Data.Total++
			copied := int64(0)
			err := m.eachPGPage(ctx, t, nil, func(cols []string, rows [][]any) error {
				types := make([]string, len(t.Info.Columns))
				for i, c := range t.Info.Columns {
					types[i] = c.DataType
				}
				if e := m.writer.CopyData(ctx, m.objName(t.Info.TableName), mapNames(cols, m.objName), types, rows); e != nil {
					return e
				}
				copied += int64(len(rows))
				m.log.Dataf("%s 已复制 %d 行", t.Info.TableName, copied)
				return nil
			})
			if err == nil && copied != plan.rowCounts[t.Info.TableName] {
				err = fmt.Errorf("源表行数在预检后变化：预检 %d / 复制 %d", plan.rowCounts[t.Info.TableName], copied)
			}
			if err != nil {
				report.Data.Failed++
				report.Data.Items = append(report.Data.Items, ObjectResult{Name: t.Info.TableName, Error: err.Error()})
				m.log.Errorf("数据迁移失败 [%s]: %v", t.Info.TableName, err)
				return report
			}
			report.Data.Success++
		}
	}
	for _, obj := range plan.Objects {
		if obj.Stage == "drop_dependency" || obj.Stage == "table" {
			continue
		}
		if !execute(obj) {
			return report
		}
	}
	if !m.cfg.objectMode() && len(m.cfg.ViewNames) == 0 && m.cfg.Content != "schema_only" {
		for _, t := range plan.tables {
			src, e1 := m.reader.CountRows(ctx, t.Info.TableName)
			dst, e2 := m.writer.CountRows(ctx, m.objName(t.Info.TableName))
			rc := TableRowCount{Table: m.objName(t.Info.TableName), Src: src, Dst: dst, Match: e1 == nil && e2 == nil && src == dst}
			report.RowCounts = append(report.RowCounts, rc)
			if !rc.Match {
				m.log.Errorf("行数校验失败 [%s]: 源=%d 目标=%d 源错误=%v 目标错误=%v", t.Info.TableName, src, dst, e1, e2)
			}
		}
	}
	m.log.Donef("PG → MySQL 迁移结束，表 %d，数据 %d", report.Tables.Success, report.Data.Success)
	return report
}

var _ source.Reader = (*source.PostgresReader)(nil)
