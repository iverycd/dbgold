package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDamengNativeCapabilities(t *testing.T) {
	require.True(t, isSupportedPair("dameng", "dameng"))
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	GetSupportedPairs(c)
	var pairs []SupportedPair
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &pairs))
	count := 0
	for _, pair := range pairs {
		if isDamengNativePair(pair.Source, pair.Target) {
			count++
			require.True(t, pair.SupportsFullSchema)
			require.True(t, pair.SupportsTriggers)
		}
	}
	require.Equal(t, 1, count)
}

func TestDamengNativeOptionsPreserveDefinitionsAndOverwriteChoice(t *testing.T) {
	enabled := true
	req := startDataMigrationRequest{MigrateMode: "all", OverwriteExisting: true, LowerCaseNames: true, CharInLength: true, UseNvarchar2: true, Distributed: true, ChangeOwner: &enabled, StripViewSchemas: "APP"}
	require.NoError(t, normalizeDamengNativeRequest(&req))
	require.Equal(t, "both", req.MigrateContent)
	require.True(t, req.MigrateTriggers)
	require.True(t, req.OverwriteExisting)
	require.False(t, req.LowerCaseNames)
	require.False(t, req.CharInLength)
	require.False(t, req.UseNvarchar2)
	require.False(t, req.Distributed)
	require.NotNil(t, req.ChangeOwner)
	require.False(t, *req.ChangeOwner)
	require.Empty(t, req.StripViewSchemas)
}

func TestDamengNativePartialRequestsDoNotCreateJobs(t *testing.T) {
	cases := []struct {
		name, body string
		handler    gin.HandlerFunc
	}{
		{"filtered", `{"src_conn_id":1,"dst_conn_id":2,"migrate_mode":"include","table_filter":"T"}`, StartDataMigration},
		{"schema only", `{"src_conn_id":1,"dst_conn_id":2,"migrate_mode":"all","migrate_content":"schema_only"}`, StartDataMigration},
		{"excluded", `{"src_conn_id":1,"dst_conn_id":2,"migrate_mode":"all","excluded_objects":[{"kind":"table","name":"T"}]}`, StartDataMigration},
		{"views", `{"src_conn_id":1,"dst_conn_id":2,"view_names":["V"]}`, MigrateViews},
		{"objects", `{"src_conn_id":1,"dst_conn_id":2,"migrate_objects":["comments"],"table_names":["T"]}`, StartObjectMigration},
		{"partial preflight", `{"src_conn_id":1,"dst_conn_id":2,"migrate_mode":"all","migrate_objects":["indexes"]}`, PreflightDataMigration},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			migrationHandlerTestDB(t)
			require.NoError(t, store.CreateConnection(&store.Connection{OwnerID: 7, DBType: "dameng", Name: "src"}))
			require.NoError(t, store.CreateConnection(&store.Connection{OwnerID: 7, DBType: "dameng", Name: "dst"}))
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Set("userID", uint(7))
			c.Set("role", "user")
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			tc.handler(c)
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "Schema")
			var count int64
			require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}

func TestDamengNativeSourceOwnerSnapshotIsExact(t *testing.T) {
	job := store.DataMigrationJob{DstDBType: "dameng"}
	migrationSourceLocation(&job, &store.Connection{DBType: "dameng", Database: "DefaultOwner"}, "MixedOwner")
	require.Equal(t, "MixedOwner", job.SourceSchema)
	binding := initialRepairBinding(&job, "{}", false)
	require.Equal(t, "MixedOwner", sourceOverride(binding, "dameng"))
}

func TestDamengNativeBatchOverwriteDefaultAndOptIn(t *testing.T) {
	for _, raw := range []string{"", "overwrite_existing=false", "overwrite_existing=true"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
		c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		require.Equal(t, raw == "overwrite_existing=true", parseBatchOptions(c).OverwriteExisting)
	}
}

func TestDamengNativeRepairOnlyAcceptsFrozenObject(t *testing.T) {
	sql := `CREATE OR REPLACE PROCEDURE "Dst"."P" AS BEGIN NULL; END;`
	native := damengnative.Object{ID: "procedure", Kind: "procedure", Name: "P", Schema: "Dst", Units: []damengnative.Unit{{SQL: sql, Phase: "after_data"}}}
	object := repair.Object{ID: "procedure", Kind: "procedure", TargetName: "P", Schema: "Dst", Native: &native, NativeFingerprint: damengnative.Fingerprint(native), Units: []repair.Unit{{ID: "create", SQL: sql, OriginalSQL: sql, Selected: true}}}
	require.NoError(t, validateRepairObjectScope(object, object.Units, "dameng"))
	require.Error(t, validateRepairObjectScope(object, object.Units, "postgres"))
	edited := append([]repair.Unit(nil), object.Units...)
	edited[0].SQL += " DROP TABLE X"
	require.Error(t, validateRepairObjectScope(object, edited, "dameng"))
	object.NativeFingerprint = "changed"
	require.Error(t, validateRepairObjectScope(object, object.Units, "dameng"))
}

