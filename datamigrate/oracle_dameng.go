package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/oracledameng"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/tsqldameng"
	"dbgold/datamigrate/typemap"
)

func (m *Migrator) isOracleDameng() bool {
	return m.reader.DBType() == "oracle" && m.writer.DBType() == "dameng"
}

func (m *Migrator) oracleDamengSQLOptions() oracledameng.Options {
	opt := oracledameng.Options{CharInLength: m.cfg.CharInLength, UseNvarchar2: m.cfg.UseNvarchar2}
	if d, ok := m.writer.Dialect().(*dialect.DaMengDialect); ok {
		opt.UseDate = d.OracleDamengUsesDate()
	}
	return opt
}

// This is deliberately limited to the selected tables and observational target
// checks. Object conversion errors continue through the normal category reports.
func (m *Migrator) preflightOracleDameng(ctx context.Context, tables []string, report *MigrationReport, failed map[string]bool) bool {
	issue := func(table string, err error) {
		failed[table], m.dmBlocked[table] = true, true
		report.Tables.Failed++
		result := objectResultForError(table, "", err)
		result.Stage = "preflight"
		report.Tables.Items = append(report.Tables.Items, result)
		m.log.Errorf("Oracle → 达梦表预检 [%s]，跳过该表及依赖对象: %v", table, err)
	}
	if m.cfg.TargetSchema != "" {
		if err := tsqldameng.ValidName(m.cfg.TargetSchema); err != nil {
			m.cancelOracleDamengTask(ctx, fmt.Errorf("目标 schema 无效: %w", err))
			return false
		}
	}
	if m.cfg.objectMode() {
		return true
	}
	opt := dialect.TypeOpt{CharInLength: m.cfg.CharInLength, UseNvarchar2: m.cfg.UseNvarchar2}
	for _, table := range tables {
		if ctx.Err() != nil {
			return false
		}
		if failed[table] {
			continue
		}
		p := m.dmPlans[table]
		if p == nil {
			issue(table, fmt.Errorf("缺少表结构"))
			continue
		}
		if err := m.preflightOracleDamengTable(ctx, table, p, opt); err != nil {
			if m.stopOracleDamengTask(ctx, err) {
				return false
			}
			issue(table, err)
			continue
		}
	}
	return true
}

func (m *Migrator) preflightOracleDamengTable(ctx context.Context, table string, p *damengTablePlan, opt dialect.TypeOpt) error {
	if checker, ok := m.reader.(interface {
		ValidateOracleDamengSource(context.Context, []*source.TableDDLInfo) error
	}); ok {
		if err := checker.ValidateOracleDamengSource(ctx, []*source.TableDDLInfo{p.source}); err != nil {
			return err
		}
	}
	if err := prepareOracleDamengWriter(ctx, m.writer, []*source.TableDDLInfo{p.target}, opt); err != nil {
		return err
	}
	if m.cfg.Content != "data_only" {
		if err := validateOracleDamengDefaults(ctx, m.writer, []*source.TableDDLInfo{p.target}, opt); err != nil {
			return err
		}
		_, _, err := m.buildCreateTableDDL(ctx, table)
		return err
	}
	// Data-only does not inspect defaults, indexes, or unrelated objects.
	for _, c := range p.source.Columns {
		if err := typemap.ValidateOracleDamengColumn(c); err != nil {
			return err
		}
	}
	validator, ok := m.writer.(interface {
		ValidateOracleDamengExisting(context.Context, *source.TableDDLInfo, dialect.TypeOpt) error
	})
	if !ok {
		return fmt.Errorf("目标写入器不支持仅数据模式的列兼容检查")
	}
	if err := validator.ValidateOracleDamengExisting(ctx, p.target, opt); err != nil {
		return err
	}
	if checker, ok := m.writer.(interface {
		ValidateOracleDamengLoadTriggers(context.Context, *source.TableDDLInfo) error
	}); ok {
		return checker.ValidateOracleDamengLoadTriggers(ctx, p.target)
	}
	return nil
}

