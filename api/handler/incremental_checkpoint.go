package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"dbgold/datamigrate/cdc"
	"dbgold/store"
)

const legacyPGSchemaMessage = "旧版 PostgreSQL 系列 CDC 任务缺少冻结的源结构，请重新建立全量或经核验的增量起点"

func incrementalSchemaError(j *store.IncrementalMigrationJob) string {
	if j.SourceSchemaVersion == cdc.SourceSchemaVersion {
		return ""
	}
	if j.DstDBType == "dameng" {
		return cdc.LegacyDamengSchemaMessage
	}
	return legacyPGSchemaMessage
}

func claimIncrementalTarget(ctx context.Context, cfg cdc.Config) error {
	jobs, err := store.LegacyIncrementalTargetJobs(cfg.TargetSchema)
	if err != nil {
		return err
	}
	err = cdc.ClaimTargetSchema(ctx, cfg, jobs, func(owner string) (bool, error) {
		job, err := store.GetIncrementalJob(owner)
		if store.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return job.Status == "stopped" || job.Status == "aborted", nil
	})
	if errors.Is(err, cdc.ErrTargetOwnershipConflict) {
		return errIncrementalTargetInUse
	}
	return err
}

func checkIncrementalTargetOwner(ctx context.Context, cfg cdc.Config) error {
	if !cfg.ManagedCheckpoint {
		return nil
	}
	return cdc.CheckTargetSchemaOwner(ctx, cfg)
}

func releaseIncrementalTarget(ctx context.Context, j *store.IncrementalMigrationJob, src, dst *store.Connection) {
	if !j.CheckpointManaged {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := cdc.ReleaseTargetSchema(ctx, configFromIncremental(j, src, dst)); err != nil {
		slog.Warn("释放增量迁移目标占用失败", "job_id", j.JobID, "error", err)
	}
}

func checkIncrementalEndpoints(j *store.IncrementalMigrationJob, src, dst *store.Connection) error {
	if (j.SrcDBType != "" && j.SrcDBType != src.DBType) || (j.DstDBType != "" && j.DstDBType != dst.DBType) ||
		(j.SrcConnHost != "" && (!strings.EqualFold(j.SrcConnHost, src.Host) || j.SrcConnPort != src.Port)) ||
		(j.DstConnHost != "" && (!strings.EqualFold(j.DstConnHost, dst.Host) || j.DstConnPort != dst.Port || j.DstConnDatabase != dst.Database)) {
		return fmt.Errorf("连接端点与任务原始配置不一致，请恢复原端点或重建增量任务")
	}
	return nil
}

func checkIncrementalRecovery(ctx context.Context, j *store.IncrementalMigrationJob, src, dst *store.Connection) error {
	if err := checkIncrementalEndpoints(j, src, dst); err != nil {
		return err
	}
	cfg := configFromIncremental(j, src, dst)
	cfg.RequireExistingCheckpoint = true
	record, exists, err := cdc.LoadTargetBootstrapRecord(ctx, cfg)
	if err != nil {
		return fmt.Errorf("读取目标检查点失败，禁止重新初始化: %w", err)
	}
	if !exists {
		return cdc.ErrCheckpointMissing
	}
	if err = cdc.ValidatePositionAvailable(ctx, cfg, record.Position); err != nil {
		return fmt.Errorf("目标检查点无法从源 binlog 继续: %w", err)
	}
	if err = record.SourceSchema.Validate(j.SrcDatabase); err != nil {
		return err
	}
	if j.CheckpointManaged {
		return checkIncrementalTargetOwner(ctx, cfg)
	}
	// Existing Dameng jobs with durable records join the physical-schema lock
	// on first use. An absent record cannot be adopted as a new job.
	if err = claimIncrementalTarget(ctx, cfg); err != nil {
		return err
	}
	if err = store.UpdateIncrementalJob(j.JobID, map[string]any{"checkpoint_managed": true}); err != nil {
		return err
	}
	j.CheckpointManaged = true
	return nil
}
