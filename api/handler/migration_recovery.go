package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/middleware"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func recoveryConfig(b *store.MigrationRepairBinding) (datamigrate.Config, error) {
	var cfg datamigrate.Config
	if !b.Configured || b.ConfigJSON == "" {
		return cfg, fmt.Errorf("旧任务缺少迁移配置，请显式补齐后预检")
	}
	if e := json.Unmarshal([]byte(b.ConfigJSON), &cfg); e != nil {
		return cfg, e
	}
	cfg.TargetSchema = b.TargetSchema
	cfg.CaptureRepair = false
	cfg.RepairSink = nil
	cfg.RepairObjectSink = nil
	cfg.IntraTableParallel = 1
	return cfg, nil
}
func openRecovery(src, dst *store.Connection, b *store.MigrationRepairBinding) (source.Reader, target.Writer, error) {
	r, e := buildSrcReader(src, sourceOverride(b, src.DBType), source.ConnPoolConfig{}, dst.DBType)
	if e != nil {
		return nil, nil, e
	}
	w, e := buildDstWriter(dst, b.TargetSchema, target.ConnPoolConfig{}, src.DBType)
	if e != nil {
		r.Close()
		return nil, nil, e
	}
	if setter, ok := w.(interface{ SetSourceType(string) }); ok {
		setter.SetSourceType(src.DBType)
	}
	return r, w, nil
}
func recoveryRelated(jobID string, o repair.Object, ids []string) ([]repair.Object, map[string]int, error) {
	objects, rows, e := repairObjects(jobID)
	if e != nil {
		return nil, nil, e
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	versions := map[string]int{}
	related := []repair.Object{}
	for i, item := range objects {
		if item.ID == o.ID {
			continue
		}
		eligible := item.Table == o.TargetName || item.Table == o.Name
		for _, dep := range item.Dependencies {
			if dep == o.ID {
				eligible = true
			}
		}
		if ids != nil {
			if want[item.ID] && !eligible {
				return nil, nil, fmt.Errorf("不能选择本表范围外的关联对象")
			}
			eligible = want[item.ID]
		}
		if !eligible {
			continue
		}
		if item.Kind == "table" || item.Kind == "data" {
			if ids != nil && want[item.ID] {
				return nil, nil, fmt.Errorf("接续只能选本表关联的附属对象")
			}
			continue
		}
		if item.Status == "succeeded" {
			continue
		}
		if err := recoveryRelatedReady(item); err != nil {
			return nil, nil, err
		}
		related = append(related, item)
		versions[item.ID] = rows[i].Version
		delete(want, item.ID)
	}
	if len(want) > 0 {
		return nil, nil, fmt.Errorf("所选对象不属于此任务或已成功")
	}
	return related, versions, nil
}
func PreviewMigrationRecovery(c *gin.Context) {
	j, b, row, o, ok := loadRepairObject(c, true)
	if !ok {
		return
	}
	if o.Kind != "table" && o.Kind != "data" {
		repairError(c, fmt.Errorf("只有表对象支持数据接续"))
		return
	}
	var req struct {
		Version          int               `json:"version"`
		ColumnMapping    map[string]string `json:"column_mapping"`
		RelatedObjectIDs []string          `json:"related_object_ids"`
	}
	if e := c.ShouldBindJSON(&req); e != nil {
		repairError(c, e)
		return
	}
	if req.Version != row.Version {
		repairError(c, store.ErrRepairConflict)
		return
	}
	if o.Kind == "table" && o.Status != "succeeded" {
		repairError(c, fmt.Errorf("请先成功执行表结构修复 DDL"))
		return
	}
	cfg, e := recoveryConfig(b)
	if e != nil {
		repairError(c, e)
		return
	}
	src, dst, e := resolveRepairConnections(c, j, b, true)
	if e != nil {
		repairError(c, e)
		return
	}
	related, versions, e := recoveryRelated(j.JobID, o, req.RelatedObjectIDs)
	if e != nil {
		repairError(c, e)
		return
	}
	mapping := o.ColumnMapping
	if req.ColumnMapping != nil {
		mapping = req.ColumnMapping
	}
	previewObject, e := recoveryPreviewObject(j.JobID, o)
	if e != nil {
		repairError(c, e)
		return
	}
	r, w, e := openRecovery(src, dst, b)
	if e != nil {
		repairError(c, e)
		return
	}
	defer r.Close()
	defer w.Close()
	p, e := datamigrate.PreviewRecovery(c.Request.Context(), r, w, cfg, previewObject, related, mapping)
	if e != nil {
		repairError(c, e)
		return
	} // Regenerated candidates become versioned drafts; execution requires a later explicit review.
	e = store.DB.Transaction(func(tx *gorm.DB) error {
		var active int64
		if err := tx.Model(&store.MigrationRepairAttempt{}).Where("job_id = ? AND status = ?", j.JobID, "running").Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return store.ErrRepairConflict
		}
		var current store.MigrationRepairObject
		if err := tx.First(&current, "job_id = ? AND object_id = ?", j.JobID, o.ID).Error; err != nil {
			return err
		}
		if current.Version != row.Version {
			return store.ErrRepairConflict
		}
		for _, candidate := range p.Related {
			var old repair.Object
			var rr store.MigrationRepairObject
			loadErr := tx.First(&rr, "job_id = ? AND object_id = ?", j.JobID, candidate.ID).Error
			if loadErr != nil {
				return loadErr
			}
			if rr.Version != versions[candidate.ID] {
				return store.ErrRepairConflict
			}
			_ = json.Unmarshal([]byte(rr.ObjectJSON), &old)
			if repairJSON(candidate) != repairJSON(old) {
				r := tx.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ? AND version = ?", j.JobID, candidate.ID, rr.Version).Updates(map[string]any{"object_json": repairJSON(candidate), "version": rr.Version + 1})
				if r.Error != nil {
					return r.Error
				}
				if r.RowsAffected != 1 {
					return store.ErrRepairConflict
				}
				versions[candidate.ID]++
			}
		}
		return nil
	})
	if e != nil {
		repairError(c, e)
		return
	}
	saved := store.MigrationRecoveryPreview{Token: uuid.NewString(), JobID: j.JobID, ObjectID: o.ID, ObjectVersion: row.Version, BindingVersion: b.Version, PreviewJSON: repairJSON(p), RelatedVersionsJSON: repairJSON(versions), ExpiresAt: time.Now().Add(15 * time.Minute)}
	if e = store.DB.Create(&saved).Error; e != nil {
		repairError(c, e)
		return
	}
	c.JSON(200, gin.H{"token": saved.Token, "preview": p})
}
func ExecuteMigrationRecovery(c *gin.Context) {
	j, b, row, o, ok := loadRepairObject(c, true)
	if !ok {
		return
	}
	var req struct {
		Token            string `json:"token"`
		RequestID        string `json:"request_id"`
		Confirmed        bool   `json:"confirmed"`
		SourceQuiesced   bool   `json:"source_quiesced"`
		TargetQuiesced   bool   `json:"target_quiesced"`
		Truncate         bool   `json:"truncate"`
		ConfirmationText string `json:"confirmation_text"`
	}
	if e := c.ShouldBindJSON(&req); e != nil {
		repairError(c, e)
		return
	}
	hash := repair.Hash(req)
	if req.RequestID == "" || !req.Confirmed || !req.SourceQuiesced || !req.TargetQuiesced {
		repairError(c, fmt.Errorf("请确认接续范围并保持源表、目标表停写"))
		return
	}
	if existingRepairRequest(c, j.JobID, req.RequestID, hash) {
		return
	}
	var saved store.MigrationRecoveryPreview
	if e := store.DB.First(&saved, "token = ? AND job_id = ? AND object_id = ?", req.Token, j.JobID, o.ID).Error; e != nil {
		repairError(c, fmt.Errorf("预检不存在，请重新预检"))
		return
	}
	if time.Now().After(saved.ExpiresAt) || saved.ObjectVersion != row.Version || saved.BindingVersion != b.Version {
		repairError(c, fmt.Errorf("预检已过期或草稿已变化，请重新预检"))
		return
	}
	var p repair.RecoveryPreview
	var versions map[string]int
	if e := json.Unmarshal([]byte(saved.PreviewJSON), &p); e != nil {
		repairError(c, e)
		return
	}
	_ = json.Unmarshal([]byte(saved.RelatedVersionsJSON), &versions)
	if req.Truncate && req.ConfirmationText != p.QualifiedTarget {
		repairError(c, fmt.Errorf("请输入目标完整名称 %s 以确认仅清空本表", p.QualifiedTarget))
		return
	}
	if p.DataRequired && p.TargetRows > 0 && !req.Truncate {
		repairError(c, fmt.Errorf("目标非空，必须单独确认清空本表后完整重导"))
		return
	}
	for _, related := range p.Related {
		rr, e := store.GetMigrationRepairObject(j.JobID, related.ID)
		if e != nil || versions[related.ID] != rr.Version {
			repairError(c, store.ErrRepairConflict)
			return
		}
		if related.RequiresReview {
			repairError(c, fmt.Errorf("关联对象 %s 的候选 SQL 需先人工审阅并保存", related.Name))
			return
		}
		if e = recoveryRelatedReady(related); e != nil {
			repairError(c, e)
			return
		}
		if scopeErr := validateRepairObjectScope(related, related.Units, j.DstDBType); scopeErr != nil {
			repairError(c, scopeErr)
			return
		}
		if related.Native == nil {
			if e = validateRepairUnits(related.Units); e != nil {
				repairError(c, fmt.Errorf("关联对象 %s: %w", related.Name, e))
				return
			}
		}
	}
	cfg, e := recoveryConfig(b)
	if e != nil {
		repairError(c, e)
		return
	}
	src, dst, e := resolveRepairConnections(c, j, b, true)
	if e != nil {
		repairError(c, e)
		return
	}
	a := &store.MigrationRepairAttempt{ID: uuid.NewString(), JobID: j.JobID, RequestID: req.RequestID, RequestHash: hash, ObjectID: o.ID, OwnerID: middleware.GetCurrentUserID(c), Kind: "recovery", Status: "running", Scope: migrationScope(dst, b.TargetSchema), SnapshotJSON: repairJSON(gin.H{"object": o, "binding": b, "preview": p, "truncate": req.Truncate}), UnitsJSON: "[]"}
	claimed, newAttempt, e := store.ClaimRepairAttempt(a, row.Version, b.Version, versions)
	if e != nil {
		repairError(c, e)
		return
	}
	if !newAttempt {
		c.JSON(202, attemptResponse(claimed))
		return
	}
	response := attemptResponse(a)
	startRepairWorker(a, func(ctx context.Context) {
		r, w, e := openRecovery(src, dst, b)
		if e != nil {
			finishRepair(a, o, e)
			return
		}
		defer r.Close()
		defer w.Close()
		if linkReader, ok := r.(damengnative.LinkDDLReader); ok {
			ctx = damengnative.WithSourceLinkResolver(ctx, linkReader, b.SourceSchema)
		}
		previewObject, e := recoveryPreviewObject(j.JobID, o)
		if e != nil {
			finishRepair(a, o, e)
			return
		}
		fresh, e := datamigrate.PreviewRecovery(ctx, r, w, cfg, previewObject, p.Related, p.ColumnMapping)
		if e != nil {
			finishRepair(a, o, e)
			return
		}
		if fresh.Fingerprint != p.Fingerprint {
			finishRepair(a, o, fmt.Errorf("执行前源/目标结构或数据已变化，请重新预检"))
			return
		}
		var executedUnits []repair.Unit
		observedContext := repair.WithObserver(ctx, recoveryAttemptObserver(a, &executedUnits))
		result, e := datamigrate.RunRecovery(observedContext, r, w, cfg, fresh, req.Truncate, func(progress repair.RecoveryResult) error {
			a.ResultJSON = repairJSON(progress)
			return store.DB.Model(&store.MigrationRepairAttempt{}).Where("id = ? AND status = ?", a.ID, "running").Update("result_json", a.ResultJSON).Error
		})
		a.ResultJSON = repairJSON(result)
		if result.DataStatus == "succeeded" {
			o.DataStatus = "succeeded"
			if o.Kind == "data" {
				o.Status = "succeeded"
			}
		} else if e != nil {
			o.DataStatus = result.Status
		} else if !fresh.DataRequired && o.DataStatus != "succeeded" {
			o.DataStatus = "not_required"
		}
		if e == nil || result.DataStatus == "succeeded" {
			o.ColumnMapping = fresh.ColumnMapping
		}
		syncNativeRecoveryUnits(&o, executedUnits)
		if syncErr := persistRecoveryData(j.JobID, o); syncErr != nil {
			a.Status = "unknown"
			finishRepair(a, o, syncErr)
			return
		}
		if e != nil {
			if result.Status == "unknown" {
				a.Status = "unknown"
			}
			finishRepair(a, o, e)
			return
		}
		completed := map[string]bool{o.ID: true}
		all, _, loadErr := repairObjects(j.JobID)
		if loadErr != nil {
			finishRepair(a, o, loadErr)
			return
		}
		for _, item := range all {
			if item.Status == "succeeded" {
				completed[item.ID] = true
			}
		}
		nativeRecoveryCompleteManaged(all, completed)
		var firstErr error
		sequenceColumns := map[string]bool{}
		orderedRelated := orderedRecoveryObjects(fresh.Related)
		for position, item := range orderedRelated {
			if ctx.Err() != nil {
				firstErr = ctx.Err()
				break
			}
			blocked := false
			for _, dep := range item.Dependencies {
				if !completed[dep] {
					blocked = true
				}
			}
			if blocked {
				item.Status = "blocked"
				item.Error = "依赖对象尚未成功"
				if firstErr == nil {
					firstErr = fmt.Errorf("关联对象 %s 的依赖尚未完成", item.Name)
				}
			} else {
				selectedUnits := nativeRecoveryPhaseUnits(item, "install")
				units, executeErr := item.Units, error(nil)
				if item.Native == nil || nativeRecoveryHasSelected(selectedUnits) {
					units, executeErr = target.ExecuteRepair(ctx, w, item, selectedUnits, func(units []repair.Unit) error {
						return persistRepairProgress(a, item, nativeRecoveryMergeSelections(item, units), executedUnits)
					})
				}
				units = nativeRecoveryMergeSelections(item, units)
				for i := range units {
					if units[i].Status == "succeeded" {
						units[i].Selected = false
					}
				}
				item.Units = units
				executedUnits = append(executedUnits, units...)
				if executeErr != nil {
					item.Status = "failed"
					for _, unit := range units {
						if unit.Status == "unknown" || unit.Status == "running" {
							item.Status = "unknown"
						}
					}
					item.Error = executeErr.Error()
					if firstErr == nil {
						firstErr = executeErr
					}
				} else {
					item.Status = "succeeded"
					if item.Native != nil && !item.AllUnitsSucceeded() {
						item.Status = "pending"
					}
					item.Verification = nil
					item.ManualVerification = nil
					completed[item.ID] = nativeRecoveryInstallationComplete(item)
					if completed[item.ID] {
						nativeRecoveryCompleteManaged(all, completed)
					}
					if completed[item.ID] && item.Kind == "sequence" && item.Sequence != nil {
						sequenceColumns[item.Sequence.ColumnName] = true
						if item.Sequence.Native != nil {
							for _, b := range item.Sequence.Native.Bindings {
								if b.TableName == fresh.TargetTable {
									sequenceColumns[b.ColumnName] = true
								}
							}
						}
					}
				}
			}
			orderedRelated[position] = item
			if e = store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", j.JobID, item.ID).Updates(map[string]any{"object_json": repairJSON(item), "version": gorm.Expr("version + 1")}).Error; e != nil {
				a.Status = "unknown"
				firstErr = e
				break
			}
		}
		if ctx.Err() == nil && cfg.Content != "schema_only" && len(sequenceColumns) > 0 {
			sequences, calibrationErr := datamigrate.RecoverySequences(ctx, r, w, cfg, fresh)
			if calibrationErr == nil {
				sequences = recoveryCalibrationScope(sequences, sequenceColumns)
				calibrationErr = target.CalibrateRecovery(observedContext, w, fresh.TargetTable, sequences)
			}
			if calibrationErr != nil && firstErr == nil {
				firstErr = fmt.Errorf("数据已完成，最终自增校准失败: %w", calibrationErr)
			}
		}
		if firstErr == nil && src.DBType == "dameng" && dst.DBType == "dameng" {
			var pin func(context.Context, func(context.Context) error) error
			if finalizer, ok := w.(interface {
				WithDamengNativeEventFinalization(context.Context, func(context.Context) error) error
			}); ok {
				pin = finalizer.WithDamengNativeEventFinalization
			}
			_, firstErr = executeNativeRecoveryFinalStates(ctx, orderedRelated, nativeRecoveryFinalization{
				execute: func(run context.Context, item repair.Object, units []repair.Unit, step func([]repair.Unit) error) ([]repair.Unit, error) {
					return target.ExecuteRepair(run, w, item, units, step)
				},
				proof: func(proof context.Context) error {
					objects, _, err := repairObjects(j.JobID)
					if err != nil {
						return err
					}
					if err = datamigrate.NativeRepairFinalizationReady(objects, ""); err != nil {
						return err
					}
					return datamigrate.VerifyNativeRepairFinalization(proof, r, w, cfg, objects)
				},
				pinEvents: pin,
				progress: func(item repair.Object, units []repair.Unit) error {
					return persistRepairProgress(a, item, units, executedUnits)
				},
				save: func(item repair.Object) error {
					executedUnits = append(executedUnits, item.Units...)
					return store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", j.JobID, item.ID).Updates(map[string]any{"object_json": repairJSON(item), "version": gorm.Expr("version + 1")}).Error
				},
			})
		}
		// The table remains repaired even when an independent attached object fails.
		finishRepair(a, o, firstErr)
	})
	c.JSON(202, response)
}

