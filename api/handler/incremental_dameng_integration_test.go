//go:build integration

package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbgold/config"
	"dbgold/datamigrate/cdc"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func TestIncrementalDamengRunnerLifecycleIntegration(t *testing.T) {
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" {
		t.Skip("set CDC_DAMENG_INTEGRATION=1, CDC_MYSQL_DSN and CDC_DAMENG_DSN")
	}
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"gtid", "file"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			srcDB, dstDB, src, dst, schema := incrementalDamengFixture(t, ctx)
			mustIncrementalDamengExec(t, ctx, srcDB, `CREATE TABLE ValueCases (
				Id INT PRIMARY KEY, UnsignedNum BIGINT UNSIGNED, EnumVal ENUM('alpha','中文','quote''d'),
				SetVal SET('alpha','beta','gamma'), Note VARCHAR(40) CHARACTER SET latin1,
				Bits BIT(1), Amount DECIMAL(30,10), Payload VARBINARY(8)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
			mustIncrementalDamengExec(t, ctx, srcDB, `CREATE TABLE BagRows (ValueNum INT) ENGINE=InnoDB`)
			mustIncrementalDamengExec(t, ctx, dstDB, `CREATE TABLE "`+schema+`"."VALUECASES" (
				"ID" INT PRIMARY KEY, "UNSIGNEDNUM" NUMBER(20,0), "ENUMVAL" VARCHAR(255),
				"SETVAL" VARCHAR(1024), "NOTE" VARCHAR(160), "BITS" NUMBER(1), "AMOUNT" NUMBER(30,10), "PAYLOAD" BLOB)`)
			mustIncrementalDamengExec(t, ctx, dstDB, `CREATE TABLE "`+schema+`"."BAGROWS" ("VALUENUM" INT)`)
			start, err := cdc.CurrentPosition(ctx, srcDB)
			require.NoError(t, err)
			req := incrementalRequest{SrcConnID: src.ID, DstConnID: dst.ID, SrcDatabase: src.Database, TargetSchema: strings.ToLower(schema),
				StartMode: "incremental_only", PositionMode: mode, MigrateMode: "all", KeylessChangePolicy: "full_row_match"}
			if mode == "gtid" {
				req.StartGTID = start.GTID
			} else {
				req.StartFile, req.StartPosition = start.File, start.Pos
			}
			response := incrementalDamengHandler(t, ctx, StartIncremental, "", req)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var created struct {
				JobID string `json:"job_id"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &created))
			require.NotEmpty(t, created.JobID)
			t.Cleanup(func() {
				cdc.Registry.Cancel(created.JobID, "abort")
				require.Eventually(t, func() bool { return !cdc.Registry.Running(created.JobID) }, 5*time.Second, 20*time.Millisecond)
			})
			job := waitIncrementalDamengStatus(t, created.JobID, "running")
			require.Equal(t, schema, job.TargetSchema)
			cfg := configFromIncremental(job, src, dst)
			const unsigned = "18446744073709551615"
			const amount = "12345678901234567890.1234567890"
			tx, err := srcDB.BeginTx(ctx, nil)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `INSERT INTO ValueCases VALUES (1,?,'中文','gamma,alpha',?,b'1',?,?)`, unsigned, "café", amount, []byte{0, 1, 2, 255})
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `INSERT INTO ValueCases(Id) VALUES (2)`)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `INSERT INTO BagRows VALUES (7)`)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			rollback, err := srcDB.BeginTx(ctx, nil)
			require.NoError(t, err)
			_, err = rollback.ExecContext(ctx, `INSERT INTO ValueCases(Id) VALUES (99)`)
			require.NoError(t, err)
			require.NoError(t, rollback.Rollback())
			waitIncrementalDamengCheckpoint(t, ctx, srcDB, cfg)
			var gotUnsigned, gotEnum, gotSet, gotNote, gotAmount string
			var bits, payloadLength int
			require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT CAST("UNSIGNEDNUM" AS VARCHAR(40)),"ENUMVAL","SETVAL","NOTE",CAST("AMOUNT" AS VARCHAR(80)),"BITS",DBMS_LOB.GETLENGTH("PAYLOAD") FROM "`+schema+`"."VALUECASES" WHERE "ID"=1`).Scan(&gotUnsigned, &gotEnum, &gotSet, &gotNote, &gotAmount, &bits, &payloadLength))
			require.Equal(t, unsigned, gotUnsigned)
			require.Equal(t, "中文", gotEnum)
			require.Equal(t, "alpha,gamma", gotSet)
			require.Equal(t, "café", gotNote)
			require.Equal(t, strings.TrimRight(amount, "0"), strings.TrimRight(gotAmount, "0"))
			require.Equal(t, 1, bits)
			require.Equal(t, 4, payloadLength)
			mustIncrementalDamengExec(t, ctx, srcDB, `UPDATE ValueCases SET EnumVal='quote''d',SetVal='gamma,beta' WHERE Id=1`)
			mustIncrementalDamengExec(t, ctx, srcDB, `DELETE FROM ValueCases WHERE Id=2`)
			waitIncrementalDamengCheckpoint(t, ctx, srcDB, cfg)
			var count int
			require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+schema+`"."VALUECASES"`).Scan(&count))
			require.Equal(t, 1, count, "rolled-back source transactions must never appear in DM")
			require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT "ENUMVAL","SETVAL" FROM "`+schema+`"."VALUECASES" WHERE "ID"=1`).Scan(&gotEnum, &gotSet))
			require.Equal(t, "quote'd", gotEnum)
			require.Equal(t, "beta,gamma", gotSet)

			response = incrementalDamengHandler(t, ctx, PauseIncremental, created.JobID, nil)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			waitIncrementalDamengStatus(t, created.JobID, "paused_manual")
			mustIncrementalDamengExec(t, ctx, srcDB, `INSERT INTO BagRows VALUES (8)`)
			mustIncrementalDamengExec(t, ctx, srcDB, `INSERT INTO ValueCases(Id) VALUES (3)`)
			response = incrementalDamengHandler(t, ctx, ResumeIncremental, created.JobID, nil)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			waitIncrementalDamengCheckpoint(t, ctx, srcDB, cfg)
			require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+schema+`"."BAGROWS"`).Scan(&count))
			require.Equal(t, 2, count, "checkpoint restart must not replay the previous keyless INSERT")

			beforeDDL, exists, err := cdc.LoadTargetCheckpoint(ctx, cfg)
			require.NoError(t, err)
			require.True(t, exists)
			mustIncrementalDamengExec(t, ctx, srcDB, `ALTER TABLE ValueCases ADD Extra INT NULL`)
			job = waitIncrementalDamengStatus(t, created.JobID, "paused_ddl")
			require.Contains(t, job.BlockingDDL, "ALTER TABLE")
			afterDDL, _, err := cdc.LoadTargetCheckpoint(ctx, cfg)
			require.NoError(t, err)
			require.True(t, cdc.PositionEquivalent(beforeDDL, afterDDL), "unacknowledged DDL must not advance checkpoint")
			mustIncrementalDamengExec(t, ctx, dstDB, `ALTER TABLE "`+schema+`"."VALUECASES" ADD "EXTRA" INT NULL`)
			mustIncrementalDamengExec(t, ctx, srcDB, `INSERT INTO ValueCases(Id,Extra) VALUES (4,44)`)
			response = incrementalDamengHandler(t, ctx, AckIncrementalDDL, created.JobID, nil)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			waitIncrementalDamengCheckpoint(t, ctx, srcDB, cfg)
			var extra int
			require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT "EXTRA" FROM "`+schema+`"."VALUECASES" WHERE "ID"=4`).Scan(&extra))
			require.Equal(t, 44, extra)
			waitIncrementalDamengStatus(t, created.JobID, "running")
			response = incrementalDamengHandler(t, ctx, PrepareIncrementalCutover, created.JobID, nil)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			waitIncrementalDamengStatus(t, created.JobID, "ready_to_cutover")
			response = incrementalDamengHandler(t, ctx, StopIncremental, created.JobID, nil)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			waitIncrementalDamengStatus(t, created.JobID, "stopped")
		})
	}
}

func incrementalDamengHandler(t *testing.T, ctx context.Context, handler gin.HandlerFunc, jobID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	router := gin.New()
	router.POST("/jobs/:jobID", func(c *gin.Context) {
		c.Set("userID", uint(1))
		c.Set("role", "user")
		handler(c)
	})
	if jobID == "" {
		jobID = "create"
	}
	request := httptest.NewRequest(http.MethodPost, "/jobs/"+jobID, bytes.NewReader(payload)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func waitIncrementalDamengStatus(t *testing.T, jobID, status string) *store.IncrementalMigrationJob {
	t.Helper()
	var job *store.IncrementalMigrationJob
	require.Eventually(t, func() bool {
		var err error
		job, err = store.GetIncrementalJob(jobID)
		return err == nil && job.Status == status
	}, 15*time.Second, 20*time.Millisecond, "waiting for %s", status)
	return job
}

func waitIncrementalDamengCheckpoint(t *testing.T, ctx context.Context, sourceDB *sql.DB, cfg cdc.Config) {
	t.Helper()
	head, err := cdc.CurrentPosition(ctx, sourceDB)
	require.NoError(t, err)
	if cfg.Start.GTID == "" {
		head.GTID = ""
	}
	require.Eventually(t, func() bool {
		checkpoint, exists, err := cdc.LoadTargetCheckpoint(ctx, cfg)
		return err == nil && exists && cdc.PositionReached(checkpoint, head)
	}, 15*time.Second, 20*time.Millisecond)
}

// The opt-in test owns a unique source database and target schema. Both DSNs
// must be supplied explicitly; no existing application tables are reused.
func TestIncrementalDamengBootstrapIntegration(t *testing.T) {
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" {
		t.Skip("set CDC_DAMENG_INTEGRATION=1, CDC_MYSQL_DSN and CDC_DAMENG_DSN")
	}
	t.Run("snapshot_then_cdc_resume_and_cutover", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		srcDB, dstDB, src, dst, schema := incrementalDamengFixture(t, ctx)
		mustIncrementalDamengExec(t, ctx, srcDB, "SET SESSION sql_mode=''")
		mustIncrementalDamengExec(t, ctx, srcDB, "CREATE TABLE MixedOrders (Id INT AUTO_INCREMENT PRIMARY KEY, Payload VARCHAR(40), Stamp DATETIME(6)) ENGINE=InnoDB")
		mustIncrementalDamengExec(t, ctx, srcDB, "INSERT INTO MixedOrders VALUES (1,'snapshot','2024-00-15 12:34:56.123456'),(2,'delete-me','2024-05-06 07:08:09.123456')")
		job := incrementalDamengJob(t, src, dst, schema)
		preflight := cdc.Preflight(ctx, configFromIncremental(job, src, dst), false)
		require.True(t, preflight.OK, "%v", preflight.Errors)
		require.NoError(t, runIncrementalBootstrap(ctx, job, src, dst))
		require.True(t, job.BootstrapDone)
		require.Equal(t, 1, job.EffectiveCount)
		var timestamp string
		require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT TO_CHAR("STAMP", 'YYYY-MM-DD HH24:MI:SS.FF6') FROM "`+schema+`"."MIXEDORDERS" WHERE "ID"=1`).Scan(&timestamp))
		require.Equal(t, "1900-01-01 00:00:00.000000", timestamp, "snapshot must use the DM reader before MySQL normalizes partial zero dates")

		mustIncrementalDamengExec(t, ctx, srcDB, "UPDATE MixedOrders SET Payload='updated' WHERE Id=1")
		mustIncrementalDamengExec(t, ctx, srcDB, "DELETE FROM MixedOrders WHERE Id=2")
		mustIncrementalDamengExec(t, ctx, srcDB, "INSERT INTO MixedOrders VALUES (3,'after-snapshot','2025-01-02 03:04:05.654321')")
		runIncrementalDamengToHead(t, ctx, srcDB, job, src, dst)
		var payload string
		require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT "PAYLOAD" FROM "`+schema+`"."MIXEDORDERS" WHERE "ID"=1`).Scan(&payload))
		require.Equal(t, "updated", payload)
		var count int
		require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+schema+`"."MIXEDORDERS"`).Scan(&count))
		require.Equal(t, 2, count)

		// A second runner uses the target checkpoint, even though the job's
		// original snapshot start position has not changed.
		mustIncrementalDamengExec(t, ctx, srcDB, "INSERT INTO MixedOrders VALUES (4,'after-resume','2025-02-03 04:05:06.123456')")
		runIncrementalDamengToHead(t, ctx, srcDB, job, src, dst)
		cfg := configFromIncremental(job, src, dst)
		results, match, err := cdc.ValidateCounts(ctx, cfg)
		require.NoError(t, err)
		require.True(t, match, "%+v", results)
		require.Len(t, results, 1)
		require.Equal(t, int64(3), results[0].Target)
		require.NoError(t, cdc.SyncSequences(ctx, cfg))
		mustIncrementalDamengExec(t, ctx, dstDB, `INSERT INTO "`+schema+`"."MIXEDORDERS" ("PAYLOAD") VALUES ('target-write')`)
		var id int
		require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT "ID" FROM "`+schema+`"."MIXEDORDERS" WHERE "PAYLOAD"='target-write'`).Scan(&id))
		require.Greater(t, id, 4)
	})
	t.Run("first_and_middle_table_failures_require_review", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		srcDB, dstDB, src, dst, schema := incrementalDamengFixture(t, ctx)
		for _, table := range []string{"AFailed", "MFailed"} {
			mustIncrementalDamengExec(t, ctx, srcDB, "CREATE TABLE "+table+" (Id INT PRIMARY KEY, Shape GEOMETRY) ENGINE=InnoDB")
			mustIncrementalDamengExec(t, ctx, srcDB, "INSERT INTO "+table+" VALUES (1,ST_GeomFromText('POINT(1 2)'))")
		}
		for _, table := range []string{"BHealthy", "ZHealthy"} {
			mustIncrementalDamengExec(t, ctx, srcDB, "CREATE TABLE "+table+" (Id INT PRIMARY KEY, Payload VARCHAR(40)) ENGINE=InnoDB")
			mustIncrementalDamengExec(t, ctx, srcDB, "INSERT INTO "+table+" VALUES (1,'snapshot')")
		}
		job := incrementalDamengJob(t, src, dst, schema)
		job.LowerCaseNames = true // Existing API clients may still send this flag.
		require.NoError(t, store.UpdateIncrementalJob(job.JobID, map[string]any{"lower_case_names": true}))
		preflight := cdc.Preflight(ctx, configFromIncremental(job, src, dst), false)
		require.True(t, preflight.OK, "%v", preflight.Errors)
		require.ErrorIs(t, runIncrementalBootstrap(ctx, job, src, dst), cdc.ErrBootstrapReview)
		stored, err := store.GetIncrementalJob(job.JobID)
		require.NoError(t, err)
		require.Equal(t, "paused_bootstrap_review", stored.Status)
		require.Equal(t, 2, stored.EffectiveCount)
		require.Equal(t, 2, stored.ExcludedCount)
		for _, table := range []string{"BHEALTHY", "ZHEALTHY"} {
			var count int
			require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+schema+`"."`+table+`"`).Scan(&count))
			require.Equal(t, 1, count, "healthy tables after each failure must still be copied")
		}
		for _, table := range []string{"AFAILED", "MFAILED"} {
			var count int
			require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=? AND TABLE_NAME=?`, schema, table).Scan(&count))
			require.Zero(t, count)
		}
		cfg := configFromIncremental(job, src, dst)
		record, exists, err := cdc.LoadTargetBootstrapRecord(ctx, cfg)
		require.NoError(t, err)
		require.True(t, exists)
		require.Equal(t, "review_pending", record.State)
		require.ElementsMatch(t, []string{"BHealthy", "ZHealthy"}, record.EffectiveTables)
		review, found := loadIncrementalFailureReview(ctx, stored)
		require.True(t, found)
		script := renderIncrementalFailedDDL(stored, review, exportableBootstrapFailures(review), time.Now())
		require.Contains(t, script, "AFailed")
		require.Contains(t, script, "MFailed")
		require.Contains(t, script, "GEOMETRY")
		require.NoError(t, cdc.FinalizeTargetBootstrap(ctx, cfg, record))
		record.State = "completed"
		require.NoError(t, applyCompletedBootstrapRecord(job, record))
		mustIncrementalDamengExec(t, ctx, srcDB, "INSERT INTO AFailed VALUES (2,ST_GeomFromText('POINT(3 4)'))")
		for _, table := range []string{"BHealthy", "ZHealthy"} {
			mustIncrementalDamengExec(t, ctx, srcDB, "UPDATE "+table+" SET Payload='after-review' WHERE Id=1")
		}
		runIncrementalDamengToHead(t, ctx, srcDB, job, src, dst)
		for _, table := range []string{"BHEALTHY", "ZHEALTHY"} {
			var payload string
			require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT "PAYLOAD" FROM "`+schema+`"."`+table+`" WHERE "ID"=1`).Scan(&payload))
			require.Equal(t, "after-review", payload)
		}
	})
}

