package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dbgold/datamigrate"
	"dbgold/datamigrate/repair"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

const pgManualFunction = `CREATE FUNCTION app.audit_deletion() RETURNS trigger AS $body$
DECLARE removed_id integer;
BEGIN removed_id := OLD.id; RETURN OLD; END;
$body$ LANGUAGE plpgsql;`
const pgManualTrigger = `CREATE TRIGGER history_delete AFTER DELETE ON app.orders FOR EACH ROW EXECUTE PROCEDURE app.audit_deletion();`

func pgManualTriggerFailure() datamigrate.ObjectResult {
	return datamigrate.ObjectResult{
		Name: "history_delete", SourceName: "History_Delete", Table: "Orders",
		TargetName: "history_delete", TargetTable: "orders", TargetSchema: "app",
		Stage: "unsupported", SourceDDL: "CREATE TRIGGER History_Delete ... DECLARE ...", Error: "语句 declare 需要人工适配",
	}
}

func TestPGManualTriggerHistoryDraftAPI(t *testing.T) {
	seedHandlerRepair(t)
	require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Where("job_id = ?", "job").Updates(map[string]any{"src_db_type": "mysql", "dst_db_type": "vastbase"}).Error)
	require.NoError(t, store.DB.Model(&store.MigrationRepairBinding{}).Where("job_id = ?", "job").Update("target_schema", "app").Error)
	item := pgManualTriggerFailure()
	report := datamigrate.MigrationReport{TriggerMigrationEnabled: true, Triggers: datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{item}}}
	reportJSON := repairJSON(report)
	require.NoError(t, store.CreateDataMigrationReport(&store.DataMigrationReport{JobID: "job", ReportJSON: reportJSON}))
	legacy := datamigrate.LegacyRepairObjects(report, "app")[0]
	// Simulate an already-imported repair ledger, using the former source-name fallback.
	legacy.Table, legacy.SourceName, legacy.TriggerFailureStage = item.Table, item.Name, ""
	row := store.MigrationRepairObject{JobID: "job", ObjectID: legacy.ID, Version: 1, ObjectJSON: repairJSON(legacy)}
	require.NoError(t, store.DB.Create(&row).Error)
	path := "/jobs/job/repairs/" + legacy.ID
	var record struct {
		Object  repair.Object `json:"object"`
		Version int           `json:"version"`
	}
	get := repairRequest(t, http.MethodGet, path, "", 1, GetMigrationRepair)
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())
	require.NoError(t, json.Unmarshal(get.Body.Bytes(), &record))
	require.True(t, record.Object.ManualTriggerEditable)
	require.True(t, record.Object.DiagnosticOnly)
	require.Equal(t, item.TargetTable, record.Object.Table)
	require.Equal(t, item.SourceName, record.Object.SourceName)
	require.Empty(t, record.Object.Units)

	save := func(version int, functionSQL, triggerSQL string) *httptest.ResponseRecorder {
		t.Helper()
		request := repairJSON(map[string]any{"version": version, "mode": "manual_trigger", "reviewed": true,
			"units": []repair.Unit{{SQL: functionSQL, Status: "succeeded"}, {SQL: triggerSQL, Destructive: true}}})
		return repairRequest(t, http.MethodPut, path, request, 1, SaveMigrationRepairDraft)
	}
	first := save(1, pgManualFunction, pgManualTrigger)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	record.Object = repair.Object{}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &record))
	require.Equal(t, 2, record.Version)
	require.True(t, record.Object.TrustedManualTrigger())
	require.Equal(t, item.SourceDDL, record.Object.SourceDDL)
	require.Equal(t, item.Error, record.Object.OriginalError)
	require.False(t, record.Object.DiagnosticOnly)
	require.Equal(t, pgManualFunction, record.Object.Units[0].SQL)
	require.Equal(t, pgManualTrigger, record.Object.Units[1].SQL)
	for _, unit := range record.Object.Units {
		require.Equal(t, "pending", unit.Status)
		require.False(t, unit.Destructive)
	}
	require.Equal(t, http.StatusConflict, save(1, pgManualFunction, pgManualTrigger).Code)
	changed := strings.Replace(pgManualFunction, "removed_id := OLD.id", "removed_id := OLD.id + 1", 1)
	second := save(2, changed, pgManualTrigger)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	record.Object = repair.Object{}
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &record))
	require.Equal(t, 2, record.Object.ManualTrigger.Revision)
	require.Equal(t, item.Error, record.Object.ManualTrigger.Original.Error)
	reloaded := repairRequest(t, http.MethodGet, path, "", 1, GetMigrationRepair)
	require.NoError(t, json.Unmarshal(reloaded.Body.Bytes(), &record))
	require.True(t, record.Object.TrustedManualTrigger())
	require.Equal(t, changed, record.Object.Units[0].SQL)
	original, err := store.GetDataMigrationReport("job")
	require.NoError(t, err)
	require.Equal(t, reportJSON, original.ReportJSON)
	data := repair.Object{ID: "data", Kind: "data", Table: item.Table, SourceTable: item.Table, TargetName: item.Table, Schema: "app", Status: "failed", Legacy: true}
	dataRow := store.MigrationRepairObject{JobID: "job", ObjectID: data.ID, Version: 1, ObjectJSON: repairJSON(data)}
	require.NoError(t, store.DB.Create(&dataRow).Error)
	require.ErrorContains(t, validateManualTriggerDependencies("job", record.Object), "数据尚未完成")
	data.Status = "succeeded"
	require.NoError(t, store.DB.Model(&dataRow).Update("object_json", repairJSON(data)).Error)
	require.NoError(t, validateManualTriggerDependencies("job", record.Object))

	record.Object.Units[0].Status, record.Object.Status = "unknown", "unknown"
	require.NoError(t, store.DB.Model(&row).Update("object_json", repairJSON(record.Object)).Error)
	require.False(t, canEditManualTrigger(&store.DataMigrationJob{SrcDBType: "mysql", DstDBType: "vastbase"}, record.Object))
	execute := repairRequest(t, http.MethodPost, path, `{"version":3,"request_id":"unknown","confirmed":true}`, 1, ExecuteMigrationRepair)
	require.Equal(t, http.StatusBadRequest, execute.Code, execute.Body.String())
	require.Contains(t, execute.Body.String(), "核对目标触发器状态")
}

