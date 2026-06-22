package store

import "gorm.io/gorm"

func CreateConnection(c *Connection) error {
	return DB.Create(c).Error
}

func ListConnections(ownerID uint, isAdmin bool) ([]Connection, error) {
	var list []Connection
	q := DB.Order("id")
	if !isAdmin {
		q = q.Where("owner_id = ?", ownerID)
	}
	if err := q.Find(&list).Error; err != nil {
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

// GetConnectionOwned 取连接并做归属校验：普通用户只能取自己的连接，admin 可取任意。
// 非归属时返回 gorm.ErrRecordNotFound（与不存在同样处理，不暴露存在性）。
func GetConnectionOwned(id, ownerID uint, isAdmin bool) (*Connection, error) {
	c, err := GetConnection(id)
	if err != nil {
		return nil, err
	}
	if !isAdmin && c.OwnerID != ownerID {
		return nil, gorm.ErrRecordNotFound
	}
	return c, nil
}

func UpdateConnection(id uint, updates map[string]any) error {
	return DB.Model(&Connection{}).Where("id = ?", id).Updates(updates).Error
}

func DeleteConnection(id uint) error {
	return DB.Delete(&Connection{}, id).Error
}
