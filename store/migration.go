package store

import "time"

type MigrationHistory struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	OwnerID       uint      `gorm:"index;not null;default:0" json:"owner_id"`
	Type          string    `gorm:"not null" json:"type"` // diff | full | selective
	SrcConnID     uint      `json:"src_conn_id"`
	SrcDatabase   string    `json:"src_database"`
	DstConnID     uint      `json:"dst_conn_id"`
	DstDatabase   string    `json:"dst_database"`
	SQLStatements string    `gorm:"type:text" json:"sql_statements"` // JSON array string
	Status        string    `gorm:"not null;default:'success'" json:"status"`
	ErrorMessage  string    `json:"error_message,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

func CreateMigration(m *MigrationHistory) error {
	return DB.Create(m).Error
}

func ListMigrations(ownerID uint, isAdmin bool) ([]MigrationHistory, error) {
	var list []MigrationHistory
	q := DB.Order("id desc")
	if !isAdmin {
		q = q.Where("owner_id = ?", ownerID)
	}
	if err := q.Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func GetMigration(id uint) (*MigrationHistory, error) {
	var m MigrationHistory
	if err := DB.First(&m, id).Error; err != nil {
		return nil, err
	}
	return &m, nil
}
