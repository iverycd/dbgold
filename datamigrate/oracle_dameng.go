package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/oracledameng"
	"dbgold/datamigrate/repair"
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
func (m *Migrator) preflightOracleDameng(ctx context.Context, tables []string, report *MigrationReport) bool {
	if m.cfg.objectMode() {
		return true
	}
	ready := true
	issue := func(table string, err error) {
		ready = false
		report.Tables.Failed++
		result := objectResultForError(table, "", err)
		result.Stage = "preflight"
		report.Tables.Items = append(report.Tables.Items, result)
		m.log.Errorf("Oracle → 达梦表预检 [%s]: %v", table, err)
	}
	if m.cfg.TargetSchema != "" {
		if err := tsqldameng.ValidName(m.cfg.TargetSchema); err != nil {
			issue("目标 schema", err)
		}
	}
	infos := make([]*source.TableDDLInfo, 0, len(tables))
	sourceInfos := make([]*source.TableDDLInfo, 0, len(tables))
	opt := dialect.TypeOpt{CharInLength: m.cfg.CharInLength, UseNvarchar2: m.cfg.UseNvarchar2}
	for _, table := range tables {
		if err := ctx.Err(); err != nil {
			issue(table, err)
			return false
		}
		p := m.dmPlans[table]
		if p == nil {
			issue(table, fmt.Errorf("缺少表结构"))
			continue
		}
		infos = append(infos, p.target)
		sourceInfos = append(sourceInfos, p.source)
	}
	if !ready {
		return false
	}
	if checker, ok := m.reader.(interface {
		ValidateOracleDamengSource(context.Context, []*source.TableDDLInfo) error
	}); ok {
		if err := checker.ValidateOracleDamengSource(ctx, sourceInfos); err != nil {
			issue("源端类型能力检查", err)
			return false
		}
	}
	if err := prepareOracleDamengWriter(ctx, m.writer, infos, opt); err != nil {
		issue("目标能力检查", err)
		return false
	}
	if m.cfg.Content != "schema_only" {
		if err := prepareOracleDamengLoad(ctx, m.writer); err != nil {
			issue("目标数据写入能力检查", err)
			return false
		}
	}
	for _, table := range tables {
		p := m.dmPlans[table]
		if m.cfg.Content != "data_only" {
			if err := validateOracleDamengDefaults(ctx, m.writer, []*source.TableDDLInfo{p.target}, opt); err != nil {
				issue(table, err)
				continue
			}
			if _, _, err := m.buildCreateTableDDL(ctx, table); err != nil {
				issue(table, err)
			}
			continue
		}
		// Data-only does not inspect defaults, indexes, or unrelated objects.
		valid := true
		for _, c := range p.source.Columns {
			if err := typemap.ValidateOracleDamengColumn(c); err != nil {
				issue(table, err)
				valid = false
			}
		}
		if valid {
			if validator, ok := m.writer.(interface {
				ValidateOracleDamengExisting(context.Context, *source.TableDDLInfo, dialect.TypeOpt) error
			}); ok {
				if err := validator.ValidateOracleDamengExisting(ctx, p.target, opt); err != nil {
					issue(table, err)
				}
			} else {
				issue(table, fmt.Errorf("目标写入器不支持仅数据模式的列兼容检查"))
			}
			if validator, ok := m.writer.(interface {
				ValidateOracleDamengLoadTriggers(context.Context, *source.TableDDLInfo) error
			}); ok {
				if err := validator.ValidateOracleDamengLoadTriggers(ctx, p.target); err != nil {
					issue(table, err)
				}
			}
		}
	}
	return ready
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

func prepareOracleDamengLoad(ctx context.Context, w any) error {
	if checker, ok := w.(interface{ PrepareOracleDamengLoad(context.Context) error }); ok {
		return checker.PrepareOracleDamengLoad(ctx)
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
		return source.FilterNativeSequences(values, selected)
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
				v.Unsupported = err.Error()
			} else {
				v = prepared
			}
		}
	}
	return v
}

func (m *Migrator) calibrateOracleDameng(ctx context.Context, report *MigrationReport, tables []string) {
	values, err := m.reader.GetSequences(ctx)
	if err != nil {
		recordMetadataFailure(&report.Sequences, err)
		return
	}
	selected := map[string]bool{}
	for _, table := range tables {
		selected[table] = true
	}
	for _, seq := range m.selectedSequences(values, selected) {
		seq = m.prepareSourceSequence(ctx, seq)
		seq = m.targetSequence(seq)
		name := dialect.SequenceName(m.writer.Dialect(), seq)
		report.Sequences.Total++
		if seq.Unsupported != "" {
			err = fmt.Errorf("%s", seq.Unsupported)
		} else if calibrator, ok := m.writer.(interface {
			CalibrateOracleDameng(context.Context, source.SequenceInfo) error
		}); ok {
			err = m.captureRepairCall(ctx, repair.Object{Kind: "sequence", Name: name, Table: seq.TableName, TargetName: name, Sequence: &seq}, nil, func(ctx context.Context) error { return calibrator.CalibrateOracleDameng(ctx, seq) })
		} else {
			err = fmt.Errorf("目标写入器不支持已有 Oracle 关联序列续号")
		}
		if err != nil {
			report.Sequences.Failed++
			report.Sequences.Items = append(report.Sequences.Items, objectResultForError(name, "", err))
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
