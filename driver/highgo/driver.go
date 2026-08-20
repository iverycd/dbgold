package highgo

import (
	"database/sql"
	"fmt"

	_ "dbgold/internal/highgopq"
)

// Driver 使用瀚高专用协议驱动建立连接，支持瀚高的 SM3 认证方式。
type Driver struct {
	db *sql.DB
}

func New() *Driver { return &Driver{} }

func (d *Driver) Connect(dsn string) error {
	db, err := sql.Open("highgo", dsn)
	if err != nil {
		return err
	}
	d.db = db
	return d.Ping()
}

func (d *Driver) Ping() error {
	if d.db == nil {
		return fmt.Errorf("not connected")
	}
	return d.db.Ping()
}

func (d *Driver) Close() error {
	if d.db != nil {
		return d.db.Close()
	}
	return nil
}
