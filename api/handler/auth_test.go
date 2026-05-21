package handler_test

import (
	"bytes"
	"dbgold/api/handler"
	"dbgold/config"
	"dbgold/middleware"
	"dbgold/store"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupHandlerTest(t *testing.T) {
	t.Helper()
	store.Init(&config.Config{SQLitePath: ":memory:"})
	middleware.SetJWTSecret("test-secret")
	handler.SetJWTSecret("test-secret")
	gin.SetMode(gin.TestMode)
}

func TestLogin_Success(t *testing.T) {
	setupHandlerTest(t)
	store.CreateUser("admin", "Admin@123", "admin")

	r := gin.New()
	r.POST("/api/auth/login", handler.Login)

	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "Admin@123"})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotEmpty(t, resp["token"])
}

func TestLogin_WrongPassword(t *testing.T) {
	setupHandlerTest(t)
	store.CreateUser("admin", "Admin@123", "admin")

	r := gin.New()
	r.POST("/api/auth/login", handler.Login)

	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "wrong"})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
