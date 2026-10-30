package datamigrate

import (
	"context"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"fmt"
	"slices"
)

func recoverPartitionRows(ctx context.Context, r source.Reader, w target.Writer, cfg Config, p repair.RecoveryPreview, truncate bool, progress func(repair.RecoveryResult) error, ops recoveryOps, res *repair.RecoveryResult) error {
	reader, ok := partitionSourceReader(r).(source.PartitionReader)
	if !ok {
		return fmt.Errorf("源库缺少固定游标分区读取能力")
	}
	started := false
	begin := func() error {
		if started {
			return nil
		}
		if e := verifyPartitionKeySourceChecks(ctx, r, p.SourceTable, p.PartitionPlan); e != nil {
			return e
		}
		if e := target.VerifyPartitionKeyColumns(ctx, w, p.TargetTable, p.PartitionPlan); e != nil {
			return e
		}
		if progress != nil {
			if e := progress(*res); e != nil {
				return e
			}
		}
		if truncate {
			if e := ops.truncate(ctx, w, p.TargetTable); e != nil {
				return e
			}
			if p.PartitionPlan != nil && p.PartitionPlan.Target != nil && p.PartitionPlan.Target.Interval != nil {
				actual, e := w.(target.PartitionInspector).InspectPartitionTable(ctx, p.TargetTable)
				if e != nil {
					return e
				}
				if e = partition.Equivalent(p.PartitionPlan.Target, actual); e != nil {
					return fmt.Errorf("清空后 INTERVAL 分区不完整，先修复结构后再补迁: %w", e)
				}
			}
		}
		started = true
		return nil
	}
	size := cfg.PageSize
	if size <= 0 || size > 20000 {
		size = 2000
	}
	err := reader.StreamPartitionRows(ctx, p.SourceTable, size, func(cols, types []string, rows [][]any) error {
		if e := checkRecoveryPageColumns(p, cols); e != nil {
			return e
		}
		var e error
		if source.IsPGFamily(r.DBType()) && w.DBType() == "mysql" {
			rows, e = convertPGPage(source.PGTable{Info: source.TableDDLInfo{TableName: p.SourceTable, Columns: p.SourceColumns}}, cols, rows)
			if e != nil {
				return e
			}
		}
		if e = begin(); e != nil {
			return e
		}
		mapped := make([]string, len(cols))
		for i, c := range cols {
			mapped[i] = p.ColumnMapping[c]
		}
		if e = w.CopyData(ctx, p.TargetTable, mapped, types, rows); e != nil {
			res.Status = "unknown"
			return fmt.Errorf("分区补迁写入失败，已确认提交 %d 行，禁止直接追加重试: %w", res.CommittedRows, e)
		}
		res.CommittedRows += int64(len(rows))
		if progress != nil {
			return progress(*res)
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
		if err = begin(); err != nil {
			return err
		}
	}
	fresh, err := reader.PartitionMetadata(ctx, p.SourceTable)
	if err != nil {
		return err
	}
	if err = partition.Equivalent(p.PartitionPlan.Source, fresh); err != nil {
		return fmt.Errorf("补迁期间源分区定义漂移: %w", err)
	}
	if err = verifyPartitionKeySourceChecks(ctx, r, p.SourceTable, p.PartitionPlan); err != nil {
		return err
	}
	inspector := w.(target.PartitionInspector)
	actual, err := inspector.InspectPartitionTable(ctx, p.TargetTable)
	if err != nil {
		return err
	}
	if err = partition.Equivalent(p.PartitionPlan.Target, actual); err != nil {
		return err
	}
	if err = target.VerifyPartitionKeyColumns(ctx, w, p.TargetTable, p.PartitionPlan); err != nil {
		return err
	}
	sourceCounts, err := partitionSourceReader(r).(source.PartitionCounter).CountPartitionRows(ctx, p.SourceTable, fresh)
	if err != nil {
		return err
	}
	targetCounts, err := inspector.CountPartitionRows(ctx, p.TargetTable, actual)
	if err != nil {
		return err
	}
	for _, mapping := range p.PartitionPlan.Mapping {
		a, aok := partitionRowCount(sourceCounts, mapping.SourceSchema, mapping.SourceName, 0)
		b, bok := partitionRowCount(targetCounts, mapping.TargetSchema, mapping.TargetName, mapping.TargetOrdinal)
		if !aok || !bok || a != b {
			return fmt.Errorf("补迁叶分区 %s 行数核对失败: 源=%d 目标=%d", mapping.SourceName, a, b)
		}
	}
	return nil
}

type partitionRecoveryValidation struct {
	source, target             validation.Scanner
	sourceLeaves, targetLeaves validation.PartitionScanner
	plan                       validation.Plan
}

// Freeze before preparation/truncation. Recovery may use an explicitly repaired
// column mapping, so bind that exact mapping instead of deriving object casing.
func freezePartitionRecoveryValidation(ctx context.Context, r source.Reader, w target.Writer, cfg Config, p repair.RecoveryPreview) (*partitionRecoveryValidation, error) {
	if !cfg.ValidateData || p.PartitionPlan == nil {
		return nil, nil
	}
	src, sourceOK := validationSource(r)
	dst, targetOK := w.(validation.Scanner)
	sourceLeaves, sourceLeavesOK := partitionSourceReader(r).(validation.PartitionScanner)
	targetLeaves, targetLeavesOK := w.(validation.PartitionScanner)
	if !sourceOK || !targetOK || !sourceLeavesOK || !targetLeavesOK {
		return nil, fmt.Errorf("分区恢复原值校验要求源和目标支持根表及逐叶扫描")
	}
	metadata, err := src.DescribeValidationTable(ctx, p.SourceTable)
	if err != nil {
		return nil, fmt.Errorf("冻结恢复源原值列: %w", err)
	}
	if len(metadata.Columns) == 0 || len(metadata.Columns) != len(p.SourceColumns) {
		return nil, fmt.Errorf("恢复源原值列数量与预览不一致")
	}
	actual, err := dst.DescribeValidationTable(ctx, p.TargetTable)
	if err != nil {
		return nil, fmt.Errorf("冻结恢复目标原值列: %w", err)
	}
	if len(actual.Columns) != len(metadata.Columns) {
		return nil, fmt.Errorf("恢复源目标原值列数量不同")
	}
	byName := make(map[string]validation.Column, len(actual.Columns))
	for _, column := range actual.Columns {
		if _, duplicate := byName[column.Name]; duplicate {
			return nil, fmt.Errorf("恢复目标原值列名重复: %s", column.Name)
		}
		byName[column.Name] = column
	}
	plan := validation.Plan{SourceTable: p.SourceTable, TargetTable: p.TargetTable, SourceColumns: slices.Clone(metadata.Columns), PrimaryKey: slices.Clone(metadata.PrimaryKey)}
	for _, column := range plan.SourceColumns {
		mapped, ok := byName[p.ColumnMapping[column.Name]]
		if !ok {
			return nil, fmt.Errorf("恢复原值列映射不存在或重复: %s", column.Name)
		}
		if column.Kind == "" || column.Kind == validation.Unsupported || mapped.Kind == "" || mapped.Kind == validation.Unsupported {
			return nil, fmt.Errorf("恢复列 %s 缺少无损原值校验器", column.Name)
		}
		plan.TargetColumns = append(plan.TargetColumns, mapped)
		delete(byName, mapped.Name)
	}
	return &partitionRecoveryValidation{src, dst, sourceLeaves, targetLeaves, plan}, nil
}

func (v *partitionRecoveryValidation) validate(ctx context.Context, r source.Reader, w target.Writer, p repair.RecoveryPreview) error {
	if v == nil {
		return nil
	}
	checked := func(result validation.TableResult) error {
		if result.Status == validation.StatusPassed {
			return nil
		}
		return fmt.Errorf("分区恢复原值校验 %s: %s，缺失=%d 多余=%d 内容不同=%d 重复键=%d %s", result.SourceTable, result.Status, result.MissingRows, result.ExtraRows, result.ChangedRows, result.DuplicateKeys, result.Error)
	}
	if err := checked(validation.Validate(ctx, v.source, v.target, v.plan)); err != nil {
		return err
	}
	for _, mapping := range p.PartitionPlan.Mapping {
		sourceLocation, sourceOK := partitionLocation(p.PartitionPlan.Source, mapping.SourceSchema, mapping.SourceName, 0)
		targetLocation, targetOK := partitionLocation(p.PartitionPlan.Target, mapping.TargetSchema, mapping.TargetName, mapping.TargetOrdinal)
		if !sourceOK || !targetOK {
			return fmt.Errorf("恢复原值校验缺少冻结叶分区身份")
		}
		result := validation.Validate(ctx, partitionValueScanner{v.source, v.sourceLeaves, p.SourceTable, sourceLocation}, partitionValueScanner{v.target, v.targetLeaves, p.TargetTable, targetLocation}, v.plan)
		result.SourceTable = p.SourceTable + " PARTITION " + partition.LeafKey(mapping.SourceSchema, mapping.SourceName)
		if err := checked(result); err != nil {
			return err
		}
	}
	fresh, err := partitionSourceReader(r).(source.PartitionReader).PartitionMetadata(ctx, p.SourceTable)
	if err != nil {
		return err
	}
	if err = partition.Equivalent(p.PartitionPlan.Source, fresh); err != nil {
		return fmt.Errorf("恢复原值校验期间源分区定义漂移: %w", err)
	}
	if err = verifyPartitionKeySourceChecks(ctx, r, p.SourceTable, p.PartitionPlan); err != nil {
		return err
	}
	actual, err := w.(target.PartitionInspector).InspectPartitionTable(ctx, p.TargetTable)
	if err != nil {
		return err
	}
	if err = partition.Equivalent(p.PartitionPlan.Target, actual); err != nil {
		return fmt.Errorf("恢复原值校验期间目标分区定义漂移: %w", err)
	}
	if err = target.VerifyPartitionKeyColumns(ctx, w, p.TargetTable, p.PartitionPlan); err != nil {
		return err
	}
	return ctx.Err()
}
