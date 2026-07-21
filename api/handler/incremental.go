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
	SrcConnID           uint   `json:"src_conn_id" binding:"required"`
	DstConnID           uint   `json:"dst_conn_id" binding:"required"`
	SrcDatabase         string `json:"src_database" binding:"required"`
	TargetSchema        string `json:"target_schema" binding:"required"`
	StartMode           string `json:"start_mode" binding:"required,oneof=full_then_cdc incremental_only"`
	PositionMode        string `json:"position_mode" binding:"omitempty,oneof=auto gtid file"`
	StartGTID           string `json:"start_gtid"`
	StartFile           string `json:"start_file"`
	StartPosition       uint32 `json:"start_position"`
	ServerID            uint32 `json:"server_id"`
	MigrateMode         string `json:"migrate_mode" binding:"required,oneof=all include exclude"`
	TableFilter         string `json:"table_filter"`
	LowerCaseNames      bool   `json:"lower_case_names"`
	BootstrapPolicy     string `json:"bootstrap_failure_policy" binding:"omitempty,oneof=review_and_exclude fail_all"`
	KeylessChangePolicy string `json:"keyless_change_policy" binding:"omitempty,oneof=full_row_match"`
}

var incrementalStartMu sync.Mutex

var incrementalTargetTypes = map[string]bool{
	"postgres": true,
	"gaussdb":  true,
	"highgo":   true,
	"kingbase": true,
	"seabox":   true,
}

func isSupportedIncrementalTarget(dbType string) bool {
	return incrementalTargetTypes[dbType]
}

func incrementalConfig(req incrementalRequest, jobID string, src, dst *store.Connection) cdc.Config {
	srcCopy := *src
	srcCopy.Database = req.SrcDatabase
	return cdc.Config{JobID: jobID, SourceDSN: buildDSN(&srcCopy), SourceHost: src.Host, SourcePort: uint16(src.Port), SourceUser: src.Username,
		SourcePassword: src.Password, SourceDatabase: req.SrcDatabase, TargetDSN: buildDSN(dst), TargetDBType: dst.DBType, TargetSchema: req.TargetSchema,
		Mode: req.MigrateMode, Filter: req.TableFilter, LowerCaseNames: req.LowerCaseNames, ServerID: req.ServerID,
		Start: cdc.Position{File: req.StartFile, Pos: req.StartPosition, GTID: req.StartGTID}, KeylessChangePolicy: "full_row_match"}
}