// Preparation is read-only and happens before rendering DDL as well as before
// execution, so reports and repair units share the same capability-based types.
func prepareOracleDamengWriter(ctx context.Context, w interface{ Dialect() dialect.Dialect }, infos []*source.TableDDLInfo, opt dialect.TypeOpt) error {
	if checker, ok := w.(interface {
		PrepareOracleDameng(context.Context, []*source.TableDDLInfo, dialect.TypeOpt) error
	}); ok {
		if err := checker.PrepareOracleDameng(ctx, infos, opt); err != nil {
			return err
		}
	} else if checker, ok := w.(interface {
		ValidateOracleDamengCapabilities(context.Context, []*source.TableDDLInfo) error
	}); ok {
		if err := checker.ValidateOracleDamengCapabilities(ctx, infos); err != nil {
			return err
		}
	}
	if profile, ok := w.(interface{ OracleDamengUsesDate() bool }); ok {
		if d, ok := w.Dialect().(*dialect.DaMengDialect); ok {
			d.SetOracleDamengDate(profile.OracleDamengUsesDate())
		}
	}
	return nil
}

func validateOracleDamengDefaults(ctx context.Context, w any, infos []*source.TableDDLInfo, opt dialect.TypeOpt) error {
	if checker, ok := w.(interface {
		ValidateOracleDamengDefaults(context.Context, []*source.TableDDLInfo, dialect.TypeOpt) error
	}); ok {
		return checker.ValidateOracleDamengDefaults(ctx, infos, opt)
	}
	return nil
}

// A native sequence can serve multiple tables. Scope its bindings, not its
// representative TableName, then emit one object per original sequence.
func (m *Migrator) selectedSequences(values []source.SequenceInfo, selected map[string]bool) []source.SequenceInfo {
	if m.isOracleDameng() {
		all := source.FilterNativeSequences(values, selected)
		healthy := map[string]bool{}
		for table, included := range selected {
			if included && !m.dmBlocked[table] {
				healthy[table] = true
			}
		}
		out := source.FilterNativeSequences(all, healthy)
		for i := range out {
			if out[i].Native == nil {
				continue
			}
			for _, original := range all {
				if original.Native != nil && original.Native.Name == out[i].Native.Name {
					// The representative table identifies the durable repair object;
					// only Bindings determine which tables are read or modified.
					out[i].TableName, out[i].ColumnName = original.TableName, original.ColumnName
					break
				}
			}
		}
		return out
	}
	out := make([]source.SequenceInfo, 0, len(values))
	for _, v := range values {
		if selected[v.TableName] {
			out = append(out, v)
		}
	}
	return out
}

func (m *Migrator) targetSequence(v source.SequenceInfo) source.SequenceInfo {
	v.TableName, v.ColumnName = m.objName(v.TableName), m.targetColumn(v.TableName, v.ColumnName)
	if v.Native != nil && m.isOracleDameng() {
		n := *v.Native
		n.Name = m.objName(n.Name)
		n.Bindings = slices.Clone(n.Bindings)
		for i, b := range n.Bindings {
			n.Bindings[i].SourceColumnName = b.ColumnName
			n.Bindings[i].TableName = m.objName(b.TableName)
			n.Bindings[i].ColumnName = m.targetColumn(b.TableName, b.ColumnName)
			n.Bindings[i].TriggerName = m.objName(b.TriggerName)
		}
		v.Native = &n
		if v.Unsupported == "" {
			prepared, err := dialect.NormalizeOracleDamengSequence(v)
			if err != nil {
				v.Unsupported, v.FailureStage = err.Error(), "preflight"
			} else {
				m.logOracleDamengSequenceBounds(v, prepared)
				v = prepared
			}
		}
	}
	return v
}

