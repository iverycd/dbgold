package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dbgold/config"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func taskListRequest(handler gin.HandlerFunc, path string) *httptest.ResponseRecorder {
	router := gin.New()
	router.GET("/jobs", func(c *gin.Context) {
		c.Set("userID", uint(1))
		c.Set("role", "user")
		handler(c)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func TestTaskListHandlersReturnPagesAndValidateQueries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store.Init(&config.Config{SQLitePath: ":memory:"})
	require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{OwnerID: 1, JobID: "data-list", Status: "done"}))
	require.NoError(t, store.CreateIncrementalJob(&store.IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-list", Status: "running"}))

	dataResponse := taskListRequest(ListDataMigrationJobs, "/jobs?page=1&page_size=20&status=done&origin=all")
	require.Equal(t, http.StatusOK, dataResponse.Code)
	var dataPage map[string]any
	require.NoError(t, json.Unmarshal(dataResponse.Body.Bytes(), &dataPage))
	require.EqualValues(t, 1, dataPage["total"])
	require.Len(t, dataPage["items"], 1)

	incrementalResponse := taskListRequest(ListIncremental, "/jobs?page=1&page_size=20&status=active")
	require.Equal(t, http.StatusOK, incrementalResponse.Code)
	var incrementalPage map[string]any
	require.NoError(t, json.Unmarshal(incrementalResponse.Body.Bytes(), &incrementalPage))
	require.EqualValues(t, 1, incrementalPage["total"])

	for _, path := range []string{
		"/jobs?page=0", "/jobs?page_size=25", "/jobs?status=unknown", "/jobs?origin=unknown",
	} {
		require.Equal(t, http.StatusBadRequest, taskListRequest(ListDataMigrationJobs, path).Code, path)
	}
	require.Equal(t, http.StatusBadRequest, taskListRequest(ListIncremental, "/jobs?status=running").Code)
}