func validateIncrementalConnections(c *gin.Context, req incrementalRequest) (*store.Connection, *store.Connection, bool) {
	src, e := store.GetConnectionOwned(req.SrcConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if e != nil || src.DBType != "mysql" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "增量迁移源库必须是可访问的 MySQL 连接"})
		return nil, nil, false
	}
	dst, e := store.GetConnectionOwned(req.DstConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if e != nil || !isSupportedIncrementalTarget(dst.DBType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "增量迁移目标库必须是可访问的 PostgreSQL、GaussDB、HighGo、Kingbase 或 SeaBox 连接"})
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
	if req.BootstrapPolicy == "" {
		req.BootstrapPolicy = "fail_all"
	}
	if req.KeylessChangePolicy == "" {
		req.KeylessChangePolicy = "full_row_match"
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
	locatorStrategies := cdc.LocatorStrategiesFromTables(pf.Tables)
	locatorJSON, _ := json.Marshal(locatorStrategies)
	primaryCount, uniqueCount, fullRowCount := locatorStrategyCounts(locatorStrategies)
	j := &store.IncrementalMigrationJob{OwnerID: middleware.GetCurrentUserID(c), JobID: jobID, SrcConnID: req.SrcConnID, DstConnID: req.DstConnID,
		SrcDBType: src.DBType, DstDBType: dst.DBType, SrcDatabase: req.SrcDatabase, TargetSchema: req.TargetSchema,
		SrcConnName: src.Name, SrcConnHost: src.Host, SrcConnPort: src.Port, SrcConnDatabase: req.SrcDatabase, SrcConnUsername: src.Username,
		DstConnName: dst.Name, DstConnHost: dst.Host, DstConnPort: dst.Port, DstConnDatabase: dst.Database, DstConnUsername: dst.Username,
		StartMode: req.StartMode, PositionMode: req.PositionMode, StartGTID: req.StartGTID,
		StartFile: req.StartFile, StartPosition: req.StartPosition, ServerID: req.ServerID, MigrateMode: req.MigrateMode, TableFilter: req.TableFilter,
		LowerCaseNames: req.LowerCaseNames, BootstrapPolicy: req.BootstrapPolicy, KeylessChangePolicy: req.KeylessChangePolicy,
		LocatorStrategyVersion: cdc.LocatorStrategyVersion, LocatorStrategiesJSON: string(locatorJSON), PrimaryLocatorCount: primaryCount,
		UniqueLocatorCount: uniqueCount, FullRowLocatorCount: fullRowCount, BootstrapState: "pending", Status: "initializing", Phase: "initializing"}
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
			if record, ok, e := cdc.LoadTargetBootstrapRecord(ctx, cfg); e != nil {
				runErr = fmt.Errorf("读取目标 checkpoint 失败，已禁止重新全量: %w", e)
			} else if ok && record.State == "completed" {
				runErr = applyCompletedBootstrapRecord(j, record)
			} else if ok && record.State == "review_pending" {
				runErr = applyReviewBootstrapRecord(j, record)
				if runErr == nil {
					runErr = cdc.ErrBootstrapReview
				}
			} else if ok {
				runErr = fmt.Errorf("全量 checkpoint 状态为 %s，不能安全恢复或自动重跑", record.State)
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
		if errors.Is(runErr, cdc.ErrBootstrapReview) {
			return
		}
		if errors.Is(runErr, cdc.ErrRowConflict) {
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
			if !j.BootstrapDone {
				_ = cdc.AbortTargetBootstrap(context.Background(), configFromIncremental(j, src, dst))
			}
			now := time.Now()
			_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "aborted", "phase": "aborted", "bootstrap_state": "aborted", "summary": "任务已放弃", "finished_at": &now})
			if j.StartMode == "full_then_cdc" && !j.BootstrapDone {
				appendIncrementalLifecycleLog(j.JobID, "snapshot_init", "warn", "用户已放弃未完成的全量任务")
			}
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
	if fresh != nil && (fresh.SkippedCount > 0 || fresh.ExcludedCount > 0) {
		status = "ready_with_warnings"
		summary = "已追到边界且行数一致，但存在被排除表或同步警告，请人工确认"
	}
	_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": status, "phase": "ready", "validation_state": "passed", "validation_json": string(payload), "caught_up": true, "lag_seconds": 0, "last_error": "", "summary": summary})
}

