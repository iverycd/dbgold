package dameng

import (
	"database/sql"
	"dbgold/diff"
	"dbgold/schema"
	"fmt"

	_ "gitee.com/chunanyong/dm"
)

type Driver struct {
	db *sql.DB
}

func New() *Driver { return &Driver{} }

func (d *Driver) Connect(dsn string) error {
	db, err := sql.Open("dm", dsn)
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

func (d *Driver) ExtractSchema(dbName string) (*schema.Schema, error) {
	return nil, fmt.Errorf("达梦暂不支持 schema diff")
}

func (d *Driver) ExtractFullObjects(dbName string) (*schema.FullSchema, error) {
	return nil, fmt.Errorf("达梦暂不支持 schema diff")
}

func (d *Driver) GenerateDiffSQL(r *diff.Result, lowerCase bool) ([]string, error) {
	return nil, fmt.Errorf("达梦暂不支持 schema diff")
}

func (d *Driver) GenerateFullMigrationSQL(src, dst *schema.FullSchema, lowerCase bool) ([]string, error) {
	return nil, fmt.Errorf("达梦暂不支持 schema diff")
}

func (d *Driver) GenerateSelectiveSQL(objects *schema.SelectedObjects, lowerCase bool) ([]string, error) {
	return nil, fmt.Errorf("达梦暂不支持 schema diff")
}
