package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dbgold/config"
	"dbgold/datamigrate"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestConfigFromIncrementalUsesFrozenEffectiveTables(t *testing.T) {
	job := &store.IncrementalMigrationJob{
		JobID:          "job-1",
		SrcDatabase:    "source_db",
		TargetSchema:   "target_schema",
		MigrateMode:    "all",
		BootstrapDone:  true,
		EffectiveJSON:  `["Orders","customers"]`,
		LowerCaseNames: true,
	}
	src := &store.Connection{DBType: "mysql", Host: "127.0.0.1", Port: 3306, Username: "root"}
	dst := &store.Connection{DBType: "postgres", Host: "127.0.0.1", Port: 5432, Username: "postgres", Database: "target"}

	cfg := configFromIncremental(job, src, dst)
	require.Equal(t, []string{"Orders", "customers"}, cfg.TableNames)

	job.EffectiveJSON = ""
	cfg = configFromIncremental(job, src, dst)
	require.Nil(t, cfg.TableNames, "legacy completed jobs must retain their original table filter")

	job.EffectiveJSON = "not-json"
	cfg = configFromIncremental(job, src, dst)
	require.NotNil(t, cfg.TableNames)
	require.Empty(t, cfg.TableNames, "a corrupt frozen manifest must fail closed")
}

func TestStrictBootstrapFailure(t *testing.T) {
	success := datamigrate.MigrationReport{
		Tables:    datamigrate.CategoryReport{Total: 1, Success: 1},
		Data:      datamigrate.CategoryReport{Total: 1, Success: 1},
		RowCounts: []datamigrate.TableRowCount{{Table: "users", Src: 2, Dst: 2, Match: true}},
	}
	require.NoError(t, strictBootstrapFailure(success, []string{"users"}))

	withIndexFailure := success
	withIndexFailure.Indexes = datamigrate.CategoryReport{Total: 1, Failed: 1}
	require.ErrorContains(t, strictBootstrapFailure(withIndexFailure, []string{"users"}), "对象创建失败")

	withMismatch := success
	withMismatch.RowCounts = []datamigrate.TableRowCount{{Table: "users", Src: 2, Dst: 1, Match: false}}
	require.ErrorContains(t, strictBootstrapFailure(withMismatch, []string{"users"}), "行数校验失败")
}

func TestIncrementalLogJournalFlushesAndClassifies(t *testing.T) {
	store.Init(&config.Config{SQLitePath: ":memory:"})
	jobRecord := &store.IncrementalMigrationJob{OwnerID: 1, JobID: "journal-job", StartMode: "full_then_cdc", Status: "snapshot"}
	require.NoError(t, store.CreateIncrementalJob(jobRecord))
	logJob := &datamigrate.Job{LogCh: make(chan string, 4096)}
	done := startIncrementalLogJournal(jobRecord.JobID, logJob)
	for _, line := range []string{
		"10:00:00.000 [INFO] === Phase 1: 创建表结构 ===",
		"10:00:00.001 [DDL] 创建表 users ... OK",
		"10:00:00.002 [INFO] === Phase 2: 迁移数据 ===",
		"10:00:00.003 [DATA] 迁移 users: 第 1 页 (10 行) ... OK",
	} {
		logJob.LogCh <- line
	}
	close(logJob.LogCh)
	require.Zero(t, <-done)

	page, err := store.ListIncrementalMigrationLogs(jobRecord.JobID, 0, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 4)
	require.Equal(t, "snapshot_schema", page.Items[0].Phase)
	require.Equal(t, "ddl", page.Items[1].Level)
	require.Equal(t, "snapshot_data", page.Items[2].Phase)
	require.Equal(t, "data", page.Items[3].Level)
}

func TestIncrementalLogJournalReplaysLargeMultiTableStream(t *testing.T) {
	store.Init(&config.Config{SQLitePath: ":memory:"})
	jobRecord := &store.IncrementalMigrationJob{OwnerID: 1, JobID: "journal-large", StartMode: "full_then_cdc", Status: "snapshot"}
	require.NoError(t, store.CreateIncrementalJob(jobRecord))
	logJob := &datamigrate.Job{LogCh: make(chan string, 4096)}
	done := startIncrementalLogJournal(jobRecord.JobID, logJob)
	logJob.LogCh <- "10:00:00.000 [INFO] === Phase 2: 迁移数据 ==="
	for table := 1; table <= 101; table++ {
		for page := 1; page <= 5; page++ {
			logJob.LogCh <- fmt.Sprintf("10:00:00.000 [DATA] 迁移 table_%03d: 第 %d 页 (20000 行) ... OK", table, page)
		}
	}
	close(logJob.LogCh)
	require.Zero(t, <-done)

	tail, err := store.ListIncrementalMigrationLogs(jobRecord.JobID, 0, 0, 500)
	require.NoError(t, err)
	require.Len(t, tail.Items, 500)
	require.True(t, tail.HasOlder)
	require.Equal(t, "snapshot_data", tail.Items[len(tail.Items)-1].Phase)
	older, err := store.ListIncrementalMigrationLogs(jobRecord.JobID, 0, tail.OldestID, 500)
	require.NoError(t, err)
	require.Len(t, older.Items, 6)
	require.False(t, older.HasOlder)
	require.True(t, older.HasNewer)
}

