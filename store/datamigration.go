package store

import "time"

type ConnSnapshot struct {
	ID       uint   `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
}

type DataMigrationJobWithConn struct {
	DataMigrationJob
	SrcConn *ConnSnapshot `json:"src_conn"`
	DstConn *ConnSnapshot `json:"dst_conn"`
}

type DataMigrationJob struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	JobID          string     `gorm:"uniqueIndex;not null" json:"job_id"`
	SrcConnID      uint       `json:"src_conn_id"`
	DstConnID      uint       `json:"dst_conn_id"`
	SrcDBType      string     `json:"src_db_type"`
	DstDBType      string     `json:"dst_db_type"`
	MigrateMode    string     `json:"migrate_mode"` // all / exclude / include
	TableFilter    string     `json:"table_filter"`
	PageSize       int        `json:"page_size"`
	MaxParallel    int        `json:"max_parallel"`
	Status         string     `json:"status"` // running / done / failed / cancelled
	Summary        string     `json:"summary"`
	CreatedAt      time.Time  `json:"created_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	LowerCaseNames bool       `json:"lower_case_names"`
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

func ListDataMigrationJobsWithConn() ([]DataMigrationJobWithConn, error) {
	jobs, err := ListDataMigrationJobs()
	if err != nil {
		return nil, err
	}

	// 收集所有涉及的连接 ID
	idSet := make(map[uint]struct{})
	for _, j := range jobs {
		idSet[j.SrcConnID] = struct{}{}
		idSet[j.DstConnID] = struct{}{}
	}
	ids := make([]uint, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}

	// 批量查连接
	var conns []Connection
	if len(ids) > 0 {
		DB.Where("id IN ?", ids).Find(&conns)
	}
	connMap := make(map[uint]*ConnSnapshot, len(conns))
	for i := range conns {
		c := &conns[i]
		connMap[c.ID] = &ConnSnapshot{
			ID: c.ID, Name: c.Name, Host: c.Host,
			Port: c.Port, Database: c.Database, Username: c.Username,
		}
	}

	result := make([]DataMigrationJobWithConn, len(jobs))
	for i, j := range jobs {
		result[i] = DataMigrationJobWithConn{
			DataMigrationJob: j,
			SrcConn:          connMap[j.SrcConnID],
			DstConn:          connMap[j.DstConnID],
		}
	}
	return result, nil
}
