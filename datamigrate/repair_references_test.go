package datamigrate

import (
	"testing"

	"dbgold/datamigrate/repair"
	"github.com/stretchr/testify/require"
)

func TestRepairReferencesPreserveDraftsAndDisambiguateTables(t *testing.T) {
	report := newMigrationReport()
	report.Indexes.Items = []ObjectResult{
		{Name: "ix", Table: "A", Stage: "unsupported", SourceDDL: "source A", Error: "unsupported"},
		{Name: "ix", Table: "B", Stage: "unsupported", SourceDDL: "source B", Error: "unsupported"},
	}
	report.Tables.Items = []ObjectResult{{Name: "元数据读取", Stage: "metadata", Error: "connection lost"}, {Name: "源表列表", Error: "connection lost"}}
	objects := []repair.Object{
		{Kind: "index", Name: "ix", SourceTable: "B", Table: "b", TargetName: "ix", Schema: "app", Status: "draft", Units: []repair.Unit{{SQL: "manual target SQL"}}},
		{Kind: "index", Name: "ix", TargetName: "ix"}, // Ambiguous old report: don't guess.
		{Kind: "table", Name: "元数据读取", TargetName: "元数据读取", Schema: "app"},
		{Kind: "table", Name: "源表列表", TargetName: "源表列表", Schema: "app"},
	}
	EnrichRepairReferences(objects, report)
	require.Equal(t, "source B", objects[0].SourceDDL)
	require.Equal(t, "draft", objects[0].Status)
	require.Equal(t, "manual target SQL", objects[0].Units[0].SQL)
	require.Empty(t, objects[1].SourceDDL)
	require.True(t, objects[2].DiagnosticOnly)
	require.True(t, objects[3].DiagnosticOnly)
	require.False(t, objects[2].CanEditDDL())
	legacy := LegacyRepairObjects(report, "app")
	for _, object := range legacy {
		if object.Kind == "index" {
			require.Empty(t, object.DDL)
			require.Empty(t, object.Units)
			require.NotEmpty(t, object.SourceDDL)
			require.True(t, object.CanEditDDL())
		}
	}
}
