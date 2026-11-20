package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/valueconv"
)

type pgOracleExecutor interface {
	ExecutePGOracleStatements(context.Context, string, string, []dialect.Statement) error
	PGOracleSequenceIdentity(context.Context, pgoracle.SequencePlan) (*pgoracle.SequenceIdentity, error)
}

func (m *Migrator) runPGOracle(ctx context.Context) (report MigrationReport) {
	report = newMigrationReport()
	defer func() {
		if warnings, ok := m.writer.(interface {
			MigrationWarnings() []valueconv.MigrationWarning
		}); ok {
			report.Warnings = append(report.Warnings, warnings.MigrationWarnings()...)
		}
	}()
	m.log.Info("=== PG 内核 → Oracle：只读结构预检；复制阶段逐值严格校验 ===")
	p := m.PreflightPGOracle(ctx)
	report.PGOraclePreflight = p
	for _, o := range p.Objects {
		if o.Object.Kind == "trigger" {
			report.TriggerMigrationEnabled = true
		}
	}
	for _, t := range p.tables {
		if e := p.blocked[tableRef(t.Info.TableName)]; e != nil {
			m.blockMigrationTable(t.Info.TableName, "preflight", e)
		}
	}
	for _, issue := range p.Issues {
		if issue.Severity == "warning" {
			m.log.Warnf("%s: %s", issue.Object.Name, issue.Reason)
			report.Warnings = append(report.Warnings, valueconv.MigrationWarning{Table: issue.Object.Table, Reason: issue.Reason, Count: 1})
		}
	}
	if m.cfg.CaptureRepair {
		if err := m.beginPGOracleRepair(p); err != nil {
			report.Data.Total++
			report.Data.Failed++
			report.Data.Items = append(report.Data.Items, ObjectResult{Name: "修复记录持久化", Stage: "task", Error: err.Error()})
			m.stopMigrationTask(ctx, err)
			return report
		}
	}
	m.logPreparationComplete(ctx)
	columns := map[string][]source.ColumnInfo{}
	var validationTables []string
	for _, t := range p.tables {
		cs := slices.Clone(t.Info.Columns)
		for i := range cs {
			cs[i].Name = m.objName(cs[i].Name)
		}
		columns[m.objName(t.Info.TableName)] = cs
		validationTables = append(validationTables, t.Info.TableName)
	}
	if setter, ok := m.writer.(interface {
		ConfigurePGOracle(map[string][]source.ColumnInfo)
	}); ok {
		setter.ConfigurePGOracle(columns)
	}
	m.freezeDataValidation(ctx, validationTables)
	objects := slices.Clone(p.Objects)
	p.createdTables = map[string]bool{}
	order := map[string]int{"table": 0, "index": 1, "check": 1, "sequence": 2, "foreign_key": 3, "view": 4, "comment": 5, "trigger": 6}
	sort.SliceStable(objects, func(i, j int) bool { return order[objects[i].Stage] < order[objects[j].Stage] })
	failObject := func(o PlannedObject, stage string, e error) error {
		c := pgCategory(&report, o.Object.Kind)
		c.Total++
		c.Failed++
		c.Items = append(c.Items, ObjectResult{ObjectKind: o.Object.Kind, Name: o.Object.Name, SourceName: o.Object.Name, Table: o.Object.Table, TargetName: o.Target, TargetSchema: m.cfg.TargetSchema, Stage: stage, DDL: dialect.JoinSQL(o.Statements), SourceDDL: p.sourceDDL[o.Object], Error: e.Error()})
		m.log.Errorf("%s %s [%s]: %v", o.Object.Kind, o.Object.Name, stage, e)
		if err := m.markPGOracleObjectFailure(p, o, stage, e); err != nil {
			m.stopMigrationTask(ctx, err)
			return err
		}
		return nil
	}
	execute := func(o PlannedObject) error {
		if e := p.dependencyError(o.Object, map[ObjectRef]bool{}); e != nil {
			stage := "dependency"
			if p.blocked[o.Object] != nil {
				stage = "preflight"
				if o.Object.Kind == "view" && !p.catalogViews[o.Object.Name] {
					stage = "metadata"
				}
			}
			return failObject(o, stage, e)
		}
		if !p.CanStart || p.globalError {
			return failObject(o, "preflight", fmt.Errorf("全局预检失败，未执行"))
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		e := m.executePGOracleObject(ctx, p, o)
		if e != nil {
			p.blocked[o.Object] = e
			if o.Object.Kind == "table" {
				m.blockMigrationTable(o.Object.Name, "execute", e)
			}
			stage := "execute"
			if target.IsViewValidationError(e) {
				stage = "validation"
			}
			if saveErr := failObject(o, stage, e); saveErr != nil {
				return saveErr
			}
			if m.stopMigrationTask(ctx, e) {
				return e
			}
			if o.Object.Kind == "table" {
				if !m.protectRetainedMigrationTables(ctx, validationTables, nil, p.createdTables) {
					return context.Cause(ctx)
				}
				for _, table := range validationTables {
					if err := m.migrationTableError(table); err != nil {
						p.blocked[tableRef(table)] = err
					}
				}
			}
			return nil
		}
		c := pgCategory(&report, o.Object.Kind)
		c.Total++
		c.Success++
		if o.Object.Kind == "table" {
			p.createdTables[o.Object.Name] = true
		}
		if o.Object.Kind == "view" {
			c.Items = append(c.Items, ObjectResult{ObjectKind: "view", Name: o.Object.Name, SourceName: o.Object.Name, TargetName: o.Target, TargetSchema: m.cfg.TargetSchema, DDL: dialect.JoinSQL(o.Statements)})
		}
		m.log.DDLf("%s %s ... OK", o.Object.Kind, o.Target)
		return nil
	}
	for _, o := range objects {
		if o.Stage == "table" {
			if e := execute(o); e != nil {
				return report
			}
		}
	}
	if !p.objectOnly && !p.viewOnly && m.cfg.Content != "schema_only" {
		for _, t := range p.tables {
			ref := tableRef(t.Info.TableName)
			report.Data.Total++
			if e := p.dependencyError(ref, map[ObjectRef]bool{}); e != nil || p.globalError {
				if e == nil {
					e = fmt.Errorf("全局预检失败")
				}
				report.Data.Failed++
				report.Data.Items = append(report.Data.Items, ObjectResult{Name: ref.Name, Table: ref.Name, Stage: "dependency", Error: "表未就绪，未复制数据: " + e.Error()})
				continue
			}
			if err := ctx.Err(); err != nil {
				report.Data.Failed++
				report.Data.Items = append(report.Data.Items, ObjectResult{Name: ref.Name, Stage: "task", Error: err.Error()})
				return report
			}
			baseline, e := m.writer.CountRows(ctx, m.objName(ref.Name))
			if e != nil {
				report.addDataIssue(ref.Name, m.objName(ref.Name), rowCountError, countReadError(nil, e).Error())
				report.Data.Failed++
				report.Data.Items = append(report.Data.Items, ObjectResult{Name: ref.Name, Error: e.Error()})
				p.blocked[ref] = e
				m.blockMigrationTable(ref.Name, "data", e)
				if m.stopMigrationTask(ctx, e) {
					return report
				}
				continue
			}
			copied := int64(0)
			page := 1
			failedPage := 0
			err := m.eachPGOraclePage(ctx, t, func(cols []string, types []string, rows [][]any) error {
				if e := m.writer.CopyData(ctx, m.objName(ref.Name), mapNames(cols, m.objName), types, rows); e != nil {
					failedPage = page
					return fmt.Errorf("第 %d 页失败，已提交 %d 行: %w", page, copied, e)
				}
				copied += int64(len(rows))
				page++
				m.log.Dataf("%s 已提交 %d 行", ref.Name, copied)
				return nil
			})
			if err == nil {
				src, se := m.reader.CountRows(ctx, ref.Name)
				dst, de := m.writer.CountRows(ctx, m.objName(ref.Name))
				rc := TableRowCount{Table: m.objName(ref.Name), Src: src, Dst: dst - baseline, Match: se == nil && de == nil && src == copied && dst-baseline == copied}
				detail := ""
				if se != nil {
					err = se
				} else if de != nil {
					err = de
				} else if !rc.Match {
					err = fmt.Errorf("行数校验失败：源=%d 已提交=%d 目标新增=%d", src, copied, dst-baseline)
					detail = err.Error()
				}
				report.addRowCount(ref.Name, rc.Table, rc, countReadError(se, de), detail)
			} else {
				report.addDataIssue(ref.Name, m.objName(ref.Name), copyIssueKind(err), err.Error())
			}
			if err != nil {
				report.Data.Failed++
				report.Data.Items = append(report.Data.Items, ObjectResult{Name: ref.Name, Table: ref.Name, TargetName: m.objName(ref.Name), Stage: "execute", CommittedRows: copied, FailedPage: failedPage, Error: fmt.Sprintf("已提交 %d 行；%v", copied, err)})
				p.blocked[ref] = err
				m.blockMigrationTable(ref.Name, "data", err)
				m.log.Errorf("表 %s 数据失败，保留此前提交的 %d 行: %v", ref.Name, copied, err)
				if m.stopMigrationTask(ctx, err) {
					return report
				}
				continue
			}
			report.Data.Success++
			m.markDataValidationCopied(ref.Name)
		}
	}
	for _, o := range objects {
		if o.Stage != "table" {
			if e := execute(o); e != nil {
				return report
			}
		}
	}
	m.logMigrationCompletion("PG → Oracle 迁移结束：表 %d，数据 %d", report.Tables.Success, report.Data.Success)
	return report
}

func (m *Migrator) eachPGOraclePage(ctx context.Context, t source.PGTable, fn func([]string, []string, [][]any) error) error {
	if err := m.pgOracleValidationCopyReady(t.Info.TableName); err != nil {
		return dataCheckError{verificationError, err}
	}
	limit := m.cfg.PageSize
	if limit <= 0 {
		limit = 2000
	}
	if limit > 20000 {
		limit = 20000
	}
	consume := func(cols, _ []string, rows [][]any) error {
		if len(rows) == 0 {
			return nil
		}
		if len(cols) != len(t.Info.Columns) {
			return fmt.Errorf("表 %s 列数量在预检后改变", t.Info.TableName)
		}
		types := make([]string, len(cols))
		for i, c := range t.Info.Columns {
			if cols[i] != c.Name {
				return fmt.Errorf("表 %s 列顺序在预检后改变", t.Info.TableName)
			}
			types[i] = c.DataType
		}
		for _, row := range rows {
			if len(row) != len(cols) {
				return fmt.Errorf("表 %s 数据行宽与列元数据不匹配", t.Info.TableName)
			}
		}
		return fn(cols, types, rows)
	}
	if s := m.partitionState; s != nil {
		if p := s.tables[t.Info.TableName]; p != nil && p.Plan != nil {
			if e := m.partitionCopyReady(t.Info.TableName); e != nil {
				return dataCheckError{verificationError, e}
			}
			if e := m.recheckPartitionSource(ctx, t.Info.TableName); e != nil {
				return dataCheckError{verificationError, e}
			}
			inspector, ok := m.writer.(target.PartitionInspector)
			if !ok {
				return dataCheckError{verificationError, fmt.Errorf("目标缺少分区核验能力")}
			}
			actual, e := inspector.InspectPartitionTable(ctx, p.TargetTable)
			if e != nil {
				return dataCheckError{verificationError, e}
			}
			if e = partition.Equivalent(p.Plan.Target, actual); e != nil {
				return dataCheckError{verificationError, e}
			}
			e = m.reader.(source.PartitionReader).StreamPartitionRows(ctx, t.Info.TableName, limit, consume)
			if e == nil {
				p.dataExecuted = true
			}
			return e
		}
	}
	for offset := int64(0); ; offset += int64(limit) {
		if e := ctx.Err(); e != nil {
			return e
		}
		cols, types, rows, e := m.reader.ReadPage(ctx, t.Info.TableName, pgPrimary(t), offset, int64(limit))
		if e != nil {
			return e
		}
		if len(rows) == 0 {
			return nil
		}
		if e = consume(cols, types, rows); e != nil {
			return e
		}
		if len(rows) < limit {
			return nil
		}
	}
}

func (m *Migrator) executePGOracleObject(ctx context.Context, p *PGOraclePreflight, o PlannedObject) error {
	executor, ok := m.writer.(pgOracleExecutor)
	if !ok {
		return fmt.Errorf("Writer 缺少 PG Oracle 冻结计划执行能力")
	}
	obj := m.pgOracleRepairObject(p, o)
	return m.captureRepairCall(ctx, obj, o.Statements, func(c context.Context) error {
		if o.Object.Kind == "table" {
			if e := m.recheckPartitionSource(c, o.Object.Name); e != nil {
				return e
			}
			if e := m.createPartitionTable(c, o.Object.Name, o.Statements); e != nil {
				return e
			}
			if s := m.partitionState; s != nil {
				if t := s.tables[o.Object.Name]; t != nil {
					t.schemaExecuted = true
				}
			}
			return nil
		}
		if seq := p.sequences[o.Object]; seq != nil && seq.Identity != nil {
			w, ok := m.writer.(pgOracleRecoveryWriter)
			if !ok {
				return fmt.Errorf("目标缺少工具序列核验能力")
			}
			if e := w.InspectPGOracleSequence(c, *seq); e != nil {
				return e
			}
			if e := w.ValidatePGOracleSequenceBindings(c, *seq, false); e != nil {
				return e
			}
		}
		if e := executor.ExecutePGOracleStatements(c, o.Object.Kind, o.Target, o.Statements); e != nil {
			return e
		}
		if seq := p.sequences[o.Object]; seq != nil {
			identity, e := executor.PGOracleSequenceIdentity(c, *seq)
			if e != nil {
				return e
			}
			seq.Identity = identity
			if m.repairs != nil {
				m.repairs.mu.Lock()
				defer m.repairs.mu.Unlock()
				for i := range m.repairs.objects {
					saved := m.repairs.objects[i].PGOracleSequence
					if saved != nil && saved.Schema == seq.Schema && saved.Name == seq.Name {
						m.repairs.objects[i].PGOracleSequence = seq
						if e := m.flushRepairObjectLocked(m.repairs.objects[i]); e != nil {
							return e
						}
					}
				}
			}
		}
		return nil
	})
}

func (m *Migrator) pgOracleRepairObject(p *PGOraclePreflight, o PlannedObject) repair.Object {
	table := m.objName(o.Object.Table)
	sourceTable := o.Object.Table
	if o.Object.Kind == "table" {
		table = o.Target
		sourceTable = o.Object.Name
	}
	obj := repair.Object{ID: repairID(o.Object.Kind, table, o.Target), Kind: o.Object.Kind, Name: o.Object.Name, SourceName: o.Object.Name, SourceTable: sourceTable, Table: table, TargetName: o.Target, Schema: m.cfg.TargetSchema, DDL: dialect.JoinSQL(o.Statements), SourceDDL: p.sourceDDL[o.Object], Units: unitsFor(o.Statements), Dependencies: []string{}, Status: "pending", DataStatus: "not_required", PGOracleSequence: p.sequences[o.Object]}
	if o.Object.Kind == "trigger" {
		for _, dep := range p.dependencies[o.Object] {
			if seq := p.sequences[dep]; seq != nil {
				obj.PGOracleSequence = seq
			}
		}
	}
	for _, dep := range p.dependencies[o.Object] {
		dt := m.objName(dep.Table)
		dn := m.objName(dep.Name)
		if dep.Kind == "table" {
			dt = dn
		}
		for _, planned := range p.Objects {
			if planned.Object == dep {
				dn = planned.Target
			}
		}
		obj.Dependencies = append(obj.Dependencies, repairID(dep.Kind, dt, dn))
	}
	if o.Object.Kind == "table" {
		obj.ColumnMapping = map[string]string{}
		for _, t := range p.tables {
			if t.Info.TableName == sourceTable {
				obj.SourceColumns = slices.Clone(t.Info.Columns)
				obj.Columns = slices.Clone(t.Info.Columns)
				for i := range obj.Columns {
					obj.ColumnMapping[obj.Columns[i].Name] = m.objName(obj.Columns[i].Name)
					obj.Columns[i].Name = m.objName(obj.Columns[i].Name)
				}
			}
		}
		if !p.objectOnly && !p.viewOnly && m.cfg.Content != "schema_only" {
			obj.DataStatus = "pending"
		}
		if s := m.partitionState; s != nil {
			if t := s.tables[sourceTable]; t != nil {
				obj.PartitionPlan = t.Plan
			}
		}
	}
	// PG-specific object SQL is immutable unless edited through the ordinary
	// scoped DDL editor; do not attach legacy MySQL Oracle regeneration metadata.
	if e := p.dependencyError(o.Object, map[ObjectRef]bool{}); e != nil {
		obj.Status = "blocked"
		obj.Error = e.Error()
		obj.RequiresReview = true
		obj.DiagnosticOnly = len(o.Statements) == 0 && (o.Object.Kind != "view" || !p.catalogViews[o.Object.Name])
	}
	return obj
}

func (m *Migrator) beginPGOracleRepair(p *PGOraclePreflight) error {
	m.repairs = &repairRecorder{}
	if m.cfg.RepairObjectSink != nil && m.cfg.RepairSink == nil {
		err := migrationTaskFailure{fmt.Errorf("单对象修复记录保存需要配置完整清单保存回调")}
		m.repairs.sinkErr = err
		return err
	}
	for _, o := range p.Objects {
		m.repairs.appendObject(m.pgOracleRepairObject(p, o))
	}
	if p.dataOnly || p.objectOnly {
		for _, t := range p.tables {
			ref := tableRef(t.Info.TableName)
			o := m.pgOracleRepairObject(p, PlannedObject{Object: ref, Target: m.objName(ref.Name)})
			if o.Status == "pending" {
				o.Status = "succeeded"
			}
			m.repairs.appendObject(o)
		}
	}
	m.repairs.mu.Lock()
	defer m.repairs.mu.Unlock()
	return m.flushRepairsLocked()
}

func (m *Migrator) markPGOracleObjectFailure(p *PGOraclePreflight, o PlannedObject, stage string, err error) error {
	if m.repairs == nil {
		return nil
	}
	obj := m.pgOracleRepairObject(p, o)
	m.repairs.mu.Lock()
	defer m.repairs.mu.Unlock()
	i := m.repairs.objectPosition(obj.ID)
	if i < 0 {
		i = m.repairs.appendObject(obj)
	}
	x := &m.repairs.objects[i]
	x.Error = err.Error()
	x.RequiresReview = true
	if stage == "preflight" && o.Object.Kind == "view" && !x.DiagnosticOnly && len(x.Units) == 0 {
		x.Status = "failed"
	} else if stage == "dependency" || stage == "preflight" {
		x.Status = "blocked"
		for j := range x.Units {
			if x.Units[j].Status == "pending" {
				x.Units[j].Status = "blocked"
				x.Units[j].Error = err.Error()
			}
		}
	} else if x.Status != "unknown" {
		x.Status = "failed"
	}
	return m.flushRepairObjectLocked(*x)
}
