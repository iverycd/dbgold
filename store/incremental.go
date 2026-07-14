package store

import (
	"time"

	"gorm.io/gorm"
)

// IncrementalMigrationJob stores CDC configuration and operational state. The
// authoritative apply checkpoint lives in the target PostgreSQL transaction.
type IncrementalMigrationJob struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	OwnerID        uint       `gorm:"index;not null" json:"owner_id"`
	JobID          string     `gorm:"uniqueIndex;not null" json:"job_id"`
	SrcConnID      uint       `json:"src_conn_id"`
	DstConnID      uint       `json:"dst_conn_id"`
	SrcDatabase    string     `json:"src_database"`
	TargetSchema   string     `json:"target_schema"`
	StartMode      string     `json:"start_mode"`    // full_then_cdc | incremental_only
	PositionMode   string     `json:"position_mode"` // auto | gtid | file
	StartGTID      string     `json:"start_gtid"`
	StartFile      string     `json:"start_file"`
	StartPosition  uint32     `json:"start_position"`
	ServerID       uint32     `json:"server_id"`
	MigrateMode    string     `json:"migrate_mode"`
	TableFilter    string     `json:"table_filter"`
	LowerCaseNames bool       `json:"lower_case_names"`
	Status         string     `gorm:"index" json:"status"`
	Phase          string     `json:"phase"`
	Summary        string     `json:"summary"`
	LastError      string     `json:"last_error"`
	BlockingDDL    string     `json:"blocking_ddl"`
	BlockingFile   string     `json:"blocking_file"`
	BlockingPos    uint32     `json:"blocking_position"`
	BlockingGTID   string     `json:"blocking_gtid"`
	CheckpointGTID string     `json:"checkpoint_gtid"`
	CheckpointFile string     `json:"checkpoint_file"`
	CheckpointPos  uint32     `json:"checkpoint_position"`
	InsertCount    int64      `json:"insert_count"`
	UpdateCount    int64      `json:"update_count"`
	DeleteCount    int64      `json:"delete_count"`
	SkippedCount   int64      `json:"skipped_count"`
	WarningCount   int64      `json:"warning_count"`
	LastEventAt    *time.Time `json:"last_event_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

func CreateIncrementalJob(j *IncrementalMigrationJob) error { return DB.Create(j).Error }

func GetIncrementalJob(jobID string) (*IncrementalMigrationJob, error) {
	var j IncrementalMigrationJob
	if err := DB.Where("job_id = ?", jobID).First(&j).Error; err != nil {
		return nil, err
	}
	return &j, nil
}

func ListIncrementalJobs(ownerID uint, isAdmin bool) ([]IncrementalMigrationJob, error) {
	var jobs []IncrementalMigrationJob
	q := DB.Order("id desc")
	if !isAdmin {
		q = q.Where("owner_id = ?", ownerID)
	}
	return jobs, q.Find(&jobs).Error
}

func UpdateIncrementalJob(jobID string, fields map[string]any) error {
	return DB.Model(&IncrementalMigrationJob{}).Where("job_id = ?", jobID).Updates(fields).Error
}

// PauseInterruptedIncrementalJobs enforces manual resume after process restart.
func PauseInterruptedIncrementalJobs() error {
	return DB.Model(&IncrementalMigrationJob{}).
		Where("status IN ?", []string{"initializing", "snapshot", "catching_up", "running", "reconnecting", "pausing"}).
		Updates(map[string]any{
			"status": "paused_restart", "phase": "paused",
			"summary": "服务已重启，请手工恢复任务",
		}).Error
}

func IsNotFound(err error) bool { return err == gorm.ErrRecordNotFound }
