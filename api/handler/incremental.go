package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
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

var incrementalStartMu sync.Mutex

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
	if req.StartMode == "full_then_cdc" {
		if strings.EqualFold(pf.GTIDMode, "ON") {
			req.PositionMode = "gtid"
		} else {
			req.PositionMode = "file"
		}
	} else if req.StartGTID != "" {
		req.PositionMode = "gtid"
	} else {
		req.PositionMode = "file"
	}
	j := &store.IncrementalMigrationJob{OwnerID: middleware.GetCurrentUserID(c), JobID: jobID, SrcConnID: req.SrcConnID, DstConnID: req.DstConnID,
		SrcDatabase: req.SrcDatabase, TargetSchema: req.TargetSchema, StartMode: req.StartMode, PositionMode: req.PositionMode, StartGTID: req.StartGTID,
		StartFile: req.StartFile, StartPosition: req.StartPosition, ServerID: req.ServerID, MigrateMode: req.MigrateMode, TableFilter: req.TableFilter,
		LowerCaseNames: req.LowerCaseNames, Status: "initializing", Phase: "initializing"}
	incrementalStartMu.Lock()
	defer incrementalStartMu.Unlock()
	if exists, e := store.HasOpenIncrementalTarget(req.DstConnID, req.TargetSchema); e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	} else if exists {
		c.JSON(409, gin.H{"error": "该目标连接和 Schema 已有未终止的增量任务；请先恢复或放弃原任务，避免并发删表/写入"})
		return
	}
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
	if j.CutoverFile != "" || j.CutoverGTID != "" {
		cdc.Registry.RequestCutover(j.JobID, cdc.Position{File: j.CutoverFile, Pos: j.CutoverPos, GTID: j.CutoverGTID})
	}
	go func() {
		var runErr error
		if j.StartMode == "full_then_cdc" && !j.BootstrapDone {
			cfg := configFromIncremental(j, src, dst)
			if cp, ok, e := cdc.LoadTargetCheckpoint(ctx, cfg); e != nil {
				runErr = fmt.Errorf("读取目标 checkpoint 失败，已禁止重新全量: %w", e)
			} else if ok {
				j.BootstrapDone = true
				j.CheckpointFile, j.CheckpointPos, j.CheckpointGTID = cp.File, cp.Pos, cp.GTID
				runErr = store.UpdateIncrementalJob(j.JobID, map[string]any{"bootstrap_completed": true, "checkpoint_file": cp.File, "checkpoint_position": cp.Pos, "checkpoint_gtid": cp.GTID, "start_file": cp.File, "start_position": cp.Pos, "start_gtid": cp.GTID})
			} else {
				runErr = runIncrementalBootstrap(ctx, j, src, dst)
			}
		}
		if runErr == nil && ctx.Err() == nil {
			runErr = runCDC(ctx, j, src, dst)
		}
		action := cdc.Registry.Remove(j.JobID)
		if errors.Is(runErr, cdc.ErrCutoverReady) {
			fresh, loadErr := store.GetIncrementalJob(j.JobID)
			if loadErr != nil {
				return
			}
			if fresh.CutoverFile == "" && fresh.CutoverGTID == "" {
				// A concurrent cancel-cutover may win just as the runner reaches
				// the old boundary. Continue from the authoritative checkpoint
				// instead of publishing a stale validation result.
				launchIncremental(fresh, src, dst)
				return
			}
			finishIncrementalCutover(fresh, src, dst)
			return
		}
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
		if action == "abort" {
			now := time.Now()
			_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "aborted", "phase": "aborted", "summary": "任务已放弃", "finished_at": &now})
			return
		}
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "failed", "phase": "failed", "last_error": runErr.Error(), "summary": runErr.Error()})
		}
	}()
}

