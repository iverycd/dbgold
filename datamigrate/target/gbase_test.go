package target

import (
	"testing"

	"dbgold/datamigrate/dialect"
)

func TestGBaseWriterUsesPostgresDialect(t *testing.T) {
	writer := &GBaseWriter{PostgresWriter: &PostgresWriter{
		dbType: "gbase",
		dia:    dialect.NewPostgres("gbase"),
	}}

	if got := writer.DBType(); got != "gbase" {
		t.Fatalf("DBType() = %q, want gbase", got)
	}
	if got := writer.Dialect().Name(); got != "gbase" {
		t.Fatalf("Dialect().Name() = %q, want gbase", got)
	}
	if writer.Dialect().Caps().SupportsDistribute {
		t.Fatal("GBase must not enable distributed-table migration")
	}
}
