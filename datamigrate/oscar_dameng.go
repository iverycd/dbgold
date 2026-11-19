package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"dbgold/datamigrate/valueconv"
)

type oscarDamengTableStream interface {
	StreamOscarDamengTable(context.Context, string, int, func([]string, []string, [][]any) error) error
}

func (m *Migrator) isOscarDameng() bool {
	return m.reader.DBType() == "oscar" && m.writer.DBType() == "dameng"
}

func validateOscarDamengExisting(ctx context.Context, w target.Writer, info *source.TableDDLInfo) error {
	checker, ok := w.(interface {
		ValidateOscarDamengExisting(context.Context, *source.TableDDLInfo) error
	})
	if !ok {
		return fmt.Errorf("目标 Writer 缺少神通到达梦的已有表核验能力")
	}
	return checker.ValidateOscarDamengExisting(ctx, info)
}

func (m *Migrator) preflightOscarDamengAppend(ctx context.Context, tables []string, failed map[string]bool, report *MigrationReport) {
	m.oscarAppendCounts = make(map[string]int64, len(tables))
	for _, table := range tables {
		if failed[table] || m.migrationTableBlocked(table) {
			continue
		}
		dst := m.objName(table)
		count, err := m.writer.CountRows(ctx, dst)
		if err == nil {
			m.oscarAppendCounts[table] = count
			continue
		}
		if m.stopMigrationTask(ctx, err) {
			return
		}
		err = fmt.Errorf("读取目标表追加前行数: %w", err)
		m.blockMigrationTable(table, "preflight", err)
		failed[table] = true
		report.Data.Failed++
		report.Data.Items = append(report.Data.Items, ObjectResult{Name: table, TargetTable: dst, Stage: "preflight", Error: err.Error()})
		report.addDataIssue(table, dst, rowCountError, err.Error())
	}
}

func (m *Migrator) migrateOscarDamengTable(ctx context.Context, table string) error {
	stream, ok := m.reader.(oscarDamengTableStream)
	if !ok {
		return fmt.Errorf("神通源库缺少达梦迁移的固定游标读取能力")
	}
	if m.cfg.Content == "data_only" {
		if err := m.oscarAppendReady(ctx, table); err != nil {
			m.stopMigrationTask(ctx, err)
			return err
		}
	}
	var copied int64
	err := stream.StreamOscarDamengTable(ctx, table, m.cfg.PageSize, func(cols, types []string, rows [][]any) error {
		dst := make([]string, len(cols))
		for i, col := range cols {
			dst[i] = m.targetColumn(table, col)
		}
		if err := m.copyTableBatch(ctx, m.objName(table), dst, types, rows); err != nil {
			return fmt.Errorf("表 %s 已提交 %d 行，当前批次写入失败: %w", table, copied, err)
		}
		copied += int64(len(rows))
		return nil
	})
	if err != nil {
		m.stopMigrationTask(ctx, err)
		m.log.Errorf("迁移神通表 [%s] 失败: %v", table, err)
		return err
	}
	m.log.Dataf("迁移 %s: %d 行 ... OK", table, copied)
	return nil
}

func (m *Migrator) checkOscarDamengForeignKey(fk source.FKInfo) source.FKInfo {
	if fk.RefSchema != "" && fk.RefSchema != m.dmSourceSchema {
		fk.Unsupported = fmt.Sprintf("跨 Schema 外键引用 %s.%s 需要手工适配", fk.RefSchema, fk.RefTable)
		return fk
	}
	if err := dialect.ValidateOscarDamengForeignKey(fk); err != nil {
		fk.Unsupported = err.Error()
	} else if strings.EqualFold(strings.TrimSpace(fk.OnDelete), "RESTRICT") {
		fk.OnDelete = "NO ACTION"
	}
	return fk
}

func (m *Migrator) migrationForeignKeyStatements(fk source.FKInfo) []dialect.Statement {
	if m.isOscarDameng() {
		return dialect.OscarDamengForeignKeyStatements(m.writer.Dialect(), m.cfg.TargetSchema, fk)
	}
	return m.writer.Dialect().ForeignKeyStatements(m.cfg.TargetSchema, fk)
}

func (m *Migrator) oscarDamengViewResult(view source.ViewInfo) ObjectResult {
	result := ObjectResult{ObjectKind: "view", Name: view.ViewName, SourceName: view.ViewName,
		TargetName: m.objName(view.ViewName), TargetSchema: m.cfg.TargetSchema, SourceDDL: view.Definition,
		Stage: "unsupported", Error: "神通到达梦的视图需要手工适配"}
	if view.Unsupported != "" {
		result.Error += "；" + view.Unsupported
	}
	if deps, err := migrationViewDependencies(view.Definition, "oscar"); err == nil {
		for _, dep := range deps {
			if failed := m.migrationTargetTableError(m.objName(dep)); failed != nil {
				result.Stage, result.Error = "dependency", fmt.Sprintf("依赖表 %s 迁移失败: %v", dep, failed)
				break
			}
		}
	}
	return result
}

func (m *Migrator) recordOscarDamengViews(views []source.ViewInfo, report *MigrationReport) {
	for _, view := range views {
		result := m.oscarDamengViewResult(view)
		if result.Stage == "dependency" {
			report.Views.Total++
			report.Views.Failed++
			report.Views.Items = append(report.Views.Items, result)
			continue
		}
		m.oscarDamengManualWarning(report, view.ViewName, result.Error, result.SourceDDL)
	}
}

func (m *Migrator) recordOscarDamengManualObjects(ctx context.Context, report *MigrationReport) {
	r, ok := m.reader.(interface {
		OscarManualObjects(context.Context) ([]source.OscarManualObject, error)
	})
	if !ok || ctx.Err() != nil {
		return
	}
	objects, err := r.OscarManualObjects(ctx)
	if m.stopMigrationTask(ctx, err) {
		return
	}
	if err != nil {
		m.oscarDamengManualWarning(report, "对象盘点", err.Error(), "")
	}
	labels := map[string]string{"routine": "函数或存储过程", "trigger": "触发器", "sequence": "独立或共享序列"}
	for _, object := range objects {
		m.oscarDamengManualWarning(report, object.Name, labels[object.Kind]+"需要在达梦手工适配", object.Definition)
	}
}

func (m *Migrator) oscarDamengManualWarning(report *MigrationReport, name, reason, definition string) {
	report.Warnings = append(report.Warnings, valueconv.MigrationWarning{
		Table: m.dmSourceSchema, Column: name, Count: 1, Reason: reason, SourceDDL: definition,
	})
}

func oscarDamengUnverifiableColumns(plan validation.Plan) string {
	var columns []string
	for i, column := range plan.SourceColumns {
		dst := plan.TargetColumns[i]
		if column.Kind == "" || column.Kind == validation.Unsupported || dst.Kind == "" || dst.Kind == validation.Unsupported {
			columns = append(columns, fmt.Sprintf("%s (%s → %s)", column.Name, column.DatabaseType, dst.DatabaseType))
		}
	}
	if len(columns) == 0 {
		return ""
	}
	return "以下列缺少无损内容校验能力：" + strings.Join(columns, "、")
}

func oscarDamengIdentityShape(columns []source.ColumnInfo) []source.ColumnInfo {
	columns = slices.Clone(columns)
	for i, col := range columns {
		if col.IdentityInfo != nil {
			columns[i].IdentityInfo = &source.IdentityInfo{Increment: col.IdentityInfo.Increment}
		}
	}
	return columns
}
