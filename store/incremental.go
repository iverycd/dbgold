package store

import (
	"time"

	"gorm.io/gorm"
)

// IncrementalMigrationJob stores CDC configuration and operational state. The
// authoritative apply checkpoint lives in the target PostgreSQL transaction.
type IncrementalMigrationJob struct {
	ID                     uint       `gorm:"primaryKey" json:"id"`
	OwnerID                uint       `gorm:"index;not null" json:"owner_id"`
	JobID                  string     `gorm:"uniqueIndex;not null" json:"job_id"`
	SrcConnID              uint       `json:"src_conn_id"`
	DstConnID              uint       `json:"dst_conn_id"`
	SrcDatabase            string     `json:"src_database"`
	TargetSchema           string     `json:"target_schema"`
	StartMode              string     `json:"start_mode"`    // full_then_cdc | incremental_only
	PositionMode           string     `json:"position_mode"` // auto | gtid | file
	StartGTID              string     `gorm:"column:start_gtid" json:"start_gtid"`
	StartFile              string     `json:"start_file"`
	StartPosition          uint32     `json:"start_position"`
	ServerID               uint32     `json:"server_id"`
	MigrateMode            string     `json:"migrate_mode"`
	TableFilter            string     `json:"table_filter"`
	LowerCaseNames         bool       `json:"lower_case_names"`
	BootstrapPolicy        string     `gorm:"column:bootstrap_failure_policy" json:"bootstrap_failure_policy"`
	KeylessChangePolicy    string     `gorm:"column:keyless_change_policy;not null;default:'full_row_match'" json:"keyless_change_policy"`
	LocatorStrategyVersion int        `gorm:"column:locator_strategy_version;not null;default:0" json:"locator_strategy_version"`
	LocatorStrategiesJSON  string     `gorm:"column:locator_strategies_json" json:"-"`
	PrimaryLocatorCount    int        `gorm:"column:primary_locator_count;not null;default:0" json:"primary_locator_count"`
	UniqueLocatorCount     int        `gorm:"column:unique_locator_count;not null;default:0" json:"unique_locator_count"`
	FullRowLocatorCount    int        `gorm:"column:full_row_locator_count;not null;default:0" json:"full_row_locator_count"`
	BootstrapState         string     `json:"bootstrap_state"`
	BootstrapDone          bool       `gorm:"column:bootstrap_completed" json:"bootstrap_completed"`
	PendingGTID            string     `gorm:"column:pending_gtid" json:"pending_gtid"`
	PendingFile            string     `json:"pending_file"`
	PendingPos             uint32     `gorm:"column:pending_position" json:"pending_position"`
	EffectiveCount         int        `gorm:"column:effective_table_count" json:"effective_table_count"`
	ExcludedCount          int        `gorm:"column:excluded_table_count" json:"excluded_table_count"`
	ManifestHash           string     `gorm:"column:bootstrap_manifest_hash" json:"bootstrap_manifest_hash"`
	EffectiveJSON          string     `gorm:"column:effective_tables_json" json:"-"`
	ExcludedJSON           string     `gorm:"column:excluded_tables_json" json:"-"`
	BootstrapReport        string     `gorm:"column:bootstrap_report_json" json:"-"`
	FailedObjectCount      int        `gorm:"column:failed_object_count;not null;default:0" json:"failed_object_count"`
	FailedDDLCount         int        `gorm:"column:failed_ddl_count;not null;default:0" json:"failed_ddl_count"`
	Status                 string     `gorm:"index" json:"status"`
	Phase                  string     `json:"phase"`
	Summary                string     `json:"summary"`
	LastError              string     `json:"last_error"`
	BlockingDDL            string     `json:"blocking_ddl"`
	BlockingFile           string     `json:"blocking_file"`
	BlockingPos            uint32     `gorm:"column:blocking_position" json:"blocking_position"`
	BlockingGTID           string     `gorm:"column:blocking_gtid" json:"blocking_gtid"`
	ConflictTable          string     `json:"conflict_table"`
	ConflictAction         string     `json:"conflict_action"`
	ConflictFile           string     `json:"conflict_file"`
	ConflictPos            uint32     `gorm:"column:conflict_position" json:"conflict_position"`
	ConflictGTID           string     `gorm:"column:conflict_gtid" json:"conflict_gtid"`
	ConflictError          string     `json:"conflict_error"`
	ConflictBeforeHash     string     `json:"conflict_before_hash"`
	CheckpointGTID         string     `gorm:"column:checkpoint_gtid" json:"checkpoint_gtid"`
	CheckpointFile         string     `json:"checkpoint_file"`
	CheckpointPos          uint32     `gorm:"column:checkpoint_position" json:"checkpoint_position"`
	SourceHeadGTID         string     `gorm:"column:source_head_gtid" json:"source_head_gtid"`
	SourceHeadFile         string     `json:"source_head_file"`
	SourceHeadPos          uint32     `gorm:"column:source_head_position" json:"source_head_position"`
	CaughtUp               bool       `json:"caught_up"`
	LagSeconds             int64      `json:"lag_seconds"`
	CutoverGTID            string     `gorm:"column:cutover_gtid" json:"cutover_gtid"`
	CutoverFile            string     `json:"cutover_file"`
	CutoverPos             uint32     `gorm:"column:cutover_position" json:"cutover_position"`
	ValidationState        string     `json:"validation_state"`
	ValidationJSON         string     `json:"validation_json"`
	InsertCount            int64      `json:"insert_count"`
	UpdateCount            int64      `json:"update_count"`
	DeleteCount            int64      `json:"delete_count"`
	SkippedCount           int64      `json:"skipped_count"`
	WarningCount           int64      `json:"warning_count"`
	LogDroppedCount        int64      `gorm:"column:log_dropped_count;not null;default:0" json:"log_dropped_count"`
	LastEventAt            *time.Time `json:"last_event_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	FinishedAt             *time.Time `json:"finished_at,omitempty"`
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

func HasOpenIncrementalTarget(dstConnID uint, targetSchema string) (bool, error) {
	var count int64
	err := DB.Model(&IncrementalMigrationJob{}).
		Where("dst_conn_id = ? AND target_schema = ? AND status NOT IN ?", dstConnID, targetSchema, []string{"stopped", "aborted"}).
		Count(&count).Error
	return count > 0, err
}

func UpdateIncrementalJob(jobID string, fields map[string]any) error {
	return DB.Model(&IncrementalMigrationJob{}).Where("job_id = ?", jobID).Updates(fields).Error
}

// UpdateIncrementalJobIfStatus performs a compare-and-set state transition.
// It is used where a runner completion can race with an operator action.
func UpdateIncrementalJobIfStatus(jobID string, statuses []string, fields map[string]any) (bool, error) {
	result := DB.Model(&IncrementalMigrationJob{}).
		Where("job_id = ? AND status IN ?", jobID, statuses).
		Updates(fields)
	return result.RowsAffected == 1, result.Error
}

// PauseInterruptedIncrementalJobs enforces manual resume after process restart.
func PauseInterruptedIncrementalJobs() error {
	interruptedStatuses := []string{"initializing", "snapshot", "catching_up", "running", "reconnecting", "pausing", "cutting_over", "validating"}
	var interruptedSnapshots []IncrementalMigrationJob
	if err := DB.Select("job_id").
		Where("status IN ? AND start_mode = ? AND bootstrap_completed = ? AND locator_strategy_version = ?", interruptedStatuses, "full_then_cdc", false, 1).
		Find(&interruptedSnapshots).Error; err != nil {
		return err
	}
	if err := DB.Model(&IncrementalMigrationJob{}).
		Where("status IN ? AND locator_strategy_version = ?", interruptedStatuses, 1).
		Updates(map[string]any{
			"status": "paused_restart", "phase": "paused",
			"summary": "服务已重启，请手工恢复任务",
		}).Error; err != nil {
		return err
	}
	if len(interruptedSnapshots) == 0 {
		return nil
	}
	now := time.Now()
	logs := make([]IncrementalMigrationLog, 0, len(interruptedSnapshots))
	for _, job := range interruptedSnapshots {
		logs = append(logs, IncrementalMigrationLog{
			JobID: job.JobID, Phase: "snapshot_init", Level: "warn",
			Line:      now.Format("15:04:05.000") + " [WARN] 全量快照被服务重启中断，请检查目标 checkpoint 后手工恢复",
			CreatedAt: now,
		})
	}
	if err := AppendIncrementalMigrationLogs(logs); err != nil {
		// Restart safety is authoritative; observability remains best-effort.
		for _, job := range interruptedSnapshots {
			_ = AddIncrementalLogDroppedCount(job.JobID, 1)
		}
	}
	return nil
}

// DiscardLegacyIncrementalJobs prevents tasks created before locator
// strategies were frozen from resuming with changed row-addressing semantics.
func DiscardLegacyIncrementalJobs() error {
	now := time.Now()
	return DB.Model(&IncrementalMigrationJob{}).
		Where("status NOT IN ? AND locator_strategy_version <> ?", []string{"stopped", "aborted"}, 1).
		Updates(map[string]any{
			"status": "aborted", "phase": "aborted", "finished_at": &now,
			"summary":    "CDC定位策略已升级，旧任务不能恢复，请重新执行全量快照",
			"last_error": "CDC定位策略已升级，旧任务不能恢复，请重新执行全量快照",
		}).Error
}

func IsNotFound(err error) bool { return err == gorm.ErrRecordNotFound }
