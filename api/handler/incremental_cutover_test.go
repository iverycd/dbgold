package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dbgold/config"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestIncrementalCutoverAcknowledgmentGates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store.Init(&config.Config{SQLitePath: ":memory:"})
	router := gin.New()
	router.POST("/jobs/:jobID", func(c *gin.Context) {
		c.Set("userID", uint(1))
		c.Set("role", "user")
		StopIncremental(c)
	})
	tests := []struct {
		name, status, validation, content, body, message string
	}{
		{"legacy count-only", "ready_to_cutover", "passed", "", `{}`, "缺少完整的校验结果"},
		{"count check failed", "ready_with_warnings", "mismatch", contentValidationNeedsAcknowledgment, `{"acknowledge_content_validation":true}`, "缺少完整的校验结果"},
		{"source or sequence check failed", "cutover_blocked", "failed", contentValidationNeedsAcknowledgment, `{"acknowledge_content_validation":true}`, "必须先停止源库写入"},
		{"content requires separate acknowledgment", "ready_with_warnings", "passed", contentValidationNeedsAcknowledgment, `{"acknowledge_warnings":true,"acknowledge_exclusions":true}`, "必须单独确认豁免"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			jobID := string(rune('a' + i))
			require.NoError(t, store.CreateIncrementalJob(&store.IncrementalMigrationJob{
				OwnerID: 1, JobID: jobID, Status: tc.status, ValidationState: tc.validation, ContentValidationState: tc.content,
			}))
			req := httptest.NewRequest(http.MethodPost, "/jobs/"+jobID, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, http.StatusConflict, response.Code)
			require.Contains(t, response.Body.String(), tc.message)
			job, err := store.GetIncrementalJob(jobID)
			require.NoError(t, err)
			require.Equal(t, tc.status, job.Status)
			require.Nil(t, job.ContentValidationWaivedAt)
		})
	}
}

func TestIncrementalCutoverCompletionRejectsStaleValidation(t *testing.T) {
	store.Init(&config.Config{SQLitePath: ":memory:"})
	require.NoError(t, store.CreateIncrementalJob(&store.IncrementalMigrationJob{
		JobID: "cutover-attempt", Status: "ready_with_warnings", ValidationState: "passed", ContentValidationState: contentValidationNeedsAcknowledgment,
	}))
	job, err := store.GetIncrementalJob("cutover-attempt")
	require.NoError(t, err)
	require.NoError(t, store.UpdateIncrementalJob(job.JobID, map[string]any{"cutover_position": 999}))
	updated, err := store.CompleteIncrementalCutover(job, map[string]any{"status": "stopped"})
	require.NoError(t, err)
	require.False(t, updated)
	job, err = store.GetIncrementalJob(job.JobID)
	require.NoError(t, err)
	updated, err = store.CompleteIncrementalCutover(job, map[string]any{"status": "stopped"})
	require.NoError(t, err)
	require.True(t, updated)
}
