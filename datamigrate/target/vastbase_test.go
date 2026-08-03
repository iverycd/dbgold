package target

import (
	"testing"

	"dbgold/datamigrate/dialect"
)

func TestVastbaseWriterUsesPostgresDialect(t *testing.T) {
	writer := &VastbaseWriter{PostgresWriter: &PostgresWriter{
		dbType: "vastbase",
		dia:    dialect.NewPostgres("vastbase"),
	}}

	if got := writer.DBType(); got != "vastbase" {
		t.Fatalf("DBType() = %q, want vastbase", got)
	}
	if got := writer.Dialect().Name(); got != "vastbase" {
		t.Fatalf("Dialect().Name() = %q, want vastbase", got)
	}
	if writer.Dialect().Caps().SupportsDistribute {
		t.Fatal("Vastbase must not enable the GaussDB-only distribute capability")
	}
}