func runIncrementalBootstrap(ctx context.Context, j *store.IncrementalMigrationJob, src, dst *store.Connection) (resultErr error) {
	logPhase := "snapshot_init"
	appendIncrementalLifecycleLog(j.JobID, "snapshot_init", "info", "开始建立 MySQL 一致性全量快照")
	defer func() {
		if resultErr == nil || errors.Is(resultErr, cdc.ErrBootstrapReview) {
			return
		}
		level := "error"
		if errors.Is(resultErr, context.Canceled) {
			level = "warn"
		}
		appendIncrementalLifecycleLog(j.JobID, logPhase, level, "全量快照结束: "+resultErr.Error())
	}()
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
	snapshotPosition := cdc.Position{File: pos.File, Pos: pos.Pos, GTID: pos.GTID}
	appendIncrementalLifecycleLog(j.JobID, "snapshot_init", "info", "已获取一致快照起始位点: "+formatIncrementalPosition(snapshotPosition))
	cfg := configFromIncremental(j, src, dst)
	if e = cdc.SaveTargetBootstrapRecord(ctx, cfg, cdc.BootstrapRecord{State: "snapshot_in_progress", Position: snapshotPosition, LocatorStrategyVersion: cdc.LocatorStrategyVersion}); e != nil {
		return fmt.Errorf("保存全量快照待完成位点失败: %w", e)
	}
	j.BootstrapState = "snapshot_in_progress"
	j.PendingFile, j.PendingPos, j.PendingGTID = pos.File, pos.Pos, pos.GTID
	if e = store.UpdateIncrementalJob(j.JobID, map[string]any{"bootstrap_state": "snapshot_in_progress", "pending_file": pos.File, "pending_position": pos.Pos, "pending_gtid": pos.GTID}); e != nil {
		return fmt.Errorf("保存全量快照待完成状态失败: %w", e)
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
	w, e := buildDstWriter(dst, j.TargetSchema, target.ConnPoolConfig{})
	if e != nil {
		return e
	}
	defer w.Close()
	logJob := &datamigrate.Job{LogCh: make(chan string, 4096)}
	journalDone := startIncrementalLogJournal(j.JobID, logJob)
	m := datamigrate.NewMigrator(r, w, logJob, datamigrate.Config{PageSize: 20000, MaxParallel: 1, IntraTableParallel: 1, Mode: j.MigrateMode, Filter: j.TableFilter, Content: "both", LowerCaseNames: j.LowerCaseNames, TargetSchema: j.TargetSchema, ChangeOwner: true, TargetDBType: dst.DBType})
	report := m.Run(ctx)
	close(logJob.LogCh)
	<-journalDone
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = r.FinishSnapshot(); e != nil {
		return e
	}
	snapshotActive = false
	logPhase = "snapshot_validation"
	appendIncrementalLifecycleLog(j.JobID, "snapshot_validation", "info", "一致性全量快照读取事务已结束，正在分类迁移结果")
	// Persist the structured report before strict-mode evaluation. Otherwise a
	// fail_all task would terminate with only transient log lines and the exact
	// failed DDL could no longer be exported.
	preliminaryReview := cdc.BuildBootstrapReview(snapshotPosition, expectedTables, report, j.LowerCaseNames, nil)
	preliminaryReview.State = "snapshot_in_progress"
	preliminaryReview.LocatorStrategyVersion = cdc.LocatorStrategyVersion
	preliminaryJSON, _ := json.Marshal(preliminaryReview)
	failedCount, failedDDLCount := bootstrapFailureCounts(preliminaryReview.FailedObjects)
	if e = store.UpdateIncrementalJob(j.JobID, map[string]any{
		"bootstrap_report_json": string(preliminaryJSON), "failed_object_count": failedCount, "failed_ddl_count": failedDDLCount,
	}); e != nil {
		return fmt.Errorf("保存全量失败报告摘要失败: %w", e)
	}
	if e = cdc.SaveTargetBootstrapRecord(ctx, cfg, preliminaryReview.BootstrapRecord); e != nil {
		return fmt.Errorf("保存全量失败报告失败: %w", e)
	}
	if j.BootstrapPolicy != "review_and_exclude" {
		if strictErr := strictBootstrapFailure(report, expectedTables); strictErr != nil {
			_ = cdc.AbortTargetBootstrap(context.Background(), cfg)
			preliminaryReview.State = "aborted"
			preliminaryJSON, _ = json.Marshal(preliminaryReview)
			_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"bootstrap_state": "aborted", "bootstrap_report_json": string(preliminaryJSON)})
			return strictErr
		}
	}

	sourceDB, e := cdc.OpenSource(cfg.SourceDSN)
	if e != nil {
		return e
	}
	tableInfos, e := cdc.LoadExactTables(ctx, sourceDB, cfg.SourceDatabase, expectedTables)
	sourceDB.Close()
	if e != nil {
		return fmt.Errorf("读取 CDC 表元数据失败: %w", e)
	}
	compatibility, e := cdc.ValidateTargetTableCompatibility(ctx, cfg, tableInfos)
	if e != nil {
		return fmt.Errorf("验证目标表 CDC 兼容性失败: %w", e)
	}
	logPhase = "bootstrap_review"
	review := cdc.BuildBootstrapReview(snapshotPosition, expectedTables, report, j.LowerCaseNames, compatibility)
	resolvedTables, e := cdc.ResolveLocatorStrategies(ctx, cfg, tableInfos)
	if e != nil {
		return fmt.Errorf("解析 CDC 行定位策略失败: %w", e)
	}
	effectiveSet := make(map[string]bool, len(review.EffectiveTables))
	for _, table := range review.EffectiveTables {
		effectiveSet[table] = true
	}
	allStrategies := cdc.LocatorStrategiesFromTables(resolvedTables)
	for _, strategy := range allStrategies {
		if effectiveSet[strategy.Table] {
			review.LocatorStrategies = append(review.LocatorStrategies, strategy)
		}
	}
	review.LocatorStrategyVersion = cdc.LocatorStrategyVersion
	failedCount, failedDDLCount = bootstrapFailureCounts(review.FailedObjects)
	if len(review.ExcludedTables) > 0 {
		if foreignKeys, fkErr := r.GetForeignKeys(ctx); fkErr == nil {
			excluded := make(map[string]bool, len(review.ExcludedTables))
			for _, issue := range review.ExcludedTables {
				excluded[issue.Table] = true
			}
			for _, fk := range foreignKeys {
				if !excluded[fk.TableName] && excluded[fk.RefTable] {
					review.Warnings = append(review.Warnings, fmt.Sprintf("外键 %s.%s 引用了被排除表 %s，确认排除时该外键会被移除", fk.TableName, fk.ConstraintName, fk.RefTable))
				}
			}
		}
	}
	appendIncrementalLifecycleLog(j.JobID, "bootstrap_review", "info", fmt.Sprintf("全量结果分类完成：%d 张表可继续，%d 张表需排除", len(review.EffectiveTables), len(review.ExcludedTables)))
	for _, issue := range review.ExcludedTables {
		appendIncrementalLifecycleLog(j.JobID, "bootstrap_review", "warn", fmt.Sprintf("排除候选表 %s，阶段=%s，错误=%s", issue.Table, issue.Stage, issue.Error))
	}
	for _, warning := range review.Warnings {
		appendIncrementalLifecycleLog(j.JobID, "bootstrap_review", "warn", warning)
	}
	if j.BootstrapPolicy != "review_and_exclude" && len(review.ExcludedTables) > 0 {
		review.State = "aborted"
		payload, _ := json.Marshal(review)
		_ = cdc.SaveTargetBootstrapRecord(context.Background(), cfg, review.BootstrapRecord)
		_ = cdc.AbortTargetBootstrap(context.Background(), cfg)
		_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"bootstrap_state": "aborted", "bootstrap_report_json": string(payload), "failed_object_count": failedCount, "failed_ddl_count": failedDDLCount})
		return fmt.Errorf("全量完成后 CDC 兼容性校验失败: excluded=%d", len(review.ExcludedTables))
	}
	if len(review.EffectiveTables) == 0 {
		review.State = "aborted"
		payload, _ := json.Marshal(review)
		_ = cdc.SaveTargetBootstrapRecord(context.Background(), cfg, review.BootstrapRecord)
		_ = cdc.AbortTargetBootstrap(context.Background(), cfg)
		_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"bootstrap_state": "aborted", "bootstrap_report_json": string(payload), "excluded_table_count": len(review.ExcludedTables), "failed_object_count": failedCount, "failed_ddl_count": failedDDLCount})
		return fmt.Errorf("全量快照没有可安全进入 CDC 的成功表")
	}

	effectiveJSON, _ := json.Marshal(review.EffectiveTables)
	excludedJSON, _ := json.Marshal(review.ExcludedTables)
	reviewJSON, _ := json.Marshal(review)
	if len(review.ExcludedTables) > 0 {
		if e = cdc.SaveTargetBootstrapRecord(ctx, cfg, review.BootstrapRecord); e != nil {
			return fmt.Errorf("保存全量审阅 checkpoint 失败: %w", e)
		}
		j.BootstrapState = "review_pending"
		j.EffectiveJSON, j.ExcludedJSON = string(effectiveJSON), string(excludedJSON)
		j.EffectiveCount, j.ExcludedCount, j.ManifestHash = len(review.EffectiveTables), len(review.ExcludedTables), review.ManifestHash
		locatorJSON, _ := json.Marshal(review.LocatorStrategies)
		primaryCount, uniqueCount, fullRowCount := locatorStrategyCounts(review.LocatorStrategies)
		if e = store.UpdateIncrementalJob(j.JobID, map[string]any{
			"status": "paused_bootstrap_review", "phase": "bootstrap_review", "bootstrap_state": "review_pending",
			"effective_tables_json": string(effectiveJSON), "excluded_tables_json": string(excludedJSON), "bootstrap_report_json": string(reviewJSON),
			"effective_table_count": len(review.EffectiveTables), "excluded_table_count": len(review.ExcludedTables), "bootstrap_manifest_hash": review.ManifestHash,
			"failed_object_count": failedCount, "failed_ddl_count": failedDDLCount,
			"locator_strategy_version": review.LocatorStrategyVersion, "locator_strategies_json": string(locatorJSON),
			"primary_locator_count": primaryCount, "unique_locator_count": uniqueCount, "full_row_locator_count": fullRowCount,
			"summary": fmt.Sprintf("全量完成：%d 张表可继续，%d 张表待确认排除", len(review.EffectiveTables), len(review.ExcludedTables)),
		}); e != nil {
			return e
		}
		appendIncrementalLifecycleLog(j.JobID, "bootstrap_review", "warn", "全量存在失败表，等待人工确认排除范围后再启动 CDC")
		return cdc.ErrBootstrapReview
	}

	review.State = "completed"
	if e = cdc.FinalizeTargetBootstrap(ctx, cfg, review.BootstrapRecord); e != nil {
		return fmt.Errorf("完成全量 checkpoint 失败: %w", e)
	}
	if e = applyCompletedBootstrapRecord(j, review.BootstrapRecord); e != nil {
		return e
	}
	_ = store.UpdateIncrementalJob(j.JobID, map[string]any{"bootstrap_report_json": string(reviewJSON), "failed_object_count": failedCount, "failed_ddl_count": failedDDLCount})
	appendIncrementalLifecycleLog(j.JobID, "catching_up", "done", "全量快照完成，正在从原始快照位点追赶 binlog")
	return store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "catching_up", "phase": "catching_up", "summary": "全量完成，正在追赶 binlog"})
}

