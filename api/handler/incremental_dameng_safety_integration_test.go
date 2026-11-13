//go:build integration

package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"dbgold/datamigrate/cdc"
	"dbgold/datamigrate/source"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func startDamengSafetyJob(t *testing.T, ctx context.Context, src, dst *store.Connection, schema string, start cdc.Position, mode string) *store.IncrementalMigrationJob {
	t.Helper()
	req := incrementalRequest{SrcConnID: src.ID, DstConnID: dst.ID, SrcDatabase: src.Database, TargetSchema: schema, StartMode: "incremental_only", MigrateMode: "all"}
	if mode == "gtid" {
		req.StartGTID = start.GTID
	} else {
		req.StartFile, req.StartPosition = start.File, start.Pos
	}
	response := incrementalDamengHandler(t, ctx, StartIncremental, "", req)
	require.Equal(t, 200, response.Code, response.Body.String())
	var created struct {
		JobID string `json:"job_id"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &created))
	t.Cleanup(func() {
		cdc.Registry.Cancel(created.JobID, "abort")
		require.Eventually(t, func() bool { return !cdc.Registry.Running(created.JobID) }, 5*time.Second, 10*time.Millisecond)
	})
	return waitIncrementalDamengStatus(t, created.JobID, "running")
}
func TestIncrementalDamengSchemaAndAcknowledgementSafetyIntegration(t *testing.T) {
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" {
		t.Skip("requires disposable MySQL and DM")
	}
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"gtid", "file"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			source, target, src, dst, schema := incrementalDamengFixture(t, ctx)
			mustIncrementalDamengExec(t, ctx, source, "CREATE TABLE Items (Id INT PRIMARY KEY,A INT,B INT)")
			mustIncrementalDamengExec(t, ctx, source, "CREATE TABLE Bag (V INT)")
			mustIncrementalDamengExec(t, ctx, source, "INSERT INTO Items VALUES (1,1,2)")
			mustIncrementalDamengExec(t, ctx, target, `CREATE TABLE "`+schema+`"."ITEMS" ("ID" INT PRIMARY KEY,"A" INT,"B" INT)`)
			mustIncrementalDamengExec(t, ctx, target, `CREATE TABLE "`+schema+`"."BAG" ("V" INT)`)
			mustIncrementalDamengExec(t, ctx, target, `INSERT INTO "`+schema+`"."ITEMS" VALUES (1,1,2)`)
			start, err := cdc.CurrentPosition(ctx, source)
			require.NoError(t, err)
			job := startDamengSafetyJob(t, ctx, src, dst, schema, start, mode)
			cfg := configFromIncremental(job, src, dst)
			pause := incrementalDamengHandler(t, ctx, PauseIncremental, job.JobID, nil)
			require.Equal(t, 200, pause.Code, pause.Body.String())
			waitIncrementalDamengStatus(t, job.JobID, "paused_manual")
			mustIncrementalDamengExec(t, ctx, source, "UPDATE Items SET A=11,B=22 WHERE Id=1")
			mustIncrementalDamengExec(t, ctx, source, "ALTER TABLE Items MODIFY B INT AFTER Id")
			resume := incrementalDamengHandler(t, ctx, ResumeIncremental, job.JobID, nil)
			require.Equal(t, 200, resume.Code, resume.Body.String())
			job = waitIncrementalDamengStatus(t, job.JobID, "paused_ddl")
			require.Eventually(t, func() bool { return !cdc.Registry.Running(job.JobID) }, time.Second, 10*time.Millisecond)
			var a, b int
			require.NoError(t, target.QueryRowContext(ctx, `SELECT "A","B" FROM "`+schema+`"."ITEMS" WHERE "ID"=1`).Scan(&a, &b))
			require.Equal(t, 11, a)
			require.Equal(t, 22, b, "old UPDATE must use old column order")
			boundary := cdc.Position{File: job.BlockingFile, Pos: job.BlockingPos, GTID: job.BlockingGTID}
			before, exists, err := cdc.LoadTargetCheckpoint(ctx, cfg)
			require.NoError(t, err)
			require.True(t, exists)
			require.False(t, cdc.PositionReached(before, boundary))
			// Fail only the application-state update AFTER the target commit.
			require.NoError(t, store.DB.Callback().Update().Before("gorm:update").Register("test:fail-ddl-mirror", func(db *gorm.DB) {
				if fields, ok := db.Statement.Dest.(map[string]any); ok && fields["status"] == "initializing" {
					db.AddError(errors.New("injected application-state persistence failure"))
				}
			}))
			failedAck := incrementalDamengHandler(t, ctx, AckIncrementalDDL, job.JobID, nil)
			require.NoError(t, store.DB.Callback().Update().Remove("test:fail-ddl-mirror"))
			require.Equal(t, 500, failedAck.Code, failedAck.Body.String())
			require.False(t, cdc.Registry.Running(job.JobID))
			waitIncrementalDamengStatus(t, job.JobID, "paused_ddl")
			saved, exists, err := cdc.LoadTargetBootstrapRecord(ctx, cfg)
			require.NoError(t, err)
			require.True(t, exists)
			require.True(t, cdc.PositionEquivalent(saved.Position, boundary))
			require.Equal(t, []string{"Id", "B", "A"}, saved.SourceSchema.Tables[1].Columns)
			mustIncrementalDamengExec(t, ctx, source, "INSERT INTO Bag VALUES (7)")
			var wg sync.WaitGroup
			codes := make(chan int, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					response := incrementalDamengHandler(t, ctx, AckIncrementalDDL, job.JobID, nil)
					codes <- response.Code
				}()
			}
			wg.Wait()
			close(codes)
			success, conflict := 0, 0
			for code := range codes {
				if code == http.StatusOK {
					success++
				}
				if code == http.StatusConflict {
					conflict++
				}
			}
			require.Equal(t, 1, success)
			require.Equal(t, 1, conflict)
			waitIncrementalDamengCheckpoint(t, ctx, source, cfg)
			mustIncrementalDamengExec(t, ctx, source, "UPDATE Items SET A=33,B=44 WHERE Id=1")
			waitIncrementalDamengCheckpoint(t, ctx, source, cfg)
			require.NoError(t, target.QueryRowContext(ctx, `SELECT "A","B" FROM "`+schema+`"."ITEMS" WHERE "ID"=1`).Scan(&a, &b))
			require.Equal(t, 33, a)
			require.Equal(t, 44, b)
			pause = incrementalDamengHandler(t, ctx, PauseIncremental, job.JobID, nil)
			require.Equal(t, 200, pause.Code)
			waitIncrementalDamengStatus(t, job.JobID, "paused_manual")
			resume = incrementalDamengHandler(t, ctx, ResumeIncremental, job.JobID, nil)
			require.Equal(t, 200, resume.Code)
			waitIncrementalDamengStatus(t, job.JobID, "running")
			var count int
			require.NoError(t, target.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+schema+`"."BAG"`).Scan(&count))
			require.Equal(t, 1, count)
			// Add/drop column snapshots are accepted only after target adaptation.
			mustIncrementalDamengExec(t, ctx, source, "ALTER TABLE Items ADD C INT")
			job = waitIncrementalDamengStatus(t, job.JobID, "paused_ddl")
			require.Eventually(t, func() bool { return !cdc.Registry.Running(job.JobID) }, time.Second, 10*time.Millisecond)
			badAck := incrementalDamengHandler(t, ctx, AckIncrementalDDL, job.JobID, nil)
			require.Equal(t, 409, badAck.Code, badAck.Body.String())
			require.Contains(t, badAck.Body.String(), "不兼容")
			mustIncrementalDamengExec(t, ctx, target, `ALTER TABLE "`+schema+`"."ITEMS" ADD "C" INT`)
			ack := incrementalDamengHandler(t, ctx, AckIncrementalDDL, job.JobID, nil)
			require.Equal(t, 200, ack.Code, ack.Body.String())
			mustIncrementalDamengExec(t, ctx, source, "UPDATE Items SET C=55 WHERE Id=1")
			waitIncrementalDamengCheckpoint(t, ctx, source, cfg)
			require.NoError(t, target.QueryRowContext(ctx, `SELECT "C" FROM "`+schema+`"."ITEMS" WHERE "ID"=1`).Scan(&count))
			require.Equal(t, 55, count)
			mustIncrementalDamengExec(t, ctx, source, "ALTER TABLE Items DROP COLUMN B")
			job = waitIncrementalDamengStatus(t, job.JobID, "paused_ddl")
			require.Eventually(t, func() bool { return !cdc.Registry.Running(job.JobID) }, time.Second, 10*time.Millisecond)
			mustIncrementalDamengExec(t, ctx, target, `ALTER TABLE "`+schema+`"."ITEMS" DROP COLUMN "B"`)
			ack = incrementalDamengHandler(t, ctx, AckIncrementalDDL, job.JobID, nil)
			require.Equal(t, 200, ack.Code, ack.Body.String())
			mustIncrementalDamengExec(t, ctx, source, "UPDATE Items SET A=66 WHERE Id=1")
			waitIncrementalDamengCheckpoint(t, ctx, source, cfg)
			require.NoError(t, target.QueryRowContext(ctx, `SELECT "A" FROM "`+schema+`"."ITEMS" WHERE "ID"=1`).Scan(&a))
			require.Equal(t, 66, a)
			// Two pending DDLs cannot be acknowledged using only the latest schema.
			mustIncrementalDamengExec(t, ctx, source, "ALTER TABLE Items ADD D INT")
			job = waitIncrementalDamengStatus(t, job.JobID, "paused_ddl")
			require.Eventually(t, func() bool { return !cdc.Registry.Running(job.JobID) }, time.Second, 10*time.Millisecond)
			mustIncrementalDamengExec(t, ctx, source, "ALTER TABLE Items ADD E INT")
			mustIncrementalDamengExec(t, ctx, target, `ALTER TABLE "`+schema+`"."ITEMS" ADD "E" INT`)
			mustIncrementalDamengExec(t, ctx, target, `ALTER TABLE "`+schema+`"."ITEMS" ADD "D" INT`)
			before, _, err = cdc.LoadTargetCheckpoint(ctx, cfg)
			require.NoError(t, err)
			ack = incrementalDamengHandler(t, ctx, AckIncrementalDDL, job.JobID, nil)
			require.Equal(t, 409, ack.Code, ack.Body.String())
			require.Contains(t, ack.Body.String(), "源 DDL")
			after, _, err := cdc.LoadTargetCheckpoint(ctx, cfg)
			require.NoError(t, err)
			require.True(t, cdc.PositionEquivalent(before, after))
		})
	}
}

