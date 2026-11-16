package handler

import (
	"encoding/json"
	"fmt"

	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/repair"
	"dbgold/store"
	"gorm.io/gorm"
)

func inheritPGOracleSequenceIdentity(object *repair.Object, plan pgoracle.SequencePlan) (bool, error) {
	p := object.PGOracleSequence
	if p == nil || p.Schema != plan.Schema || p.Name != plan.Name {
		return false, nil
	}
	if plan.Identity == nil {
		return false, fmt.Errorf("序列 %s 缺少待传播身份", plan.Name)
	}
	a, b := *p, plan
	a.Identity, b.Identity = nil, nil
	if repair.Hash(a) != repair.Hash(b) {
		return false, fmt.Errorf("序列 %s 的关联冻结方案不一致，拒绝传播身份", plan.Name)
	}
	if p.Identity != nil {
		if *p.Identity != *plan.Identity {
			return false, fmt.Errorf("序列 %s 的关联对象已有冲突身份", plan.Name)
		}
		return false, nil
	}
	copy := *p
	identity := *plan.Identity
	copy.Identity = &identity
	object.PGOracleSequence = &copy
	return true, nil
}

// Run inside the same transaction that persists the successful CREATE unit.
// Trigger JSON records are independent and do not inherit a Go pointer update.
func persistPGOracleSequenceIdentity(tx *gorm.DB, jobID string, object repair.Object) error {
	if object.Kind != "sequence" || object.PGOracleSequence == nil || object.PGOracleSequence.Identity == nil || !object.AllUnitsSucceeded() {
		return nil
	}
	if !object.TrustedPGOracleSequence() {
		return fmt.Errorf("成功序列的冻结方案不可验证，拒绝传播身份")
	}
	var rows []store.MigrationRepairObject
	if err := tx.Where("job_id = ? AND object_id <> ?", jobID, object.ID).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		var related repair.Object
		if err := json.Unmarshal([]byte(row.ObjectJSON), &related); err != nil {
			return fmt.Errorf("读取关联修复对象身份: %w", err)
		}
		changed, err := inheritPGOracleSequenceIdentity(&related, *object.PGOracleSequence)
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		result := tx.Model(&store.MigrationRepairObject{}).Where("id = ? AND version = ?", row.ID, row.Version).Updates(map[string]any{"object_json": repairJSON(related), "version": gorm.Expr("version + 1")})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return store.ErrRepairConflict
		}
	}
	return nil
}
