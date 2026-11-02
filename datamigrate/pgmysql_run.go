package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
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
func (m *Migrator) runPGMySQL(ctx context.Context) (report MigrationReport) {
	report = newMigrationReport()
	m.log.Info("=== PG → MySQL 结构预检（只读，不扫描业务数据）===")
	plan := m.PreflightPGMySQL(ctx)
	report.Preflight = plan
	for _, table := range plan.tables {
		ref := tableRef(table.Info.TableName)
		if err := plan.blockedDependency(ref, map[ObjectRef]bool{}); err != nil {
			m.blockMigrationTable(ref.Name, "preflight", err)
		}
	}
	schemaRun := !m.cfg.objectMode() && len(m.cfg.ViewNames) == 0 && m.cfg.Content != "data_only"
	if schemaRun {
		report.Tables.Total = len(plan.tables)
		for _, t := range plan.tables {
			if err := plan.blockedDependency(tableRef(t.Info.TableName), map[ObjectRef]bool{}); err != nil {
				report.Tables.Failed++
				report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: t.Info.TableName, Table: t.Info.TableName, Stage: "preflight", Error: err.Error()})
			}
		}
	}
	if !plan.CanStart {
		for _, issue := range plan.Issues {
			if issue.Severity == "error" {
				m.log.Errorf("预检阻止 [%s]: %s", issue.Object.Name, issue.Reason)
			}
		}
		return report
	}
	validationTables := make([]string, 0, len(plan.tables))
	for _, table := range plan.tables {
		validationTables = append(validationTables, table.Info.TableName)
	}
	m.freezeDataValidation(ctx, validationTables)
	if err := m.recheckPGMySQLText(ctx, plan, m.writer.(mysqlInspector).InspectionDB()); err != nil {
		plan.add(ObjectRef{Kind: "target", Name: plan.Target.Database}, err.Error(), false)
		plan.finish()
		m.log.Errorf("执行前复核失败: %v", err)
		return report
	}
	var dependencyTarget oracleMySQLTarget
	completed := false
	if len(plan.targetDependencies) > 0 {
		dependencyTarget, _ = m.writer.(oracleMySQLTarget)
		if dependencyTarget == nil {
			plan.add(ObjectRef{Kind: "target", Name: plan.Target.Database}, "目标 Writer 缺少旧外键恢复能力", false)
			plan.finish()
			return report
		}
		state := m.oracleMySQLState()
		state.lowerCaseNames = plan.Target.LowerCaseTableNames
		for _, fk := range plan.targetDependencies {
			state.dependencies = append(state.dependencies, OracleMySQLDependencyResult{ForeignKey: fk, DropStatus: "pending", RestoreStatus: "not_required"})
		}
		defer func() {
			if !completed {
				m.restoreOracleMySQLDependencies(dependencyTarget)
			}
			report.TargetDependencies = append([]OracleMySQLDependencyResult{}, state.dependencies...)
		}()
		if err := m.registerOracleMySQLDependencies(); err != nil {
			m.stopMigrationTask(ctx, err)
			return report
		}
		for i := range state.dependencies {
			if err := m.executeOracleMySQLDependency(ctx, dependencyTarget, i, false); err != nil {
				report.Constraints.Total++
				report.Constraints.Failed++
				report.Constraints.Items = append(report.Constraints.Items, ObjectResult{Name: state.dependencies[i].ForeignKey.ConstraintName, Stage: "drop_dependency", Error: err.Error()})
				m.stopMigrationTask(ctx, err)
				return report
			}
		}
	}
	for _, issue := range plan.Issues {
		if issue.Severity == "warning" {
			m.log.Warnf("%s", issue.Reason)
		}
	}
	m.log.Infof("结构预检通过，计划 %d 个对象；数据在复制或创建约束时验证，失败时已提交批次可能保留", len(plan.Objects))
	m.logPreparationComplete(ctx)
	copiedCounts := map[string]int64{}
	autoMax := map[string]map[string]int64{}
	execute := func(obj PlannedObject) error {
		cat := pgCategory(&report, obj.Object.Kind)
		if obj.Stage != "drop_dependency" && !(schemaRun && obj.Object.Kind == "table") {
			cat.Total++
		}
		if e := ctx.Err(); e != nil {
			cat.Failed++
			cat.Items = append(cat.Items, ObjectResult{Name: obj.Object.Name, Error: e.Error()})
			return e
		}
		err := m.repairPlanned(ctx, obj) // common statement execution; no regeneration
		if err != nil {
			cat.Failed++
			detail := err.Error()
			if obj.Object.Table != "" {
				detail = fmt.Sprintf("表 %s，已提交 %d 行；%s", obj.Object.Table, copiedCounts[obj.Object.Table], detail)
			}
			cat.Items = append(cat.Items, ObjectResult{Name: obj.Object.Name, DDL: dialect.JoinSQL(obj.Statements), Error: detail})
			m.log.Errorf("执行失败 [%s]: %v", obj.Object.Name, err)
			return err
		}
		if obj.Stage != "drop_dependency" {
			cat.Success++
			if obj.Object.Kind == "view" {
				cat.Items = append(cat.Items, ObjectResult{Name: obj.Object.Name, DDL: dialect.JoinSQL(obj.Statements)})
			}
		}
		m.log.DDLf("%s %s ... OK", obj.Object.Kind, obj.Object.Name)
		return nil
	}
	for _, obj := range plan.Objects {
		if obj.Stage != "table" {
			continue
		}
		if plan.objectBlocked(obj.Object) {
			continue
		}
		if m.oracleMySQL != nil {
			m.oracleMySQL.touched[m.oracleMySQLTargetKey(obj.Target)] = true
		}
		if err := execute(obj); err != nil {
			plan.blocked[obj.Object] = err
			m.blockMigrationTable(obj.Object.Name, "execute", err)
			if m.stopMigrationTask(ctx, err) {
				return report
			}
			continue
		}
		if m.oracleMySQL != nil {
			m.oracleMySQL.postEligible[obj.Object.Name] = true
		}
	}

	if !m.cfg.objectMode() && len(m.cfg.ViewNames) == 0 && m.cfg.Content != "data_only" {
		// Indexes embedded into CREATE TABLE and inline comments are real migrated
		// objects too; they must appear in the totals without duplicate execution.
		for _, t := range plan.tables {
			if plan.objectBlocked(tableRef(t.Info.TableName)) {
				continue
			}
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
			if plan.objectBlocked(tableRef(t.Info.TableName)) {
				continue
			}
			if err := ctx.Err(); err != nil {
				report.Data.Failed++
				report.Data.Items = append(report.Data.Items, ObjectResult{Name: t.Info.TableName, Error: err.Error()})
				return report
			}
			report.Data.Total++
			copied := int64(0)
			batch := 1
			autoMax[t.Info.TableName] = map[string]int64{}
			err := m.eachPGPage(ctx, t, nil, func(cols []string, rows [][]any) error {
				if err := checkPGCopyPacket(t, rows, plan.Target.MaxAllowedPacket); err != nil {
					return err
				}
				types := make([]string, len(t.Info.Columns))
				for i, c := range t.Info.Columns {
					types[i] = c.DataType
				}
				if e := m.writer.CopyData(ctx, m.objName(t.Info.TableName), mapNames(cols, m.objName), types, rows); e != nil {
					// MySQL does not name a column for every conversion error.
					// A single-column insert still has an unambiguous source column.
					if len(cols) == 1 {
						return fmt.Errorf("%s.%s: %w", t.Info.TableName, cols[0], e)
					}
					return e
				}
				copied += int64(len(rows))
				copiedCounts[t.Info.TableName] = copied
				for _, row := range rows {
					for i, c := range t.Info.Columns {
						if c.Extra == "auto_increment" {
							if n, ok := row[i].(int64); ok && n > autoMax[t.Info.TableName][c.Name] {
								autoMax[t.Info.TableName][c.Name] = n
							}
						}
					}
				}
				m.log.Dataf("%s 已复制 %d 行", t.Info.TableName, copied)
				batch++
				return nil
			})
			if err != nil {
				err = fmt.Errorf("表 %s，第 %d 批失败，已提交 %d 行：%w", t.Info.TableName, batch, copied, err)
				report.Data.Failed++
				report.Data.Items = append(report.Data.Items, ObjectResult{Name: t.Info.TableName, Error: err.Error()})
				m.log.Errorf("数据迁移失败 [%s]: %v", t.Info.TableName, err)
				return report
			}
			report.Data.Success++
			m.markDataValidationCopied(t.Info.TableName)
		}
	}
	for _, obj := range plan.Objects {
		if obj.Stage == "drop_dependency" || obj.Stage == "table" {
			continue
		}
		if err := plan.blockedDependency(obj.Object, map[ObjectRef]bool{}); err != nil {
			m.recordPGBlockedObject(obj, err, plan.dependencies[obj.Object]...)
			cat := pgCategory(&report, obj.Object.Kind)
			cat.Total++
			cat.Failed++
			cat.Items = append(cat.Items, ObjectResult{Name: obj.Object.Name, Table: obj.Object.Table, Stage: "dependency", DDL: dialect.JoinSQL(obj.Statements), Error: "依赖对象未成功迁移：" + err.Error()})
			continue
		}
		if obj.Stage == "sequence" {
			stmts, err := m.pgExecutionSequence(ctx, plan, obj.Object, autoMax)
			if err != nil {
				report.Sequences.Total++
				report.Sequences.Failed++
				detail := fmt.Sprintf("表 %s，已提交 %d 行；自增调整失败：%v", obj.Object.Table, copiedCounts[obj.Object.Table], err)
				report.Sequences.Items = append(report.Sequences.Items, ObjectResult{Name: obj.Object.Name, Error: detail})
				m.log.Errorf("%s", detail)
				if m.stopMigrationTask(ctx, err) {
					return report
				}
				plan.blocked[obj.Object] = err
				continue
			}
			obj.Statements = stmts
		}
		if err := execute(obj); err != nil {
			if m.stopMigrationTask(ctx, err) {
				return report
			}
			plan.blocked[obj.Object] = err
		}
	}
	if !m.cfg.objectMode() && len(m.cfg.ViewNames) == 0 && m.cfg.Content != "schema_only" {
		for _, t := range plan.tables {
			if plan.objectBlocked(tableRef(t.Info.TableName)) {
				continue
			}
			src, e1 := m.reader.CountRows(ctx, t.Info.TableName)
			if m.stopMigrationTask(ctx, e1) {
				return report
			}
			dst, e2 := m.writer.CountRows(ctx, m.objName(t.Info.TableName))
			if m.stopMigrationTask(ctx, e2) {
				return report
			}
			copied := copiedCounts[t.Info.TableName]
			// Partition data-only runs append to the frozen target. Compare the
			// committed increment here; finishPartitions also proves every leaf.
			increment := dst
			if s := m.partitionState; m.cfg.Content == "data_only" && s != nil {
				if p := s.tables[t.Info.TableName]; p != nil && p.Plan != nil {
					for _, mapping := range p.Plan.Mapping {
						baseline, ok := partitionRowCount(p.Baseline, mapping.TargetSchema, mapping.TargetName, mapping.TargetOrdinal)
						if !ok && e2 == nil {
							e2 = fmt.Errorf("缺少目标分区 %s 的追加基线", mapping.TargetName)
						}
						increment -= baseline
					}
				}
			}
			rc := TableRowCount{Table: m.objName(t.Info.TableName), Src: src, Dst: increment, Match: e1 == nil && e2 == nil && src == increment && src == copied}
			report.RowCounts = append(report.RowCounts, rc)
			if !rc.Match {
				detail := fmt.Sprintf("行数校验失败：源=%d 目标本次行数=%d 已提交=%d 源错误=%v 目标错误=%v", src, increment, copied, e1, e2)
				report.Data.Success--
				report.Data.Failed++
				report.Data.Items = append(report.Data.Items, ObjectResult{Name: t.Info.TableName, Error: detail})
				m.log.Errorf("行数校验失败 [%s]: %s", t.Info.TableName, detail)
			}
		}
	}
	if dependencyTarget != nil {
		m.finalizeMySQLDependencies(dependencyTarget)
	}
	completed = true
	m.logMigrationCompletion("PG → MySQL 迁移结束，表 %d，数据 %d", report.Tables.Success, report.Data.Success)
	return report
}

