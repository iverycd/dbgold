package store

import (
	"dbgold/config"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

var DB *gorm.DB

type User struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Username  string    `gorm:"uniqueIndex;not null" json:"username"`
	Password  string    `gorm:"not null" json:"-"`
	Role      string    `gorm:"not null;default:'user'" json:"role"`
	Enabled   bool      `gorm:"default:true" json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

type Connection struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	OwnerID   uint      `gorm:"index;not null;default:0" json:"owner_id"`
	Name      string    `gorm:"not null" json:"name"`
	DBType    string    `gorm:"not null" json:"db_type"`
	Host      string    `gorm:"not null" json:"host"`
	Port      int       `gorm:"not null" json:"port"`
	Database  string    `gorm:"not null" json:"database"`
	Username  string    `gorm:"not null" json:"username"`
	Password  string    `gorm:"not null" json:"-"`
	Env       string    `gorm:"index;default:''" json:"env"`
	CreatedAt time.Time `json:"created_at"`
}

type LoginHistory struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Username  string    `gorm:"not null;index" json:"username"`
	ClientIP  string    `gorm:"not null" json:"client_ip"`
	Success   bool      `gorm:"not null" json:"success"`
	CreatedAt time.Time `json:"created_at"`
}

// BatchMigration 表示一次批量迁移（Excel 上传产生的批次）。
// 其下的子任务为带 BatchID 的 DataMigrationJob；批量连接信息不入 Connection 表，
// 仅保存在子任务的快照字段中（与现有连接管理隔离）。
type BatchMigration struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	OwnerID    uint       `gorm:"index;not null;default:0" json:"owner_id"`
	BatchID    string     `gorm:"uniqueIndex;not null" json:"batch_id"`
	FileName   string     `json:"file_name"`
	Total      int        `json:"total"`
	Status     string     `json:"status"` // running / done / cancelled
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func Init(cfg *config.Config) {
	if err := InitWithError(cfg); err != nil {
		slog.Error("failed to initialize database", "err", err)
		os.Exit(1)
	}
}

func InitWithError(cfg *config.Config) error {
	var err error
	DB, err = gorm.Open(sqlite.Open(cfg.SQLitePath), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	if err := DB.AutoMigrate(&User{}, &Connection{}, &DataMigrationJob{}, &DataMigrationReport{}, &IncrementalMigrationJob{}, &IncrementalMigrationLog{}, &LoginHistory{}, &BatchMigration{}, &MigrationTicket{}, &QueryAudit{}); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	return nil
}

func Close() error {
	if DB == nil {
		return nil
	}
	sqlDB, err := DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