func bootstrapFailureCounts(items []cdc.BootstrapFailedObject) (total, withDDL int) {
	for _, item := range items {
		total++
		if strings.TrimSpace(item.DDL) != "" {
			withDDL++
		}
	}
	return total, withDDL
}

func locatorStrategyCounts(strategies []cdc.LocatorStrategy) (primary, unique, fullRow int) {
	for _, strategy := range strategies {
		switch strategy.Strategy {
		case cdc.LocatorPrimaryKey:
			primary++
		case cdc.LocatorUniqueKey:
			unique++
		case cdc.LocatorFullRow:
			fullRow++
		}
	}
	return
}

func formatIncrementalPosition(position cdc.Position) string {
	filePosition := position.File
	if filePosition != "" {
		filePosition = fmt.Sprintf("%s:%d", position.File, position.Pos)
	}
	if position.GTID == "" {
		return filePosition
	}
	if filePosition == "" {
		return "GTID " + position.GTID
	}
	return filePosition + " · GTID " + position.GTID
}

func strictBootstrapFailure(report datamigrate.MigrationReport, expectedTables []string) error {
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
	if report.PrimaryKeys.Failed+report.Indexes.Failed+report.Constraints.Failed+report.Sequences.Failed > 0 {
		return fmt.Errorf("全量快照对象创建失败: primary_keys=%d indexes=%d constraints=%d sequences=%d", report.PrimaryKeys.Failed, report.Indexes.Failed, report.Constraints.Failed, report.Sequences.Failed)
	}
	return nil
}

