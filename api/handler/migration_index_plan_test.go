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

type indexPlanTestWriter struct {
	target.Writer
	oid   uint32
	calls int
}

func (*indexPlanTestWriter) Dialect() dialect.Dialect { return dialect.NewPostgres("gaussdb") }
func (*indexPlanTestWriter) Close() error             { return nil }
func (w *indexPlanTestWriter) PlanGaussDBIndex(_ context.Context, idx source.IndexInfo) (source.IndexInfo, error) {
	w.calls++
	idx.GaussDB = &source.GaussDBIndexPlan{Base: source.GaussDBDistribution{OID: w.oid, Schema: "App", Table: "Patients", Mode: "H", Columns: []string{"patient_id"}}, Global: true, Columns: []string{"auto_id"}}
	return idx, nil
}
func seedIndexPlanHandler(t *testing.T) *indexPlanTestWriter {
	t.Helper()
	require.NoError(t, store.InitWithError(&config.Config{SQLitePath: ":memory:"}))
	dst := &store.Connection{OwnerID: 1, Name: "Target", DBType: "gaussdb", Host: "127.0.0.1", Port: 5432, Database: "test", Username: "USER"}
	require.NoError(t, store.CreateConnection(dst))
	require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{JobID: "job", OwnerID: 1, Status: "failed", SrcDBType: "mysql", DstDBType: "gaussdb", DstConnID: dst.ID, DstSchema: "App", DstConnHost: dst.Host, DstConnPort: dst.Port, DstConnDatabase: dst.Database, DstConnUsername: dst.Username}))
	idx := source.IndexInfo{TableName: "Patients", IndexName: "auto_id", Columns: []string{"auto_id"}, IsUnique: true}
	ddl := dialect.NewPostgres("gaussdb").IndexStatements("App", idx)[0].SQL
	obj := repair.Object{ID: "idx", Kind: "index", Name: "auto_id", TargetName: "auto_id", Table: "Patients", Schema: "App", Status: "failed", OriginalStatus: "failed", OriginalError: "Cannot create index", Index: &idx, Units: []repair.Unit{{ID: "create", SQL: ddl, OriginalSQL: ddl, Status: "failed", Selected: true}}}
	require.NoError(t, store.SeedMigrationRepairs(&store.MigrationRepairBinding{JobID: "job", DstConnID: dst.ID, TargetSchema: "App", TargetFingerprint: connectionFingerprint(dst), Configured: true, Version: 1}, []store.MigrationRepairObject{{JobID: "job", ObjectID: "idx", ObjectJSON: repairJSON(obj), Version: 1}}))
	w := &indexPlanTestWriter{oid: 42}
	old := newMigrationIndexPlanWriter
	newMigrationIndexPlanWriter = func(*store.Connection, string, target.ConnPoolConfig) (target.Writer, error) { return w, nil }
	t.Cleanup(func() { newMigrationIndexPlanWriter = old })
	return w
}
func TestGaussDBIndexPlanRefreshAndSaveAreReadOnly(t *testing.T) {
	w := seedIndexPlanHandler(t)
	before, err := store.GetMigrationRepairObject("job", "idx")
	require.NoError(t, err)
	response := repairRequest(t, http.MethodPost, "/jobs/job/repairs/idx", `{"version":1}`, 1, RefreshMigrationIndexPlan)
	require.Equal(t, 200, response.Code, response.Body.String())
	var candidate struct {
		Object  repair.Object
		Version int
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &candidate))
	require.Contains(t, candidate.Object.Units[0].SQL, "CREATE GLOBAL UNIQUE INDEX")
	after, err := store.GetMigrationRepairObject("job", "idx")
	require.NoError(t, err)
	require.Equal(t, before.ObjectJSON, after.ObjectJSON)
	body := map[string]any{"version": 1, "units": candidate.Object.Units, "index_plan": true}
	response = repairRequest(t, http.MethodPut, "/jobs/job/repairs/idx", repairJSON(body), 1, SaveMigrationRepairDraft)
	require.Equal(t, 400, response.Code)
	body["reviewed"] = true
	response = repairRequest(t, http.MethodPut, "/jobs/job/repairs/idx", repairJSON(body), 1, SaveMigrationRepairDraft)
	require.Equal(t, 200, response.Code, response.Body.String())
	var saved struct{ Object repair.Object }
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &saved))
	require.NotNil(t, saved.Object.Index.GaussDB)
	require.Equal(t, "Cannot create index", saved.Object.OriginalError)
	require.Equal(t, 2, w.calls)
	var attempts int64
	require.NoError(t, store.DB.Model(&store.MigrationRepairAttempt{}).Count(&attempts).Error)
	require.Zero(t, attempts)
}
func TestGaussDBIndexPlanSaveRejectsSameSQLAfterTableRecreation(t *testing.T) {
	w := seedIndexPlanHandler(t)
	r := repairRequest(t, http.MethodPost, "/jobs/job/repairs/idx", `{"version":1}`, 1, RefreshMigrationIndexPlan)
	require.Equal(t, 200, r.Code, r.Body.String())
	var candidate struct{ Object repair.Object }
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &candidate))
	w.oid = 99
	r = repairRequest(t, http.MethodPut, "/jobs/job/repairs/idx", repairJSON(map[string]any{"version": 1, "units": candidate.Object.Units, "index_plan": true, "reviewed": true}), 1, SaveMigrationRepairDraft)
	require.Equal(t, 400, r.Code, r.Body.String())
	row, err := store.GetMigrationRepairObject("job", "idx")
	require.NoError(t, err)
	require.Equal(t, 1, row.Version)
}
func TestGaussDBIndexPlanCannotOverwriteManualOrUnknownWork(t *testing.T) {
	for _, status := range []string{"unknown", "running", "succeeded", "unverified"} {
		require.Error(t, checkIndexPlanRefreshable(repair.Object{Status: status}))
	}
	require.Error(t, checkIndexPlanRefreshable(repair.Object{Status: "failed", Units: []repair.Unit{{SQL: "edited", OriginalSQL: "original"}}}))
}
func TestGaussDBGlobalIndexScopeValidation(t *testing.T) {
	obj := repair.Object{Kind: "index", Schema: "App", Table: "Patients", TargetName: "auto_id"}
	unit := repair.Unit{SQL: `CREATE GLOBAL UNIQUE INDEX "auto_id" ON "App"."Patients" USING ubtree ("auto_id") DISTRIBUTE BY HASH ("auto_id");`, Selected: true}
	require.NoError(t, validateRepairObjectScope(obj, []repair.Unit{unit}, "gaussdb"))
	require.Error(t, validateRepairObjectScope(obj, []repair.Unit{unit}, "postgres"))
	obj.Table = "Wrong"
	require.Error(t, validateRepairObjectScope(obj, []repair.Unit{unit}, "gaussdb"))
	obj.Kind = "distribution"
	require.ErrorContains(t, validateRepairObjectScope(obj, []repair.Unit{unit}, "gaussdb"), "原地修改")
}
