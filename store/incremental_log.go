package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

var ErrConflictingIncrementalLogCursors = errors.New("after_id and before_id cannot be used together")

// IncrementalMigrationLog is a durable journal entry emitted while an
// incremental job performs its initial full snapshot. Rows are deliberately
// independent from the CDC checkpoint: losing a log must never affect data
// migration correctness.
type IncrementalMigrationLog struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement;index:idx_incremental_log_job_cursor,priority:2" json:"id"`
	JobID     string    `gorm:"not null;index:idx_incremental_log_job_cursor,priority:1" json:"-"`
	Phase     string    `gorm:"not null" json:"phase"`
	Level     string    `gorm:"not null" json:"level"`
	Line      string    `gorm:"not null" json:"line"`
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
}

// IncrementalMigrationLogPage contains an ascending slice of journal entries
// and indicates whether entries exist on either side of that slice.
type IncrementalMigrationLogPage struct {
	Items    []IncrementalMigrationLog `json:"items"`
	OldestID uint64                    `json:"oldest_id"`
	NewestID uint64                    `json:"newest_id"`
	HasOlder bool                      `json:"has_older"`
	HasNewer bool                      `json:"has_newer"`
}

// AppendIncrementalMigrationLogs inserts a journal batch in one SQLite
// transaction. Callers should keep batches bounded so the observer does not
// hold SQLite's writer lock for an extended period.
func AppendIncrementalMigrationLogs(logs []IncrementalMigrationLog) error {
	if len(logs) == 0 {
		return nil
	}
	return DB.Create(&logs).Error
}

// ListIncrementalMigrationLogs reads one cursor page. afterID and beforeID are
// mutually exclusive. With neither cursor, the newest page is returned. Items
// are always returned oldest first, including tail and before-cursor queries.
func ListIncrementalMigrationLogs(jobID string, afterID, beforeID uint64, limit int) (*IncrementalMigrationLogPage, error) {
	page := &IncrementalMigrationLogPage{Items: make([]IncrementalMigrationLog, 0)}
	if afterID > 0 && beforeID > 0 {
		return nil, ErrConflictingIncrementalLogCursors
	}
	if limit <= 0 {
		return page, nil
	}

	query := DB.Where("job_id = ?", jobID)
	switch {
	case afterID > 0:
		query = query.Where("id > ?", afterID).Order("id ASC")
	case beforeID > 0:
		query = query.Where("id < ?", beforeID).Order("id DESC")
	default:
		query = query.Order("id DESC")
	}
	if err := query.Limit(limit).Find(&page.Items).Error; err != nil {
		return nil, err
	}
	if afterID == 0 {
		reverseIncrementalLogs(page.Items)
	}
	if len(page.Items) == 0 {
		var count int64
		switch {
		case afterID > 0:
			if err := DB.Model(&IncrementalMigrationLog{}).
				Where("job_id = ? AND id <= ?", jobID, afterID).Limit(1).Count(&count).Error; err != nil {
				return nil, err
			}
			page.HasOlder = count > 0
		case beforeID > 0:
			if err := DB.Model(&IncrementalMigrationLog{}).
				Where("job_id = ? AND id >= ?", jobID, beforeID).Limit(1).Count(&count).Error; err != nil {
				return nil, err
			}
			page.HasNewer = count > 0
		}
		return page, nil
	}

	page.OldestID = page.Items[0].ID
	page.NewestID = page.Items[len(page.Items)-1].ID
	var olderCount, newerCount int64
	if err := DB.Model(&IncrementalMigrationLog{}).
		Where("job_id = ? AND id < ?", jobID, page.OldestID).
		Limit(1).Count(&olderCount).Error; err != nil {
		return nil, err
	}
	if err := DB.Model(&IncrementalMigrationLog{}).
		Where("job_id = ? AND id > ?", jobID, page.NewestID).
		Limit(1).Count(&newerCount).Error; err != nil {
		return nil, err
	}
	page.HasOlder = olderCount > 0
	page.HasNewer = newerCount > 0
	return page, nil
}

// AddIncrementalLogDroppedCount atomically records journal lines that could
// not be persisted. The counter remains visible even when no warning line can
// itself be written to SQLite.
func AddIncrementalLogDroppedCount(jobID string, count int64) error {
	if count <= 0 {
		return nil
	}
	return DB.Model(&IncrementalMigrationJob{}).
		Where("job_id = ?", jobID).
		UpdateColumn("log_dropped_count", gorm.Expr("log_dropped_count + ?", count)).Error
}

// CleanupExpiredIncrementalMigrationLogs removes logs only for explicitly
// terminal stopped/aborted jobs whose recorded finish time predates cutoff.
// Failed and paused jobs are intentionally retained because they can still be
// diagnosed or resumed.
func CleanupExpiredIncrementalMigrationLogs(cutoff time.Time) (int64, error) {
	terminalJobs := DB.Model(&IncrementalMigrationJob{}).
		Select("job_id").
		Where("status IN ? AND finished_at IS NOT NULL AND finished_at < ?", []string{"stopped", "aborted"}, cutoff)
	result := DB.Where("job_id IN (?)", terminalJobs).Delete(&IncrementalMigrationLog{})
	return result.RowsAffected, result.Error
}

func reverseIncrementalLogs(logs []IncrementalMigrationLog) {
	for left, right := 0, len(logs)-1; left < right; left, right = left+1, right-1 {
		logs[left], logs[right] = logs[right], logs[left]
	}
}
