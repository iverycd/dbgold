package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func TestNativeViewDraftPersistsFrozenIdentityAndResumesCompile(t *testing.T) {
	seedHandlerRepair(t)
	require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Where("job_id=?", "job").Updates(map[string]any{"src_db_type": "dameng", "dst_db_type": "dameng", "dst_schema": "APP"}).Error)
	require.NoError(t, store.DB.Model(&store.MigrationRepairBinding{}).Where("job_id=?", "job").Update("target_schema", "APP").Error)
	const original = `CREATE VIEW "APP"."V" AS SELECT 0 FROM DUAL`
	const edited = `CREATE OR REPLACE VIEW "APP"."V" AS SELECT 1 FROM DUAL UNION ALL SELECT 2 FROM DUAL`
	const compile = `ALTER VIEW "APP"."V" COMPILE`
	n := damengnative.Object{ID: "view", Kind: "view", Name: "V", Schema: "APP", Dependencies: []string{"source-table"}, CompileSQL: compile, Units: []damengnative.Unit{{SQL: original, Phase: "after_data"}, {SQL: compile, Phase: "compile"}}}
	o := repair.Object{ID: n.ID, Kind: n.Kind, TargetName: n.Name, Schema: n.Schema, Native: &n, Dependencies: n.Dependencies, NativeFingerprint: damengnative.Fingerprint(n), DDL: original + "\n" + compile, OriginalError: "自动创建失败", Status: "failed", Units: []repair.Unit{{SQL: original, OriginalSQL: original, Status: "failed"}, {SQL: compile, OriginalSQL: compile, Status: "pending"}}}
	require.NoError(t, store.DB.Create(&store.MigrationRepairObject{JobID: "job", ObjectID: o.ID, Version: 1, ObjectJSON: repairJSON(o)}).Error)
	path := "/jobs/job/repairs/view"
	save := func(version int, sql string, want int) repair.Object {
		result := repairRequest(t, http.MethodPut, path, repairJSON(map[string]any{"mode": "native_view", "version": version, "reviewed": true, "units": []repair.Unit{{SQL: sql, Status: "succeeded"}}}), 1, SaveMigrationRepairDraft)
		require.Equal(t, want, result.Code, result.Body.String())
		var resp struct{ Object repair.Object }
		require.NoError(t, json.Unmarshal(result.Body.Bytes(), &resp))
		return resp.Object
	}
	for _, invalid := range []string{`CREATE VIEW "other"."V" AS SELECT 1`, `CREATE VIEW "Other" AS SELECT 1`, `CREATE MATERIALIZED VIEW "V" AS SELECT 1`, `CREATE VIEW "V" AS SELECT 1; DROP VIEW "V"`} {
		save(1, invalid, http.StatusBadRequest)
	}
	current := save(1, edited, http.StatusOK)
	require.True(t, current.NativeViewEditable)
	require.True(t, current.TrustedNative())
	require.Equal(t, o.NativeFingerprint, current.NativeViewDraft.OriginalFingerprint)
	require.Equal(t, o.OriginalError, current.OriginalError)
	require.Equal(t, o.DDL, current.DDL)
	require.Equal(t, o.Dependencies, current.Dependencies)
	require.Equal(t, "pending", current.Units[0].Status)
	require.ErrorContains(t, validateMigrationRepairDependencies("job", current), "依赖尚未成功")
	current.Status = "failed"
	current.Units[0].Status = "succeeded"
	current.Units[1].Status = "failed"
	require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id=? AND object_id=?", "job", o.ID).Update("object_json", repairJSON(current)).Error)
	current = save(2, edited, http.StatusOK)
	require.Equal(t, "succeeded", current.Units[0].Status)
	require.False(t, current.Units[0].Selected)
	require.True(t, current.Units[1].Selected)
	require.Equal(t, edited, current.NativeViewDraft.InstalledSQL)
	for _, kind := range []string{"table", "materialized_view", "view_log", "sequence", "trigger"} {
		sibling := current
		sibling.Kind = kind
		require.False(t, sibling.CanEditNativeView(), kind)
	}
}
