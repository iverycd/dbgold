package store

import (
	"dbgold/datamigrate/repair"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrRepairConflict = errors.New("修复记录已变化或目标正在执行其他任务，请刷新后重试")

type MigrationRepairObject struct {
	ID         uint   `gorm:"primaryKey"`
	JobID      string `gorm:"uniqueIndex:repair_object;not null"`
	ObjectID   string `gorm:"uniqueIndex:repair_object;not null"`
	ObjectJSON string `gorm:"type:text;not null"`
	Version    int    `gorm:"not null;default:1"`
	UpdatedAt  time.Time
}

type MigrationRepairBinding struct {
	JobID             string `gorm:"primaryKey"`
	SrcConnID         uint
	DstConnID         uint
	SourceDatabase    string
	SourceSchema      string
	TargetSchema      string
	SourceFingerprint string
	TargetFingerprint string
	ConfigJSON        string `gorm:"type:text"`
	Configured        bool
	Legacy            bool
	Version           int `gorm:"not null;default:1"`
	UpdatedAt         time.Time
}

type MigrationRepairAttempt struct {
	ID               string     `gorm:"primaryKey" json:"id"`
	JobID            string     `gorm:"uniqueIndex:repair_request;index;not null" json:"job_id"`
	RequestID        string     `gorm:"uniqueIndex:repair_request;not null" json:"-"`
	RequestHash      string     `gorm:"not null" json:"-"`
	ObjectID         string     `gorm:"index;not null" json:"object_id"`
	OwnerID          uint       `json:"owner_id"`
	Kind             string     `json:"kind"`
	Status           string     `gorm:"index" json:"status"`
	Scope            string     `json:"-"`
	SnapshotJSON     string     `gorm:"type:text" json:"-"`
	UnitsJSON        string     `gorm:"type:text" json:"-"`
	ResultJSON       string     `gorm:"type:text" json:"-"`
	VerificationJSON string     `gorm:"type:text" json:"-"`
	Error            string     `gorm:"type:text" json:"error,omitempty"`
	CancelRequested  bool       `json:"cancel_requested"`
	CreatedAt        time.Time  `json:"created_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
}

type MigrationRecoveryPreview struct {
	Token               string `gorm:"primaryKey"`
	JobID               string `gorm:"index;not null"`
	ObjectID            string
	ObjectVersion       int
	BindingVersion      int
	PreviewJSON         string `gorm:"type:text"`
	RelatedVersionsJSON string `gorm:"type:text"`
	ExpiresAt           time.Time
}

type MigrationRepairLock struct {
	Scope     string `gorm:"primaryKey"`
	Owner     string `gorm:"index;not null"`
	CreatedAt time.Time
}

func SeedMigrationRepairs(binding *MigrationRepairBinding, objects []MigrationRepairObject) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(binding).Error; err != nil {
			return err
		}
		for i := range objects {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&objects[i]).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func GetMigrationRepairBinding(jobID string) (*MigrationRepairBinding, error) {
	var b MigrationRepairBinding
	err := DB.First(&b, "job_id = ?", jobID).Error
	return &b, err
}
func GetMigrationRepairObject(jobID, objectID string) (*MigrationRepairObject, error) {
	var o MigrationRepairObject
	err := DB.First(&o, "job_id = ? AND object_id = ?", jobID, objectID).Error
	return &o, err
}
func ListMigrationRepairObjects(jobID string) ([]MigrationRepairObject, error) {
	var objects []MigrationRepairObject
	err := DB.Where("job_id = ?", jobID).Order("id").Find(&objects).Error
	return objects, err
}
func SaveMigrationRepairObject(jobID, objectID string, version int, payload string) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&MigrationRepairAttempt{}).Where("job_id = ? AND status = ?", jobID, "running").Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrRepairConflict
		}
		r := tx.Model(&MigrationRepairObject{}).Where("job_id = ? AND object_id = ? AND version = ?", jobID, objectID, version).Updates(map[string]any{"object_json": payload, "version": version + 1, "updated_at": time.Now()})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return ErrRepairConflict
		}
		return nil
	})
}

func AcquireMigrationScope(scope, owner string) error {
	r := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&MigrationRepairLock{Scope: scope, Owner: owner})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrRepairConflict
	}
	return nil
}
func ReleaseMigrationScope(scope, owner string) error {
	return DB.Where("scope = ? AND owner = ?", scope, owner).Delete(&MigrationRepairLock{}).Error
}

// ClaimRepairAttempt atomically fences duplicate requests, stale drafts and target conflicts.
func ClaimRepairAttempt(a *MigrationRepairAttempt, version, bindingVersion int, relatedVersions ...map[string]int) (*MigrationRepairAttempt, bool, error) {
	var existing MigrationRepairAttempt
	claimed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		err := tx.First(&existing, "job_id = ? AND request_id = ?", a.JobID, a.RequestID).Error
		if err == nil {
			if existing.RequestHash != a.RequestHash {
				return ErrRepairConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var o MigrationRepairObject
		if err = tx.First(&o, "job_id = ? AND object_id = ?", a.JobID, a.ObjectID).Error; err != nil {
			return err
		}
		var b MigrationRepairBinding
		if err = tx.First(&b, "job_id = ?", a.JobID).Error; err != nil {
			return err
		}
		if o.Version != version || b.Version != bindingVersion {
			return ErrRepairConflict
		}
		for _, versions := range relatedVersions {
			for id, want := range versions {
				var related MigrationRepairObject
				if err := tx.First(&related, "job_id = ? AND object_id = ?", a.JobID, id).Error; err != nil {
					return err
				}
				if related.Version != want {
					return ErrRepairConflict
				}
			}
		}
		var count int64
		if err = tx.Model(&MigrationRepairAttempt{}).Where("job_id = ? AND status = ?", a.JobID, "running").Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrRepairConflict
		}
		r := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&MigrationRepairLock{Scope: a.Scope, Owner: a.ID})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return ErrRepairConflict
		}
		if err = tx.Create(a).Error; err != nil {
			return err
		}
		existing = *a
		claimed = true
		return nil
	})
	return &existing, claimed, err
}

func FinishRepairAttempt(a *MigrationRepairAttempt, objectPayload string) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&MigrationRepairAttempt{}).Where("id = ? AND status = ?", a.ID, "running").Updates(map[string]any{"status": a.Status, "error": a.Error, "units_json": a.UnitsJSON, "result_json": a.ResultJSON, "verification_json": a.VerificationJSON, "finished_at": a.FinishedAt}).Error; err != nil {
			return err
		}
		if objectPayload != "" {
			if err := tx.Model(&MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", a.JobID, a.ObjectID).Updates(map[string]any{"object_json": objectPayload, "version": gorm.Expr("version + 1"), "updated_at": time.Now()}).Error; err != nil {
				return err
			}
		}
		return tx.Where("scope = ? AND owner = ?", a.Scope, a.ID).Delete(&MigrationRepairLock{}).Error
	})
}

// A crash cannot prove whether the remote database committed. Never replay automatically.
func MarkInterruptedRepairsUnknown() error {
	return DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		var active []MigrationRepairAttempt
		completedData := map[string][]repair.Object{}
		if err := tx.Where("status = ?", "running").Find(&active).Error; err != nil {
			return err
		}
		for _, a := range active {
			var row MigrationRepairObject
			if err := tx.First(&row, "job_id = ? AND object_id = ?", a.JobID, a.ObjectID).Error; err != nil {
				return err
			}
			var obj repair.Object
			if err := json.Unmarshal([]byte(row.ObjectJSON), &obj); err != nil {
				return err
			}
			if a.Kind == "recovery" {
				var result repair.RecoveryResult
				_ = json.Unmarshal([]byte(a.ResultJSON), &result)
				if result.DataStatus == "succeeded" {
					obj.DataStatus = "succeeded"
					completedData[a.JobID] = append(completedData[a.JobID], obj)
					if obj.Kind == "data" {
						obj.Status = "succeeded"
					}
				} else {
					obj.DataStatus = "unknown"
				}
			} else {
				obj.Status = "unknown"
			}
			payload, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			if err = tx.Model(&row).Updates(map[string]any{"object_json": string(payload), "version": row.Version + 1}).Error; err != nil {
				return err
			}
		}
		var objects []MigrationRepairObject
		if err := tx.Find(&objects).Error; err != nil {
			return err
		}
		for _, row := range objects {
			var obj repair.Object
			if err := json.Unmarshal([]byte(row.ObjectJSON), &obj); err != nil {
				return err
			}
			changed := obj.NormalizeLegacyExecution()
			for _, done := range completedData[row.JobID] {
				if (obj.Kind == "table" || obj.Kind == "data") && (obj.Table != "" && obj.Table == done.Table || obj.SourceTable != "" && obj.SourceTable == done.SourceTable) {
					obj.DataStatus = "succeeded"
					if obj.Kind == "data" {
						obj.Status = "succeeded"
					}
					changed = true
				}
			}
			if obj.Status == "running" {
				obj.Status = "unknown"
				changed = true
			}
			if obj.DataStatus == "running" {
				obj.DataStatus = "unknown"
				changed = true
			}
			for i := range obj.Units {
				if obj.Units[i].Status == "running" {
					obj.Units[i].Status = "unknown"
					changed = true
				}
			}
			if changed {
				payload, err := json.Marshal(obj)
				if err != nil {
					return err
				}
				if err = tx.Model(&row).Updates(map[string]any{"object_json": string(payload), "version": row.Version + 1}).Error; err != nil {
					return err
				}
				if err = tx.Model(&DataMigrationJob{}).Where("job_id = ? AND status = ?", row.JobID, "running").Updates(map[string]any{"status": "failed", "summary": "服务重启，部分对象执行结果未知，禁止自动重放", "finished_at": &now}).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Model(&MigrationRepairAttempt{}).Where("status = ?", "running").Updates(map[string]any{"status": "unknown", "error": "服务重启，数据库执行结果未知，禁止自动重放", "finished_at": &now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&DataMigrationJob{}).Where("status = ?", "running").Updates(map[string]any{"status": "failed", "summary": "服务重启，原迁移已中断；请查看保留的对象执行状态", "finished_at": &now}).Error; err != nil {
			return err
		}
		return tx.Where("1 = 1").Delete(&MigrationRepairLock{}).Error
	})
}
