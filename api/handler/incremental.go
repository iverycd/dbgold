package handler

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/cdc"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/middleware"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type incrementalRequest struct {
	SrcConnID      uint   `json:"src_conn_id" binding:"required"`
	DstConnID      uint   `json:"dst_conn_id" binding:"required"`
	SrcDatabase    string `json:"src_database" binding:"required"`
	TargetSchema   string `json:"target_schema" binding:"required"`
	StartMode      string `json:"start_mode" binding:"required,oneof=full_then_cdc incremental_only"`
	PositionMode   string `json:"position_mode" binding:"omitempty,oneof=auto gtid file"`
	StartGTID      string `json:"start_gtid"`
	StartFile      string `json:"start_file"`
	StartPosition  uint32 `json:"start_position"`
	ServerID       uint32 `json:"server_id"`
	MigrateMode    string `json:"migrate_mode" binding:"required,oneof=all include exclude"`
	TableFilter    string `json:"table_filter"`
	LowerCaseNames bool   `json:"lower_case_names"`
}

func incrementalConfig(req incrementalRequest, jobID string, src, dst *store.Connection) cdc.Config {
	srcCopy := *src
	srcCopy.Database = req.SrcDatabase
	return cdc.Config{JobID: jobID, SourceDSN: buildDSN(&srcCopy), SourceHost: src.Host, SourcePort: uint16(src.Port), SourceUser: src.Username,
		SourcePassword: src.Password, SourceDatabase: req.SrcDatabase, TargetDSN: buildDSN(dst), TargetSchema: req.TargetSchema,
		Mode: req.MigrateMode, Filter: req.TableFilter, LowerCaseNames: req.LowerCaseNames, ServerID: req.ServerID,
		Start: cdc.Position{File: req.StartFile, Pos: req.StartPosition, GTID: req.StartGTID}}
}

func validateIncrementalConnections(c *gin.Context, req incrementalRequest) (*store.Connection, *store.Connection, bool) {
	src, e := store.GetConnectionOwned(req.SrcConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if e != nil || src.DBType != "mysql" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "增量迁移源库必须是可访问的 MySQL 连接"})
		return nil, nil, false
	}
	dst, e := store.GetConnectionOwned(req.DstConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if e != nil || dst.DBType != "postgres" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "增量迁移目标库必须是可访问的 PostgreSQL 连接"})
		return nil, nil, false
	}
	if req.StartMode == "incremental_only" {
		hasGTID := strings.TrimSpace(req.StartGTID) != ""
		hasFile := req.StartFile != "" && req.StartPosition >= 4
		if hasGTID == hasFile {
			c.JSON(http.StatusBadRequest, gin.H{"error": "仅增量模式必须且只能提供 GTID 或 file/position 之一"})
			return nil, nil, false
		}
	}
	return src, dst, true
}

func PreflightIncremental(c *gin.Context) {
	var req incrementalRequest
	if e := c.ShouldBindJSON(&req); e != nil {
		c.JSON(400, gin.H{"error": e.Error()})
		return
	}
	src, dst, ok := validateIncrementalConnections(c, req)
	if !ok {
		return
	}
	cfg := incrementalConfig(req, "preflight", src, dst)
	c.JSON(200, cdc.Preflight(c.Request.Context(), cfg, req.StartMode == "incremental_only"))
}

func StartIncremental(c *gin.Context) {
	var req incrementalRequest
	if e := c.ShouldBindJSON(&req); e != nil {
		c.JSON(400, gin.H{"error": e.Error()})
		return
	}
	src, dst, ok := validateIncrementalConnections(c, req)
	if !ok {
		return
	}
	jobID := uuid.NewString()
	if req.ServerID == 0 {
		h := fnv.New32a()
		_, _ = h.Write([]byte(jobID))
		req.ServerID = 1000 + h.Sum32()%4000000000
	}
	cfg := incrementalConfig(req, jobID, src, dst)
	pf := cdc.Preflight(c.Request.Context(), cfg, req.StartMode == "incremental_only")
	if !pf.OK {
		c.JSON(400, gin.H{"error": "增量迁移预检未通过", "preflight": pf})
		return
	}
	j := &store.IncrementalMigrationJob{OwnerID: middleware.GetCurrentUserID(c), JobID: jobID, SrcConnID: req.SrcConnID, DstConnID: req.DstConnID,
		SrcDatabase: req.SrcDatabase, TargetSchema: req.TargetSchema, StartMode: req.StartMode, PositionMode: req.PositionMode, StartGTID: req.StartGTID,
		StartFile: req.StartFile, StartPosition: req.StartPosition, ServerID: req.ServerID, MigrateMode: req.MigrateMode, TableFilter: req.TableFilter,
		LowerCaseNames: req.LowerCaseNames, Status: "initializing", Phase: "initializing"}
	if e := store.CreateIncrementalJob(j); e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	}
	launchIncremental(j, src, dst)
	c.JSON(200, gin.H{"job_id": jobID, "preflight": pf})
}