func configFromIncremental(j *store.IncrementalMigrationJob, src, dst *store.Connection) cdc.Config {
	req := incrementalRequest{SrcDatabase: j.SrcDatabase, TargetSchema: j.TargetSchema, MigrateMode: j.MigrateMode, TableFilter: j.TableFilter, LowerCaseNames: j.LowerCaseNames, ServerID: j.ServerID, StartGTID: j.StartGTID, StartFile: j.StartFile, StartPosition: j.StartPosition}
	cfg := incrementalConfig(req, j.JobID, src, dst)
	cfg.KeylessChangePolicy = j.KeylessChangePolicy
	cfg.LocatorStrategyVersion = j.LocatorStrategyVersion
	_ = json.Unmarshal([]byte(j.LocatorStrategiesJSON), &cfg.LocatorStrategies)
	if j.BootstrapDone && j.EffectiveJSON != "" {
		var tables []string
		if json.Unmarshal([]byte(j.EffectiveJSON), &tables) == nil && tables != nil {
			cfg.TableNames = tables
		} else {
			// A completed new-format task must never fall back to the original
			// wildcard filter, because that could re-include excluded tables.
			cfg.TableNames = []string{}
		}
	}
	return cfg
}

func applyCompletedBootstrapRecord(j *store.IncrementalMigrationJob, record cdc.BootstrapRecord) error {
	if record.LocatorStrategyVersion != cdc.LocatorStrategyVersion {
		return fmt.Errorf("CDC 定位策略版本不兼容，旧任务不能恢复，请重新执行全量快照")
	}
	effectiveJSON, _ := json.Marshal(record.EffectiveTables)
	excludedJSON, _ := json.Marshal(record.ExcludedTables)
	legacyScope := record.ManifestHash == "" && len(record.EffectiveTables) == 0 && len(record.ExcludedTables) == 0
	j.BootstrapDone = true
	j.BootstrapState = "completed"
	j.StartFile, j.StartPosition, j.StartGTID = record.Position.File, record.Position.Pos, record.Position.GTID
	j.CheckpointFile, j.CheckpointPos, j.CheckpointGTID = record.Position.File, record.Position.Pos, record.Position.GTID
	fields := map[string]any{
		"bootstrap_completed": true, "bootstrap_state": "completed",
		"start_file": record.Position.File, "start_position": record.Position.Pos, "start_gtid": record.Position.GTID,
		"checkpoint_file": record.Position.File, "checkpoint_position": record.Position.Pos, "checkpoint_gtid": record.Position.GTID,
	}
	locatorJSON, _ := json.Marshal(record.LocatorStrategies)
	primaryCount, uniqueCount, fullRowCount := locatorStrategyCounts(record.LocatorStrategies)
	j.LocatorStrategyVersion, j.LocatorStrategiesJSON = record.LocatorStrategyVersion, string(locatorJSON)
	j.PrimaryLocatorCount, j.UniqueLocatorCount, j.FullRowLocatorCount = primaryCount, uniqueCount, fullRowCount
	fields["locator_strategy_version"], fields["locator_strategies_json"] = record.LocatorStrategyVersion, string(locatorJSON)
	fields["primary_locator_count"], fields["unique_locator_count"], fields["full_row_locator_count"] = primaryCount, uniqueCount, fullRowCount
	failedCount, failedDDLCount := bootstrapFailureCounts(record.FailedObjects)
	j.FailedObjectCount, j.FailedDDLCount = failedCount, failedDDLCount
	fields["failed_object_count"], fields["failed_ddl_count"] = failedCount, failedDDLCount
	if !legacyScope {
		j.EffectiveJSON, j.ExcludedJSON = string(effectiveJSON), string(excludedJSON)
		j.EffectiveCount, j.ExcludedCount, j.ManifestHash = len(record.EffectiveTables), len(record.ExcludedTables), record.ManifestHash
		fields["effective_tables_json"], fields["excluded_tables_json"] = string(effectiveJSON), string(excludedJSON)
		fields["effective_table_count"], fields["excluded_table_count"] = len(record.EffectiveTables), len(record.ExcludedTables)
		fields["bootstrap_manifest_hash"] = record.ManifestHash
	}
	return store.UpdateIncrementalJob(j.JobID, fields)
}