func TestIncrementalDamengRejectsHistoricalDDLStartIntegration(t *testing.T) {
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" {
		t.Skip("requires disposable MySQL and DM")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	source, target, src, dst, schema := incrementalDamengFixture(t, ctx)
	mustIncrementalDamengExec(t, ctx, source, "CREATE TABLE Items (Id INT PRIMARY KEY,A INT,B INT)")
	mustIncrementalDamengExec(t, ctx, target, `CREATE TABLE "`+schema+`"."ITEMS" ("ID" INT PRIMARY KEY,"A" INT,"B" INT)`)
	start, err := cdc.CurrentPosition(ctx, source)
	require.NoError(t, err)
	mustIncrementalDamengExec(t, ctx, source, "INSERT INTO Items VALUES (1,11,22)")
	insertHead, err := cdc.CurrentPosition(ctx, source)
	require.NoError(t, err)
	mustIncrementalDamengExec(t, ctx, source, "ALTER TABLE Items MODIFY B INT AFTER Id")
	for _, mode := range []string{"gtid", "file"} {
		req := incrementalRequest{SrcConnID: src.ID, DstConnID: dst.ID, SrcDatabase: src.Database, TargetSchema: schema, StartMode: "incremental_only", MigrateMode: "all"}
		if mode == "gtid" {
			req.StartGTID = start.GTID
		} else {
			req.StartFile, req.StartPosition = start.File, start.Pos
		}
		response := incrementalDamengHandler(t, ctx, StartIncremental, "", req)
		require.Equal(t, 400, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), "源 DDL")
	}
	// A GTID hole must not hide a DDL claimed as already executed while older
	// rows (encoded with the pre-DDL column order) are still missing.
	head, err := cdc.CurrentPosition(ctx, source)
	require.NoError(t, err)
	provided, err := gomysql.ParseMysqlGTIDSet(head.GTID)
	require.NoError(t, err)
	missing, err := gomysql.ParseMysqlGTIDSet(insertHead.GTID)
	require.NoError(t, err)
	prior, err := gomysql.ParseMysqlGTIDSet(start.GTID)
	require.NoError(t, err)
	require.NoError(t, missing.(*gomysql.MysqlGTIDSet).Minus(*prior.(*gomysql.MysqlGTIDSet)))
	require.NoError(t, provided.(*gomysql.MysqlGTIDSet).Minus(*missing.(*gomysql.MysqlGTIDSet)))
	response := incrementalDamengHandler(t, ctx, StartIncremental, "", incrementalRequest{SrcConnID: src.ID, DstConnID: dst.ID, SrcDatabase: src.Database, TargetSchema: schema, StartMode: "incremental_only", MigrateMode: "all", StartGTID: provided.String()})
	require.Equal(t, 400, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "源 DDL")
	var count int
	require.NoError(t, target.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "%s"."ITEMS"`, schema)).Scan(&count))
	require.Zero(t, count)
}

func TestIncrementalDamengUnsupportedTransactionsIntegration(t *testing.T) {
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" {
		t.Skip("requires disposable MySQL and DM")
	}
	for _, kind := range []string{"compression", "xa"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			source, target, src, dst, schema := incrementalDamengFixture(t, ctx)
			mustIncrementalDamengExec(t, ctx, source, "CREATE TABLE Items (Id INT,Payload VARCHAR(4000))")
			mustIncrementalDamengExec(t, ctx, target, `CREATE TABLE "`+schema+`"."ITEMS" ("ID" INT,"PAYLOAD" VARCHAR(4000))`)
			start, err := cdc.CurrentPosition(ctx, source)
			require.NoError(t, err)
			job := startDamengSafetyJob(t, ctx, src, dst, schema, start, "gtid")
			cfg := configFromIncremental(job, src, dst)
			before, exists, err := cdc.LoadTargetCheckpoint(ctx, cfg)
			require.NoError(t, err)
			require.True(t, exists)
			if kind == "compression" {
				mustIncrementalDamengExec(t, ctx, source, "SET SESSION binlog_transaction_compression=ON")
				mustIncrementalDamengExec(t, ctx, source, "INSERT INTO Items VALUES (1,REPEAT('x',2000))")
				mustIncrementalDamengExec(t, ctx, source, "SET SESSION binlog_transaction_compression=OFF")
			} else {
				mustIncrementalDamengExec(t, ctx, source, "XA START 'cdc_safety'")
				mustIncrementalDamengExec(t, ctx, source, "INSERT INTO Items VALUES (1,'prepared')")
				mustIncrementalDamengExec(t, ctx, source, "XA END 'cdc_safety'")
				mustIncrementalDamengExec(t, ctx, source, "XA PREPARE 'cdc_safety'")
				t.Cleanup(func() { _, _ = source.Exec("XA ROLLBACK 'cdc_safety'") })
			}
			mustIncrementalDamengExec(t, ctx, source, "INSERT INTO Items VALUES (2,'ordinary transaction')")
			failed := waitIncrementalDamengStatus(t, job.JobID, "failed")
			require.Contains(t, failed.LastError, "原检查点保留")
			after, _, err := cdc.LoadTargetCheckpoint(ctx, cfg)
			require.NoError(t, err)
			require.True(t, cdc.PositionEquivalent(before, after))
			var count int
			require.NoError(t, target.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+schema+`"."ITEMS"`).Scan(&count))
			require.Zero(t, count)
			if kind == "compression" {
				mustIncrementalDamengExec(t, ctx, source, "SET GLOBAL binlog_transaction_compression=ON")
				defer func() { _, _ = source.Exec("SET GLOBAL binlog_transaction_compression=OFF") }()
				pf := cdc.Preflight(ctx, cfg, false)
				require.False(t, pf.OK)
				require.Contains(t, fmt.Sprint(pf.Errors), "binlog_transaction_compression")
			}
		})
	}
}