func (m *Migrator) prepareSourceSequence(ctx context.Context, v source.SequenceInfo) source.SequenceInfo {
	if m.isOracleDameng() && v.Native != nil && v.Unsupported == "" {
		if preparer, ok := m.reader.(interface {
			PrepareNativeSequence(context.Context, source.SequenceInfo) (source.SequenceInfo, error)
		}); ok {
			prepared, err := preparer.PrepareNativeSequence(ctx, v)
			if err != nil {
				m.stopOracleDamengTask(ctx, err)
				v.Unsupported = err.Error()
				v.FailureStage = "preflight"
			} else {
				v = prepared
			}
		}
	}
	return v
}

func (m *Migrator) reportBlockedOracleSequence(report *MigrationReport, v source.SequenceInfo) {
	if !m.isOracleDameng() {
		return
	}
	blocked := []string{}
	if v.Native != nil {
		for _, binding := range v.Native.Bindings {
			if m.dmBlocked[binding.TableName] {
				blocked = append(blocked, binding.TableName+"."+binding.ColumnName)
			}
		}
	} else if m.dmBlocked[v.TableName] {
		blocked = append(blocked, v.TableName+"."+v.ColumnName)
	}
	if len(blocked) == 0 {
		return
	}
	target := m.targetSequence(v)
	name := dialect.SequenceName(m.writer.Dialect(), target)
	result := objectResultForError(name, "", fmt.Errorf("关联表迁移失败，已跳过序列绑定 %s；健康表绑定继续处理", strings.Join(blocked, ", ")))
	result.Stage, result.Table, result.TargetTable = "dependency", v.TableName, target.TableName
	result.TargetName, result.TargetSchema = name, m.cfg.TargetSchema
	report.Sequences.Total++
	report.Sequences.Failed++
	report.Sequences.Items = append(report.Sequences.Items, result)
}

func (m *Migrator) calibrateOracleDameng(ctx context.Context, report *MigrationReport, tables []string) {
	values, err := m.reader.GetSequences(ctx)
	if err != nil {
		if m.stopOracleDamengTask(ctx, err) {
			return
		}
		recordMetadataFailure(&report.Sequences, err)
		return
	}
	selected := map[string]bool{}
	for _, table := range tables {
		selected[table] = true
	}
	for _, seq := range source.FilterNativeSequences(values, selected) {
		m.reportBlockedOracleSequence(report, seq)
	}
	for _, seq := range m.selectedSequences(values, selected) {
		original := seq
		seq = m.prepareSourceSequence(ctx, seq)
		if ctx.Err() != nil {
			return
		}
		seq = m.targetSequence(seq)
		report.Sequences.Total++
		if seq.Unsupported != "" {
			err = fmt.Errorf("%s", seq.Unsupported)
		} else if calibrator, ok := m.writer.(interface {
			CalibrateOracleDameng(context.Context, source.SequenceInfo) error
		}); ok {
			err = m.captureSequenceCall(ctx, seq, nil, func(ctx context.Context) error { return calibrator.CalibrateOracleDameng(ctx, seq) })
		} else {
			err = fmt.Errorf("目标写入器不支持已有 Oracle 关联序列续号")
		}
		if err != nil {
			if m.stopOracleDamengTask(ctx, err) {
				return
			}
			report.Sequences.Failed++
			report.Sequences.Items = append(report.Sequences.Items, m.sequenceFailure(original, seq, "", err))
		} else {
			report.Sequences.Success++
		}
	}
}

func nativeSequenceDependsOn(v source.SequenceInfo, targetTable string) bool {
	if v.Native == nil {
		return v.TableName == targetTable
	}
	for _, b := range v.Native.Bindings {
		if b.TableName == targetTable {
			return true
		}
	}
	return false
}

func oracleDamengUpperNames(src, dst string) bool {
	return dst == "dameng" && (src == "oracle" || src == "sqlserver")
}

func nativeSequenceKey(v source.SequenceInfo) string {
	if v.Native != nil {
		return strings.ToUpper(v.Native.Name)
	}
	return v.TableName + "." + v.ColumnName
}