func launchIncremental(j *store.IncrementalMigrationJob, src, dst *store.Connection) {
	ctx, err := cdc.Registry.Register(j.JobID)
	if err != nil {
		return
	}
	go func() {
		var runErr error
		if j.StartMode == "full_then_cdc" && j.CheckpointFile == "" && j.CheckpointGTID == "" {
			runErr = runIncrementalBootstrap(ctx, j, src, dst)
		}
		if runErr == nil && ctx.Err() == nil {
			runErr = runCDC(ctx, j, src, dst)
		}
		action := cdc.Registry.Remove(j.JobID)
		if errors.Is(runErr, cdc.ErrDDLPause) {
			return
		}
		if action == "pause" {
			_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "paused_manual", "phase": "paused", "summary": "用户已暂停"})
			return
		}
		if action == "stop" {
			cfg := configFromIncremental(j, src, dst)
			_ = cdc.SyncSequences(context.Background(), cfg)
			now := time.Now()
			_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "stopped", "phase": "stopped", "summary": "用户已停止", "finished_at": &now})
			return
		}
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "failed", "phase": "failed", "last_error": runErr.Error(), "summary": runErr.Error()})
		}
	}()
}

func runIncrementalBootstrap(ctx context.Context, j *store.IncrementalMigrationJob, src, dst *store.Connection) error {
	_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "snapshot", "phase": "snapshot", "summary": "正在建立一致快照"})
	srcCopy := *src
	srcCopy.Database = j.SrcDatabase
	r, e := source.NewMySQL(buildDSN(&srcCopy), j.SrcDatabase, source.ConnPoolConfig{MaxOpenConns: 4, MaxIdleConns: 2})
	if e != nil {
		return e
	}
	defer r.Close()
	pos, e := r.CaptureConsistentSnapshot(ctx)
	if e != nil {
		return e
	}
	w, e := target.NewPostgres(buildDSN(dst), j.TargetSchema, target.ConnPoolConfig{})
	if e != nil {
		return e
	}
	defer w.Close()
	logJob := &datamigrate.Job{LogCh: make(chan string, 512)}
	done := make(chan struct{})
	go func() {
		for range logJob.LogCh {
		}
		close(done)
	}()
	m := datamigrate.NewMigrator(r, w, logJob, datamigrate.Config{PageSize: 20000, MaxParallel: 1, IntraTableParallel: 1, Mode: j.MigrateMode, Filter: j.TableFilter, Content: "both", LowerCaseNames: j.LowerCaseNames, TargetSchema: j.TargetSchema, ChangeOwner: true, TargetDBType: "postgres"})
	report := m.Run(ctx)
	close(logJob.LogCh)
	<-done
	if e = r.FinishSnapshot(); e != nil {
		return e
	}
	if report.Tables.Failed+report.Data.Failed > 0 {
		return fmt.Errorf("全量快照存在失败表: structure=%d data=%d", report.Tables.Failed, report.Data.Failed)
	}
	j.StartFile, j.StartPosition, j.StartGTID = pos.File, pos.Pos, pos.GTID
	_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "catching_up", "phase": "catching_up", "start_file": pos.File, "start_position": pos.Pos, "start_gtid": pos.GTID, "summary": "全量完成，正在追赶 binlog"})
	return nil
}

func configFromIncremental(j *store.IncrementalMigrationJob, src, dst *store.Connection) cdc.Config {
	req := incrementalRequest{SrcDatabase: j.SrcDatabase, TargetSchema: j.TargetSchema, MigrateMode: j.MigrateMode, TableFilter: j.TableFilter, LowerCaseNames: j.LowerCaseNames, ServerID: j.ServerID, StartGTID: j.StartGTID, StartFile: j.StartFile, StartPosition: j.StartPosition}
	return incrementalConfig(req, j.JobID, src, dst)
}

