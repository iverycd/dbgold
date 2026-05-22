package store

import "time"

type DataMigrationJob struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	JobID       string     `gorm:"uniqueIndex;not null" json:"job_id"`
	SrcConnID   uint       `json:"src_conn_id"`
	DstConnID   uint       `json:"dst_conn_id"`
	SrcDBType   string     `json:"src_db_type"`
	DstDBType   string     `json:"dst_db_type"`
	MigrateMode string     `json:"migrate_mode"` // all / exclude / include
	TableFilter string     `json:"table_filter"`
	PageSize    int        `json:"page_size"`
	MaxParallel int        `json:"max_parallel"`
	Status      string     `json:"status"` // running / done / failed / cancelled
	Summary     string     `json:"summary"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

func CreateDataMigrationJob(j *DataMigrationJob) error {
	return DB.Create(j).Error
}

func UpdateDataMigrationJob(j *DataMigrationJob) error {
	return DB.Save(j).Error
}

func GetDataMigrationJob(jobID string) (*DataMigrationJob, error) {
	var j DataMigrationJob
	if err := DB.Where("job_id = ?", jobID).First(&j).Error; err != nil {
		return nil, err
	}
	return &j, nil
}

func ListDataMigrationJobs() ([]DataMigrationJob, error) {
	var jobs []DataMigrationJob
	if err := DB.Order("id desc").Find(&jobs).Error; err != nil {
		return nil, err
	}
	return jobs, nil
}