func TestPGManualTriggerRequiresUnambiguousOriginAndIdentity(t *testing.T) {
	item := pgManualTriggerFailure()
	report := datamigrate.MigrationReport{TriggerMigrationEnabled: true, Triggers: datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{item}}}
	objects := datamigrate.LegacyRepairObjects(report, "app")
	datamigrate.EnrichRepairReferences(objects, report)
	o := objects[0]
	job := &store.DataMigrationJob{SrcDBType: "mysql", DstDBType: "postgres"}
	for _, target := range []string{"postgres", "vastbase", "gaussdb", "highgo", "gbase", "seabox", "kingbase"} {
		require.True(t, canEditManualTrigger(&store.DataMigrationJob{SrcDBType: "mysql", DstDBType: target}, o), target)
	}
	for _, sql := range []string{
		strings.Replace(pgManualTrigger, "ON app.orders", "ON other.orders", 1),
		strings.Replace(pgManualTrigger, "ON app.orders", "ON app.other", 1),
		strings.Replace(pgManualTrigger, "history_delete", "other", 1),
		strings.Replace(pgManualTrigger, "app.audit_deletion", "app.other", 1),
		pgManualTrigger + ` DROP TABLE app.orders;`,
	} {
		_, err := prepareManualTriggerDraft(job, o, []repair.Unit{{SQL: pgManualFunction}, {SQL: sql}}, true)
		require.Error(t, err, sql)
	}
	for _, stage := range []string{"metadata", "execute", "identity_conflict", ""} {
		candidate := o
		candidate.TriggerFailureStage = stage
		require.False(t, canEditManualTrigger(job, candidate), stage)
	}
	item.SourceName = "history_delete"
	report.Triggers.Items = append(report.Triggers.Items, item)
	datamigrate.EnrichRepairReferences(objects, report)
	require.False(t, canEditManualTrigger(job, objects[0]))
}

func TestPGManualTriggerGeneratedFailureRetainsFrozenPlan(t *testing.T) {
	o, _ := triggerDependencyFixture(t, false)
	o.ID, o.Status = "generated", "failed"
	p := o.MySQLPGTrigger
	o.SourceName, o.SourceTable = p.Source.Name, p.Source.TableName
	o.Units = []repair.Unit{
		{ID: "function", SQL: p.FunctionSQL, OriginalSQL: p.FunctionSQL, Status: "rolled_back", Selected: true},
		{ID: "trigger", SQL: p.TriggerSQL, OriginalSQL: p.TriggerSQL, Status: "failed", Selected: true},
	}
	job := &store.DataMigrationJob{SrcDBType: "mysql", DstDBType: "postgres"}
	before := repairJSON(o)
	candidate, err := prepareManualTriggerDraft(job, o, o.Units, true)
	require.NoError(t, err)
	require.Equal(t, before, repairJSON(o))
	require.NotNil(t, candidate.ManualTrigger.Original.MySQLPGTrigger)
	require.Nil(t, candidate.MySQLPGTrigger)
	require.NoError(t, validateRepairObjectScope(candidate, candidate.Units, "vastbase"))
	require.Error(t, validateRepairObjectScope(candidate, candidate.Units, "dameng"))
	for _, state := range []string{"succeeded", "unknown", "running"} {
		o.Units[0].Status = state
		require.False(t, canEditManualTrigger(job, o), state)
	}
}