func applyReviewBootstrapRecord(j *store.IncrementalMigrationJob, record cdc.BootstrapRecord) error {
	if record.LocatorStrategyVersion != cdc.LocatorStrategyVersion {
		return fmt.Errorf("CDC 定位策略版本不兼容，旧任务不能恢复，请重新执行全量快照")
	}
	effectiveJSON, _ := json.Marshal(record.EffectiveTables)
	excludedJSON, _ := json.Marshal(record.ExcludedTables)
	j.BootstrapState = "review_pending"
	j.PendingFile, j.PendingPos, j.PendingGTID = record.Position.File, record.Position.Pos, record.Position.GTID
	j.EffectiveJSON, j.ExcludedJSON = string(effectiveJSON), string(excludedJSON)
	j.EffectiveCount, j.ExcludedCount, j.ManifestHash = len(record.EffectiveTables), len(record.ExcludedTables), record.ManifestHash
	failedCount, failedDDLCount := bootstrapFailureCounts(record.FailedObjects)
	locatorJSON, _ := json.Marshal(record.LocatorStrategies)
	primaryCount, uniqueCount, fullRowCount := locatorStrategyCounts(record.LocatorStrategies)
	j.FailedObjectCount, j.FailedDDLCount = failedCount, failedDDLCount
	return store.UpdateIncrementalJob(j.JobID, map[string]any{
		"status": "paused_bootstrap_review", "phase": "bootstrap_review", "bootstrap_state": "review_pending",
		"pending_file": record.Position.File, "pending_position": record.Position.Pos, "pending_gtid": record.Position.GTID,
		"effective_tables_json": string(effectiveJSON), "excluded_tables_json": string(excludedJSON),
		"effective_table_count": len(record.EffectiveTables), "excluded_table_count": len(record.ExcludedTables),
		"bootstrap_manifest_hash": record.ManifestHash, "summary": "全量存在失败表，请确认排除范围后继续",
		"failed_object_count": failedCount, "failed_ddl_count": failedDDLCount,
		"locator_strategy_version": record.LocatorStrategyVersion, "locator_strategies_json": string(locatorJSON),
		"primary_locator_count": primaryCount, "unique_locator_count": uniqueCount, "full_row_locator_count": fullRowCount,
	})
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
		},
		RowConflict: func(conflict cdc.RowConflict) {
			_ = store.UpdateIncrementalJob(j.JobID, map[string]any{
				"status": "paused_row_conflict", "phase": "paused", "summary": "目标端更新前记录无法唯一定位，请修复后恢复",
				"conflict_table": conflict.Table, "conflict_action": conflict.Action, "conflict_file": conflict.Position.File,
				"conflict_position": conflict.Position.Pos, "conflict_gtid": conflict.Position.GTID,
				"conflict_error": conflict.Error, "conflict_before_hash": conflict.BeforeHash,
			})
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
	filter, e := parseJobListFilter(c, incrementalListStatuses, false)
	if e != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": e.Error()})
		return
	}
	j, e := store.QueryIncrementalJobsWithConn(middleware.GetCurrentUserID(c), middleware.IsAdmin(c), filter)
	if e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	}
	c.JSON(200, j)
}
func GetIncremental(c *gin.Context) {
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	detail, e := store.GetIncrementalJobWithConn(j.JobID)
	if e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	}
	c.JSON(200, detail)
}

