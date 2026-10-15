package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dbgold/datamigrate"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func migrationHandlerTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migration.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	previous := store.DB
	store.DB = db
	t.Cleanup(func() {
		store.DB = previous
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&store.DataMigrationJob{}, &store.Connection{}, &store.User{}))
}

func TestMigrationStartupReportsErrorsAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%t", cancelled), func(t *testing.T) {
			migrationHandlerTestDB(t)
			dbJob := &store.DataMigrationJob{JobID: "startup", Status: "running"}
			require.NoError(t, store.CreateDataMigrationJob(dbJob))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			job := &datamigrate.Job{LogCh: make(chan string, 4)}
			startup := newMigrationStartup(ctx, job, dbJob)
			started, proceed := startup.begin("初始化迁移准备信息")
			require.True(t, proceed)
			require.Contains(t, <-job.LogCh, "[INFO]   初始化迁移准备信息…")
			if cancelled {
				cancel()
			}
			require.False(t, startup.finish("初始化迁移准备信息", started, errors.New("测试准备错误")))
			persisted, err := store.GetDataMigrationJob(dbJob.JobID)
			require.NoError(t, err)
			line := <-job.LogCh
			if cancelled {
				require.Equal(t, "cancelled", persisted.Status)
				require.Contains(t, line, "[WARN]")
				require.Contains(t, line, "任务已取消")
				require.NotContains(t, line, "[ERROR]")
			} else {
				require.Equal(t, "failed", persisted.Status)
				require.Equal(t, "初始化迁移准备信息失败: 测试准备错误", persisted.Summary)
				require.Contains(t, line, "[ERROR] 初始化迁移准备信息失败: 测试准备错误")
			}
			require.NotNil(t, persisted.FinishedAt)
		})
	}
}

func TestMigrationStartupCompletionIncludesElapsedTimeAndNeverBlocks(t *testing.T) {
	job := &datamigrate.Job{LogCh: make(chan string, 2)}
	startup := newMigrationStartup(context.Background(), job, &store.DataMigrationJob{})
	started, proceed := startup.begin("连接源数据库")
	require.True(t, proceed)
	require.True(t, startup.finish("连接源数据库", started, nil))
	require.Contains(t, <-job.LogCh, "连接源数据库…")
	require.Contains(t, <-job.LogCh, "连接源数据库完成，耗时 ")

	// A batch child can run without a browser subscriber. Startup logs must
	// retain the existing non-blocking behavior when its channel is full.
	job.LogCh <- "full"
	job.LogCh <- "full"
	done := make(chan struct{})
	go func() {
		startup.begin("连接目标数据库")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startup logging blocked on a full channel")
	}
}

// Accept TCP but withhold the MySQL handshake, so the real Reader constructor
// remains pending until the test deliberately releases the connection.
func blockedMigrationSource(t *testing.T) (*store.Connection, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		<-release
	}()
	var once sync.Once
	unblock := func() {
		once.Do(func() {
			listener.Close()
			close(release)
			<-done
		})
	}
	t.Cleanup(unblock)
	addr := listener.Addr().(*net.TCPAddr)
	return &store.Connection{OwnerID: 1, Name: "source", DBType: "mysql", Host: addr.IP.String(), Port: addr.Port, Database: "source", Username: "reader"}, unblock
}

func TestRunDataMigrationLogsBeforeSourceConnectAndStopsAfterCancellation(t *testing.T) {
	migrationHandlerTestDB(t)
	src, unblock := blockedMigrationSource(t)
	dst := &store.Connection{DBType: "postgres"}
	dbJob := &store.DataMigrationJob{JobID: "blocked-startup", Status: "running"}
	require.NoError(t, store.CreateDataMigrationJob(dbJob))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &datamigrate.Job{LogCh: make(chan string, 16)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDataMigration(ctx, job, dbJob, src, dst, migrationParams{})
	}()
	t.Cleanup(func() {
		cancel()
		unblock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("migration worker did not exit")
		}
	})
	select {
	case line := <-job.LogCh:
		require.Contains(t, line, "连接源数据库…")
	case <-time.After(time.Second):
		t.Fatal("no startup log before source connection completed")
	}
	cancel()
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("migration did not exit after source released")
	}
	persisted, err := store.GetDataMigrationJob(dbJob.JobID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", persisted.Status)
	for len(job.LogCh) > 0 {
		require.NotContains(t, <-job.LogCh, "连接目标数据库")
	}
}

func TestStartObjectMigrationLogsBeforeSourceConnectionCompletes(t *testing.T) {
	migrationHandlerTestDB(t)
	src, unblock := blockedMigrationSource(t)
	dst := &store.Connection{OwnerID: 1, Name: "target", DBType: "postgres"}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/objects", func(c *gin.Context) {
		c.Set("userID", uint(1))
		c.Set("role", "user")
		StartObjectMigration(c)
	})
	body := fmt.Sprintf(`{"src_conn_id":%d,"dst_conn_id":%d,"migrate_objects":["indexes"],"table_names":["orders"]}`, src.ID, dst.ID)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/objects", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var response struct {
		JobID string `json:"job_id"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	job := datamigrate.Registry.Get(response.JobID)
	require.NotNil(t, job)
	t.Cleanup(func() {
		job.Cancel()
		unblock()
		require.Eventually(t, func() bool { return datamigrate.Registry.Get(response.JobID) == nil }, 3*time.Second, time.Millisecond)
	})
	select {
	case line := <-job.LogCh:
		require.Contains(t, line, "连接源数据库…")
	case <-time.After(time.Second):
		t.Fatal("object migration produced no startup log")
	}
}
