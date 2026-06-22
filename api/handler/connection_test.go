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

func setupConnTest(t *testing.T) {
	t.Helper()
	store.Init(&config.Config{SQLitePath: ":memory:"})
	gin.SetMode(gin.TestMode)
}

// fakeAuth 注入认证中间件设置的 userID/role，模拟已登录用户。
func fakeAuth(userID uint, role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("userID", userID)
		c.Set("role", role)
		c.Next()
	}
}

func TestCreateAndListConnection(t *testing.T) {
	setupConnTest(t)
	r := gin.New()
	r.Use(fakeAuth(1, "user"))
	r.POST("/api/connections", handler.CreateConnection)
	r.GET("/api/connections", handler.GetConnections)

	body, _ := json.Marshal(map[string]any{
		"name": "local-mysql", "db_type": "mysql",
		"host": "localhost", "port": 3306,
		"database": "testdb", "username": "root", "password": "pass",
	})
	req := httptest.NewRequest("POST", "/api/connections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	req2 := httptest.NewRequest("GET", "/api/connections", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)

	var list []map[string]any
	require.NoError(t, json.NewDecoder(w2.Body).Decode(&list))
	assert.Len(t, list, 1)
	assert.Equal(t, "local-mysql", list[0]["name"])
}

func TestDeleteConnection(t *testing.T) {
	setupConnTest(t)
	conn := &store.Connection{
		OwnerID: 1,
		Name:    "del-me", DBType: "mysql",
		Host: "localhost", Port: 3306,
		Database: "db", Username: "u", Password: "p",
	}
	require.NoError(t, store.CreateConnection(conn))

	r := gin.New()
	r.Use(fakeAuth(1, "user"))
	r.DELETE("/api/connections/:id", handler.DeleteConnection)
	req := httptest.NewRequest("DELETE", "/api/connections/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}
