package store

func CreateConnection(c *Connection) error {
	return DB.Create(c).Error
}

func ListConnections() ([]Connection, error) {
	var list []Connection
	if err := DB.Order("id").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func GetConnection(id uint) (*Connection, error) {
	var c Connection
	if err := DB.First(&c, id).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

func UpdateConnection(id uint, updates map[string]any) error {
	return DB.Model(&Connection{}).Where("id = ?", id).Updates(updates).Error
}

func DeleteConnection(id uint) error {
	return DB.Delete(&Connection{}, id).Error
}
