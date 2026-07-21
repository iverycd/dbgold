package api

import (
	"dbgold/config"
	"dbgold/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupRouterTest(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store.Init(&config.Config{SQLitePath: ":memory:"})
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>dbgold</html>"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log('ok')"), 0o644))
	r, err := NewRouterWithOptions(RouterOptions{StaticDir: dir})
	require.NoError(t, err)
	return r, dir
}

func TestHealthEndpoints(t *testing.T) {
	r, _ := setupRouterTest(t)
	for _, path := range []string{"/api/health/live", "/api/health/ready"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, w.Code)
	}
}

func TestStaticSiteAndSPAFallback(t *testing.T) {
	r, _ := setupRouterTest(t)

	asset := httptest.NewRecorder()
	r.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	assert.Equal(t, http.StatusOK, asset.Code)
	assert.Contains(t, asset.Header().Get("Cache-Control"), "immutable")

	spa := httptest.NewRecorder()
	r.ServeHTTP(spa, httptest.NewRequest(http.MethodGet, "/history/data/job-1", nil))
	assert.Equal(t, http.StatusOK, spa.Code)
	assert.Contains(t, spa.Body.String(), "dbgold")
	assert.Equal(t, "no-cache", spa.Header().Get("Cache-Control"))
	index := httptest.NewRecorder()
	r.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	assert.Equal(t, "no-cache", index.Header().Get("Cache-Control"))

	missingAPI := httptest.NewRecorder()
	r.ServeHTTP(missingAPI, httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil))
	assert.Equal(t, http.StatusNotFound, missingAPI.Code)
	assert.NotContains(t, missingAPI.Body.String(), "<html>")
}
