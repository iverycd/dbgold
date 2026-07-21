package store

import "time"

// QueryAudit records query-console execution metadata. Result sets are never persisted.
type QueryAudit struct {
	ID             uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	OwnerID        uint      `gorm:"index;not null" json:"owner_id"`
	ConnectionID   uint      `gorm:"index;not null" json:"connection_id"`
	ConnectionName string    `gorm:"not null" json:"connection_name"`
	DBType         string    `gorm:"not null" json:"db_type"`
	Namespace      string    `json:"namespace"`
	SQLText        string    `gorm:"type:text;not null" json:"sql"`
	StatementType  string    `gorm:"index;not null" json:"statement_type"`
	RiskLevel      string    `gorm:"not null" json:"risk_level"`
	Confirmed      bool      `gorm:"not null" json:"confirmed"`
	Status         string    `gorm:"index;not null" json:"status"`
	DurationMS     int64     `json:"duration_ms"`
	RowCount       int64     `json:"row_count"`
	AffectedRows   int64     `json:"affected_rows"`
	Truncated      bool      `json:"truncated"`
	ErrorText      string    `gorm:"type:text" json:"error,omitempty"`
	ClientIP       string    `json:"client_ip"`
	CreatedAt      time.Time `gorm:"index;not null" json:"created_at"`
	Username       string    `gorm:"column:username;->;-:migration" json:"username,omitempty"`
}

type QueryAuditFilter struct {
	OwnerID      uint
	AllOwners    bool
	ConnectionID uint
	Status       string
	BeforeID     uint64
	Limit        int
}

func CreateQueryAudit(audit *QueryAudit) error {
	return DB.Create(audit).Error
}

func ListQueryAudits(filter QueryAuditFilter) ([]QueryAudit, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := DB.Table("query_audits AS qa").
		Select("qa.*, users.username AS username").
		Joins("LEFT JOIN users ON users.id = qa.owner_id").
		Order("qa.id DESC").Limit(limit)
	if !filter.AllOwners {
		q = q.Where("qa.owner_id = ?", filter.OwnerID)
	}
	if filter.ConnectionID != 0 {
		q = q.Where("qa.connection_id = ?", filter.ConnectionID)
	}
	if filter.Status != "" {
		q = q.Where("qa.status = ?", filter.Status)
	}
	if filter.BeforeID != 0 {
		q = q.Where("qa.id < ?", filter.BeforeID)
	}
	var list []QueryAudit
	return list, q.Scan(&list).Error
}

func CleanupExpiredQueryAudits(before time.Time) (int64, error) {
	result := DB.Where("created_at < ?", before).Delete(&QueryAudit{})
	return result.RowsAffected, result.Error
}
