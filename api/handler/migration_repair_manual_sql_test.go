package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/target"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func TestManualSQLDraftExecutesSavedTextOnOwnedTarget(t *testing.T) {
	seedHandlerRepair(t)
	db, err := store.DB.DB()
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	dst := &store.Connection{OwnerID: 2, DBType: "dameng", Host: "127.0.0.1", Port: 5236}
	require.NoError(t, store.CreateConnection(dst))
	require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Where("job_id = ?", "job").Updates(map[string]any{"src_db_type": "mysql", "dst_db_type": dst.DBType}).Error)
	require.NoError(t, store.DB.Model(&store.MigrationRepairBinding{}).Where("job_id = ?", "job").Updates(map[string]any{"dst_conn_id": dst.ID, "target_schema": "APP"}).Error)
	const path = "/jobs/job/repairs/view"
	const original = `CREATE OR REPLACE VIEW "APP"."ORIGINAL" AS SELECT 1 FROM DUAL`
	const sql = "  CREATE OR REPLACE force VIEW \"APP\".\"EDITED\" AS\nSELECT 'x;y' FROM DUAL;\n"
	obj := repair.Object{ID: "view", Kind: "view", Schema: "APP", TargetName: "original", Status: "failed", DDL: original,
		Units: []repair.Unit{{ID: "create", SQL: original, OriginalSQL: original, Status: "failed"}}}
	require.NoError(t, store.DB.Create(&store.MigrationRepairObject{JobID: "job", ObjectID: obj.ID, Version: 1, ObjectJSON: repairJSON(obj)}).Error)
	body := repairJSON(map[string]any{"version": 1, "mode": "manual_sql", "reviewed": true, "units": []repair.Unit{{ID: "create", Selected: true, SQL: sql, Status: "succeeded", OriginalSQL: "client original"}}})
	require.Equal(t, http.StatusNotFound, repairRequest(t, http.MethodPut, path, body, 2, SaveMigrationRepairDraft).Code)
	res := repairRequest(t, http.MethodPut, path, body, 1, SaveMigrationRepairDraft)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	require.Equal(t, http.StatusConflict, repairRequest(t, http.MethodPut, path, body, 1, SaveMigrationRepairDraft).Code)
	row, err := store.GetMigrationRepairObject("job", "view")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &obj))
	require.True(t, obj.ManualSQL)
	require.Equal(t, sql, obj.Units[0].SQL)
	require.Equal(t, original, obj.Units[0].OriginalSQL)
	require.Equal(t, "failed", obj.Units[0].Status)
	req := `{"version":2,"confirmed":true,"request_id":"manual"}`
	res = repairRequest(t, http.MethodPost, path, req, 1, ExecuteMigrationRepair)
	require.Equal(t, http.StatusBadRequest, res.Code)
	require.Contains(t, res.Body.String(), "有权限的原目标连接")

	require.NoError(t, store.DB.Model(dst).Update("owner_id", 1).Error)
	before := newMigrationRepairWriter
	t.Cleanup(func() { newMigrationRepairWriter = before })
	newMigrationRepairWriter = func(c *store.Connection, schema string, _ target.ConnPoolConfig) (target.Writer, error) {
		if c.ID != dst.ID || schema != obj.Schema {
			return nil, errors.New("unexpected target")
		}
		return nil, errors.New("target connection error")
	}
	res = repairRequest(t, http.MethodPost, path, req, 1, ExecuteMigrationRepair)
	require.Equal(t, http.StatusAccepted, res.Code, res.Body.String())
	var attempt store.MigrationRepairAttempt
	require.Eventually(t, func() bool {
		return store.DB.First(&attempt, "job_id = ? AND request_id = ?", "job", "manual").Error == nil && attempt.Status != "running"
	}, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, "target connection error", attempt.Error)
	var snapshot struct{ Object repair.Object }
	require.NoError(t, json.Unmarshal([]byte(attempt.SnapshotJSON), &snapshot))
	require.Equal(t, obj.Units, snapshot.Object.Units)
	require.True(t, snapshot.Object.ManualSQL)
	require.Equal(t, http.StatusAccepted, repairRequest(t, http.MethodPost, path, req, 1, ExecuteMigrationRepair).Code)
}

