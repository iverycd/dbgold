package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var newMigrationTriggerPlanReader = buildSrcReader
var newMigrationTriggerPlanWriter = func(conn *store.Connection, schema string, pool target.ConnPoolConfig) (target.Writer, error) {
	return buildDstWriter(conn, schema, pool)
}

type triggerPlanRequest struct {
	Version        int    `json:"version"`
	BindingVersion int    `json:"binding_version"`
	PlanHash       string `json:"plan_hash"`
	Reviewed       bool   `json:"reviewed"`
}

// RefreshMigrationTriggerPlan only reads the existing ledger and current source.
// A preview cannot establish executable authority or modify the original report.
func RefreshMigrationTriggerPlan(c *gin.Context) { migrationTriggerPlanRequest(c, false) }

// SaveMigrationTriggerPlan stores only a freshly regenerated, reviewed plan. SQL
// from the client is never an execution candidate; execution is a separate action.
func SaveMigrationTriggerPlan(c *gin.Context) { migrationTriggerPlanRequest(c, true) }

func migrationTriggerPlanRequest(c *gin.Context, save bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	job, ok := ownedRepairJob(c, true)
	if !ok {
		return
	}
	// Opening a plan must not seed a legacy ledger or manufacture identities.
	if _, err := store.GetMigrationRepairBinding(job.JobID); err != nil {
		repairError(c, fmt.Errorf("请先打开修复列表并绑定原源库和目标连接"))
		return
	}
	job, binding, row, original, ok := loadRepairObject(c, true)
	if !ok {
		return
	}
	var req triggerPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		repairError(c, err)
		return
	}
	if req.Version != row.Version {
		repairError(c, store.ErrRepairConflict)
		return
	}
	if save && (!req.Reviewed || req.PlanHash == "") {
		repairError(c, fmt.Errorf("请先生成并核对完整触发器方案后确认保存"))
		return
	}
	if save && req.BindingVersion != binding.Version {
		repairError(c, store.ErrRepairConflict)
		return
	}
	if err := validateTriggerPlanOrigin(job, binding, original); err != nil {
		repairError(c, err)
		return
	}
	cfg, err := recoveryConfig(binding)
	if err != nil {
		repairError(c, err)
		return
	}
	if cfg.Content == "data_only" {
		repairError(c, fmt.Errorf("仅数据任务不能重新生成触发器方案"))
		return
	}
	src, dst, err := resolveRepairConnections(c, job, binding, true)
	if err != nil {
		repairError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	reader, err := newMigrationTriggerPlanReader(src, sourceOverride(binding, src.DBType), source.ConnPoolConfig{}, dst.DBType)
	if err != nil {
		repairError(c, err)
		return
	}
	defer reader.Close()
	writer, err := newMigrationTriggerPlanWriter(dst, binding.TargetSchema, target.ConnPoolConfig{})
	if err != nil {
		repairError(c, err)
		return
	}
	defer writer.Close()
	existing, existingRows, err := repairObjects(job.JobID)
	if err != nil {
		repairError(c, err)
		return
	}
	candidate, err := datamigrate.BuildMySQLPGTriggerRepairPlan(ctx, reader, writer, cfg, original, existing)
	if err != nil {
		repairError(c, err)
		return
	}
	if err := ctx.Err(); err != nil {
		repairError(c, err)
		return
	}
	if err := validateGeneratedTriggerCandidate(original, candidate, job.DstDBType); err != nil {
		repairError(c, err)
		return
	}
	dependencyVersions, err := triggerPlanDependencyVersions(candidate, existingRows)
	if err != nil {
		repairError(c, err)
		return
	}
	hash := repair.Hash([]any{"mysql-pg-trigger-plan", row.Version, binding.Version, cfg, candidate, dependencyVersions})
	if save && req.PlanHash != hash {
		repairError(c, fmt.Errorf("触发器方案或源元数据已变化，请重新生成并核对"))
		return
	}
	if err := triggerPlanVersionsCurrent(job.JobID, row.ObjectID, row.Version, binding.Version); err != nil {
		repairError(c, err)
		return
	}
	if err := triggerPlanDependenciesCurrent(job.JobID, dependencyVersions); err != nil {
		repairError(c, err)
		return
	}
	if !save {
		c.JSON(http.StatusOK, gin.H{"object": candidate, "version": row.Version, "binding_version": binding.Version, "plan_hash": hash, "message": "已按当前转换规则生成触发器方案，请核对两个只读执行单元并确认保存；尚未执行目标 SQL"})
		return
	}
	if err := saveTriggerPlanCandidate(job.JobID, row.ObjectID, row.Version, binding.Version, candidate, dependencyVersions); err != nil {
		repairError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"object": candidate, "version": row.Version + 1})
}

