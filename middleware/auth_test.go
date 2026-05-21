package middleware_test

import (
	"dbgold/config"
	"dbgold/middleware"
	"dbgold/store"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
)

func makeToken(userID uint, role string, secret string, expired bool) string {
	exp := time.Now().Add(time.Hour)
	if expired {
		exp = time.Now().Add(-time.Hour)
	}
	claims := &middleware.Claims{
		UserID: userID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	token, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	return token
}

func setupMiddlewareTest(t *testing.T) {
	t.Helper()
	store.Init(&config.Config{SQLitePath: ":memory:"})
	middleware.SetJWTSecret("test-secret")
	gin.SetMode(gin.TestMode)
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	setupMiddlewareTest(t)
	u, _ := store.CreateUser("alice", "pass", "user")

	r := gin.New()
	r.Use(middleware.Auth())
	r.GET("/protected", func(c *gin.Context) { c.Status(http.StatusOK) })

	token := makeToken(u.ID, "user", "test-secret", false)
	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuthMiddleware_NoToken(t *testing.T) {
	setupMiddlewareTest(t)
	r := gin.New()
	r.Use(middleware.Auth())
	r.GET("/protected", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest("GET", "/protected", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthMiddleware_ExpiredToken(t *testing.T) {
	setupMiddlewareTest(t)
	u, _ := store.CreateUser("bob", "pass", "user")
	token := makeToken(u.ID, "user", "test-secret", true)

	r := gin.New()
	r.Use(middleware.Auth())
	r.GET("/protected", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAdminOnlyMiddleware_NonAdmin(t *testing.T) {
	setupMiddlewareTest(t)
	u, _ := store.CreateUser("carol", "pass", "user")
	token := makeToken(u.ID, "user", "test-secret", false)

	r := gin.New()
	r.Use(middleware.Auth(), middleware.AdminOnly())
	r.GET("/admin", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest("GET", "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}
