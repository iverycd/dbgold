package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"dbgold/datamigrate"
	"dbgold/datamigrate/repair"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func TestNativeManualTriggerHTTPRevisionsPreserveSourceAndDisabledRestore(t *testing.T) {
	seedHandlerRepair(t)
	require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Where("job_id = ?", "job").Updates(map[string]any{"src_db_type": "dameng", "dst_db_type": "dameng", "dst_schema": "APP"}).Error)
	require.NoError(t, store.DB.Model(&store.MigrationRepairBinding{}).Where("job_id = ?", "job").Update("target_schema", "APP").Error)
	original := manualTriggerNativeFixture()
	require.True(t, original.TrustedNative())
	require.NoError(t, store.DB.Create(&store.MigrationRepairObject{JobID: "job", ObjectID: original.ID, Version: 1, ObjectJSON: repairJSON(original)}).Error)
	// Real failed native reports also appear in the general legacy/reference
	// projection. That projection must not demote the trusted native ledger to
	// diagnostic-only before its first manual revision.
	report := datamigrate.MigrationReport{TriggerMigrationEnabled: true, Triggers: datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "TRG", Table: "ORDERS", DDL: manualTriggerOriginal, Error: "original compile failure"}}}}
	reportJSON := repairJSON(report)
	require.NoError(t, store.CreateDataMigrationReport(&store.DataMigrationReport{JobID: "job", ReportJSON: reportJSON}))
	path := "/jobs/job/repairs/trigger"
	type response struct {
		Object  repair.Object `json:"object"`
		Version int           `json:"version"`
	}
	save := func(version int, sql string) response {
		request := repairJSON(map[string]any{"mode": "manual_trigger", "version": version, "reviewed": true, "units": []repair.Unit{{SQL: sql, Status: "succeeded", Selected: false}}})
		result := repairRequest(t, http.MethodPut, path, request, 1, SaveMigrationRepairDraft)
		require.Equal(t, http.StatusOK, result.Code, result.Body.String())
		var saved response
		require.NoError(t, json.Unmarshal(result.Body.Bytes(), &saved))
		return saved
	}
	first := save(1, manualTriggerRevised)
	require.Equal(t, 2, first.Version)
	require.True(t, first.Object.TrustedNative())
	require.True(t, first.Object.TrustedManualTrigger())
	require.Equal(t, repairJSON(original), repairJSON(first.Object.ManualTrigger.Original))
	require.Equal(t, "DISABLED", first.Object.Native.Status)
	require.Equal(t, original.Native.Units[2], first.Object.Native.Units[2])
	require.Equal(t, "pending", first.Object.Units[0].Status)
	require.True(t, first.Object.Units[0].Selected, "client status and selection must not skip a new CREATE revision")

	// Simulate a known partial failure: CREATE committed, compile failed, state
	// restore is still pending. Saving identical SQL must not replay CREATE.
	partial := first.Object
	partial.ManualTriggerEditable = false
	partial.Status = "failed"
	partial.Units[0].Status, partial.Units[0].Selected = "succeeded", false
	partial.Units[1].Status, partial.Units[1].Error = "failed", "compile failed again"
	require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "trigger").Update("object_json", repairJSON(partial)).Error)
	same := save(2, manualTriggerRevised)
	require.Equal(t, 3, same.Version)
	require.Equal(t, 1, same.Object.ManualTrigger.Revision)
	require.Equal(t, "succeeded", same.Object.Units[0].Status)
	require.False(t, same.Object.Units[0].Selected)
	require.Equal(t, "failed", same.Object.Units[1].Status)
	require.True(t, same.Object.Units[1].Selected)

	// An explicit body edit creates a new complete program revision, while the
	// immutable original remains the source-drift baseline across reloads.
	secondSQL := strings.Replace(manualTriggerRevised, ":OLD.ID > 0", ":OLD.ID > 1", 1)
	second := save(3, secondSQL)
	require.Equal(t, 4, second.Version)
	require.Equal(t, 2, second.Object.ManualTrigger.Revision)
	require.Equal(t, []string{manualTriggerRevised}, second.Object.ManualTrigger.PreviousSQL)
	require.Equal(t, repairJSON(original), repairJSON(second.Object.ManualTrigger.Original))
	for _, unit := range second.Object.Units {
		require.Equal(t, "pending", unit.Status)
		require.True(t, unit.Selected)
	}
	get := repairRequest(t, http.MethodGet, path, "", 1, GetMigrationRepair)
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())
	var reloaded response
	require.NoError(t, json.Unmarshal(get.Body.Bytes(), &reloaded))
	require.True(t, reloaded.Object.TrustedManualTrigger())
	require.True(t, reloaded.Object.ManualTriggerEditable)
	require.Equal(t, "DISABLED", reloaded.Object.Native.Status)
	require.Equal(t, original.Native.Units[2], reloaded.Object.Native.Units[2])
	stale := repairRequest(t, http.MethodPost, path, `{"version":2,"request_id":"stale-manual-version","confirmed":true}`, 1, ExecuteMigrationRepair)
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	unchangedReport, err := store.GetDataMigrationReport("job")
	require.NoError(t, err)
	require.Equal(t, reportJSON, unchangedReport.ReportJSON)
}
