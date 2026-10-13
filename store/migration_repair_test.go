package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"dbgold/config"
	"dbgold/datamigrate/repair"
	"github.com/stretchr/testify/require"
)

func seedRepairTest(t *testing.T, job string) {
	t.Helper()
	require.NoError(t, CreateDataMigrationJob(&DataMigrationJob{JobID: job, Status: "failed"}))
	require.NoError(t, SeedMigrationRepairs(&MigrationRepairBinding{JobID: job, Version: 1}, []MigrationRepairObject{{JobID: job, ObjectID: "table", ObjectJSON: `{"id":"table","status":"failed","units":[{"id":"one","status":"failed"}]}`, Version: 1}}))
}

func TestStartupNormalizesLegacyUnverifiedExecutionWithoutReplaying(t *testing.T) {
	cfg := &config.Config{SQLitePath: filepath.Join(t.TempDir(), "legacy.db")}
	require.NoError(t, InitWithError(cfg))
	seedRepairTest(t, "legacy")
	originalReport := `{"indexes":{"failed":1}}`
	require.NoError(t, CreateDataMigrationReport(&DataMigrationReport{JobID: "legacy", ReportJSON: originalReport}))
	cases := []struct {
		id, want string
		units    []repair.Unit
	}{
		{"expression", "succeeded", []repair.Unit{{ID: "ddl", SQL: "CREATE INDEX ix ON t ((id+1))", Status: "succeeded", Selected: true}}},
		{"missing", "unknown", nil},
		{"missing SQL", "unknown", []repair.Unit{{ID: "ddl", Status: "succeeded"}}},
		{"partial", "unknown", []repair.Unit{{ID: "one", SQL: "CREATE TABLE t(id int)", Status: "succeeded"}, {ID: "two", SQL: "ALTER TABLE t ADD name text", Status: "failed"}}},
		{"ambiguous", "unknown", []repair.Unit{{ID: "ddl", SQL: "CREATE INDEX ix ON t(id)", Status: "unknown"}}},
	}
	for _, tc := range cases {
		o := repair.Object{ID: tc.id, Kind: "index", Schema: "app", Status: "unverified", Units: tc.units, OriginalError: "original error", DataStatus: "pending", Verification: &repair.Verification{Status: "unknown", Message: "old verification limitation"}}
		payload, err := json.Marshal(o)
		require.NoError(t, err)
		require.NoError(t, DB.Create(&MigrationRepairObject{JobID: "legacy", ObjectID: tc.id, Version: 4, ObjectJSON: string(payload)}).Error)
	}
	oldHistory := &MigrationRepairAttempt{ID: "old-audit", JobID: "legacy", ObjectID: "expression", RequestID: "old", Kind: "verification", Status: "unknown", VerificationJSON: `{"status":"unknown","message":"old check"}`, SnapshotJSON: `{"object":{"status":"unverified"}}`}
	require.NoError(t, DB.Create(oldHistory).Error)
	require.NoError(t, Close())
	require.NoError(t, InitWithError(cfg))
	for _, tc := range cases {
		row, err := GetMigrationRepairObject("legacy", tc.id)
		require.NoError(t, err)
		var o repair.Object
		require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &o))
		require.Equal(t, tc.want, o.Status, tc.id)
		require.Equal(t, 5, row.Version, tc.id)
		require.Equal(t, "original error", o.OriginalError)
		require.Equal(t, "pending", o.DataStatus)
		if o.Status == "succeeded" {
			require.False(t, o.Units[0].Selected)
		}
	}
	var persisted MigrationRepairAttempt
	require.NoError(t, DB.First(&persisted, "id = ?", oldHistory.ID).Error)
	require.Equal(t, oldHistory.VerificationJSON, persisted.VerificationJSON)
	require.Equal(t, oldHistory.SnapshotJSON, persisted.SnapshotJSON)
	report, err := GetDataMigrationReport("legacy")
	require.NoError(t, err)
	require.Equal(t, originalReport, report.ReportJSON)
	require.NoError(t, Close())
	require.NoError(t, InitWithError(cfg)) // A repeated startup does not update the same records again.
	row, err := GetMigrationRepairObject("legacy", "expression")
	require.NoError(t, err)
	require.Equal(t, 5, row.Version)
	var count int64
	require.NoError(t, DB.Model(&MigrationRepairAttempt{}).Where("job_id = ?", "legacy").Count(&count).Error)
	require.EqualValues(t, 1, count)
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
