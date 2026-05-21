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

func TestDiffSchemas_InlineSchemas(t *testing.T) {
	store.Init(&config.Config{SQLitePath: ":memory:"})
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.POST("/api/diff", handler.DiffSchemas)

	body, _ := json.Marshal(map[string]any{
		"src_schema": map[string]any{
			"tables": []map[string]any{
				{"name": "users", "columns": []map[string]any{
					{"name": "id", "type": "int", "nullable": false},
				}},
			},
		},
		"dst_schema": map[string]any{
			"tables": []map[string]any{
				{"name": "users", "columns": []map[string]any{
					{"name": "id", "type": "int", "nullable": false},
					{"name": "email", "type": "varchar(255)", "nullable": true},
				}},
				{"name": "orders", "columns": []map[string]any{
					{"name": "id", "type": "int", "nullable": false},
				}},
			},
		},
	})

	req := httptest.NewRequest("POST", "/api/diff", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var result map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&result))

	added, ok := result["AddedTables"].([]any)
	require.True(t, ok)
	assert.Len(t, added, 1)
}
