package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIncrementalMigrationLogAutoMigrateAndBatchAppend(t *testing.T) {
	setupTestDB(t)
	require.True(t, DB.Migrator().HasTable(&IncrementalMigrationLog{}))
	require.True(t, DB.Migrator().HasIndex(&IncrementalMigrationLog{}, "idx_incremental_log_job_cursor"))

	base := time.Date(2026, 7, 17, 10, 0, 0, 0, time.Local)
	logs := []IncrementalMigrationLog{
		{JobID: "log-batch", Phase: "snapshot_schema", Level: "ddl", Line: "[DDL] create a", CreatedAt: base},
		{JobID: "log-batch", Phase: "snapshot_data", Level: "data", Line: "[DATA] copy a", CreatedAt: base.Add(time.Second)},
	}
	require.NoError(t, AppendIncrementalMigrationLogs(logs))
	assert.NotZero(t, logs[0].ID)
	assert.Greater(t, logs[1].ID, logs[0].ID)

	var count int64
	require.NoError(t, DB.Model(&IncrementalMigrationLog{}).Where("job_id = ?", "log-batch").Count(&count).Error)
	assert.Equal(t, int64(2), count)
	require.NoError(t, AppendIncrementalMigrationLogs(nil))
}

func TestListIncrementalMigrationLogsByCursor(t *testing.T) {
	setupTestDB(t)
	base := time.Date(2026, 7, 17, 10, 0, 0, 0, time.Local)
	logs := make([]IncrementalMigrationLog, 0, 7)
	for i := 1; i <= 6; i++ {
		logs = append(logs, IncrementalMigrationLog{
			JobID: "cursor-job", Phase: "snapshot_data", Level: "data",
			Line: fmt.Sprintf("line-%d", i), CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
		if i == 3 {
			logs = append(logs, IncrementalMigrationLog{
				JobID: "other-job", Phase: "snapshot_data", Level: "info",
				Line: "unrelated", CreatedAt: base,
			})
		}
	}
	require.NoError(t, AppendIncrementalMigrationLogs(logs))

	tail, err := ListIncrementalMigrationLogs("cursor-job", 0, 0, 2)
	require.NoError(t, err)
	require.Len(t, tail.Items, 2)
	assert.Equal(t, []string{"line-5", "line-6"}, logLines(tail.Items))
	assert.True(t, tail.HasOlder)
	assert.False(t, tail.HasNewer)
	assert.Equal(t, tail.Items[0].ID, tail.OldestID)
	assert.Equal(t, tail.Items[1].ID, tail.NewestID)

	older, err := ListIncrementalMigrationLogs("cursor-job", 0, tail.OldestID, 2)
	require.NoError(t, err)
	require.Len(t, older.Items, 2)
	assert.Equal(t, []string{"line-3", "line-4"}, logLines(older.Items))
	assert.True(t, older.HasOlder)
	assert.True(t, older.HasNewer)

	newer, err := ListIncrementalMigrationLogs("cursor-job", older.NewestID, 0, 1)
	require.NoError(t, err)
	require.Len(t, newer.Items, 1)
	assert.Equal(t, "line-5", newer.Items[0].Line)
	assert.True(t, newer.HasOlder)
	assert.True(t, newer.HasNewer)

	_, err = ListIncrementalMigrationLogs("cursor-job", 1, 2, 10)
	assert.ErrorIs(t, err, ErrConflictingIncrementalLogCursors)
	empty, err := ListIncrementalMigrationLogs("missing", 0, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, empty.Items)
	assert.False(t, empty.HasOlder)
	assert.False(t, empty.HasNewer)

	afterEnd, err := ListIncrementalMigrationLogs("cursor-job", logs[len(logs)-1].ID+100, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, afterEnd.Items)
	assert.True(t, afterEnd.HasOlder)
	assert.False(t, afterEnd.HasNewer)

	beforeStart, err := ListIncrementalMigrationLogs("cursor-job", 0, logs[0].ID, 10)
	require.NoError(t, err)
	assert.Empty(t, beforeStart.Items)
	assert.False(t, beforeStart.HasOlder)
	assert.True(t, beforeStart.HasNewer)
}

func TestAddIncrementalLogDroppedCount(t *testing.T) {
	setupTestDB(t)
	job := &IncrementalMigrationJob{OwnerID: 1, JobID: "log-dropped", Status: "snapshot"}
	require.NoError(t, CreateIncrementalJob(job))
	require.NoError(t, AddIncrementalLogDroppedCount(job.JobID, 2))
	require.NoError(t, AddIncrementalLogDroppedCount(job.JobID, 3))
	require.NoError(t, AddIncrementalLogDroppedCount(job.JobID, 0))
	got, err := GetIncrementalJob(job.JobID)
	require.NoError(t, err)
	assert.Equal(t, int64(5), got.LogDroppedCount)
}

func TestCleanupExpiredIncrementalMigrationLogs(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)
	oldFinished := cutoff.Add(-time.Hour)
	recentFinished := cutoff.Add(time.Hour)
	exactFinished := cutoff
	jobs := []IncrementalMigrationJob{
		{OwnerID: 1, JobID: "old-stopped", Status: "stopped", FinishedAt: &oldFinished},
		{OwnerID: 1, JobID: "old-aborted", Status: "aborted", FinishedAt: &oldFinished},
		{OwnerID: 1, JobID: "old-failed", Status: "failed", FinishedAt: &oldFinished},
		{OwnerID: 1, JobID: "old-paused", Status: "paused_restart", FinishedAt: &oldFinished},
		{OwnerID: 1, JobID: "recent-stopped", Status: "stopped", FinishedAt: &recentFinished},
		{OwnerID: 1, JobID: "exact-stopped", Status: "stopped", FinishedAt: &exactFinished},
		{OwnerID: 1, JobID: "unfinished-stopped", Status: "stopped"},
	}
	for i := range jobs {
		require.NoError(t, CreateIncrementalJob(&jobs[i]))
	}
	logs := make([]IncrementalMigrationLog, 0, len(jobs))
	for _, job := range jobs {
		logs = append(logs, IncrementalMigrationLog{JobID: job.JobID, Phase: "snapshot_init", Level: "info", Line: job.JobID, CreatedAt: now})
	}
	require.NoError(t, AppendIncrementalMigrationLogs(logs))

	deleted, err := CleanupExpiredIncrementalMigrationLogs(cutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(2), deleted)
	var remaining []IncrementalMigrationLog
	require.NoError(t, DB.Order("id ASC").Find(&remaining).Error)
	assert.Equal(t, []string{"old-failed", "old-paused", "recent-stopped", "exact-stopped", "unfinished-stopped"}, logJobIDs(remaining))
}

func TestPauseInterruptedSnapshotWritesWarningLog(t *testing.T) {
	setupTestDB(t)
	job := &IncrementalMigrationJob{
		OwnerID: 1, JobID: "restart-snapshot", StartMode: "full_then_cdc",
		Status: "snapshot", Phase: "snapshot", BootstrapDone: false,
	}
	require.NoError(t, CreateIncrementalJob(job))
	require.NoError(t, PauseInterruptedIncrementalJobs())
	page, err := ListIncrementalMigrationLogs(job.JobID, 0, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "snapshot_init", page.Items[0].Phase)
	assert.Equal(t, "warn", page.Items[0].Level)
	assert.Contains(t, page.Items[0].Line, "服务重启中断")

	// A second startup pass must not duplicate the interruption entry because
	// the task is already paused_restart.
	require.NoError(t, PauseInterruptedIncrementalJobs())
	page, err = ListIncrementalMigrationLogs(job.JobID, 0, 0, 10)
	require.NoError(t, err)
	assert.Len(t, page.Items, 1)
}

func logLines(logs []IncrementalMigrationLog) []string {
	result := make([]string, len(logs))
	for i := range logs {
		result[i] = logs[i].Line
	}
	return result
}

func logJobIDs(logs []IncrementalMigrationLog) []string {
	result := make([]string, len(logs))
	for i := range logs {
		result[i] = logs[i].JobID
	}
	return result
}
