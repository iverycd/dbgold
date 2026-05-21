package handler_test

import (
	"bytes"
	"dbgold/api/handler"
	"dbgold/store"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateUser(t *testing.T) {
	setupHandlerTest(t)

	r := gin.New()
	r.POST("/api/admin/users", handler.CreateUser)

	body, _ := json.Marshal(map[string]string{
		"username": "newuser", "password": "Pass@123", "role": "user",
	})
	req := httptest.NewRequest("POST", "/api/admin/users", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "newuser", resp["username"])
}

func TestListUsers(t *testing.T) {
	setupHandlerTest(t)
	store.CreateUser("u1", "pass", "user")
	store.CreateUser("u2", "pass", "admin")

	r := gin.New()
	r.GET("/api/admin/users", handler.ListUsers)
	req := httptest.NewRequest("GET", "/api/admin/users", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var users []map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&users))
	assert.Len(t, users, 2)
}
