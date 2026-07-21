package store

import (
	"strings"
	"time"

	"gorm.io/gorm"
)

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

type JobListFilter struct {
	Page     int
	PageSize int
	Keyword  string
	Status   string
	Origin   string
}

type PageResult[T any] struct {
	Items    []T   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

type DataMigrationJob struct {
	ID                 uint       `gorm:"primaryKey" json:"id"`
	OwnerID            uint       `gorm:"index;not null;default:0" json:"owner_id"`
	JobID              string     `gorm:"uniqueIndex;not null" json:"job_id"`
	BatchID            string     `gorm:"index" json:"batch_id"` // 非空表示属于某批量迁移批次，空表示单任务
	SrcConnID          uint       `json:"src_conn_id"`
	DstConnID          uint       `json:"dst_conn_id"`
	SrcDBType          string     `json:"src_db_type"`
	DstDBType          string     `json:"dst_db_type"`
	MigrateMode        string     `json:"migrate_mode"` // all / exclude / include
	TableFilter        string     `json:"table_filter"`
	MigrateObjects     string     `json:"migrate_objects"` // 仅对象迁移任务:逗号拼接的对象类型,空表示普通数据迁移
	PageSize           int        `json:"page_size"`
	MaxParallel        int        `json:"max_parallel"`
	IntraTableParallel int        `json:"intra_table_parallel"`
	Status             string     `json:"status"` // running / done / failed / cancelled
	Summary            string     `json:"summary"`
	CreatedAt          time.Time  `json:"created_at"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
	LowerCaseNames     bool       `json:"lower_case_names"`
	CharInLength       bool       `json:"char_in_length"`
	UseNvarchar2       bool       `json:"use_nvarchar2"`
	ChangeOwner        bool       `json:"change_owner"`
	DstSchema          string     `json:"dst_schema"` // 目标 schema，为空时使用连接默认 search_path
	// 连接快照（迁移启动时写入，不随连接修改而变化）
	SrcConnName     string `json:"src_conn_name"`
	SrcConnHost     string `json:"src_conn_host"`
	SrcConnPort     int    `json:"src_conn_port"`
	SrcConnDatabase string `json:"src_conn_database"`
	SrcConnUsername string `json:"src_conn_username"`
	DstConnName     string `json:"dst_conn_name"`
	DstConnHost     string `json:"dst_conn_host"`
	DstConnPort     int    `json:"dst_conn_port"`
	DstConnDatabase string `json:"dst_conn_database"`
	DstConnUsername string `json:"dst_conn_username"`
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

// GetDataMigrationJobWithConn returns the frozen connection snapshots used by
// the history list. Legacy jobs fall back to the current connection records.
func GetDataMigrationJobWithConn(jobID string) (*DataMigrationJobWithConn, error) {
	j, err := GetDataMigrationJob(jobID)
	if err != nil {
		return nil, err
	}

	var srcConn, dstConn *ConnSnapshot
	if j.SrcConnName != "" {
		srcConn = &ConnSnapshot{ID: j.SrcConnID, Name: j.SrcConnName, Host: j.SrcConnHost, Port: j.SrcConnPort,
			Database: j.SrcConnDatabase, Username: j.SrcConnUsername}
		dstConn = &ConnSnapshot{ID: j.DstConnID, Name: j.DstConnName, Host: j.DstConnHost, Port: j.DstConnPort,
			Database: j.DstConnDatabase, Username: j.DstConnUsername}
	} else {
		loadCurrent := func(id uint) (*ConnSnapshot, error) {
			var conn Connection
			if loadErr := DB.First(&conn, id).Error; loadErr != nil {
				if loadErr == gorm.ErrRecordNotFound {
					return nil, nil
				}
				return nil, loadErr
			}
			return &ConnSnapshot{ID: conn.ID, Name: conn.Name, Host: conn.Host, Port: conn.Port,
				Database: conn.Database, Username: conn.Username}, nil
		}
		srcConn, err = loadCurrent(j.SrcConnID)
		if err != nil {
			return nil, err
		}
		dstConn, err = loadCurrent(j.DstConnID)
		if err != nil {
			return nil, err
		}
	}

	return &DataMigrationJobWithConn{DataMigrationJob: *j, SrcConn: srcConn, DstConn: dstConn}, nil
}

func ListDataMigrationJobs(ownerID uint, isAdmin bool) ([]DataMigrationJob, error) {
	var jobs []DataMigrationJob
	q := DB.Order("id desc")
	if !isAdmin {
		q = q.Where("owner_id = ?", ownerID)
	}
	if err := q.Find(&jobs).Error; err != nil {
		return nil, err
	}
	return jobs, nil
}

func ListDataMigrationJobsWithConn(ownerID uint, isAdmin bool) ([]DataMigrationJobWithConn, error) {
	jobs, err := ListDataMigrationJobs(ownerID, isAdmin)
	if err != nil {
		return nil, err
	}
	return decorateDataMigrationJobs(jobs)
}

func QueryDataMigrationJobsWithConn(ownerID uint, isAdmin bool, filter JobListFilter) (*PageResult[DataMigrationJobWithConn], error) {
	query := DB.Model(&DataMigrationJob{}).
		Joins("LEFT JOIN connections AS src_connections ON src_connections.id = data_migration_jobs.src_conn_id").
		Joins("LEFT JOIN connections AS dst_connections ON dst_connections.id = data_migration_jobs.dst_conn_id")
	if !isAdmin {
		query = query.Where("data_migration_jobs.owner_id = ?", ownerID)
	}
	if filter.Status != "" {
		query = query.Where("data_migration_jobs.status = ?", filter.Status)
	}
	switch filter.Origin {
	case "single":
		query = query.Where("data_migration_jobs.batch_id = '' OR data_migration_jobs.batch_id IS NULL")
	case "batch":
		query = query.Where("data_migration_jobs.batch_id <> ''")
	}
	if keyword := strings.ToLower(strings.TrimSpace(filter.Keyword)); keyword != "" {
		like := "%" + keyword + "%"
		query = query.Where(`(LOWER(COALESCE(data_migration_jobs.job_id, '')) LIKE ?
			OR LOWER(COALESCE(data_migration_jobs.batch_id, '')) LIKE ?
			OR LOWER(COALESCE(NULLIF(data_migration_jobs.src_conn_name, ''), src_connections.name, '')) LIKE ?
			OR LOWER(COALESCE(NULLIF(data_migration_jobs.src_conn_host, ''), src_connections.host, '')) LIKE ?
			OR LOWER(COALESCE(NULLIF(data_migration_jobs.src_conn_database, ''), src_connections.database, '')) LIKE ?
			OR LOWER(COALESCE(NULLIF(data_migration_jobs.dst_conn_name, ''), dst_connections.name, '')) LIKE ?
			OR LOWER(COALESCE(NULLIF(data_migration_jobs.dst_conn_host, ''), dst_connections.host, '')) LIKE ?
			OR LOWER(COALESCE(NULLIF(data_migration_jobs.dst_conn_database, ''), dst_connections.database, '')) LIKE ?
			OR LOWER(COALESCE(data_migration_jobs.dst_schema, '')) LIKE ?)`,
			like, like, like, like, like, like, like, like, like)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, err
	}
	var jobs []DataMigrationJob
	if err := query.Order("data_migration_jobs.id DESC").
		Offset((filter.Page - 1) * filter.PageSize).
		Limit(filter.PageSize).
		Find(&jobs).Error; err != nil {
		return nil, err
	}
	items, err := decorateDataMigrationJobs(jobs)
	if err != nil {
		return nil, err
	}
	return &PageResult[DataMigrationJobWithConn]{Items: items, Total: total, Page: filter.Page, PageSize: filter.PageSize}, nil
}

func decorateDataMigrationJobs(jobs []DataMigrationJob) ([]DataMigrationJobWithConn, error) {

	// 收集需要回退查表的旧记录连接 ID（快照字段为空说明是旧数据）
	idSet := make(map[uint]struct{})
	for _, j := range jobs {
		if j.SrcConnName == "" {
			idSet[j.SrcConnID] = struct{}{}
			idSet[j.DstConnID] = struct{}{}
		}
	}
	connMap := make(map[uint]*ConnSnapshot)
	if len(idSet) > 0 {
		ids := make([]uint, 0, len(idSet))
		for id := range idSet {
			ids = append(ids, id)
		}
		var conns []Connection
		DB.Where("id IN ?", ids).Find(&conns)
		for i := range conns {
			c := &conns[i]
			connMap[c.ID] = &ConnSnapshot{
				ID: c.ID, Name: c.Name, Host: c.Host,
				Port: c.Port, Database: c.Database, Username: c.Username,
			}
		}
	}

	result := make([]DataMigrationJobWithConn, len(jobs))
	for i, j := range jobs {
		var srcConn, dstConn *ConnSnapshot
		if j.SrcConnName != "" {
			srcConn = &ConnSnapshot{ID: j.SrcConnID, Name: j.SrcConnName,
				Host: j.SrcConnHost, Port: j.SrcConnPort,
				Database: j.SrcConnDatabase, Username: j.SrcConnUsername}
			dstConn = &ConnSnapshot{ID: j.DstConnID, Name: j.DstConnName,
				Host: j.DstConnHost, Port: j.DstConnPort,
				Database: j.DstConnDatabase, Username: j.DstConnUsername}
		} else {
			srcConn = connMap[j.SrcConnID]
			dstConn = connMap[j.DstConnID]
		}
		result[i] = DataMigrationJobWithConn{DataMigrationJob: j, SrcConn: srcConn, DstConn: dstConn}
	}
	return result, nil
}
