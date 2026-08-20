package typemap

import (
	"testing"

	"dbgold/datamigrate/source"
)

func TestPGTimestampMappings(t *testing.T) {
	tests := []struct {
		name   string
		mapper Mapper
		types  []string
	}{
		{name: "mysql", mapper: MySQLToPG, types: []string{"datetime", "timestamp"}},
		{name: "oracle", mapper: OracleToPG, types: []string{"DATE", "TIMESTAMP"}},
		{name: "sqlserver", mapper: SQLServerToPG, types: []string{"smalldatetime", "datetime", "datetime2"}},
		{name: "dameng", mapper: DaMengToPG, types: []string{"timestamp", "datetime"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, dataType := range tt.types {
				if got := tt.mapper(source.ColumnInfo{DataType: dataType}, false, false); got != "timestamp without time zone" {
					t.Errorf("%s maps to %q, want %q", dataType, got, "timestamp without time zone")
				}
			}
		})
	}
}

func TestOraclePGTimestampWithTimeZoneMappings(t *testing.T) {
	for _, dataType := range []string{"TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE"} {
		if got := OracleToPG(source.ColumnInfo{DataType: dataType}, false, false); got != "timestamptz" {
			t.Errorf("%s maps to %q, want %q", dataType, got, "timestamptz")
		}
	}
}

func TestAllPGTargetsUseExplicitTimestampWithoutTimeZone(t *testing.T) {
	sources := []struct {
		name     string
		dataType string
	}{
		{name: "mysql", dataType: "datetime"},
		{name: "oracle", dataType: "TIMESTAMP"},
		{name: "sqlserver", dataType: "datetime2"},
		{name: "dameng", dataType: "timestamp"},
	}
	targets := []string{"postgres", "gaussdb", "vastbase", "highgo", "seabox", "kingbase", "gbase"}

	for _, src := range sources {
		for _, target := range targets {
			mapper, ok := Get(src.name, target)
			if !ok {
				t.Errorf("missing %s -> %s type mapping", src.name, target)
				continue
			}
			if got := mapper(source.ColumnInfo{DataType: src.dataType}, false, false); got != "timestamp without time zone" {
				t.Errorf("%s %s -> %s maps to %q, want %q", src.name, src.dataType, target, got, "timestamp without time zone")
			}
		}
	}
}
