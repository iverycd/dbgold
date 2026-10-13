package store

import (
	"encoding/json"
	"testing"
	"time"

	"dbgold/datamigrate/repair"
	"github.com/stretchr/testify/require"
)

func seedRepairTest(t *testing.T, job string) {
	t.Helper()
	require.NoError(t, CreateDataMigrationJob(&DataMigrationJob{JobID: job, Status: "failed"}))
	require.NoError(t, SeedMigrationRepairs(&MigrationRepairBinding{JobID: job, Version: 1}, []MigrationRepairObject{{JobID: job, ObjectID: "table", ObjectJSON: `{"id":"table","status":"failed","units":[{"id":"one","status":"failed"}]}`, Version: 1}}))
}
func TestRepairClaimCASIdempotencyAndScope(t *testing.T) {
	setupTestDB(t)
	seedRepairTest(t, "a")
	seedRepairTest(t, "b")
	a := &MigrationRepairAttempt{ID: "attempt-a", JobID: "a", ObjectID: "table", RequestID: "click-a", RequestHash: "snapshot", Status: "running", Scope: "same-db/schema"}
	got, claimed, e := ClaimRepairAttempt(a, 1, 1)
	require.NoError(t, e)
	require.True(t, claimed)
	require.Equal(t, a.ID, got.ID)
	got, claimed, e = ClaimRepairAttempt(a, 99, 99)
	require.NoError(t, e)
	require.False(t, claimed)
	require.Equal(t, a.ID, got.ID)
	duplicate := *a
	duplicate.RequestHash = "changed"
	_, _, e = ClaimRepairAttempt(&duplicate, 1, 1)
	require.ErrorIs(t, e, ErrRepairConflict)
	require.ErrorIs(t, SaveMigrationRepairObject("a", "table", 1, `{}`), ErrRepairConflict)
	b := &MigrationRepairAttempt{ID: "attempt-b", JobID: "b", ObjectID: "table", RequestID: "click-b", RequestHash: "snapshot", Status: "running", Scope: a.Scope}
	_, _, e = ClaimRepairAttempt(b, 1, 1)
	require.ErrorIs(t, e, ErrRepairConflict)
	now := time.Now()
	a.Status = "succeeded"
	a.FinishedAt = &now
	require.NoError(t, FinishRepairAttempt(a, `{"id":"table","status":"succeeded"}`))
	_, claimed, e = ClaimRepairAttempt(b, 1, 1)
	require.NoError(t, e)
	require.True(t, claimed)
	require.ErrorIs(t, SaveMigrationRepairObject("a", "table", 1, `{}`), ErrRepairConflict)
}
func TestRepairClaimChecksRelatedDraftVersions(t *testing.T) {
	setupTestDB(t)
	seedRepairTest(t, "a")
	require.NoError(t, DB.Create(&MigrationRepairObject{JobID: "a", ObjectID: "index", Version: 2, ObjectJSON: `{}`}).Error)
	a := &MigrationRepairAttempt{ID: "a", JobID: "a", ObjectID: "table", RequestID: "once", RequestHash: "hash", Status: "running", Scope: "scope"}
	_, _, e := ClaimRepairAttempt(a, 1, 1, map[string]int{"index": 1})
	require.ErrorIs(t, e, ErrRepairConflict)
	var count int64
	require.NoError(t, DB.Model(&MigrationRepairLock{}).Count(&count).Error)
	require.Zero(t, count)
}
func TestRepairRestartDoesNotReplayOrChangeOriginalReport(t *testing.T) {
	setupTestDB(t)
	seedRepairTest(t, "a")
	original := `{"tables":{"failed":1}}`
	require.NoError(t, CreateDataMigrationReport(&DataMigrationReport{JobID: "a", ReportJSON: original}))
	a := &MigrationRepairAttempt{ID: "attempt", JobID: "a", ObjectID: "table", RequestID: "click", RequestHash: "snapshot", Status: "running", Kind: "ddl", Scope: "scope"}
	_, _, e := ClaimRepairAttempt(a, 1, 1)
	require.NoError(t, e)
	require.NoError(t, DB.Model(&MigrationRepairObject{}).Where("job_id = ?", "a").Update("object_json", `{"id":"table","status":"running","units":[{"id":"one","status":"succeeded"},{"id":"two","status":"running"}]}`).Error)
	require.NoError(t, MarkInterruptedRepairsUnknown())
	var loaded MigrationRepairAttempt
	require.NoError(t, DB.First(&loaded, "id = ?", a.ID).Error)
	require.Equal(t, "unknown", loaded.Status)
	row, e := GetMigrationRepairObject("a", "table")
	require.NoError(t, e)
	var o repair.Object
	require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &o))
	require.Equal(t, "unknown", o.Status)
	require.Equal(t, "succeeded", o.Units[0].Status)
	require.Equal(t, "unknown", o.Units[1].Status)
	report, e := GetDataMigrationReport("a")
	require.NoError(t, e)
	require.Equal(t, original, report.ReportJSON)
	require.NoError(t, AcquireMigrationScope("scope", "next"))
}