func finishIncrementalCutover(j *store.IncrementalMigrationJob, src, dst *store.Connection) {
	cfg := configFromIncremental(j, src, dst)
	claimed, claimErr := store.UpdateIncrementalJobIfStatus(j.JobID, []string{"cutting_over"}, map[string]any{"status": "validating", "phase": "validating", "validation_state": "running", "summary": "已到达切换边界，正在校验行数和序列"})
	if claimErr != nil {
		return
	}
	if !claimed {
		fresh, e := store.GetIncrementalJob(j.JobID)
		if e == nil && fresh.Status == "running" && !cdc.Registry.Running(j.JobID) {
			launchIncremental(fresh, src, dst)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	if e := cdc.SyncSequences(ctx, cfg); e != nil {
		_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "cutover_blocked", "phase": "validation_failed", "validation_state": "failed", "last_error": "序列校正失败: " + e.Error()})
		return
	}
	results, match, e := cdc.ValidateCounts(ctx, cfg)
	payload, _ := json.Marshal(results)
	if e != nil {
		_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "cutover_blocked", "phase": "validation_failed", "validation_state": "failed", "validation_json": string(payload), "last_error": "最终校验失败: " + e.Error()})
		return
	}
	if !match {
		_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "cutover_blocked", "phase": "validation_failed", "validation_state": "mismatch", "validation_json": string(payload), "summary": "已追到边界，但存在行数不一致，禁止完成切换"})
		return
	}
	status := "ready_to_cutover"
	summary := "已追到最终边界且行数校验通过，可以完成切换"
	fresh, _ := store.GetIncrementalJob(j.JobID)
	if fresh != nil && fresh.SkippedCount > 0 {
		status = "ready_with_warnings"
		summary = "已追到边界且行数一致，但存在被跳过的无主键 UPDATE/DELETE，请人工确认"
	}
	_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": status, "phase": "ready", "validation_state": "passed", "validation_json": string(payload), "caught_up": true, "lag_seconds": 0, "last_error": "", "summary": summary})
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
	if j.PositionMode != "gtid" {
		pos.GTID = ""
	}
	snapshotActive := true
	defer func() {
		if snapshotActive {
			_ = r.FinishSnapshot()
		}
	}()
	allTables, e := r.ListTables(ctx)
	if e != nil {
		return fmt.Errorf("读取全量快照表列表失败: %w", e)
	}
	expectedTables := datamigrate.FilterTables(allTables, j.MigrateMode, j.TableFilter)
	w, e := target.NewPostgres(buildDSN(dst), j.TargetSchema, target.ConnPoolConfig{})
	if e != nil {
		return e
	}
	defer w.Close()
	logJob := &datamigrate.Job{LogCh: make(chan string, 512)}
	done := make(chan struct{})
	go func() {
		lastPersist := time.Time{}
		for line := range logJob.LogCh {
			if time.Since(lastPersist) >= 2*time.Second {
				_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"summary": "全量快照：" + line})
				lastPersist = time.Now()
			}
		}
		close(done)
	}()
	m := datamigrate.NewMigrator(r, w, logJob, datamigrate.Config{PageSize: 20000, MaxParallel: 1, IntraTableParallel: 1, Mode: j.MigrateMode, Filter: j.TableFilter, Content: "both", LowerCaseNames: j.LowerCaseNames, TargetSchema: j.TargetSchema, ChangeOwner: true, TargetDBType: "postgres"})
	report := m.Run(ctx)
	close(logJob.LogCh)
	<-done
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = r.FinishSnapshot(); e != nil {
		return e
	}
	snapshotActive = false
	if report.Tables.Total != len(expectedTables) || report.Data.Total != len(expectedTables) || report.Tables.Failed+report.Data.Failed > 0 || report.Tables.Success != report.Tables.Total || report.Data.Success != report.Data.Total {
		return fmt.Errorf("全量快照未完整完成: structure=%d/%d data=%d/%d", report.Tables.Success, report.Tables.Total, report.Data.Success, report.Data.Total)
	}
	for _, row := range report.RowCounts {
		if !row.Match {
			return fmt.Errorf("全量快照行数校验失败: table=%s source=%d target=%d", row.Table, row.Src, row.Dst)
		}
	}
	if len(report.RowCounts) != len(expectedTables) {
		return fmt.Errorf("全量快照行数校验未完整执行: checked=%d expected=%d", len(report.RowCounts), len(expectedTables))
	}
	objectFailures := report.PrimaryKeys.Failed + report.Indexes.Failed + report.Constraints.Failed + report.Sequences.Failed
	if objectFailures > 0 {
		return fmt.Errorf("全量快照对象创建失败: primary_keys=%d indexes=%d constraints=%d sequences=%d", report.PrimaryKeys.Failed, report.Indexes.Failed, report.Constraints.Failed, report.Sequences.Failed)
	}
	j.StartFile, j.StartPosition, j.StartGTID = pos.File, pos.Pos, pos.GTID
	cfg := configFromIncremental(j, src, dst)
	checkpoint := cdc.Position{File: pos.File, Pos: pos.Pos, GTID: pos.GTID}
	if e = cdc.SaveTargetCheckpoint(ctx, cfg, checkpoint); e != nil {
		return fmt.Errorf("保存全量完成 checkpoint 失败: %w", e)
	}
	j.BootstrapDone = true
	j.CheckpointFile, j.CheckpointPos, j.CheckpointGTID = pos.File, pos.Pos, pos.GTID
	return store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "catching_up", "phase": "catching_up", "bootstrap_completed": true, "start_file": pos.File, "start_position": pos.Pos, "start_gtid": pos.GTID, "checkpoint_file": pos.File, "checkpoint_position": pos.Pos, "checkpoint_gtid": pos.GTID, "summary": "全量完成，正在追赶 binlog"})
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
			fields := map[string]any{"insert_count": baseI + s.Inserts, "update_count": baseU + s.Updates, "delete_count": baseD + s.Deletes, "skipped_count": baseS + s.Skipped, "warning_count": baseW + s.Warnings, "checkpoint_file": s.Position.File, "checkpoint_position": s.Position.Pos, "checkpoint_gtid": s.Position.GTID, "source_head_file": s.SourceHead.File, "source_head_position": s.SourceHead.Pos, "source_head_gtid": s.SourceHead.GTID, "caught_up": s.CaughtUp, "lag_seconds": s.LagSeconds}
			if !s.LastEventAt.IsZero() {
				fields["last_event_at"] = s.LastEventAt
			}
			_ = store.UpdateIncrementalJob(j.JobID, fields)
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
	if j.StartMode == "full_then_cdc" && !j.BootstrapDone {
		c.JSON(409, gin.H{"error": "全量快照阶段不支持可恢复暂停；若必须中断，请放弃任务后显式新建"})
		return
	}
	if !cdc.Registry.Cancel(j.JobID, "pause") {
		c.JSON(409, gin.H{"error": "任务当前未运行"})
		return
	}
	_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "pausing", "phase": "pausing"})
	c.JSON(200, gin.H{"message": "正在安全暂停"})
}

