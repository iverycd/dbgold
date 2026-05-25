package oracle

import (
	"database/sql"
	"dbgold/diff"
	"dbgold/migrate"
	"dbgold/schema"
	"fmt"

	_ "github.com/sijms/go-ora/v2"
)

type Driver struct {
	db *sql.DB
}

func New() *Driver { return &Driver{} }

func (d *Driver) Connect(dsn string) error {
	db, err := sql.Open("oracle", dsn)
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

func (d *Driver) GenerateDiffSQL(r *diff.Result) ([]string, error) {
	return migrate.OracleGenerateDiffSQL(r, false)
}

func (d *Driver) GenerateFullMigrationSQL(src, dst *schema.FullSchema) ([]string, error) {
	return migrate.OracleGenerateFullMigrationSQL(src, dst, false)
}

func (d *Driver) GenerateSelectiveSQL(objects *schema.SelectedObjects) ([]string, error) {
	return migrate.OracleGenerateSelectiveSQL(objects, false)
}