func TestDamengNativeRecoverySynchronizesOnlySuccessfulExactUnits(t *testing.T) {
	object := repair.Object{Native: &damengnative.Object{}, Status: "failed", DataStatus: "succeeded", Units: []repair.Unit{{SQL: "CREATE TABLE T", OriginalSQL: "CREATE TABLE T", Status: "succeeded"}, {SQL: "ALTER TABLE T ENABLE CONSTRAINT C", OriginalSQL: "ALTER TABLE T ENABLE CONSTRAINT C", Status: "pending", Selected: true}}}
	syncNativeRecoveryUnits(&object, []repair.Unit{{SQL: "ALTER TABLE T ENABLE CONSTRAINT C", Status: "unknown"}, {SQL: "ALTER TABLE Other ENABLE CONSTRAINT C", Status: "succeeded"}})
	require.Equal(t, "pending", object.Units[1].Status)
	syncNativeRecoveryUnits(&object, []repair.Unit{{SQL: "ALTER TABLE T ENABLE CONSTRAINT C", Status: "succeeded"}})
	require.Equal(t, "succeeded", object.Units[1].Status)
	require.False(t, object.Units[1].Selected)
	require.Equal(t, "succeeded", object.Status)
}

func TestDamengNativeUnknownRetryRequiresFrozenPlanAndConnectionBinding(t *testing.T) {
	for _, changed := range []bool{false, true} {
		seedHandlerRepair(t)
		sql := `CREATE OR REPLACE PROCEDURE "Dst"."P" AS BEGIN NULL; END;`
		native := damengnative.Object{ID: "table", Kind: "procedure", Name: "P", Schema: "Dst", Units: []damengnative.Unit{{SQL: sql, Phase: "after_data"}}}
		object := repair.Object{ID: "table", Kind: "procedure", TargetName: "P", Schema: "Dst", Status: "unknown", Native: &native, NativeFingerprint: damengnative.Fingerprint(native), Units: []repair.Unit{{ID: "create", SQL: sql, OriginalSQL: sql, Status: "unknown"}}}
		if changed {
			object.Units[0].SQL += " DROP TABLE X"
		}
		require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Where("job_id = ?", "job").Updates(map[string]any{"src_db_type": "dameng", "dst_db_type": "dameng", "dst_schema": "Dst"}).Error)
		require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "table").Update("object_json", repairJSON(object)).Error)
		response := repairRequest(t, http.MethodPost, "/jobs/job/repairs/table", `{"version":1,"request_id":"retry","confirmed":true}`, 1, ExecuteMigrationRepair)
		require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		if changed {
			require.Contains(t, response.Body.String(), "不是可执行 DDL 的目标对象", "被修改的原生单元必须在调度前拒绝")
		} else {
			require.Contains(t, response.Body.String(), "请绑定有权限的原目标连接", "可信未知计划必须继续核对原连接，不能在 API 层被一律拒绝")
		}
		var attempts int64
		require.NoError(t, store.DB.Model(&store.MigrationRepairAttempt{}).Count(&attempts).Error)
		require.Zero(t, attempts, "缺少原连接或计划被修改时不能调度 SQL")
	}
}

func TestDamengNativeDependencyPolicyRemainsPausedUntilWholeJobProof(t *testing.T) {
	seedHandlerRepair(t)
	dependency := nativeRecoveryFixture("MV", "after_data", "mv_history", "mv_restore")
	dependency.Status = "pending"
	dependency.Units[0].Status, dependency.Units[1].Status = "succeeded", "succeeded"
	row := store.MigrationRepairObject{JobID: "job", ObjectID: dependency.ID, ObjectJSON: repairJSON(dependency), Version: 1}
	require.NoError(t, store.DB.Create(&row).Error)
	object := nativeRecoveryFixture("Dependent", "after_data")
	object.Dependencies = []string{dependency.ID}
	require.NoError(t, validateMigrationRepairDependencies("job", object), "不能要求先恢复自动刷新，才能继续安装依赖对象")
	dependency.Units[1].Status = "unknown"
	require.NoError(t, store.DB.Model(&row).Update("object_json", repairJSON(dependency)).Error)
	require.ErrorContains(t, validateMigrationRepairDependencies("job", object), "依赖尚未成功")
	dependency.Units[1].Status, dependency.DataStatus = "succeeded", "failed"
	require.NoError(t, store.DB.Model(&row).Update("object_json", repairJSON(dependency)).Error)
	require.ErrorContains(t, validateMigrationRepairDependencies("job", object), "数据尚未迁移完成")
}
