package postgres

import (
	"database/sql"
	"dbgold/diff"
	"dbgold/migrate"
	"dbgold/schema"
	"fmt"

	_ "github.com/lib/pq"
)

type Driver struct {
	db *sql.DB
}

func New() *Driver { return &Driver{} }

func (d *Driver) Connect(dsn string) error {
	db, err := sql.Open("postgres", dsn)
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

func (d *Driver) GenerateDiffSQL(r *diff.Result, lowerCase bool) ([]string, error) {
	return migrate.PostgresGenerateDiffSQL(r, lowerCase)
}

func (d *Driver) GenerateFullMigrationSQL(src, dst *schema.FullSchema, lowerCase bool) ([]string, error) {
	return migrate.PostgresGenerateFullMigrationSQL(src, dst, lowerCase)
}

func (d *Driver) GenerateSelectiveSQL(objects *schema.SelectedObjects, lowerCase bool) ([]string, error) {
	return migrate.PostgresGenerateSelectiveSQL(objects, lowerCase)
}
