package handler

import (
	"context"
	"time"

	"dbgold/datamigrate"
	"dbgold/store"
)

// Startup logs are emitted by the migration worker, which also owns LogCh's
// lifetime. HTTP cancellation handlers must not write to that channel.
type migrationStartup struct {
	ctx context.Context
	job *store.DataMigrationJob
	log *datamigrate.Logger
}

func newMigrationStartup(ctx context.Context, job *datamigrate.Job, dbJob *store.DataMigrationJob) *migrationStartup {
	return &migrationStartup{ctx: ctx, job: dbJob, log: datamigrate.NewLogger(job.LogCh)}
}

func (s *migrationStartup) begin(stage string) (time.Time, bool) {
	if s.cancelled() {
		return time.Time{}, false
	}
	s.log.Infof("%s…", stage)
	return time.Now(), true
}

func (s *migrationStartup) finish(stage string, started time.Time, err error) bool {
	if s.cancelled() {
		return false
	}
	if err != nil {
		message := stage + "失败: " + err.Error()
		s.log.Error(message)
		updateJobStatus(s.job, "failed", message)
		return false
	}
	s.log.Infof("%s完成，耗时 %s", stage, time.Since(started).Round(time.Millisecond))
	return true
}

func (s *migrationStartup) cancelled() bool {
	if s.ctx.Err() == nil {
		return false
	}
	s.log.Warn("任务已取消")
	updateJobStatus(s.job, "cancelled", "任务已取消")
	return true
}
