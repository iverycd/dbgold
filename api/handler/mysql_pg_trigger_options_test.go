package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMySQLPGTriggerCapabilityAndValidation(t *testing.T) {
	for _, dst := range []string{"postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		require.True(t, supportsTriggerMigration("mysql", dst))
		require.NoError(t, validateTriggerMigration("mysql", dst, "both", true))
		require.NoError(t, validateTriggerMigration("mysql", dst, "schema_only", true))
		require.ErrorContains(t, validateTriggerMigration("mysql", dst, "data_only", true), "仅迁移数据")
	}
	for _, pair := range [][2]string{{"oracle", "postgres"}, {"mysql", "mysql"}, {"mysql", "dameng"}, {"mysql", "oscar"}} {
		require.False(t, supportsTriggerMigration(pair[0], pair[1]))
		require.Error(t, validateTriggerMigration(pair[0], pair[1], "both", true))
		require.NoError(t, validateTriggerMigration(pair[0], pair[1], "both", false))
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	GetSupportedPairs(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	var pairs []SupportedPair
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &pairs))
	enabled := 0
	for _, pair := range pairs {
		if pair.SupportsTriggers {
			enabled++
			require.Equal(t, "mysql", pair.Source)
		}
	}
	require.Equal(t, 7, enabled)
}

func TestMySQLPGTriggerRequestsAndBatchDefaultOff(t *testing.T) {
	for _, raw := range []string{"", "migrate_triggers=false", "migrate_triggers=true"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(raw))
		c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		require.Equal(t, raw == "migrate_triggers=true", parseBatchOptions(c).MigrateTriggers)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"src_conn_id":1,"dst_conn_id":2,"migrate_objects":["triggers"],"table_names":["Orders"]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	var request startObjectMigrationRequest
	require.NoError(t, c.ShouldBindJSON(&request))
	require.Equal(t, []string{"triggers"}, request.MigrateObjects)
}

func TestMySQLPGTriggerJobSnapshotPersists(t *testing.T) {
	migrationHandlerTestDB(t)
	require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{JobID: "trigger-option", MigrateTriggers: true}))
	job, err := store.GetDataMigrationJob("trigger-option")
	require.NoError(t, err)
	require.True(t, job.MigrateTriggers)
}

func TestMySQLPGTriggerInvalidRequestsDoNotCreateJobs(t *testing.T) {
	for _, tc := range []struct {
		name, dst, body string
		objects         bool
	}{
		{"unsupported target", "dameng", `{"src_conn_id":1,"dst_conn_id":2,"migrate_mode":"all","migrate_triggers":true}`, false},
		{"data only", "postgres", `{"src_conn_id":1,"dst_conn_id":2,"migrate_mode":"all","migrate_content":"data_only","migrate_triggers":true}`, false},
		{"unsupported objects", "dameng", `{"src_conn_id":1,"dst_conn_id":2,"migrate_objects":["triggers"],"table_names":["Orders"]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			migrationHandlerTestDB(t)
			require.NoError(t, store.CreateConnection(&store.Connection{OwnerID: 7, DBType: "mysql", Name: "src"}))
			require.NoError(t, store.CreateConnection(&store.Connection{OwnerID: 7, DBType: tc.dst, Name: "dst"}))
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Set("userID", uint(7))
			c.Set("role", "user")
			c.Request = httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			if tc.objects {
				StartObjectMigration(c)
			} else {
				StartDataMigration(c)
			}
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Contains(t, response.Body.String(), "触发器")
			var count int64
			require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}