// PrepareIncrementalCutover captures the final source watermark. Operators
// must stop all writes on the MySQL instance before calling this endpoint.
func PrepareIncrementalCutover(c *gin.Context) {
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	if j.Status != "running" && j.Status != "catching_up" {
		c.JSON(409, gin.H{"error": "任务当前状态不能准备切换"})
		return
	}
	if !cdc.Registry.Running(j.JobID) {
		c.JSON(409, gin.H{"error": "CDC 任务当前未运行"})
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
	db, e := cdc.OpenSource(cfg.SourceDSN)
	if e != nil {
		c.JSON(502, gin.H{"error": e.Error()})
		return
	}
	defer db.Close()
	boundary, e := cdc.CurrentPosition(c.Request.Context(), db)
	if e != nil {
		c.JSON(502, gin.H{"error": "读取最终位点失败: " + e.Error()})
		return
	}
	updated, e := store.UpdateIncrementalJobIfStatus(j.JobID, []string{"running", "catching_up"}, map[string]any{"status": "cutting_over", "phase": "cutting_over", "cutover_file": boundary.File, "cutover_position": boundary.Pos, "cutover_gtid": boundary.GTID, "validation_state": "pending", "summary": "已锁定最终位点，源库必须继续保持停写"})
	if e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	}
	if !updated {
		c.JSON(409, gin.H{"error": "任务状态已变化，请刷新后重试"})
		return
	}
	if !cdc.Registry.RequestCutover(j.JobID, boundary) {
		rolledBack, _ := store.UpdateIncrementalJobIfStatus(j.JobID, []string{"cutting_over"}, map[string]any{"status": j.Status, "phase": j.Phase, "cutover_file": "", "cutover_position": 0, "cutover_gtid": "", "validation_state": "", "summary": "准备切换已取消，请刷新状态"})
		if !rolledBack {
			_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"cutover_file": "", "cutover_position": 0, "cutover_gtid": "", "validation_state": ""})
		}
		c.JSON(409, gin.H{"error": "任务已正在暂停或停止，请刷新状态"})
		return
	}
	j.CutoverFile, j.CutoverPos, j.CutoverGTID = boundary.File, boundary.Pos, boundary.GTID
	c.JSON(200, gin.H{"message": "已锁定切换边界，正在追赶", "boundary": boundary})
}

