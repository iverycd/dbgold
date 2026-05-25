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
	GenerateDiffSQL(d *diff.Result, lowerCase bool) ([]string, error)
	GenerateFullMigrationSQL(src, dst *schema.FullSchema, lowerCase bool) ([]string, error)
	GenerateSelectiveSQL(objects *schema.SelectedObjects, lowerCase bool) ([]string, error)
}
