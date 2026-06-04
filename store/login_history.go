package store

func CreateLoginHistory(username, clientIP string, success bool) error {
	return DB.Create(&LoginHistory{Username: username, ClientIP: clientIP, Success: success}).Error
}

func ListLoginHistory(limit int) ([]LoginHistory, error) {
	var records []LoginHistory
	err := DB.Order("created_at DESC").Limit(limit).Find(&records).Error
	return records, err
}
