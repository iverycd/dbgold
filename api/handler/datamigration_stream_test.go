package handler

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dbgold/datamigrate"
	"dbgold/middleware"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func readMigrationSSEFrame(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		frame.WriteString(line)
		if line == "\n" {
			return frame.String()
		}
	}
}

func TestMigrationStreamFlushesImmediatelyAndHeartbeatsAreComments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	job := &datamigrate.Job{LogCh: make(chan string, 1)}
	router := gin.New()
	router.GET("/stream", func(c *gin.Context) {
		streamDataMigrationLogs(c, &store.DataMigrationJob{}, job, 5*time.Millisecond)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	response, err := client.Get(server.URL + "/stream")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
	require.Equal(t, "no", response.Header.Get("X-Accel-Buffering"))
	reader := bufio.NewReader(response.Body)
	require.Equal(t, ": connected\n\n", readMigrationSSEFrame(t, reader))
	require.Equal(t, ": keep-alive\n\n", readMigrationSSEFrame(t, reader))
	require.Empty(t, job.LogCh)
	job.LogCh <- "[INFO] 测试迁移日志"
	close(job.LogCh)
	remaining, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(remaining), "event:message\ndata:[INFO] 测试迁移日志\n\n")
	require.Equal(t, 1, strings.Count(string(remaining), "data:[STREAM_END]"))
	require.NotContains(t, string(remaining), "data:keep-alive")
}

func TestMigrationStreamDisconnectDoesNotCancelOrCloseMigration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var migrationCancelled atomic.Bool
	job := &datamigrate.Job{LogCh: make(chan string, 1), Cancel: func() { migrationCancelled.Store(true) }}
	done := make(chan struct{})
	router := gin.New()
	router.GET("/stream", func(c *gin.Context) {
		defer close(done)
		streamDataMigrationLogs(c, &store.DataMigrationJob{}, job, time.Hour)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/stream", nil)
	require.NoError(t, err)
	client := server.Client()
	client.Timeout = 2 * time.Second
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, ": connected\n\n", readMigrationSSEFrame(t, bufio.NewReader(response.Body)))
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream handler did not exit after client disconnected")
	}
	require.False(t, migrationCancelled.Load())
	job.LogCh <- "migration still running"
	require.Equal(t, "migration still running", <-job.LogCh)
}

func TestMigrationStreamChecksAuthorizationBeforeFlushing(t *testing.T) {
	migrationHandlerTestDB(t)
	gin.SetMode(gin.TestMode)
	require.NoError(t, store.DB.Create(&store.User{ID: 1, Username: "owner", Enabled: true}).Error)
	require.NoError(t, store.DB.Create(&store.User{ID: 2, Username: "other", Enabled: true}).Error)
	require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{JobID: "owned", OwnerID: 1, Status: "cancelled", Summary: "任务已取消"}))
	secret := "migration-stream-test-secret"
	middleware.SetJWTSecret(secret)
	tokenFor := func(userID uint) string {
		claims := middleware.Claims{UserID: userID, Role: "user", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
		require.NoError(t, err)
		return token
	}
	for _, test := range []struct {
		name   string
		token  string
		status int
	}{
		{name: "missing token", status: http.StatusUnauthorized},
		{name: "invalid token", token: "invalid", status: http.StatusUnauthorized},
		{name: "other owner", token: tokenFor(2), status: http.StatusNotFound},
		{name: "owner", token: tokenFor(1), status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/stream", StreamDataMigration)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/stream?jobID=owned&token="+url.QueryEscape(test.token), nil)
			router.ServeHTTP(recorder, request)
			require.Equal(t, test.status, recorder.Code)
			if test.status != http.StatusOK {
				require.NotContains(t, recorder.Body.String(), ": connected")
				require.NotEqual(t, "text/event-stream", recorder.Header().Get("Content-Type"))
			} else {
				require.Contains(t, recorder.Body.String(), ": connected\n\n")
				require.Contains(t, recorder.Body.String(), "data:[WARN] 任务已取消")
				require.Contains(t, recorder.Body.String(), "data:[STREAM_END]")
			}
		})
	}
}
