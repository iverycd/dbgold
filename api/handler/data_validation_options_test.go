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

func TestDataValidationOptionsDefaultOffAndOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		request := startDataMigrationRequest{}
		raw := `{}`
		if enabled {
			raw = `{"validate_data":true}`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &request))
		require.Equal(t, enabled, request.ValidateData)
	}
	for _, raw := range []string{"", "validate_data=false", "validate_data=true", "validate_data=1"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(raw))
		c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		require.Equal(t, raw == "validate_data=true" || raw == "validate_data=1", parseBatchOptions(c).ValidateData)
	}
}

func TestDataValidationModesRequireData(t *testing.T) {
	for _, content := range []string{"", "both", "data_only"} {
		require.NoError(t, validateDataValidationMode(content, true))
	}
	require.NoError(t, validateDataValidationMode("schema_only", false))
	require.ErrorContains(t, validateDataValidationMode("schema_only", true), "包含数据")
}

func TestDataValidationJobSnapshotPersists(t *testing.T) {
	migrationHandlerTestDB(t)
	for _, enabled := range []bool{false, true} {
		jobID := "validation-default"
		if enabled {
			jobID = "validation-enabled"
		}
		require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{JobID: jobID, ValidateData: enabled}))
		job, err := store.GetDataMigrationJob(jobID)
		require.NoError(t, err)
		require.Equal(t, enabled, job.ValidateData)
	}
}

func TestDataValidationRejectsNonDataEndpointsBeforeCreatingJobs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		handler gin.HandlerFunc
	}{
		{"schema only", `{"src_conn_id":1,"dst_conn_id":2,"migrate_mode":"all","migrate_content":"schema_only","validate_data":true}`, StartDataMigration},
		{"objects", `{"src_conn_id":1,"dst_conn_id":2,"migrate_objects":["indexes"],"table_names":["Orders"],"validate_data":true}`, StartObjectMigration},
		{"views", `{"src_conn_id":1,"dst_conn_id":2,"view_names":["OrdersView"],"validate_data":true}`, MigrateViews},
	} {
		t.Run(tc.name, func(t *testing.T) {
			migrationHandlerTestDB(t)
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			tc.handler(c)
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Contains(t, response.Body.String(), "数据内容校验")
			var jobs int64
			require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Count(&jobs).Error)
			require.Zero(t, jobs)
		})
	}
}