func GetIncrementalBootstrapReview(c *gin.Context) {
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	dst, e := store.GetConnection(j.DstConnID)
	if e != nil {
		c.JSON(400, gin.H{"error": "目标连接已删除"})
		return
	}
	reviewCfg := cdc.Config{JobID: j.JobID, TargetDSN: buildDSN(dst), TargetDBType: dst.DBType, TargetSchema: j.TargetSchema, LowerCaseNames: j.LowerCaseNames}
	record, exists, e := cdc.LoadTargetBootstrapRecord(c.Request.Context(), reviewCfg)
	if e != nil {
		c.JSON(500, gin.H{"error": "读取目标 bootstrap checkpoint 失败: " + e.Error()})
		return
	}
	if !exists {
		c.JSON(404, gin.H{"error": "该任务没有可用的 bootstrap 审阅记录"})
		return
	}
	review := cdc.BootstrapReview{BootstrapRecord: record, RequestedCount: len(record.EffectiveTables) + len(record.ExcludedTables)}
	if j.BootstrapReport != "" {
		_ = json.Unmarshal([]byte(j.BootstrapReport), &review)
		review.BootstrapRecord = record
	}
	// The live checkpoint position advances after CDC starts, while the review
	// must continue to show the original consistent-snapshot position.
	if j.PendingFile != "" || j.PendingGTID != "" {
		review.Position = cdc.Position{File: j.PendingFile, Pos: j.PendingPos, GTID: j.PendingGTID}
	}
	if review.RequestedCount == 0 {
		review.RequestedCount = len(record.EffectiveTables) + len(record.ExcludedTables)
	}
	c.JSON(200, review)
}

type acceptBootstrapExclusionsRequest struct {
	ManifestHash string `json:"manifest_hash" binding:"required"`
	Acknowledge  bool   `json:"acknowledge" binding:"required"`
}

func AcceptIncrementalBootstrapExclusions(c *gin.Context) {
	var req acceptBootstrapExclusionsRequest
	if e := c.ShouldBindJSON(&req); e != nil || !req.Acknowledge {
		c.JSON(400, gin.H{"error": "必须明确确认排除表清单"})
		return
	}
	incrementalStartMu.Lock()
	defer incrementalStartMu.Unlock()
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	if j.Status != "paused_bootstrap_review" && j.BootstrapState != "completed" {
		c.JSON(409, gin.H{"error": "任务当前不处于全量排除审阅状态"})
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
	record, exists, e := cdc.LoadTargetBootstrapRecord(c.Request.Context(), cfg)
	if e != nil {
		c.JSON(500, gin.H{"error": "读取目标 bootstrap checkpoint 失败: " + e.Error()})
		return
	}
	if !exists || (record.State != "review_pending" && record.State != "completed") {
		c.JSON(409, gin.H{"error": "目标 bootstrap checkpoint 不可确认"})
		return
	}
	if req.ManifestHash != record.ManifestHash || req.ManifestHash != j.ManifestHash {
		c.JSON(409, gin.H{"error": "排除表清单已变化，请刷新后重新确认"})
		return
	}
	if record.State == "completed" && j.BootstrapDone && j.Status != "paused_bootstrap_review" {
		c.JSON(200, gin.H{"message": "失败表排除已确认，任务已进入后续阶段"})
		return
	}
	if record.State == "review_pending" {
		if e = cdc.ValidatePositionAvailable(c.Request.Context(), cfg, record.Position); e != nil {
			c.JSON(409, gin.H{"error": "原始快照位点已不可继续: " + e.Error()})
			return
		}
		if e = cdc.FinalizeTargetBootstrap(c.Request.Context(), cfg, record); e != nil {
			c.JSON(500, gin.H{"error": "确认排除表失败: " + e.Error()})
			return
		}
		record.State = "completed"
	} else if e = cdc.ValidatePositionAvailable(c.Request.Context(), cfg, record.Position); e != nil {
		// Covers the crash window after the target committed the manifest but
		// before SQLite moved the task out of bootstrap review.
		c.JSON(409, gin.H{"error": "原始快照位点已不可继续: " + e.Error()})
		return
	}
	if e = applyCompletedBootstrapRecord(j, record); e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	}
	if e = store.UpdateIncrementalJob(j.JobID, map[string]any{"status": "catching_up", "phase": "catching_up", "last_error": "", "summary": "已确认排除失败表，正在从原始快照位点追赶 binlog"}); e != nil {
		c.JSON(500, gin.H{"error": e.Error()})
		return
	}
	appendIncrementalLifecycleLog(j.JobID, "catching_up", "done", fmt.Sprintf("用户已确认排除 %d 张失败表，成功表开始追赶 binlog", len(record.ExcludedTables)))
	j.Status, j.Phase = "catching_up", "catching_up"
	if !cdc.Registry.Running(j.JobID) {
		launchIncremental(j, src, dst)
	}
	c.JSON(200, gin.H{"message": "已确认排除失败表，成功表开始追赶 binlog"})
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
	AcknowledgeWarnings   bool `json:"acknowledge_warnings"`
	AcknowledgeExclusions bool `json:"acknowledge_exclusions"`
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
		c.JSON(409, gin.H{"error": "存在同步警告，必须明确确认风险后才能完成切换"})
		return
	}
	if j.ExcludedCount > 0 && !req.AcknowledgeExclusions {
		c.JSON(409, gin.H{"error": "本次迁移排除了部分表，必须再次确认迁移范围后才能完成切换"})
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
	incrementalStartMu.Lock()
	defer incrementalStartMu.Unlock()
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
		if !j.BootstrapDone {
			if src, srcErr := store.GetConnection(j.SrcConnID); srcErr == nil {
				if dst, dstErr := store.GetConnection(j.DstConnID); dstErr == nil {
					_ = cdc.AbortTargetBootstrap(c.Request.Context(), configFromIncremental(j, src, dst))
				}
			}
		}
		now := time.Now()
		updated, e := store.UpdateIncrementalJobIfStatus(j.JobID, []string{j.Status}, map[string]any{"status": "aborted", "phase": "aborted", "bootstrap_state": "aborted", "summary": "任务已放弃", "finished_at": &now})
		if e != nil {
			c.JSON(500, gin.H{"error": e.Error()})
			return
		}
		if !updated {
			c.JSON(409, gin.H{"error": "任务状态已变化，请刷新后重试"})
			return
		}
		if j.StartMode == "full_then_cdc" && !j.BootstrapDone {
			appendIncrementalLifecycleLog(j.JobID, "snapshot_init", "warn", "用户已放弃未完成的全量任务")
		}
	}
	c.JSON(200, gin.H{"message": "任务已放弃"})
}