func incrementalDamengFixture(t *testing.T, ctx context.Context) (*sql.DB, *sql.DB, *store.Connection, *store.Connection, string) {
	t.Helper()
	myDSN, dmDSN := os.Getenv("CDC_MYSQL_DSN"), os.Getenv("CDC_DAMENG_DSN")
	require.NotEmpty(t, myDSN)
	require.NotEmpty(t, dmDSN)
	myCfg, err := mysql.ParseDSN(myDSN)
	require.NoError(t, err)
	dmURL, err := url.Parse(dmDSN)
	require.NoError(t, err)
	require.NotNil(t, dmURL.User)
	host, port, err := net.SplitHostPort(myCfg.Addr)
	require.NoError(t, err)
	myPort, err := strconv.Atoi(port)
	require.NoError(t, err)
	dmPort, err := strconv.Atoi(dmURL.Port())
	require.NoError(t, err)
	password, _ := dmURL.User.Password()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 16)
	database, schema := "cdc_api_"+suffix, "CDC_API_"+strings.ToUpper(suffix)
	src := &store.Connection{OwnerID: 1, Name: "CDC MySQL fixture", DBType: "mysql", Host: host, Port: myPort, Username: myCfg.User, Password: myCfg.Passwd, Database: database}
	dst := &store.Connection{OwnerID: 1, Name: "CDC DM fixture", DBType: "dameng", Host: dmURL.Hostname(), Port: dmPort, Username: dmURL.User.Username(), Password: password}
	admin, err := sql.Open("mysql", myDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	target, err := sql.Open("dm", dmDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	mustIncrementalDamengExec(t, ctx, admin, "CREATE DATABASE `"+database+"`")
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE `" + database + "`") })
	mustIncrementalDamengExec(t, ctx, target, `CREATE SCHEMA "`+schema+`" AUTHORIZATION "`+strings.ReplaceAll(dst.Username, `"`, `""`)+`"`)
	t.Cleanup(func() { _, _ = target.Exec(`DROP SCHEMA "` + schema + `" CASCADE`) })
	sourceDB, err := sql.Open("mysql", buildDSN(src))
	require.NoError(t, err)
	sourceDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sourceDB.Close() })
	store.Init(&config.Config{SQLitePath: filepath.Join(t.TempDir(), "incremental.db")})
	stateDB, err := store.DB.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = stateDB.Close() })
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	return sourceDB, target, src, dst, schema
}

