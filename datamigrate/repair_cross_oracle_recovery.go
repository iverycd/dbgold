package datamigrate

import (
	"context"
	"fmt"
	"slices"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

type crossOracleRecoveryWriter interface {
	SetSourceType(string)
	ConfigureCrossOracle(map[string]*source.TableDDLInfo)
	InspectCrossOracleSequence(context.Context, source.SequenceInfo, bool) error
	ValidatePGOracleDataTable(context.Context, string, []source.ColumnInfo, map[string]string) error
}

func prepareCrossOracleRecovery(ctx context.Context, r source.Reader, w target.Writer, cfg Config, p *repair.RecoveryPreview, related []repair.Object) error {
	writer, ok := w.(crossOracleRecoveryWriter)
	if !ok {
		return fmt.Errorf("Oracle Writer 缺少跨库补迁核验能力")
	}
	writer.SetSourceType(r.DBType())
	sequences, err := r.GetSequences(ctx)
	if err != nil {
		return err
	}
	evidence := CrossOracleEvidence(ctx)
	for _, o := range related {
		if o.TrustedCrossOracleSequence() && o.Sequence.Native.OracleObjectID > 0 {
			evidence = append(evidence, *o.Sequence)
		}
	}
	ctx = WithCrossOracleEvidence(ctx, evidence)
	m := NewMigrator(r, w, &Job{}, cfg)
	selected := map[string]bool{}
	for _, s := range sequences {
		if s.Native != nil {
			for _, b := range s.Native.Bindings {
				selected[b.TableName] = true
			}
		}
	}
	// A removed source generator/binding must not make a previously generated
	// column look like an ordinary column during a later recovery preview.
	for _, saved := range evidence {
		if saved.Native == nil {
			continue
		}
		bound := false
		for _, b := range saved.Native.Bindings {
			bound = bound || b.TableName == m.objName(p.SourceTable)
		}
		if !bound {
			continue
		}
		matched := false
		for _, raw := range sequences {
			if raw.Native != nil && raw.Native.Schema == saved.Native.Schema && raw.Native.Name == saved.Native.SourceName {
				matched = CrossOracleDefinition(m.crossOracleSequence(raw, selected)) == CrossOracleDefinition(saved)
				break
			}
		}
		if !matched {
			return fmt.Errorf("序列 %s 的冻结源定义或绑定已不存在或发生变化", saved.Native.Name)
		}
	}
	allowed := map[string]string{}
	for _, raw := range sequences {
		if raw.Native == nil {
			continue
		}
		bound := false
		for _, b := range raw.Native.Bindings {
			bound = bound || b.TableName == p.SourceTable && b.Schema == raw.Native.Schema
		}
		if !bound {
			continue
		}
		s := bindCrossOracleEvidence(ctx, m.crossOracleSequence(raw, selected))
		if s.Unsupported != "" {
			return fmt.Errorf("序列 %s: %s", s.Native.Name, s.Unsupported)
		}
		for _, b := range s.Native.Bindings {
			if b.TableName == m.objName(p.SourceTable) && (b.TableName != p.TargetTable || p.ColumnMapping[b.SourceColumnName] != b.ColumnName) {
				return fmt.Errorf("序列绑定列重命名需要先重新生成工具对象")
			}
		}
		if s.Native.OracleObjectID > 0 {
			if err := writer.InspectCrossOracleSequence(ctx, s, true); err != nil {
				return err
			}
			ss := w.Dialect().SequenceStatements(cfg.TargetSchema, s, "")
			for i, b := range s.Native.Bindings {
				if b.TableName == p.TargetTable {
					allowed[dialect.CrossOracleTriggerName(s, b)] = ss[i+1].SQL
				}
			}
		} else {
			pending := false
			for _, o := range related {
				if o.TrustedCrossOracleSequence() && o.Status != "succeeded" && CrossOracleDefinition(*o.Sequence) == CrossOracleDefinition(s) {
					pending = true
				}
			}
			if !pending {
				return fmt.Errorf("序列 %s 缺少身份或可恢复的冻结计划", s.Native.Name)
			}
		}
		p.CrossOracleSequences = append(p.CrossOracleSequences, s)
	}
	cols := slices.Clone(p.SourceColumns)
	for i := range cols {
		cols[i].Name = p.ColumnMapping[cols[i].Name]
	}
	writer.ConfigureCrossOracle(map[string]*source.TableDDLInfo{p.TargetTable: {TableName: p.TargetTable, Columns: cols}})
	return writer.ValidatePGOracleDataTable(ctx, p.TargetTable, cols, allowed)
}

type crossStreamReader interface {
	StreamCrossRows(context.Context, string, int, func([]string, []string, [][]any) error) error
}

func recoverCrossRows(ctx context.Context, r source.Reader, w target.Writer, cfg Config, p repair.RecoveryPreview, truncate bool, progress func(repair.RecoveryResult) error, ops recoveryOps, result *repair.RecoveryResult) error {
	reader, ok := r.(crossStreamReader)
	if !ok {
		return fmt.Errorf("跨库补迁缺少固定游标读取能力")
	}
	started := false
	begin := func() error {
		if started {
			return nil
		}
		if progress != nil {
			if e := progress(*result); e != nil {
				return e
			}
		}
		if truncate {
			if e := ops.truncate(ctx, w, p.TargetTable); e != nil {
				return e
			}
			result.TargetRows = 0
		}
		started = true
		return nil
	}
	size := cfg.PageSize
	if size <= 0 || size > 20000 {
		size = 2000
	}
	err := reader.StreamCrossRows(ctx, p.SourceTable, size, func(cols, types []string, rows [][]any) error {
		if e := checkRecoveryPageColumns(p, cols); e != nil {
			return e
		}
		if e := begin(); e != nil {
			return e
		}
		mapped := make([]string, len(cols))
		for i, c := range cols {
			mapped[i] = p.ColumnMapping[c]
		}
		if e := w.CopyData(ctx, p.TargetTable, mapped, types, rows); e != nil {
			result.Status = "unknown"
			return fmt.Errorf("写入失败，已确认提交 %d 行；禁止追加重试: %w", result.CommittedRows, e)
		}
		result.CommittedRows += int64(len(rows))
		if progress != nil {
			return progress(*result)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !started {
		if p.SourceRows != 0 {
			return fmt.Errorf("源游标为空但冻结行数非零")
		}
		return begin()
	}
	return nil
}
