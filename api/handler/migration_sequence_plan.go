package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/store"
	"github.com/gin-gonic/gin"
)

var newMigrationSequencePlanWriter = buildDstWriter

// RefreshMigrationSequencePlan returns a review candidate. Opening a plan must
// not seed a legacy ledger, save a draft, claim an attempt, or execute target SQL.
func RefreshMigrationSequencePlan(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	j, ok := ownedRepairJob(c, true)
	if !ok {
		return
	}
	if _, err := store.GetMigrationRepairBinding(j.JobID); err != nil {
		repairError(c, fmt.Errorf("请先打开修复列表并绑定原目标连接"))
		return
	}
	j, binding, row, object, ok := loadRepairObject(c, true)
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
	candidate, changes, err := migrationSequencePlan(c, j, binding, object, row.Version)
	if err != nil {
		repairError(c, err)
		return
	}
	message := "已根据目标库当前状态生成序列修复计划，请核对完整执行单元后保存草稿"
	if !changes {
		message = "目标序列及触发器均已兼容，无需变更；以下步骤仅在执行时重新核对并复用已有对象"
	}
	c.JSON(http.StatusOK, gin.H{"object": candidate, "version": row.Version, "has_changes": changes, "message": message})
}

func migrationSequencePlan(c *gin.Context, job *store.DataMigrationJob, binding *store.MigrationRepairBinding, object repair.Object, version int) (repair.Object, bool, error) {
	if job.SrcDBType != "oracle" || job.DstDBType != "dameng" || object.Kind != "sequence" || object.Sequence == nil || object.Sequence.Native == nil || object.Sequence.Native.Name == "" {
		return object, false, fmt.Errorf("仅支持保留完整 Oracle 命名序列元数据的达梦序列对象")
	}
	if !object.CanEditDDL() || object.Sequence.FailureStage == "metadata" || object.Sequence.Unsupported != "" && object.Sequence.FailureStage != "preflight" {
		return object, false, fmt.Errorf("源序列元数据缺失或不受支持，请修复源端问题后发起新的迁移任务")
	}
	seq := *object.Sequence
	if seq.FailureStage == "preflight" {
		seq.Unsupported, seq.FailureStage = "", ""
	}
	seq, err := dialect.NormalizeOracleDamengSequence(seq)
	if err != nil {
		return object, false, err
	}
	object.Sequence = &seq
	if !binding.Configured || object.Schema != binding.TargetSchema {
		return object, false, fmt.Errorf("请先绑定与此对象匹配的原目标连接及 Schema")
	}
	unknown := func(status string) bool {
		return status == "unknown" || status == "running" || status == "unverified" || status == "needs_verification"
	}
	if unknown(object.Status) {
		return object, false, fmt.Errorf("对象执行结果未知或仍在运行，禁止重新生成执行步骤")
	}
	for _, unit := range object.Units {
		if unknown(unit.Status) {
			return object, false, fmt.Errorf("存在执行结果未知或仍在运行的单元，禁止重新生成执行步骤")
		}
		if unit.SQL != unit.OriginalSQL || strings.TrimSpace(unit.SQL) != "" && unit.OriginalSQL == "" {
			return object, false, fmt.Errorf("执行单元已手动编辑，不能用自动计划覆盖；请继续核对现有草稿")
		}
	}
	var running int64
	if err := store.DB.Model(&store.MigrationRepairAttempt{}).Where("job_id = ? AND status = ?", job.JobID, "running").Count(&running).Error; err != nil {
		return object, false, err
	}
	if running != 0 {
		return object, false, fmt.Errorf("此任务仍有修复正在执行，请等待执行结果后再生成计划")
	}
	_, dst, err := resolveRepairConnections(c, job, binding, false)
	if err != nil {
		return object, false, err
	}
	writer, err := newMigrationSequencePlanWriter(dst, binding.TargetSchema, target.ConnPoolConfig{})
	if err != nil {
		return object, false, err
	}
	defer writer.Close()
	planner, ok := writer.(interface {
		PlanOracleDamengSequence(context.Context, source.SequenceInfo) ([]dialect.Statement, error)
	})
	if !ok {
		return object, false, fmt.Errorf("目标写入器不支持序列修复计划")
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	statements, err := planner.PlanOracleDamengSequence(ctx, *object.Sequence)
	if err != nil {
		return object, false, err
	}
	changes := len(statements) > 0
	if !changes {
		statements = writer.Dialect().SequenceStatements(binding.TargetSchema, *object.Sequence, "")
	}
	if len(statements) == 0 {
		return object, false, fmt.Errorf("序列元数据无法生成可核对的完整执行单元")
	}
	candidate := sequencePlanCandidate(object, statements)
	if len(candidate.Units) > 100 {
		return object, false, fmt.Errorf("序列计划的执行单元超过100个，请单独核对关联触发器")
	}
	current, err := store.GetMigrationRepairObject(job.JobID, object.ID)
	if err != nil {
		return object, false, err
	}
	currentBinding, err := store.GetMigrationRepairBinding(job.JobID)
	if err != nil {
		return object, false, err
	}
	if current.Version != version || currentBinding.Version != binding.Version {
		return object, false, store.ErrRepairConflict
	}
	return candidate, changes, nil
}

func sequencePlanCandidate(object repair.Object, statements []dialect.Statement) repair.Object {
	units := make([]repair.Unit, 0, len(object.Units)+len(statements))
	used := map[string]bool{}
	for _, unit := range object.Units {
		if unit.Status == "succeeded" {
			unit.Selected = false
			units = append(units, unit)
			used[unit.ID] = true
		}
	}
	for _, statement := range statements {
		unit := repair.Unit{ID: "sequence-plan-" + repair.Hash(statement.SQL), SQL: statement.SQL, OriginalSQL: statement.SQL, Status: "pending", Selected: true}
		for _, previous := range object.Units {
			if previous.Status != "succeeded" && previous.ID != "" && previous.SQL == statement.SQL {
				unit.ID = previous.ID
				break
			}
		}
		for used[unit.ID] {
			unit.ID += "-check"
		}
		used[unit.ID] = true
		units = append(units, unit)
	}
	object.Units = units
	object.Status = "draft"
	object.RequiresReview = true
	object.SuggestedDDL = dialect.JoinSQL(statements)
	return object
}

func validateSequencePlanDraft(candidate repair.Object, units []repair.Unit) error {
	if len(candidate.Units) != len(units) {
		return fmt.Errorf("序列计划已变化或执行单元已被编辑，请重新生成并核对计划")
	}
	for i, expected := range candidate.Units {
		if units[i].ID != expected.ID || units[i].SQL != expected.SQL {
			return fmt.Errorf("序列计划已变化或执行单元已被编辑，请重新生成并核对计划")
		}
	}
	return nil
}
