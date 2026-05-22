package store

import "time"

type DataMigrationReport struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	JobID      string    `gorm:"uniqueIndex;not null" json:"job_id"`
	ReportJSON string    `gorm:"type:text" json:"report_json"`
	CreatedAt  time.Time `json:"created_at"`
}

func CreateDataMigrationReport(r *DataMigrationReport) error {
	return DB.Create(r).Error
}

func GetDataMigrationReport(jobID string) (*DataMigrationReport, error) {
	var r DataMigrationReport
	if err := DB.Where("job_id = ?", jobID).First(&r).Error; err != nil {
		return nil, err
	}
	return &r, nil
}
