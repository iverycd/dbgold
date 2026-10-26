package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"dbgold/datamigrate"
	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/mysqldmtrigger"
	"dbgold/datamigrate/repair"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

const manualTriggerOriginal = `CREATE TRIGGER "APP"."TRG" AFTER DELETE ON "APP"."ORDERS" FOR EACH ROW BEGIN DELETE FROM "APP"."AUDIT" WHERE ID=:OLD.ID; END;`
const manualTriggerRevised = `CREATE OR REPLACE TRIGGER "APP"."TRG" AFTER DELETE ON "APP"."ORDERS" FOR EACH ROW BEGIN IF :OLD.ID > 0 THEN DELETE FROM "APP"."AUDIT" WHERE ID=:OLD.ID; END IF; END;`

func manualTriggerFixture() repair.Object {
	return repair.Object{ID: "trigger", Kind: "trigger", Name: "TRG", TargetName: "TRG", Schema: "APP", Table: "ORDERS", SourceTable: "ORDERS", Status: "failed", OriginalStatus: "failed", OriginalError: "original compile failure", Error: "original compile failure", DDL: manualTriggerOriginal, RequiresReview: true, Units: []repair.Unit{{ID: "create", SQL: manualTriggerOriginal, OriginalSQL: manualTriggerOriginal, Status: "failed", Selected: true}}}
}

func manualTriggerNativeFixture() repair.Object {
	o := manualTriggerFixture()
	o.Native = &damengnative.Object{ID: o.ID, Kind: o.Kind, Schema: o.Schema, Name: o.TargetName, Table: o.Table, Status: "DISABLED", Units: []damengnative.Unit{{SQL: manualTriggerOriginal, Phase: "after_data"}, {SQL: `ALTER TRIGGER "APP"."TRG" COMPILE`, Phase: "compile"}, {SQL: `ALTER TRIGGER "APP"."TRG" DISABLE`, Phase: "restore"}}}
	o.NativeFingerprint = damengnative.Fingerprint(o.Native)
	o.Units = nil
	for i, unit := range o.Native.Units {
		o.Units = append(o.Units, repair.Unit{ID: string(rune('a' + i)), SQL: unit.SQL, OriginalSQL: unit.SQL, Status: "failed", Selected: true})
	}
	o.Units[0].Status = "succeeded" // A failed compile may follow a successful CREATE.
	return o
}

func TestManualTriggerDraftAllOriginsPreserveProvenance(t *testing.T) {
	for _, origin := range []string{"ordinary", "legacy", "generated", "native"} {
		t.Run(origin, func(t *testing.T) {
			o := manualTriggerFixture()
			switch origin {
			case "legacy":
				o.Legacy, o.DiagnosticOnly, o.Units = true, true, nil
			case "generated":
				o.MySQLDamengTrigger = &mysqldmtrigger.Plan{}
			case "native":
				o = manualTriggerNativeFixture()
			}
			before := repairJSON(o)
			candidate, err := prepareManualTriggerDraft(&store.DataMigrationJob{DstDBType: "dameng"}, o, []repair.Unit{{SQL: manualTriggerRevised, Status: "succeeded", OriginalSQL: "forged"}}, true)
			require.NoError(t, err)
			require.Equal(t, before, repairJSON(o), "preparing a draft must not mutate the original plan")
			require.True(t, candidate.TrustedManualTrigger())
			require.Equal(t, 1, candidate.ManualTrigger.Revision)
			require.Equal(t, before, repairJSON(*candidate.ManualTrigger.Original))
			require.Equal(t, o.OriginalError, candidate.OriginalError)
			require.Equal(t, o.DDL, candidate.DDL)
			require.Nil(t, candidate.MySQLDamengTrigger)
			require.False(t, candidate.DiagnosticOnly)
			require.False(t, candidate.RequiresReview)
			for _, unit := range candidate.Units {
				require.Equal(t, "pending", unit.Status)
				require.Equal(t, unit.SQL, unit.OriginalSQL)
			}
			if origin == "native" {
				require.True(t, candidate.TrustedNative())
				require.Equal(t, "DISABLED", candidate.Native.Status)
				require.Len(t, candidate.Units, 3)
				require.Equal(t, manualTriggerRevised, candidate.Native.Units[0].SQL)
				require.Equal(t, o.Native.Units[2], candidate.Native.Units[2])
			}
			require.NoError(t, validateRepairObjectScope(candidate, candidate.Units, "dameng"))
			require.Error(t, validateRepairObjectScope(candidate, candidate.Units, "oracle"))
		})
	}
}

