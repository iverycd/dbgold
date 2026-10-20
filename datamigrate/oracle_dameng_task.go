package datamigrate

import (
	"context"
	"fmt"
)

type oracleDamengTaskKey struct{}

// The task context reaches metadata collection, table workers and post-DDL.
// Cancelling it stops dispatch as well as in-flight database operations.
func (m *Migrator) cancelOracleDamengTask(ctx context.Context, err error) {
	if cancel, ok := ctx.Value(oracleDamengTaskKey{}).(context.CancelCauseFunc); ok {
		cancel(err)
	}
}

func (m *Migrator) stopOracleDamengTask(ctx context.Context, err error) bool {
	if !m.isOracleDameng() || !isMigrationTaskError(err) {
		return false
	}
	m.cancelOracleDamengTask(ctx, err)
	return true
}

func (m *Migrator) recordOracleDamengTaskFailure(ctx context.Context, report *MigrationReport) {
	if !m.isOracleDameng() || ctx.Err() == nil {
		return
	}
	err := context.Cause(ctx)
	m.log.Errorf("任务级错误，停止后续迁移: %v", err)
	report.Tables.Failed++
	report.Tables.Total = max(report.Tables.Total, report.Tables.Failed+report.Tables.Success)
	report.Tables.Items = append(report.Tables.Items, ObjectResult{
		Name: "任务级错误", Stage: "task", Error: fmt.Sprintf("迁移任务已终止: %v", err),
	})
}