func recoveryRelatedReady(o repair.Object) error {
	if o.Status == "unknown" || o.Status == "running" || o.Status == "unverified" {
		return fmt.Errorf("关联对象 %s 执行结果未知或仍在执行，请取消选择该对象", o.Name)
	}
	for _, u := range o.Units {
		if u.Status == "unknown" || u.Status == "running" {
			return fmt.Errorf("关联对象 %s 存在执行结果未知的单元，禁止自动重放", o.Name)
		}
	}
	return nil
}

func recoveryCalibrationScope(sequences []source.SequenceInfo, selectedColumns map[string]bool) []source.SequenceInfo {
	var out []source.SequenceInfo
	for _, s := range sequences {
		if s.ColumnName != "" && selectedColumns[s.ColumnName] {
			out = append(out, s)
		}
	}
	return out
}

// Record the actual TRUNCATE/calibration SQL before mutation and each known
// result. This is attempt audit, separate from immutable object draft units.
func recoveryAttemptObserver(a *store.MigrationRepairAttempt, units *[]repair.Unit) func(repair.Event) error {
	return func(event repair.Event) error {
		index := -1
		if event.Status != "running" {
			for i := len(*units) - 1; i >= 0; i-- {
				if (*units)[i].SQL == event.SQL && (*units)[i].Status == "running" {
					index = i
					break
				}
			}
		}
		if index < 0 {
			*units = append(*units, repair.Unit{ID: "recovery:" + uuid.NewString(), SQL: event.SQL, OriginalSQL: event.SQL})
			index = len(*units) - 1
		}
		(*units)[index].Status = event.Status
		(*units)[index].Error = event.Error
		a.UnitsJSON = repairJSON(*units)
		result := store.DB.Model(&store.MigrationRepairAttempt{}).Where("id = ? AND status = ?", a.ID, "running").Update("units_json", a.UnitsJSON)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return store.ErrRepairConflict
		}
		return nil
	}
}

