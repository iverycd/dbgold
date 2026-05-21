package driver

import (
	"dbgold/diff"
	"dbgold/schema"
)

type Driver interface {
	Connect(dsn string) error
	Ping() error
	Close() error
	ExtractSchema(dbName string) (*schema.Schema, error)
	ExtractFullObjects(dbName string) (*schema.FullSchema, error)
	GenerateDiffSQL(d *diff.Result) ([]string, error)
	GenerateFullMigrationSQL(src, dst *schema.FullSchema) ([]string, error)
	GenerateSelectiveSQL(objects *schema.SelectedObjects) ([]string, error)
}