type completeCutoverRequest struct {
	AcknowledgeWarnings bool `json:"acknowledge_warnings"`
}

func StopIncremental(c *gin.Context) {
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	if j.Status != "ready_to_cutover" && j.Status != "ready_with_warnings" {
		c.JSON(409, gin.H{"error": "必须先停止源库写入并执行“准备切换”，待追平和校验通过后才能完成切换"})
		return
	}
	var req completeCutoverRequest
	_ = c.ShouldBindJSON(&req)
	if j.Status == "ready_with_warnings" && !req.AcknowledgeWarnings {
		c.JSON(409, gin.H{"error": "存在无主键变更被跳过，必须明确确认风险后才能完成切换"})
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
	boundary := cdc.Position{File: j.CutoverFile, Pos: j.CutoverPos, GTID: j.CutoverGTID}
	checkpoint, exists, e := cdc.LoadTargetCheckpoint(c.Request.Context(), cfg)
	if e != nil {
		c.JSON(502, gin.H{"error": "读取目标 checkpoint 失败: " + e.Error()})
		return
	}
	if !exists || !cdc.PositionReached(checkpoint, boundary) {
		c.JSON(409, gin.H{"error": "目标 checkpoint 尚未到达切换边界，禁止完成切换", "boundary": boundary, "checkpoint": checkpoint})
		return
	}
	db, e := cdc.OpenSource(cfg.SourceDSN)
	if e != nil {
		c.JSON(502, gin.H{"error": e.Error()})
		return
	}
	defer db.Close()
	head, e := cdc.CurrentPosition(c.Request.Context(), db)
	if e != nil {
		c.JSON(502, gin.H{"error": e.Error()})
		return
	}
	if !cdc.PositionEquivalent(head, boundary) {
		c.JSON(409, gin.H{"error": "锁定边界后源库又产生了新事务，禁止完成切换；请取消切换、恢复同步后重新停写", "boundary": boundary, "current": head})
		return
	}
	now := time.Now()
	updated, e := store.UpdateIncrementalJobIfStatus(j.JobID, []string{j.Status}, map[string]any{"status": "stopped", "phase": "completed", "last_error": "", "summary": "切换边界复核通过，CDC 已完成", "finished_at": &now})
	if e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	}
	if !updated {
		c.JSON(409, gin.H{"error": "任务状态已变化，请刷新后重试"})
		return
	}
	c.JSON(200, gin.H{"message": "增量迁移已安全完成"})
}

func CancelIncrementalCutover(c *gin.Context) {
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	if !slices.Contains([]string{"cutting_over", "ready_to_cutover", "ready_with_warnings", "cutover_blocked"}, j.Status) {
		c.JSON(409, gin.H{"error": "任务当前不在切换流程"})
		return
	}
	running := cdc.Registry.Running(j.JobID)
	var src, dst *store.Connection
	if !running {
		var e error
		src, e = store.GetConnection(j.SrcConnID)
		if e != nil {
			c.JSON(400, gin.H{"error": "源连接已删除"})
			return
		}
		dst, e = store.GetConnection(j.DstConnID)
		if e != nil {
			c.JSON(400, gin.H{"error": "目标连接已删除"})
			return
		}
	}
	fields := map[string]any{"cutover_file": "", "cutover_position": 0, "cutover_gtid": "", "validation_state": "", "validation_json": "", "last_error": "", "status": "running", "phase": "running", "summary": "已取消切换，继续持续同步"}
	updated, e := store.UpdateIncrementalJobIfStatus(j.JobID, []string{"cutting_over", "ready_to_cutover", "ready_with_warnings", "cutover_blocked"}, fields)
	if e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	}
	if !updated {
		c.JSON(409, gin.H{"error": "切换状态已变化，请刷新后重试"})
		return
	}
	j.CutoverFile, j.CutoverPos, j.CutoverGTID = "", 0, ""
	j.Status = "running"
	if running {
		cdc.Registry.ClearCutover(j.JobID)
	} else {
		fresh, loadErr := store.GetIncrementalJob(j.JobID)
		if loadErr == nil && fresh.Status == "running" {
			launchIncremental(fresh, src, dst)
		}
	}
	c.JSON(200, gin.H{"message": "已取消切换并恢复同步"})
}