func runCDC(ctx context.Context, j *store.IncrementalMigrationJob, src, dst *store.Connection) error {
	cfg := configFromIncremental(j, src, dst)
	baseI, baseU, baseD, baseS, baseW := j.InsertCount, j.UpdateCount, j.DeleteCount, j.SkippedCount, j.WarningCount
	var mu sync.Mutex
	h := cdc.Hooks{Status: func(status, phase, summary string) {
		_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": status, "phase": phase, "summary": summary})
	},
		Stats: func(s cdc.Stats) {
			mu.Lock()
			defer mu.Unlock()
			_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"insert_count": baseI + s.Inserts, "update_count": baseU + s.Updates, "delete_count": baseD + s.Deletes, "skipped_count": baseS + s.Skipped, "warning_count": baseW + s.Warnings, "checkpoint_file": s.Position.File, "checkpoint_position": s.Position.Pos, "checkpoint_gtid": s.Position.GTID, "last_event_at": s.LastEventAt})
		},
		DDL: func(sqlText string, p cdc.Position) {
			_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "paused_ddl", "phase": "paused", "summary": "检测到源库 DDL，需人工处理", "blocking_ddl": sqlText, "blocking_file": p.File, "blocking_position": p.Pos, "blocking_gtid": p.GTID})
		}}
	return cdc.NewRunner(cfg, h).Run(ctx)
}

func ownedIncremental(c *gin.Context) (*store.IncrementalMigrationJob, bool) {
	j, e := store.GetIncrementalJob(c.Param("jobID"))
	if e != nil || (!middleware.IsAdmin(c) && j.OwnerID != middleware.GetCurrentUserID(c)) {
		c.JSON(404, gin.H{"error": "增量任务不存在"})
		return nil, false
	}
	return j, true
}
func ListIncremental(c *gin.Context) {
	j, e := store.ListIncrementalJobs(middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	}
	c.JSON(200, j)
}
func GetIncremental(c *gin.Context) {
	j, ok := ownedIncremental(c)
	if ok {
		c.JSON(200, j)
	}
}
func PauseIncremental(c *gin.Context) {
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	if !cdc.Registry.Cancel(j.JobID, "pause") {
		c.JSON(409, gin.H{"error": "任务当前未运行"})
		return
	}
	_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "pausing", "phase": "pausing"})
	c.JSON(200, gin.H{"message": "正在安全暂停"})
}
func StopIncremental(c *gin.Context) {
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	if !cdc.Registry.Cancel(j.JobID, "stop") {
		if src, e := store.GetConnection(j.SrcConnID); e == nil {
			if dst, e2 := store.GetConnection(j.DstConnID); e2 == nil {
				_ = cdc.SyncSequences(c.Request.Context(), configFromIncremental(j, src, dst))
			}
		}
		now := time.Now()
		_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "stopped", "phase": "stopped", "finished_at": &now})
	}
	c.JSON(200, gin.H{"message": "正在停止"})
}

func ResumeIncremental(c *gin.Context) {
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	if cdc.Registry.Running(j.JobID) {
		c.JSON(409, gin.H{"error": "任务已在运行"})
		return
	}
	if j.Status == "paused_ddl" {
		c.JSON(409, gin.H{"error": "请先确认阻塞 DDL 已处理"})
		return
	}
	src, e := store.GetConnection(j.SrcConnID)
	if e != nil {
		c.JSON(400, gin.H{"error": "源连接已删除"})
		return
	}
	dst, e := store.GetConnection(j.DstConnID)
	if e != nil {
		c.JSON(400, gin.H{"error": "目标连接已删除"})
		return
	}
	j.Status = "initializing"
	launchIncremental(j, src, dst)
	c.JSON(200, gin.H{"message": "任务已恢复"})
}

func AckIncrementalDDL(c *gin.Context) {
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	if j.Status != "paused_ddl" {
		c.JSON(409, gin.H{"error": "任务没有待确认 DDL"})
		return
	}
	src, e := store.GetConnection(j.SrcConnID)
	if e != nil {
		c.JSON(400, gin.H{"error": "源连接已删除"})
		return
	}
	dst, e := store.GetConnection(j.DstConnID)
	if e != nil {
		c.JSON(400, gin.H{"error": "目标连接已删除"})
		return
	}
	cfg := configFromIncremental(j, src, dst)
	p := cdc.Position{File: j.BlockingFile, Pos: j.BlockingPos, GTID: j.BlockingGTID}
	if e = cdc.AcknowledgeDDL(c.Request.Context(), cfg, p); e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	}
	_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"blocking_ddl": "", "blocking_file": "", "blocking_position": 0, "blocking_gtid": "", "checkpoint_file": p.File, "checkpoint_position": p.Pos, "checkpoint_gtid": p.GTID, "status": "initializing"})
	j.CheckpointFile, j.CheckpointPos, j.CheckpointGTID = p.File, p.Pos, p.GTID
	j.BlockingDDL = ""
	launchIncremental(j, src, dst)
	c.JSON(200, gin.H{"message": "DDL 已确认，任务已恢复"})
}