func TestManualSQLDraftPreservesCommittedStepsAndNativeProvenance(t *testing.T) {
	seedHandlerRepair(t)
	require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Where("job_id = ?", "job").Updates(map[string]any{"src_db_type": "dameng", "dst_db_type": "dameng"}).Error)
	o := manualTriggerNativeFixture()
	require.NoError(t, store.DB.Create(&store.MigrationRepairObject{JobID: "job", ObjectID: o.ID, Version: 1, ObjectJSON: repairJSON(o)}).Error)
	const path = "/jobs/job/repairs/trigger"
	save := func(version int, units []repair.Unit, status int) repair.Object {
		body := repairJSON(map[string]any{"version": version, "mode": "manual_sql", "reviewed": true, "units": units})
		res := repairRequest(t, http.MethodPut, path, body, 1, SaveMigrationRepairDraft)
		require.Equal(t, status, res.Code, res.Body.String())
		var reply struct{ Object repair.Object }
		require.NoError(t, json.Unmarshal(res.Body.Bytes(), &reply))
		return reply.Object
	}
	newUnit := repair.Unit{ID: "next", SQL: "BEGIN\n  NULL;\nEND;", Selected: true}
	save(1, []repair.Unit{newUnit}, http.StatusBadRequest)
	committed := o.Units[0]
	committed.SQL = "changed"
	save(1, []repair.Unit{committed, newUnit}, http.StatusBadRequest)
	committed = o.Units[0]
	committed.Selected = true
	current := save(1, []repair.Unit{committed, newUnit}, http.StatusOK)
	require.Equal(t, o.Native, current.Native)
	require.Equal(t, o.NativeFingerprint, current.NativeFingerprint)
	require.True(t, current.ManualSQL)
	require.False(t, current.ManualTriggerEditable)
	require.True(t, current.CanExecuteDDL())
	require.False(t, current.Units[0].Selected)
	require.Equal(t, "succeeded", current.Units[0].Status)
	current.Units[1].SQL = "SELECT 1 FROM DUAL"
	current = save(2, current.Units, http.StatusOK)
	require.Equal(t, "SELECT 1 FROM DUAL", current.Units[1].SQL)
	require.NoError(t, prepareRepairExecution(&store.DataMigrationJob{}, &store.MigrationRepairBinding{TargetSchema: o.Schema}, &current))
	require.False(t, current.Units[0].Selected)
}

func TestManualSQLCannotPromoteReadOnlyObject(t *testing.T) {
	seedHandlerRepair(t)
	for _, o := range []repair.Object{
		{ID: "data", Kind: "data", Schema: "app", TargetName: "orders"},
		{ID: "native", Kind: "table", Schema: "app", TargetName: "orders", Native: &damengnative.Object{}},
		{ID: "diagnostic", Kind: "table", Schema: "app", TargetName: "metadata", DiagnosticOnly: true},
		{ID: "uncertain", Kind: "view", Schema: "app", TargetName: "v", Status: "unknown"},
	} {
		require.NoError(t, store.DB.Create(&store.MigrationRepairObject{JobID: "job", ObjectID: o.ID, Version: 1, ObjectJSON: repairJSON(o)}).Error)
		res := repairRequest(t, http.MethodPut, "/jobs/job/repairs/"+o.ID, `{"version":1,"mode":"manual_sql","manual_sql":true,"units":[{"id":"new","sql":"SELECT 1","selected":true}]}`, 1, SaveMigrationRepairDraft)
		require.Equal(t, http.StatusBadRequest, res.Code, o.ID)
	}
}