func checkPGCopyPacket(t source.PGTable, rows [][]any, maxPacket int) error {
	if maxPacket <= 0 {
		return nil
	}
	for _, row := range rows {
		n := 1024 + len(row)*16
		for _, v := range row {
			if b, ok := v.([]byte); ok {
				n += len(b)
			} else if v != nil {
				n += len(fmt.Sprint(v))
			}
		}
		if n > maxPacket {
			return fmt.Errorf("%s 行数据超过 max_allowed_packet", t.Info.TableName)
		}
	}
	return nil
}

func pgSequenceStart(c source.ColumnInfo, seq source.SequenceInfo, maximum int64) (source.SequenceInfo, error) {
	var limit int64
	switch strings.ToLower(c.DataType) {
	case "int2", "smallint":
		limit = 32767
	case "int4", "int", "integer":
		limit = 2147483647
	case "int8", "bigint":
		limit = 9223372036854775807
	default:
		return seq, fmt.Errorf("%s.%s 自增列不是受支持的整数", seq.TableName, c.Name)
	}
	if maximum >= limit || seq.StartValue > limit || seq.StartValue < 1 {
		return seq, fmt.Errorf("%s.%s 无可用的下一自增值，超过目标整数范围", seq.TableName, c.Name)
	}
	seq.StartValue = max(seq.StartValue, maximum+1)
	return seq, nil
}

