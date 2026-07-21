package main

import (
	"context"
	"dbgold/config"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseServeConfigPriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dbgold.env")
	require.NoError(t, os.WriteFile(path, []byte("PORT=19089\nLISTEN_HOST=127.0.0.1\n"), 0o600))
	t.Setenv("PORT", "20089")

	cfg, err := parseServeConfig([]string{"--config", path, "--port", "21089", "--listen-host", "0.0.0.0"})
	require.NoError(t, err)
	assert.Equal(t, "21089", cfg.Port)
	assert.Equal(t, "0.0.0.0", cfg.ListenHost)
}

func TestHealthcheckCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	require.NoError(t, runHealthcheckCommand([]string{"--url", server.URL, "--timeout", "1s"}))
}

func TestRunServerRejectsOccupiedPort(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	err = runServer(context.Background(), &config.Config{ListenHost: "127.0.0.1", Port: port})
	assert.ErrorContains(t, err, "already be in use")
}

func TestRunServerGracefulShutdown(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := probe.Addr().(*net.TCPAddr).Port
	require.NoError(t, probe.Close())

	dir := t.TempDir()
	staticDir := filepath.Join(dir, "web")
	require.NoError(t, os.MkdirAll(staticDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("dbgold"), 0o644))
	cfg := &config.Config{
		AppEnv:                  "development",
		ListenHost:              "127.0.0.1",
		Port:                    strconv.Itoa(port),
		StaticDir:               staticDir,
		SQLitePath:              filepath.Join(dir, "dbgold.db"),
		JWTSecret:               "test-secret",
		AdminUser:               "admin",
		AdminPass:               "Admin@123",
		LogDir:                  filepath.Join(dir, "logs"),
		LogLevel:                "error",
		LogMaxFiles:             2,
		LogMaxTotalMB:           10,
		UploadDir:               filepath.Join(dir, "uploads"),
		MaxUploadBytes:          1024,
		JWTExpireHours:          1,
		QueryTimeoutSeconds:     1,
		QueryMaxRows:            10,
		QueryAuditRetentionDays: 1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, cfg) }()

	url := fmt.Sprintf("http://127.0.0.1:%d/api/health/live", port)
	require.Eventually(t, func() bool {
		client := &http.Client{Timeout: 100 * time.Millisecond}
		resp, requestErr := client.Get(url)
		if requestErr != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 50*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
}
