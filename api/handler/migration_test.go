package handler_test

import (
	"bytes"
	"dbgold/api/handler"
	"dbgold/config"
	"dbgold/store"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupMigrationRouter() *gin.Engine {
	store.Init(&config.Config{SQLitePath: ":memory:"})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/migration/diff", handler.RunDiffMigration)
	r.GET("/api/migration", handler.ListMigrations)
	r.GET("/api/migration/:id", handler.GetMigration)
	return r
}

func TestRunDiffMigration_InlineSchemas(t *testing.T) {
	r := setupMigrationRouter()

	body, _ := json.Marshal(map[string]any{
		"src_schema": map[string]any{
			"name": "src_db",
			"tables": []map[string]any{
				{"name": "users", "columns": []map[string]any{
					{"name": "id", "type": "INT", "nullable": false, "primary_key": true},
				}},
			},
		},
		"dst_schema": map[string]any{
			"name": "dst_db",
			"tables": []map[string]any{
				{"name": "users", "columns": []map[string]any{
					{"name": "id", "type": "INT", "nullable": false, "primary_key": true},
					{"name": "email", "type": "VARCHAR(255)", "nullable": true},
				}},
			},
		},
		"db_type": "mysql",
	})

	req := httptest.NewRequest("POST", "/api/migration/diff", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))

	sqls, ok := resp["sql_statements"].([]any)
	require.True(t, ok)
	assert.Len(t, sqls, 1)
	assert.Contains(t, sqls[0].(string), "ADD COLUMN")
}

func TestListMigrations_Empty(t *testing.T) {
	r := setupMigrationRouter()
	req := httptest.NewRequest("GET", "/api/migration", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp []any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Len(t, resp, 0)
}
