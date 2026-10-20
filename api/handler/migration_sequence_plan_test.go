package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"dbgold/config"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

type sequencePlanTestWriter struct {
	target.Writer
	// Mutating methods remain nil, so any target DDL during plan/save panics.
	statements   []dialect.Statement
	calls        int
	lastSequence source.SequenceInfo
}

func (*sequencePlanTestWriter) Dialect() dialect.Dialect { return dialect.NewDaMeng() }
func (*sequencePlanTestWriter) Close() error             { return nil }
func (w *sequencePlanTestWriter) PlanOracleDamengSequence(_ context.Context, seq source.SequenceInfo) ([]dialect.Statement, error) {
	w.calls++
	w.lastSequence = seq
	return w.statements, nil
}

func seedSequencePlanHandler(t *testing.T) (repair.Object, *sequencePlanTestWriter) {
	t.Helper()
	require.NoError(t, store.InitWithError(&config.Config{SQLitePath: ":memory:"}))
	dst := &store.Connection{OwnerID: 1, Name: "Target", DBType: "dameng", Host: "127.0.0.1", Port: 5236, Database: "APP", Username: "USER"}
	require.NoError(t, store.CreateConnection(dst))
	require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{JobID: "job", OwnerID: 1, Status: "failed", SrcDBType: "oracle", DstDBType: "dameng", DstConnID: dst.ID, DstSchema: "APP", DstConnHost: dst.Host, DstConnPort: dst.Port, DstConnDatabase: dst.Database, DstConnUsername: dst.Username}))
	seq := source.SequenceInfo{TableName: "T", ColumnName: "ID", StartValueText: "1", Native: &source.NativeSequenceInfo{Name: "S", IncrementBy: "1", MinValue: "1", MaxValue: "1000", CacheSize: "0", Bindings: []source.SequenceBinding{{TableName: "T", ColumnName: "ID", Kind: "trigger", TriggerName: "TRG"}}}}
	ddl := dialect.NewDaMeng().SequenceStatements("APP", seq, "")
	object := repair.Object{ID: "seq", Kind: "sequence", Name: "S", TargetName: "S", Table: "T", Schema: "APP", Status: "failed", OriginalStatus: "failed", OriginalError: "original failure", Sequence: &seq, Units: []repair.Unit{{ID: "create-seq", SQL: ddl[0].SQL, OriginalSQL: ddl[0].SQL, Status: "succeeded"}, {ID: "create-trigger", SQL: ddl[1].SQL, OriginalSQL: ddl[1].SQL, Status: "failed", Selected: true}}}
	require.NoError(t, store.SeedMigrationRepairs(&store.MigrationRepairBinding{JobID: "job", DstConnID: dst.ID, TargetSchema: "APP", TargetFingerprint: connectionFingerprint(dst), Configured: true, Version: 1}, []store.MigrationRepairObject{{JobID: "job", ObjectID: "seq", ObjectJSON: repairJSON(object), Version: 1}}))
	writer := &sequencePlanTestWriter{statements: []dialect.Statement{{SQL: `ALTER SEQUENCE "APP"."S" CURRENT VALUE 42`}, ddl[1]}}
	previous := newMigrationSequencePlanWriter
	newMigrationSequencePlanWriter = func(*store.Connection, string, target.ConnPoolConfig) (target.Writer, error) { return writer, nil }
	t.Cleanup(func() { newMigrationSequencePlanWriter = previous })
	return object, writer
}