func incrementalDamengJob(t *testing.T, src, dst *store.Connection, schema string) *store.IncrementalMigrationJob {
	t.Helper()
	job := &store.IncrementalMigrationJob{OwnerID: 1, JobID: fmt.Sprintf("dm-bootstrap-%d", time.Now().UnixNano()), SrcConnID: src.ID, DstConnID: dst.ID,
		SrcDBType: "mysql", DstDBType: "dameng", SrcDatabase: src.Database, TargetSchema: schema, StartMode: "full_then_cdc", PositionMode: "gtid",
		MigrateMode: "all", BootstrapPolicy: "review_and_exclude", BootstrapState: "pending", Status: "initializing", KeylessChangePolicy: "full_row_match",
		ServerID: uint32(3000000000 + time.Now().UnixNano()%1000000000)}
	require.NoError(t, store.CreateIncrementalJob(job))
	return job
}

func mustIncrementalDamengExec(t *testing.T, ctx context.Context, db *sql.DB, statement string) {
	t.Helper()
	_, err := db.ExecContext(ctx, statement)
	require.NoError(t, err, statement)
}

func runIncrementalDamengToHead(t *testing.T, ctx context.Context, sourceDB *sql.DB, job *store.IncrementalMigrationJob, src, dst *store.Connection) {
	t.Helper()
	head, err := cdc.CurrentPosition(ctx, sourceDB)
	require.NoError(t, err)
	runCtx, err := cdc.Registry.Register(job.JobID)
	require.NoError(t, err)
	defer cdc.Registry.Remove(job.JobID)
	runCtx, cancel := context.WithTimeout(runCtx, 20*time.Second)
	defer cancel()
	require.True(t, cdc.Registry.RequestCutover(job.JobID, head))
	require.ErrorIs(t, runCDC(runCtx, job, src, dst), cdc.ErrCutoverReady)
}