func ResumeIncremental(c *gin.Context) {
	incrementalStartMu.Lock()
	defer incrementalStartMu.Unlock()
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	if cdc.Registry.Running(j.JobID) {
		c.JSON(409, gin.H{"error": "任务已在运行"})
		return
	}
	if j.LocatorStrategyVersion != cdc.LocatorStrategyVersion {
		c.JSON(409, gin.H{"error": "CDC定位策略已升级，旧任务不能恢复，请重新执行全量快照"})
		return
	}
	if !slices.Contains([]string{"paused_manual", "paused_restart", "failed", "paused_row_conflict"}, j.Status) {
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
		record, exists, loadErr := cdc.LoadTargetBootstrapRecord(c.Request.Context(), cfg)
		if loadErr != nil {
			c.JSON(500, gin.H{"error": "读取目标 checkpoint 失败，已禁止自动重跑全量: " + loadErr.Error()})
			return
		}
		if !exists {
			c.JSON(409, gin.H{"error": "全量快照未完成，不能安全断点恢复，也不会自动删表重跑；请放弃此任务后显式新建"})
			return
		}
		if record.State == "review_pending" {
			_ = applyReviewBootstrapRecord(j, record)
			c.JSON(409, gin.H{"error": "全量存在失败表，请使用“接受排除并继续”，不能通过普通恢复绕过人工确认"})
			return
		}
		if record.State != "completed" {
			c.JSON(409, gin.H{"error": "全量快照状态为 " + record.State + "，不能安全恢复，也不会自动删表重跑"})
			return
		}
		if e = applyCompletedBootstrapRecord(j, record); e != nil {
			c.JSON(500, gin.H{"error": e.Error()})
			return
		}
	}
	j.Status = "initializing"
	updated, e := store.UpdateIncrementalJobIfStatus(j.JobID, []string{"paused_manual", "paused_restart", "failed", "paused_row_conflict"}, map[string]any{
		"status": "initializing", "phase": "initializing", "last_error": "", "summary": "正在从目标 checkpoint 恢复",
		"conflict_table": "", "conflict_action": "", "conflict_file": "", "conflict_position": 0,
		"conflict_gtid": "", "conflict_error": "", "conflict_before_hash": "",
	})
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
