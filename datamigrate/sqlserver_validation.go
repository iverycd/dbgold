package datamigrate

import (
	"context"
	"fmt"

	"dbgold/datamigrate/validation"
)

func (m *Migrator) freezeSQLServerAppendContent(ctx context.Context, frozen *frozenValidationTable) {
	if !m.isSQLServer() || m.cfg.Content != "data_only" || m.hasPartitionPlan(frozen.name) {
		return // Partition preparation already freezes both root and leaf baselines.
	}
	dst, ok := m.writer.(validation.Scanner)
	if !ok {
		frozen.err = fmt.Errorf("目标不支持追加前原值基线读取")
		return
	}
	plan, err := m.dataValidationPlan(ctx, dst, *frozen)
	if err == nil {
		frozen.appendBaseline, err = validation.CaptureAppendBaseline(ctx, dst, plan)
	}
	if err != nil {
		frozen.err = fmt.Errorf("冻结 SQL Server 追加前内容基线: %w", err)
	}
}

func (m *Migrator) sqlServerAppendValidationError(table string, before int64) error {
	if !m.isSQLServer() || !m.dataValidationEnabled() || m.cfg.Content != "data_only" {
		return nil
	}
	if err := m.partitionCopyReady(table); err != nil {
		return err
	}
	for _, frozen := range m.validationTables {
		if frozen.name != table {
			continue
		}
		if frozen.err != nil {
			return fmt.Errorf("追加前原值基线冻结失败，未写入该表: %w", frozen.err)
		}
		var baselineRows int64
		if frozen.appendBaseline != nil {
			baselineRows = frozen.appendBaseline.Rows()
		} else if !m.hasPartitionPlan(table) || !allPartitionCountsZero(m.partitionState.tables[table].Baseline) {
			break
		}
		if baselineRows != before {
			return fmt.Errorf("目标表 %s 在冻结内容基线后行数已变化，冻结=%d 当前=%d，未写入该表", table, baselineRows, before)
		}
		return nil
	}
	return fmt.Errorf("追加前原值基线缺失，未写入该表")
}
