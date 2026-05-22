package store

import (
	"dbgold/config"
	"log"
	"time"

	"gorm.io/driver/sqlite"
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
	Name      string    `gorm:"not null" json:"name"`
	DBType    string    `gorm:"not null" json:"db_type"`
	Host      string    `gorm:"not null" json:"host"`
	Port      int       `gorm:"not null" json:"port"`
	Database  string    `gorm:"not null" json:"database"`
	Username  string    `gorm:"not null" json:"username"`
	Password  string    `gorm:"not null" json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

func Init(cfg *config.Config) {
	var err error
	DB, err = gorm.Open(sqlite.Open(cfg.SQLitePath), &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to open sqlite: %v", err)
	}
	if err := DB.AutoMigrate(&User{}, &Connection{}, &MigrationHistory{}, &DataMigrationJob{}); err != nil {
		log.Fatalf("failed to migrate: %v", err)
	}
}