func TestIncrementalLogJournalDegradesWhenBatchPersistenceFails(t *testing.T) {
	store.Init(&config.Config{SQLitePath: ":memory:"})
	jobRecord := &store.IncrementalMigrationJob{OwnerID: 1, JobID: "journal-failure", StartMode: "full_then_cdc", Status: "snapshot"}
	require.NoError(t, store.CreateIncrementalJob(jobRecord))

	originalAppend := appendIncrementalLogRows
	attempts := 0
	appendIncrementalLogRows = func(logs []store.IncrementalMigrationLog) error {
		attempts++
		if attempts <= 3 {
			return errors.New("sqlite busy")
		}
		return store.AppendIncrementalMigrationLogs(logs)
	}
	t.Cleanup(func() { appendIncrementalLogRows = originalAppend })

	logJob := &datamigrate.Job{LogCh: make(chan string, 1)}
	done := startIncrementalLogJournal(jobRecord.JobID, logJob)
	logJob.LogCh <- `10:00:00.000 [ERROR] invalid input syntax for type integer: "secret-value"`
	close(logJob.LogCh)
	require.Equal(t, int64(1), <-done)

	job, err := store.GetIncrementalJob(jobRecord.JobID)
	require.NoError(t, err)
	require.Equal(t, int64(1), job.LogDroppedCount)
	page, err := store.ListIncrementalMigrationLogs(jobRecord.JobID, 0, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, "warn", page.Items[0].Level)
	require.Contains(t, page.Items[0].Line, "1 条全量日志未能保存")
}

type incrementalDropTestReader struct{ source.Reader }

func (incrementalDropTestReader) DBType() string { return "mysql" }
func (incrementalDropTestReader) GetTriggerCount(context.Context) (int, error) {
	return 0, nil
}
func (incrementalDropTestReader) ListTables(context.Context) ([]string, error) {
	return nil, nil
}

type incrementalDropTestWriter struct{ target.Writer }