func TestManualTriggerDraftRejectsUnsafeIdentityAndUncertainState(t *testing.T) {
	for _, sql := range []string{
		`CREATE TRIGGER "OTHER"."TRG" AFTER DELETE ON "APP"."ORDERS" BEGIN NULL; END;`,
		`CREATE TRIGGER "APP"."OTHER" AFTER DELETE ON "APP"."ORDERS" BEGIN NULL; END;`,
		`CREATE TRIGGER "APP"."TRG" AFTER DELETE ON "APP"."OTHER" BEGIN NULL; END;`,
		`CREATE TRIGGER "APP"."TRG" AFTER LOGON ON DATABASE BEGIN NULL; END;`,
		`CREATE TRIGGER "APP"."TRG" INSTEAD OF DELETE ON "APP"."ORDERS" BEGIN NULL; END;`,
		manualTriggerRevised + ` DELETE FROM "APP"."ORDERS";`,
	} {
		_, err := prepareManualTriggerDraft(&store.DataMigrationJob{DstDBType: "dameng"}, manualTriggerFixture(), []repair.Unit{{SQL: sql}}, true)
		require.Error(t, err, sql)
	}
	for _, state := range []string{"unknown", "running", "queued", "succeeded", "unverified"} {
		o := manualTriggerFixture()
		o.Status = state
		_, err := prepareManualTriggerDraft(&store.DataMigrationJob{DstDBType: "dameng"}, o, []repair.Unit{{SQL: manualTriggerRevised}}, true)
		require.Error(t, err, state)
	}
	for _, target := range []string{"oracle", "postgres", "mysql"} {
		_, err := prepareManualTriggerDraft(&store.DataMigrationJob{DstDBType: target}, manualTriggerFixture(), []repair.Unit{{SQL: manualTriggerRevised}}, true)
		require.Error(t, err, target)
	}
	o := manualTriggerFixture()
	o.Units[0].Status = "unknown"
	require.False(t, canEditManualTrigger(&store.DataMigrationJob{DstDBType: "dameng"}, o))
}