func validateTriggerPlanOrigin(job *store.DataMigrationJob, binding *store.MigrationRepairBinding, object repair.Object) error {
	if job.SrcDBType != "mysql" || !source.IsPGFamily(job.DstDBType) || object.Kind != "trigger" || object.MySQLPGTrigger != nil || !object.DiagnosticOnly {
		return fmt.Errorf("仅支持原报告中尚未生成执行方案的 MySQL → PG 触发器转换失败记录")
	}
	if !binding.Configured || binding.ConfigJSON == "" || object.Schema == "" || object.Schema != binding.TargetSchema {
		return fmt.Errorf("请先绑定与原任务匹配的源库、目标库及 Schema")
	}
	switch object.Status {
	case "failed", "blocked", "pending", "cancelled":
	default:
		return fmt.Errorf("触发器已执行、执行结果未知或状态不允许重新生成")
	}
	if object.SourceName == "" || object.SourceTable == "" || object.TargetName == "" || object.Table == "" || strings.TrimSpace(object.SourceDDL) == "" {
		return fmt.Errorf("原记录缺少触发器身份或完整源定义，不能安全重新生成")
	}
	if len(object.Units) != 0 || strings.TrimSpace(object.DDL) != "" || strings.TrimSpace(object.SuggestedDDL) != "" {
		return fmt.Errorf("已有执行单元或手工 SQL 的触发器不能用自动方案覆盖")
	}
	reportRow, err := store.GetDataMigrationReport(job.JobID)
	if err != nil {
		return fmt.Errorf("缺少原始迁移报告，不能确认触发器转换失败记录")
	}
	var report datamigrate.MigrationReport
	if err = json.Unmarshal([]byte(reportRow.ReportJSON), &report); err != nil {
		return err
	}
	matches := 0
	if report.TriggerMigrationEnabled {
		for _, item := range report.Triggers.Items {
			if item.Stage == "unsupported" && item.SourceName == object.SourceName && item.Table == object.SourceTable && item.SourceDDL == object.SourceDDL {
				matches++
			}
		}
	}
	if matches != 1 {
		return fmt.Errorf("原报告中没有唯一匹配且源定义完整的触发器不支持记录")
	}
	var attempts int64
	if err = store.DB.Model(&store.MigrationRepairAttempt{}).Where("job_id = ? AND object_id = ? AND kind IN ?", job.JobID, object.ID, []string{"ddl", "recovery"}).Count(&attempts).Error; err != nil {
		return err
	}
	if attempts != 0 {
		return fmt.Errorf("触发器已有执行历史，不能覆盖原执行证据")
	}
	return triggerPlanVersionsCurrent(job.JobID, object.ID, 0, binding.Version)
}