func TestIncrementalDamengSnapshotPinsMetadataIntegration(t *testing.T) {
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" {
		t.Skip("requires disposable MySQL and DM")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	srcDB, _, src, _, _ := incrementalDamengFixture(t, ctx)
	srcDB.SetMaxOpenConns(3)
	mustIncrementalDamengExec(t, ctx, srcDB, "CREATE TABLE Items (Id INT PRIMARY KEY)")
	mustIncrementalDamengExec(t, ctx, srcDB, "CREATE TABLE ZEmpty (Value INT)")
	reader, err := source.NewMySQLForDameng(buildDSN(src), src.Database, source.ConnPoolConfig{MaxOpenConns: 3})
	require.NoError(t, err)
	defer reader.Close()
	var snapshot cdc.SourceSchemaSnapshot
	_, err = reader.CaptureConsistentSnapshotWithMetadata(ctx, func(tx *sql.Tx) error {
		tables, e := cdc.LoadTables(ctx, tx, src.Database, "all", "")
		if e != nil {
			return e
		}
		if e = reader.PinSnapshotTables(ctx, []string{"Items", "ZEmpty"}); e != nil {
			return e
		}
		snapshot, e = cdc.NewSourceSchema(src.Database, tables)
		return e
	})
	require.NoError(t, err)
	require.Equal(t, []string{"Value"}, snapshot.Tables[1].Columns)
	mustIncrementalDamengExec(t, ctx, srcDB, "CREATE TABLE AddedLater (Id INT)")
	names, err := reader.ListTables(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"Items", "ZEmpty"}, names, "snapshot table scope must not change")
	ddlDone := make(chan error, 1)
	go func() { _, e := srcDB.ExecContext(ctx, "ALTER TABLE ZEmpty ADD Extra INT"); ddlDone <- e }()
	require.Eventually(t, func() bool {
		var waiting int
		e := srcDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE DB=? AND STATE='Waiting for table metadata lock' AND INFO='ALTER TABLE ZEmpty ADD Extra INT'", src.Database).Scan(&waiting)
		return e == nil && waiting == 1
	}, 5*time.Second, 20*time.Millisecond, "even an empty selected table must retain its metadata lock")
	require.NoError(t, reader.FinishSnapshot())
	require.NoError(t, <-ddlDone, "DDL must proceed after snapshot release")
	names, err = reader.ListTables(ctx)
	require.NoError(t, err)
	require.Contains(t, names, "AddedLater")
}