func TestManualTriggerDraftAPIVersionsAndLegacyReport(t *testing.T) {
	seedHandlerRepair(t)
	require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Where("job_id = ?", "job").Updates(map[string]any{"dst_db_type": "dameng", "dst_schema": "APP"}).Error)
	require.NoError(t, store.DB.Model(&store.MigrationRepairBinding{}).Where("job_id = ?", "job").Update("target_schema", "APP").Error)
	o := manualTriggerFixture()
	o.DiagnosticOnly, o.Legacy, o.Units = true, true, nil
	report := datamigrate.MigrationReport{TriggerMigrationEnabled: true, Triggers: datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "TRG", Table: "ORDERS", DDL: manualTriggerOriginal, Error: o.OriginalError}}}}
	reportJSON := repairJSON(report)
	require.NoError(t, store.CreateDataMigrationReport(&store.DataMigrationReport{JobID: "job", ReportJSON: reportJSON}))
	require.NoError(t, store.DB.Create(&store.MigrationRepairObject{JobID: "job", ObjectID: o.ID, Version: 1, ObjectJSON: repairJSON(o)}).Error)
	request := func(version int, sql string) string {
		return repairJSON(map[string]any{"mode": "manual_trigger", "version": version, "reviewed": true, "units": []repair.Unit{{SQL: sql}}})
	}
	path := "/jobs/job/repairs/trigger"
	first := repairRequest(t, http.MethodPut, path, request(1, manualTriggerRevised), 1, SaveMigrationRepairDraft)
	require.Equal(t, 200, first.Code, first.Body.String())
	var saved struct {
		Object  repair.Object `json:"object"`
		Version int           `json:"version"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &saved))
	require.Equal(t, 2, saved.Version)
	require.True(t, saved.Object.ManualTriggerEditable)
	require.True(t, saved.Object.TrustedManualTrigger())
	require.Equal(t, 409, repairRequest(t, http.MethodPut, path, request(1, manualTriggerOriginal), 1, SaveMigrationRepairDraft).Code)
	second := repairRequest(t, http.MethodPut, path, request(2, manualTriggerOriginal), 1, SaveMigrationRepairDraft)
	require.Equal(t, 200, second.Code, second.Body.String())
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &saved))
	require.Equal(t, 2, saved.Object.ManualTrigger.Revision)
	require.Equal(t, manualTriggerOriginal, saved.Object.ManualTrigger.Original.DDL)
	require.Equal(t, []string{manualTriggerRevised}, saved.Object.ManualTrigger.PreviousSQL)
	get := repairRequest(t, http.MethodGet, path, "", 1, GetMigrationRepair)
	require.Equal(t, 200, get.Code, get.Body.String())
	require.NoError(t, json.Unmarshal(get.Body.Bytes(), &saved))
	require.False(t, saved.Object.DiagnosticOnly, "legacy references must not demote an approved manual revision")
	require.True(t, saved.Object.TrustedManualTrigger())
	originalReport, err := store.GetDataMigrationReport("job")
	require.NoError(t, err)
	require.Equal(t, reportJSON, originalReport.ReportJSON)
}

func TestManualTriggerDependencyDataAndSavedColumnMapping(t *testing.T) {
	seedHandlerRepair(t)
	o := manualTriggerFixture()
	o.ManualTrigger = &repair.ManualTriggerPlan{}
	table := repair.Object{ID: "parent", Kind: "table", Table: "ORDERS", TargetName: "ORDERS", Schema: "APP", Status: "succeeded", DataStatus: "pending", ColumnMapping: map[string]string{"OLD_ID": "ID"}}
	require.NoError(t, store.DB.Create(&store.MigrationRepairObject{JobID: "job", ObjectID: table.ID, Version: 1, ObjectJSON: repairJSON(table)}).Error)
	require.NoError(t, bindManualTriggerColumnMapping("job", &o))
	require.Equal(t, table.ColumnMapping, o.ColumnMapping)
	require.Contains(t, o.Dependencies, "parent")
	require.ErrorContains(t, validateMigrationRepairDependencies("job", o), "数据尚未完成")
	table.DataStatus = "succeeded"
	require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "parent").Update("object_json", repairJSON(table)).Error)
	require.NoError(t, validateMigrationRepairDependencies("job", o))
	table.Status, table.DataStatus = "unknown", "not_required"
	require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "parent").Update("object_json", repairJSON(table)).Error)
	require.NoError(t, validateMigrationRepairDependencies("job", o), "object-only trigger jobs can use an existing table's metadata-only record")
}

func TestManualTriggerLegacyMalformedBodyStillExposesEditableTableIdentity(t *testing.T) {
	for _, suffix := range []string{"BEGIN PRINT 'unterminated", "BEGIN /* unfinished", "BEGIN PRINT 'INSTEAD OF';"} {
		o := manualTriggerFixture()
		o.Table, o.Units = "", nil
		o.DiagnosticOnly, o.Legacy = true, true
		o.DDL = `CREATE TRIGGER "APP"."TRG" AFTER DELETE ON "APP"."ORDERS" ` + suffix
		j := &store.DataMigrationJob{DstDBType: "dameng"}
		require.True(t, canEditManualTrigger(j, o), suffix)
		require.Equal(t, "ORDERS", repairObjectResponse(j, o).Table)
		candidate, err := prepareManualTriggerDraft(j, o, []repair.Unit{{SQL: manualTriggerRevised}}, true)
		require.NoError(t, err, suffix)
		require.True(t, candidate.TrustedManualTrigger())
		require.Equal(t, o.DDL, candidate.ManualTrigger.Original.DDL)
	}
}
