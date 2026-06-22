package store

func CreateBatchMigration(b *BatchMigration) error {
	return DB.Create(b).Error
}

func UpdateBatchMigration(b *BatchMigration) error {
	return DB.Save(b).Error
}

func GetBatchMigration(batchID string) (*BatchMigration, error) {
	var b BatchMigration
	if err := DB.Where("batch_id = ?", batchID).First(&b).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

func ListBatchMigrations(ownerID uint, isAdmin bool) ([]BatchMigration, error) {
	var list []BatchMigration
	q := DB.Order("id desc")
	if !isAdmin {
		q = q.Where("owner_id = ?", ownerID)
	}
	if err := q.Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

// ListJobsByBatch 返回某批次下的全部子任务（含连接快照），按创建顺序排列。
func ListJobsByBatch(batchID string) ([]DataMigrationJobWithConn, error) {
	var jobs []DataMigrationJob
	if err := DB.Where("batch_id = ?", batchID).Order("id asc").Find(&jobs).Error; err != nil {
		return nil, err
	}
	result := make([]DataMigrationJobWithConn, len(jobs))
	for i, j := range jobs {
		srcConn := &ConnSnapshot{ID: j.SrcConnID, Name: j.SrcConnName,
			Host: j.SrcConnHost, Port: j.SrcConnPort,
			Database: j.SrcConnDatabase, Username: j.SrcConnUsername}
		dstConn := &ConnSnapshot{ID: j.DstConnID, Name: j.DstConnName,
			Host: j.DstConnHost, Port: j.DstConnPort,
			Database: j.DstConnDatabase, Username: j.DstConnUsername}
		result[i] = DataMigrationJobWithConn{DataMigrationJob: j, SrcConn: srcConn, DstConn: dstConn}
	}
	return result, nil
}
