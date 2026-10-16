package store

import (
	"errors"

	"gorm.io/gorm"
)

var (
	ErrConnectionChanged = errors.New("连接配置已变化，请刷新后重试")
	ErrConnectionInUse   = errors.New("该连接有未终止的增量任务，请先停止或放弃任务后再修改数据库")
)

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

// FindConnectionByOwnerName 按 owner + name 查找连接，用于「按名复用」判断。
// 未命中返回 gorm.ErrRecordNotFound。
func FindConnectionByOwnerName(ownerID uint, name string) (*Connection, error) {
	var c Connection
	if err := DB.Where("owner_id = ? AND name = ?", ownerID, name).First(&c).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

func UpdateConnection(id uint, updates map[string]any) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var current Connection
		if err := tx.First(&current, id).Error; err != nil {
			return err
		}
		if database, ok := updates["database"].(string); ok && database != current.Database {
			if err := checkConnectionDatabaseUse(tx, id); err != nil {
				return err
			}
		}
		return tx.Model(&Connection{}).Where("id = ?", id).Updates(updates).Error
	})
}

// SameConnectionExecution ignores display metadata, but includes credentials so
// a successful connection test cannot authorize a concurrently changed login.
func SameConnectionExecution(a, b *Connection) bool {
	return a.ID == b.ID && a.OwnerID == b.OwnerID && a.DBType == b.DBType &&
		a.Host == b.Host && a.Port == b.Port && a.Database == b.Database &&
		a.Username == b.Username && a.Password == b.Password
}

func checkConnectionDatabaseUse(tx *gorm.DB, id uint) error {
	var count int64
	if err := tx.Model(&IncrementalMigrationJob{}).
		Where("(src_conn_id = ? OR dst_conn_id = ?) AND status NOT IN ?", id, id, []string{"stopped", "aborted"}).
		Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return ErrConnectionInUse
	}
	return nil
}

func CheckConnectionDatabaseUse(id uint) error {
	return checkConnectionDatabaseUse(DB, id)
}

// UpdateConnectionDatabase saves only the tested database and rejects any
// change to the tested endpoint or login. Historical task snapshots are fixed.
func UpdateConnectionDatabase(expected *Connection, database string) (*Connection, error) {
	var current Connection
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&current, expected.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrConnectionChanged
			}
			return err
		}
		if !SameConnectionExecution(expected, &current) {
			return ErrConnectionChanged
		}
		if current.Database == database {
			return nil
		}
		if err := checkConnectionDatabaseUse(tx, expected.ID); err != nil {
			return err
		}
		result := tx.Model(&Connection{}).
			Where("id = ? AND owner_id = ? AND db_type = ? AND host = ? AND port = ? AND database = ? AND username = ? AND password = ?",
				expected.ID, expected.OwnerID, expected.DBType, expected.Host, expected.Port, expected.Database, expected.Username, expected.Password).
			Update("database", database)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrConnectionChanged
		}
		current.Database = database
		return nil
	})
	return &current, err
}

// CheckConnectionSnapshots is called under the same handler lock as connection
// updates immediately before a CDC job is created after network preflight.
func CheckConnectionSnapshots(connections ...*Connection) error {
	for _, expected := range connections {
		current, err := GetConnection(expected.ID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrConnectionChanged
		}
		if err != nil {
			return err
		}
		if !SameConnectionExecution(expected, current) {
			return ErrConnectionChanged
		}
	}
	return nil
}

func DeleteConnection(id uint) error {
	return DB.Delete(&Connection{}, id).Error
}