func TestIncrementalDamengAcknowledgeAndAbortIntegration(t *testing.T) {
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" {
		t.Skip("requires disposable MySQL and DM")
	}
	for attempt := 0; attempt < 3; attempt++ {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			srcDB, target, src, dst, schema := incrementalDamengFixture(t, ctx)
			mustIncrementalDamengExec(t, ctx, srcDB, "CREATE TABLE Bag (V INT)")
			mustIncrementalDamengExec(t, ctx, target, `CREATE TABLE "`+schema+`"."BAG" ("V" INT)`)
			start, err := cdc.CurrentPosition(ctx, srcDB)
			require.NoError(t, err)
			job := startDamengSafetyJob(t, ctx, src, dst, schema, start, "gtid")
			cfg := configFromIncremental(job, src, dst)
			mustIncrementalDamengExec(t, ctx, srcDB, "ALTER TABLE Bag COMMENT='ack versus abort'")
			job = waitIncrementalDamengStatus(t, job.JobID, "paused_ddl")
			require.Eventually(t, func() bool { return !cdc.Registry.Running(job.JobID) }, time.Second, 10*time.Millisecond)
			before, _, err := cdc.LoadTargetCheckpoint(ctx, cfg)
			require.NoError(t, err)
			ready := make(chan struct{})
			results := make(chan int, 2)
			for _, handler := range []gin.HandlerFunc{AckIncrementalDDL, AbortIncremental} {
				go func(h gin.HandlerFunc) { <-ready; results <- incrementalDamengHandler(t, ctx, h, job.JobID, nil).Code }(handler)
			}
			close(ready)
			for range 2 {
				code := <-results
				require.Contains(t, []int{200, 409}, code)
			}
			waitIncrementalDamengStatus(t, job.JobID, "aborted")
			require.Eventually(t, func() bool { return !cdc.Registry.Running(job.JobID) }, time.Second, 10*time.Millisecond)
			after, _, err := cdc.LoadTargetCheckpoint(ctx, cfg)
			require.NoError(t, err)
			require.True(t, cdc.PositionReached(after, before), "checkpoint may never rewind")
			retry := incrementalDamengHandler(t, ctx, AckIncrementalDDL, job.JobID, nil)
			require.Equal(t, 409, retry.Code)
			require.False(t, cdc.Registry.Running(job.JobID))
		})
	}
}

