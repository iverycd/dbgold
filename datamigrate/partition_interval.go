package datamigrate

import (
	"context"
	"fmt"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/target"
)

func (m *Migrator) createPartitionTable(ctx context.Context, table string, statements []dialect.Statement) error {
	var plan *partition.Plan
	if m.partitionState != nil {
		if t := m.partitionState.tables[table]; t != nil {
			plan = t.Plan
		}
	}
	if plan != nil && partitionNeedsColumnProof(plan.Source) {
		if err := m.recheckPartitionSource(ctx, table); err != nil {
			return err
		}
	}
	if plan == nil || plan.Interval == nil {
		if err := m.writer.CreateTable(ctx, statements); err != nil {
			return err
		}
		return target.VerifyPartitionKeyColumns(ctx, m.writer, m.objName(table), plan)
	}
	materializer, ok := m.writer.(target.PartitionIntervalMaterializer)
	if !ok {
		return fmt.Errorf("目标缺少 INTERVAL 空叶原生物化执行能力")
	}
	marker := partition.IntervalMaterializationSQL(plan)
	if len(statements) == 0 || statements[len(statements)-1].SQL != marker {
		return fmt.Errorf("INTERVAL 物化执行单元不属于冻结计划")
	}
	if err := m.writer.CreateTable(ctx, statements[:len(statements)-1]); err != nil {
		return err
	}
	return materializer.MaterializePartitionIntervals(ctx, plan)
}