func AbortIncremental(c *gin.Context) {
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	if j.Status == "stopped" || j.Status == "aborted" {
		c.JSON(409, gin.H{"error": "任务已终止"})
		return
	}
	if j.Status == "validating" {
		c.JSON(409, gin.H{"error": "正在执行最终校验，请等待校验完成后再操作"})
		return
	}
	if cdc.Registry.Running(j.JobID) {
		if !cdc.Registry.Cancel(j.JobID, "abort") {
			c.JSON(409, gin.H{"error": "任务已在执行其他生命周期操作"})
			return
		}
	} else {
		now := time.Now()
		updated, e := store.UpdateIncrementalJobIfStatus(j.JobID, []string{j.Status}, map[string]any{"status": "aborted", "phase": "aborted", "summary": "任务已放弃", "finished_at": &now})
		if e != nil {
			c.JSON(500, gin.H{"error": e.Error()})
			return
		}
		if !updated {
			c.JSON(409, gin.H{"error": "任务状态已变化，请刷新后重试"})
			return
		}
	}
	c.JSON(200, gin.H{"message": "任务已放弃"})
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
	if !slices.Contains([]string{"paused_manual", "paused_restart", "failed"}, j.Status) {
		c.JSON(409, gin.H{"error": "任务当前状态不能恢复"})
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
	if j.StartMode == "full_then_cdc" && !j.BootstrapDone {
		cfg := configFromIncremental(j, src, dst)
		checkpoint, exists, loadErr := cdc.LoadTargetCheckpoint(c.Request.Context(), cfg)
		if loadErr != nil {
			c.JSON(500, gin.H{"error": "读取目标 checkpoint 失败，已禁止自动重跑全量: " + loadErr.Error()})
			return
		}
		if !exists {
			c.JSON(409, gin.H{"error": "全量快照未完成，不能安全断点恢复，也不会自动删表重跑；请放弃此任务后显式新建"})
			return
		}
		j.BootstrapDone = true
		j.StartFile, j.StartPosition, j.StartGTID = checkpoint.File, checkpoint.Pos, checkpoint.GTID
		j.CheckpointFile, j.CheckpointPos, j.CheckpointGTID = checkpoint.File, checkpoint.Pos, checkpoint.GTID
		if e = store.UpdateIncrementalJob(j.JobID, map[string]any{"bootstrap_completed": true, "start_file": checkpoint.File, "start_position": checkpoint.Pos, "start_gtid": checkpoint.GTID, "checkpoint_file": checkpoint.File, "checkpoint_position": checkpoint.Pos, "checkpoint_gtid": checkpoint.GTID}); e != nil {
			c.JSON(500, gin.H{"error": e.Error()})
			return
		}
	}
	j.Status = "initializing"
	updated, e := store.UpdateIncrementalJobIfStatus(j.JobID, []string{"paused_manual", "paused_restart", "failed"}, map[string]any{"status": "initializing", "phase": "initializing", "last_error": "", "summary": "正在从目标 checkpoint 恢复"})
	if e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	}
	if !updated {
		c.JSON(409, gin.H{"error": "任务状态已变化，请刷新后重试"})
		return
	}
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
	updated, e := store.UpdateIncrementalJobIfStatus(j.JobID, []string{"paused_ddl"}, map[string]any{"blocking_ddl": "", "blocking_file": "", "blocking_position": 0, "blocking_gtid": "", "checkpoint_file": p.File, "checkpoint_position": p.Pos, "checkpoint_gtid": p.GTID, "status": "initializing"})
	if e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	}
	if !updated {
		c.JSON(409, gin.H{"error": "任务状态已变化，请刷新后重试"})
		return
	}
	j.CheckpointFile, j.CheckpointPos, j.CheckpointGTID = p.File, p.Pos, p.GTID
	j.BlockingDDL = ""
	launchIncremental(j, src, dst)
	c.JSON(200, gin.H{"message": "DDL 已确认，任务已恢复"})
}
