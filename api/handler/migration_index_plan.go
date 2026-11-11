package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/target"
	"dbgold/store"
	"github.com/gin-gonic/gin"
)

var newMigrationIndexPlanWriter = func(conn *store.Connection, schema string, pool target.ConnPoolConfig) (target.Writer, error) {
	return buildDstWriter(conn, schema, pool)
}

// RefreshMigrationIndexPlan only returns a review candidate. It never writes the
// ledger or changes the target. Saving re-plans against the bound target again.
func RefreshMigrationIndexPlan(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	j, ok := ownedRepairJob(c, true)
	if !ok {
		return
	}
	if _, err := store.GetMigrationRepairBinding(j.JobID); err != nil {
		repairError(c, fmt.Errorf("请先打开修复列表并绑定原目标连接"))
		return
	}
	j, binding, row, obj, ok := loadRepairObject(c, true)
	if !ok {
		return
	}
	var req struct {
		Version int `json:"version"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		repairError(c, err)
		return
	}
	if req.Version != row.Version {
		repairError(c, store.ErrRepairConflict)
		return
	}
	candidate, err := migrationIndexPlan(c, j, binding, obj, row.Version)
	if err != nil {
		repairError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"object": candidate, "version": row.Version, "has_changes": true, "message": "已按目标表实际分布生成索引计划；请核对 SQL 后保存并执行。现有表及数据不会重建。"})
}

func migrationIndexPlan(c *gin.Context, job *store.DataMigrationJob, binding *store.MigrationRepairBinding, obj repair.Object, version int) (repair.Object, error) {
	if job.SrcDBType != "mysql" || job.DstDBType != "gaussdb" || obj.Index == nil || obj.Kind != "primary_key" && obj.Kind != "index" || !obj.Index.IsPrimary && !obj.Index.IsUnique {
		return obj, fmt.Errorf("仅支持 MySQL → GaussDB 主键和唯一索引计划")
	}
	if !binding.Configured || binding.TargetSchema != obj.Schema || !obj.CanEditDDL() {
		return obj, fmt.Errorf("请绑定与此对象匹配的原目标连接及 Schema")
	}
	if err := checkIndexPlanRefreshable(obj); err != nil {
		return obj, err
	}
	var running int64
	if err := store.DB.Model(&store.MigrationRepairAttempt{}).Where("job_id = ? AND status = ?", job.JobID, "running").Count(&running).Error; err != nil {
		return obj, err
	}
	if running > 0 {
		return obj, fmt.Errorf("任务仍有修复正在执行")
	}
	_, dst, err := resolveRepairConnections(c, job, binding, false)
	if err != nil {
		return obj, err
	}
	w, err := newMigrationIndexPlanWriter(dst, binding.TargetSchema, target.ConnPoolConfig{})
	if err != nil {
		return obj, err
	}
	defer w.Close()
	planner, ok := w.(target.GaussDBIndexPlanner)
	if !ok {
		return obj, fmt.Errorf("目标 Writer 不支持 GaussDB 分布索引计划")
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	idx := *obj.Index
	// Only this planner's diagnostic can be cleared; retain unrelated source
	// incompatibilities rather than turning them into executable SQL.
	if strings.HasPrefix(idx.Unsupported, "GaussDB 分布索引检查失败:") {
		idx.Unsupported = ""
	}
	if idx.Unsupported != "" {
		return obj, fmt.Errorf("%s", idx.Unsupported)
	}
	idx.TableName = obj.Table
	idx.GaussDB = nil
	idx, err = planner.PlanGaussDBIndex(ctx, idx)
	if err != nil {
		return obj, err
	}
	if dialect.IndexName(w.Dialect(), idx) != obj.TargetName {
		return obj, fmt.Errorf("索引目标名称与原任务不一致")
	}
	obj.Index = &idx
	ss := w.Dialect().IndexStatements(obj.Schema, idx)
	obj = sequencePlanCandidate(obj, ss)
	for i := range obj.Units {
		obj.Units[i].ID = "index-plan-" + repair.Hash([]any{obj.Units[i].SQL, idx, version, binding.Version})
	}
	obj.Error = ""
	current, err := store.GetMigrationRepairObject(job.JobID, obj.ID)
	if err != nil {
		return obj, err
	}
	currentBinding, err := store.GetMigrationRepairBinding(job.JobID)
	if err != nil {
		return obj, err
	}
	if current.Version != version || currentBinding.Version != binding.Version {
		return obj, store.ErrRepairConflict
	}
	return obj, nil
}

func checkIndexPlanRefreshable(obj repair.Object) error {
	unsafe := func(status string) bool {
		return status == "unknown" || status == "running" || status == "unverified" || status == "needs_verification" || status == "succeeded"
	}
	if unsafe(obj.Status) {
		return fmt.Errorf("索引已成功、正在执行或结果未知，请先核验现有对象")
	}
	for _, u := range obj.Units {
		if unsafe(u.Status) {
			return fmt.Errorf("存在已成功或结果未知的索引步骤，禁止自动覆盖")
		}
		if u.SQL != u.OriginalSQL || strings.TrimSpace(u.SQL) != "" && u.OriginalSQL == "" {
			return fmt.Errorf("索引 SQL 已手动编辑，不能覆盖现有草稿")
		}
	}
	return nil
}