func TestIncrementalLogJournalPersistsChannelDropCount(t *testing.T) {
	store.Init(&config.Config{SQLitePath: ":memory:"})
	jobRecord := &store.IncrementalMigrationJob{OwnerID: 1, JobID: "journal-channel-drop", StartMode: "full_then_cdc", Status: "snapshot"}
	require.NoError(t, store.CreateIncrementalJob(jobRecord))

	logJob := &datamigrate.Job{LogCh: make(chan string, 1)}
	migrator := datamigrate.NewMigrator(
		incrementalDropTestReader{}, incrementalDropTestWriter{}, logJob,
		datamigrate.Config{Content: "data_only", Mode: "all", MaxParallel: 1},
	)
	migrator.Run(context.Background())
	require.Greater(t, logJob.DroppedLogCount(), uint64(0))

	done := startIncrementalLogJournal(jobRecord.JobID, logJob)
	close(logJob.LogCh)
	require.Equal(t, int64(logJob.DroppedLogCount()), <-done)

	job, err := store.GetIncrementalJob(jobRecord.JobID)
	require.NoError(t, err)
	require.Equal(t, int64(logJob.DroppedLogCount()), job.LogDroppedCount)
	page, err := store.ListIncrementalMigrationLogs(jobRecord.JobID, 0, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.Equal(t, "warn", page.Items[1].Level)
	require.Contains(t, page.Items[1].Line, "日志通道或持久化拥塞")
}

func TestIncrementalLogParsingAndSanitizing(t *testing.T) {
	require.Equal(t, "error", incrementalLogLevel("10:00:00.000 [ERROR] table name contains [DATA]"))
	require.Equal(t, "snapshot_schema", incrementalLogPhase("10:00:00.000 [INFO] === Phase 1: 创建表结构 ===", "snapshot_init"))
	require.Equal(t, "snapshot_schema", incrementalLogPhase("10:00:00.000 [ERROR] bad table Phase 2", "snapshot_schema"))

	line := sanitizeIncrementalLogLine(`10:00:00.000 [ERROR] invalid input syntax for type integer: "customer-secret"`)
	require.NotContains(t, line, "customer-secret")
	require.Contains(t, line, "<redacted>")
	line = sanitizeIncrementalLogLine(`10:00:00.000 [ERROR] duplicate key: Key (email)=(private@example.com) already exists`)
	require.NotContains(t, line, "private@example.com")
	require.Contains(t, line, "Key (email)=(<redacted>)")
	require.Contains(t, sanitizeIncrementalLogLine(`10:00:00.000 [ERROR] type "geometry" does not exist`), `"geometry"`)
}

func TestGetIncrementalLogsCursorAndOwnership(t *testing.T) {
	store.Init(&config.Config{SQLitePath: ":memory:"})
	require.NoError(t, store.CreateIncrementalJob(&store.IncrementalMigrationJob{
		OwnerID: 1, JobID: "owned-logs", StartMode: "full_then_cdc", Status: "snapshot", LogDroppedCount: 3,
	}))
	require.NoError(t, store.CreateIncrementalJob(&store.IncrementalMigrationJob{
		OwnerID: 2, JobID: "other-logs", StartMode: "full_then_cdc", Status: "snapshot",
	}))
	entries := make([]store.IncrementalMigrationLog, 0, 3)
	for i := 1; i <= 3; i++ {
		entries = append(entries, store.IncrementalMigrationLog{
			JobID: "owned-logs", Phase: "snapshot_data", Level: "data", Line: fmt.Sprintf("line-%d", i), CreatedAt: time.Now(),
		})
	}
	require.NoError(t, store.AppendIncrementalMigrationLogs(entries))

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", uint(1))
		c.Set("role", "user")
		c.Next()
	})
	router.GET("/jobs/:jobID/logs", GetIncrementalLogs)

	response := performIncrementalLogRequest(router, "/jobs/owned-logs/logs?limit=2")
	require.Equal(t, http.StatusOK, response.Code)
	var page struct {
		Items           []store.IncrementalMigrationLog `json:"items"`
		OldestID        uint64                          `json:"oldest_id"`
		NewestID        uint64                          `json:"newest_id"`
		HasOlder        bool                            `json:"has_older"`
		LogDroppedCount int64                           `json:"log_dropped_count"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&page))
	require.Equal(t, []string{"line-2", "line-3"}, []string{page.Items[0].Line, page.Items[1].Line})
	require.True(t, page.HasOlder)
	require.Equal(t, int64(3), page.LogDroppedCount)

	secondClient := performIncrementalLogRequest(router, "/jobs/owned-logs/logs?limit=2")
	require.Equal(t, http.StatusOK, secondClient.Code)
	var secondPage struct {
		Items []store.IncrementalMigrationLog `json:"items"`
	}
	require.NoError(t, json.NewDecoder(secondClient.Body).Decode(&secondPage))
	require.Equal(t, []string{"line-2", "line-3"}, []string{secondPage.Items[0].Line, secondPage.Items[1].Line})

	response = performIncrementalLogRequest(router, fmt.Sprintf("/jobs/owned-logs/logs?after_id=%d&limit=10", page.OldestID))
	require.Equal(t, http.StatusOK, response.Code)
	var newer struct {
		Items []store.IncrementalMigrationLog `json:"items"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&newer))
	require.Len(t, newer.Items, 1)
	require.Equal(t, "line-3", newer.Items[0].Line)

	response = performIncrementalLogRequest(router, fmt.Sprintf("/jobs/owned-logs/logs?before_id=%d&limit=10", page.OldestID))
	require.Equal(t, http.StatusOK, response.Code)
	var older struct {
		Items []store.IncrementalMigrationLog `json:"items"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&older))
	require.Len(t, older.Items, 1)
	require.Equal(t, "line-1", older.Items[0].Line)

	require.Equal(t, http.StatusBadRequest, performIncrementalLogRequest(router, "/jobs/owned-logs/logs?after_id=1&before_id=2").Code)
	require.Equal(t, http.StatusBadRequest, performIncrementalLogRequest(router, "/jobs/owned-logs/logs?after_id=0").Code)
	require.Equal(t, http.StatusBadRequest, performIncrementalLogRequest(router, "/jobs/owned-logs/logs?before_id=-1").Code)
	require.Equal(t, http.StatusBadRequest, performIncrementalLogRequest(router, "/jobs/owned-logs/logs?after_id=nope").Code)
	require.Equal(t, http.StatusBadRequest, performIncrementalLogRequest(router, "/jobs/owned-logs/logs?limit=1001").Code)
	require.Equal(t, http.StatusNotFound, performIncrementalLogRequest(router, "/jobs/other-logs/logs").Code)

	adminRouter := gin.New()
	adminRouter.Use(func(c *gin.Context) {
		c.Set("userID", uint(99))
		c.Set("role", "admin")
		c.Next()
	})
	adminRouter.GET("/jobs/:jobID/logs", GetIncrementalLogs)
	require.Equal(t, http.StatusOK, performIncrementalLogRequest(adminRouter, "/jobs/other-logs/logs").Code)

	require.NoError(t, store.UpdateIncrementalJob("owned-logs", map[string]any{"status": "stopped"}))
	require.Equal(t, http.StatusOK, performIncrementalLogRequest(router, "/jobs/owned-logs/logs").Code)
}

func performIncrementalLogRequest(router *gin.Engine, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