func validateGeneratedTriggerCandidate(original, candidate repair.Object, targetType string) error {
	if candidate.ID != original.ID || candidate.Kind != original.Kind || candidate.SourceName != original.SourceName || candidate.SourceTable != original.SourceTable || candidate.TargetName != original.TargetName || candidate.Table != original.Table || candidate.Schema != original.Schema || candidate.SourceDDL != original.SourceDDL {
		return fmt.Errorf("重新生成的触发器身份或源定义与原记录不一致")
	}
	if candidate.DiagnosticOnly || candidate.MySQLPGTrigger == nil || candidate.Status != "pending" || candidate.RequiresReview || len(candidate.Units) != 2 || candidate.MySQLPGTrigger.Version != mysqltrigger.CurrentVersion {
		return fmt.Errorf("未生成完整的只读触发器方案")
	}
	if candidate.OriginalError != original.OriginalError || candidate.OriginalStatus != original.OriginalStatus {
		return fmt.Errorf("重新生成方案不能修改原始迁移结果")
	}
	for _, unit := range candidate.Units {
		if unit.Status != "pending" || !unit.Selected || unit.Destructive || unit.SQL != unit.OriginalSQL {
			return fmt.Errorf("新触发器方案必须完整保留两个原生成步骤")
		}
	}
	if err := validateRepairUnits(candidate.Units); err != nil {
		return err
	}
	return validateRepairObjectScope(candidate, candidate.Units, targetType)
}

func triggerPlanVersionsCurrent(jobID, objectID string, objectVersion, bindingVersion int) error {
	current, err := store.GetMigrationRepairObject(jobID, objectID)
	if err != nil {
		return err
	}
	binding, err := store.GetMigrationRepairBinding(jobID)
	if err != nil {
		return err
	}
	job, err := store.GetDataMigrationJob(jobID)
	if err != nil {
		return err
	}
	if objectVersion != 0 && current.Version != objectVersion || binding.Version != bindingVersion || job.Status == "running" {
		return store.ErrRepairConflict
	}
	var running int64
	if err := store.DB.Model(&store.MigrationRepairAttempt{}).Where("job_id = ? AND status = ?", jobID, "running").Count(&running).Error; err != nil {
		return err
	}
	if running != 0 {
		return store.ErrRepairConflict
	}
	return nil
}

func triggerPlanDependencyVersions(candidate repair.Object, rows []store.MigrationRepairObject) (map[string]int, error) {
	versions := map[string]int{}
	for _, id := range candidate.Dependencies {
		for _, row := range rows {
			if row.ObjectID == id {
				versions[id] = row.Version
				break
			}
		}
		if versions[id] == 0 {
			return nil, fmt.Errorf("触发器依赖清单已变化，请重新生成")
		}
	}
	return versions, nil
}

func triggerPlanDependenciesCurrent(jobID string, versions map[string]int) error {
	for id, version := range versions {
		row, err := store.GetMigrationRepairObject(jobID, id)
		if err != nil || row.Version != version {
			return store.ErrRepairConflict
		}
	}
	return nil
}

func saveTriggerPlanCandidate(jobID, objectID string, objectVersion, bindingVersion int, candidate repair.Object, dependencies map[string]int) error {
	return store.DB.Transaction(func(tx *gorm.DB) error {
		// Check binding and active attempts in the same UPDATE that advances the
		// object version, so a concurrent rebind or execution cannot approve stale SQL.
		result := tx.Model(&store.MigrationRepairObject{}).
			Where("job_id = ? AND object_id = ? AND version = ?", jobID, objectID, objectVersion).
			Where("EXISTS (SELECT 1 FROM migration_repair_bindings b WHERE b.job_id = ? AND b.version = ? AND b.configured = ?)", jobID, bindingVersion, true).
			Where("NOT EXISTS (SELECT 1 FROM migration_repair_attempts a WHERE a.job_id = ? AND a.status = ?)", jobID, "running").
			Where("EXISTS (SELECT 1 FROM data_migration_jobs j WHERE j.job_id = ? AND j.status <> ?)", jobID, "running")
		for id, version := range dependencies {
			result = result.Where("EXISTS (SELECT 1 FROM migration_repair_objects d WHERE d.job_id = ? AND d.object_id = ? AND d.version = ?)", jobID, id, version)
		}
		result = result.Updates(map[string]any{"object_json": repairJSON(candidate), "version": objectVersion + 1, "updated_at": time.Now()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return store.ErrRepairConflict
		}
		return nil
	})
}