func (m *Migrator) pgExecutionSequence(ctx context.Context, p *PGMySQLPreflight, ref ObjectRef, maxima map[string]map[string]int64) ([]dialect.Statement, error) {
	for _, t := range p.tables {
		if t.Info.TableName != ref.Table {
			continue
		}
		for _, seq := range t.Sequences {
			if seq.Name != ref.Name {
				continue
			}
			for _, c := range t.Info.Columns {
				if c.Name != seq.ColumnName {
					continue
				}
				maximum := maxima[t.Info.TableName][c.Name]
				// Standalone sequence migration has no copy phase. Inspect the
				// existing target only when executing this sequence operation.
				if m.cfg.objectMode() {
					var n sql.NullInt64
					q := "SELECT MAX(" + sqlsyntax.QuoteIdent(m.objName(c.Name)) + ") FROM " + sqlsyntax.QuoteIdent(p.Target.Database) + "." + sqlsyntax.QuoteIdent(m.objName(t.Info.TableName))
					if err := m.writer.(mysqlInspector).InspectionDB().QueryRowContext(ctx, q).Scan(&n); err != nil {
						return nil, err
					}
					if n.Valid {
						maximum = n.Int64
					}
				}
				resolved, err := pgSequenceStart(c, seq.SequenceInfo, maximum)
				if err != nil {
					return nil, err
				}
				resolved.TableName, resolved.ColumnName = m.objName(resolved.TableName), m.objName(resolved.ColumnName)
				return dialect.NewMySQL().SequenceStatements(p.Target.Database, resolved, ""), nil
			}
		}
	}
	return nil, fmt.Errorf("序列 %s 缺少执行元数据", ref.Name)
}

var _ source.Reader = (*source.PostgresReader)(nil)