func TestSequencePlanRefreshIsReadOnlyAndRequiresReviewedSave(t *testing.T) {
	object, writer := seedSequencePlanHandler(t)
	before, err := store.GetMigrationRepairObject("job", "seq")
	require.NoError(t, err)
	w := repairRequest(t, http.MethodPost, "/jobs/job/repairs/seq", `{"version":1}`, 1, RefreshMigrationSequencePlan)
	require.Equal(t, 200, w.Code, w.Body.String())
	var response struct {
		Object     repair.Object `json:"object"`
		Version    int           `json:"version"`
		HasChanges bool          `json:"has_changes"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.True(t, response.HasChanges)
	require.True(t, response.Object.RequiresReview)
	require.Equal(t, 1, response.Version)
	require.Equal(t, object.Units[0], response.Object.Units[0], "committed SQL and history remain untouched")
	require.Len(t, response.Object.Units, 3)
	require.Contains(t, response.Object.Units[1].SQL, "ALTER SEQUENCE")
	for _, unit := range response.Object.Units {
		require.Equal(t, unit.SQL, unit.OriginalSQL)
	}
	after, err := store.GetMigrationRepairObject("job", "seq")
	require.NoError(t, err)
	require.Equal(t, before.ObjectJSON, after.ObjectJSON)
	require.Equal(t, before.Version, after.Version)
	var attempts int64
	require.NoError(t, store.DB.Model(&store.MigrationRepairAttempt{}).Count(&attempts).Error)
	require.Zero(t, attempts)

	body := map[string]any{"version": 1, "units": response.Object.Units, "sequence_plan": true}
	w = repairRequest(t, http.MethodPut, "/jobs/job/repairs/seq", repairJSON(body), 1, SaveMigrationRepairDraft)
	require.Equal(t, 400, w.Code)
	body["reviewed"] = true
	// Forged origins are ignored; the server regenerates the candidate itself.
	response.Object.Units[1].OriginalSQL = "forged client origin"
	body["units"] = response.Object.Units
	w = repairRequest(t, http.MethodPut, "/jobs/job/repairs/seq", repairJSON(body), 1, SaveMigrationRepairDraft)
	require.Equal(t, 200, w.Code, w.Body.String())
	var saved struct{ Object repair.Object }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &saved))
	require.False(t, saved.Object.RequiresReview)
	require.Equal(t, "draft", saved.Object.Status)
	require.Equal(t, object.Units[0], saved.Object.Units[0])
	require.Equal(t, saved.Object.Units[1].SQL, saved.Object.Units[1].OriginalSQL)
	require.Equal(t, 2, writer.calls, "opening and saving both read the actual target plan")
}

func TestSequencePlanSaveRejectsChangedTargetOrEditedSQL(t *testing.T) {
	for _, editTarget := range []bool{false, true} {
		t.Run(map[bool]string{false: "edited SQL", true: "target changed"}[editTarget], func(t *testing.T) {
			_, writer := seedSequencePlanHandler(t)
			w := repairRequest(t, http.MethodPost, "/jobs/job/repairs/seq", `{"version":1}`, 1, RefreshMigrationSequencePlan)
			require.Equal(t, 200, w.Code, w.Body.String())
			var response struct{ Object repair.Object }
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			if editTarget {
				writer.statements[0].SQL = `ALTER SEQUENCE "APP"."S" CURRENT VALUE 99`
			} else {
				response.Object.Units[1].SQL = `ALTER SEQUENCE "APP"."S" CURRENT VALUE 99`
			}
			body := repairJSON(map[string]any{"version": 1, "units": response.Object.Units, "sequence_plan": true, "reviewed": true})
			w = repairRequest(t, http.MethodPut, "/jobs/job/repairs/seq", body, 1, SaveMigrationRepairDraft)
			require.Equal(t, 400, w.Code, w.Body.String())
			row, err := store.GetMigrationRepairObject("job", "seq")
			require.NoError(t, err)
			require.Equal(t, 1, row.Version)
		})
	}
}

func TestSequencePlanRefreshCannotBypassUnknownEditedOrMissingSourceMetadata(t *testing.T) {
	for _, testCase := range []string{"unknown object", "running unit", "edited SQL", "missing metadata", "version conflict", "other owner"} {
		t.Run(testCase, func(t *testing.T) {
			object, writer := seedSequencePlanHandler(t)
			version, owner, want := 1, uint(1), 400
			switch testCase {
			case "unknown object":
				object.Status = "unknown"
			case "running unit":
				object.Units[1].Status = "running"
			case "edited SQL":
				object.Units[1].SQL += " -- manually edited"
			case "missing metadata":
				object.Sequence.Unsupported, object.Sequence.FailureStage = "missing source sequence", "metadata"
			case "version conflict":
				version, want = 2, 409
			case "other owner":
				owner, want = 2, 404
			}
			require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "seq").Update("object_json", repairJSON(object)).Error)
			w := repairRequest(t, http.MethodPost, "/jobs/job/repairs/seq", repairJSON(map[string]any{"version": version}), owner, RefreshMigrationSequencePlan)
			require.Equal(t, want, w.Code, w.Body.String())
			require.Zero(t, writer.calls)
		})
	}
}

func TestSequencePlanEmptyChangesReturnsReviewableReuseUnits(t *testing.T) {
	object, writer := seedSequencePlanHandler(t)
	writer.statements = nil
	w := repairRequest(t, http.MethodPost, "/jobs/job/repairs/seq", `{"version":1}`, 1, RefreshMigrationSequencePlan)
	require.Equal(t, 200, w.Code, w.Body.String())
	var response struct {
		Object     repair.Object `json:"object"`
		HasChanges bool          `json:"has_changes"`
		Message    string        `json:"message"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.False(t, response.HasChanges)
	require.Contains(t, response.Message, "无需变更")
	require.True(t, response.Object.RequiresReview)
	require.Equal(t, object.Units[0], response.Object.Units[0])
	require.Len(t, response.Object.Units, 3)
	for _, unit := range response.Object.Units[1:] {
		require.Equal(t, unit.SQL, unit.OriginalSQL)
		require.True(t, unit.Selected)
		require.Equal(t, "pending", unit.Status)
	}
}

func TestSequencePlanRefreshRevalidatesStoredTargetPreflightMetadata(t *testing.T) {
	object, writer := seedSequencePlanHandler(t)
	object.Sequence.Unsupported, object.Sequence.FailureStage = "old maximum exceeds int64", "preflight"
	object.Sequence.Native.MaxValue = "9999999999999999999999999998"
	object.Sequence.Native.Bindings[0].TriggerBody = "original source trigger body"
	before := repairJSON(object)
	require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "seq").Update("object_json", before).Error)
	w := repairRequest(t, http.MethodPost, "/jobs/job/repairs/seq", `{"version":1}`, 1, RefreshMigrationSequencePlan)
	require.Equal(t, 200, w.Code, w.Body.String())
	var response struct{ Object repair.Object }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Empty(t, response.Object.Sequence.Unsupported)
	require.Empty(t, response.Object.Sequence.FailureStage)
	require.Equal(t, "9223372036854775807", response.Object.Sequence.Native.MaxValue)
	require.Equal(t, "original source trigger body", response.Object.Sequence.Native.Bindings[0].TriggerBody)
	require.Equal(t, response.Object.Sequence.Native.MaxValue, writer.lastSequence.Native.MaxValue)
	row, err := store.GetMigrationRepairObject("job", "seq")
	require.NoError(t, err)
	require.Equal(t, before, row.ObjectJSON, "planning must not rewrite old metadata or history")
}

func TestSequencePlanRefreshDoesNotSeedMissingLegacyLedger(t *testing.T) {
	require.NoError(t, store.InitWithError(&config.Config{SQLitePath: ":memory:"}))
	require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{JobID: "job", OwnerID: 1, Status: "failed", SrcDBType: "oracle", DstDBType: "dameng"}))
	w := repairRequest(t, http.MethodPost, "/jobs/job/repairs/seq", `{"version":1}`, 1, RefreshMigrationSequencePlan)
	require.Equal(t, 400, w.Code, w.Body.String())
	var bindings, objects int64
	require.NoError(t, store.DB.Model(&store.MigrationRepairBinding{}).Count(&bindings).Error)
	require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Count(&objects).Error)
	require.Zero(t, bindings)
	require.Zero(t, objects)
}