func TestIncrementalDamengFullFileSnapshotRecoveryIntegration(t *testing.T) {
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" {
		t.Skip("requires disposable MySQL and DM")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	srcDB, target, src, dst, schema := incrementalDamengFixture(t, ctx)
	mustIncrementalDamengExec(t, ctx, srcDB, "CREATE TABLE Bag (V INT)")
	mustIncrementalDamengExec(t, ctx, srcDB, "INSERT INTO Bag VALUES (1)")
	job := incrementalDamengJob(t, src, dst, schema)
	job.PositionMode = "file"
	require.NoError(t, store.UpdateIncrementalJob(job.JobID, map[string]any{"position_mode": "file"}))
	require.NoError(t, runIncrementalBootstrap(ctx, job, src, dst))
	cfg := configFromIncremental(job, src, dst)
	saved, exists, err := cdc.LoadTargetBootstrapRecord(ctx, cfg)
	require.NoError(t, err)
	require.True(t, exists)
	require.Empty(t, saved.Position.GTID)
	require.NoError(t, saved.SourceSchema.Validate(src.Database))
	// A stale/corrupt application mirror must not override target metadata.
	job.SourceSchemaJSON = `{"version":999}`
	require.NoError(t, store.UpdateIncrementalJob(job.JobID, map[string]any{"source_schema_json": job.SourceSchemaJSON}))
	mustIncrementalDamengExec(t, ctx, srcDB, "UPDATE Bag SET V=2")
	runIncrementalDamengToHead(t, ctx, srcDB, job, src, dst)
	mustIncrementalDamengExec(t, ctx, srcDB, "INSERT INTO Bag VALUES (3)")
	fresh, err := store.GetIncrementalJob(job.JobID)
	require.NoError(t, err)
	runIncrementalDamengToHead(t, ctx, srcDB, fresh, src, dst)
	var count, total int
	require.NoError(t, target.QueryRowContext(ctx, `SELECT COUNT(*),SUM("V") FROM "`+schema+`"."BAG"`).Scan(&count, &total))
	require.Equal(t, 2, count)
	require.Equal(t, 5, total)
}