func persistRecoveryData(jobID string, o repair.Object) error {
	return store.DB.Transaction(func(tx *gorm.DB) error {
		var rows []store.MigrationRepairObject
		if e := tx.Where("job_id = ?", jobID).Find(&rows).Error; e != nil {
			return e
		}
		for _, row := range rows {
			var item repair.Object
			if e := json.Unmarshal([]byte(row.ObjectJSON), &item); e != nil {
				return e
			}
			if item.ID == o.ID {
				item = o
			} else {
				matched := item.Table == o.Table && item.Table != "" || item.SourceTable != "" && item.SourceTable == o.SourceTable
				if !matched || o.DataStatus != "succeeded" || (item.Kind != "table" && item.Kind != "data") {
					continue
				}
				item.DataStatus = "succeeded"
				item.ColumnMapping = o.ColumnMapping
				if item.Kind == "data" {
					item.Status = "succeeded"
				}
			}
			if e := tx.Model(&row).Updates(map[string]any{"object_json": repairJSON(item), "version": gorm.Expr("version + 1")}).Error; e != nil {
				return e
			}
		}
		return nil
	})
}

func orderedRecoveryObjects(items []repair.Object) []repair.Object {
	byID := map[string]repair.Object{}
	for _, o := range items {
		byID[o.ID] = o
	}
	seen := map[string]bool{}
	out := make([]repair.Object, 0, len(items))
	var visit func(repair.Object)
	visit = func(o repair.Object) {
		if seen[o.ID] {
			return
		}
		seen[o.ID] = true
		for _, dep := range o.Dependencies {
			if parent, ok := byID[dep]; ok {
				visit(parent)
			}
		}
		out = append(out, o)
	}
	for _, o := range items {
		visit(o)
	}
	return out
}

// A recovery attempt records physical native state changes under the same SQL
// identity as the original plan. Preserve that evidence before related retries.
func syncNativeRecoveryUnits(object *repair.Object, executed []repair.Unit) {
	if object.Native == nil {
		return
	}
	for _, observed := range executed {
		if observed.Status != "succeeded" {
			continue
		}
		for i := range object.Units {
			unit := &object.Units[i]
			if unit.SQL == observed.SQL && unit.SQL == unit.OriginalSQL {
				unit.Status, unit.Error, unit.Selected = "succeeded", "", false
			}
		}
	}
	if object.AllUnitsSucceeded() && (object.DataStatus == "succeeded" || object.DataStatus == "not_required") {
		object.Status, object.Error = "succeeded", ""
	}
}
