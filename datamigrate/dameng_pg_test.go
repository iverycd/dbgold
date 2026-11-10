package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestDamengPGDefaultKeepsDataAndDependentObjects(t *testing.T) {
	for _, dst := range []string{"postgres", "gaussdb", "seabox", "highgo", "vastbase", "gbase", "kingbase"} {
		t.Run(dst, func(t *testing.T) {
			r, w, m := partialMigrationFixture("dameng", dst, "", false)
			def := "((FALSE))"
			r.ddl["Broken"].Columns[0].Default = &def
			m.cfg.CaptureRepair = true
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), "%+v", report)
			require.Contains(t, strings.Join(w.created, "\n"), `"id" int NOT NULL DEFAULT 0`)
			require.Equal(t, 3, report.Tables.Success)
			require.Equal(t, 3, report.Data.Success)
			require.ElementsMatch(t, []string{"a", "broken", "z"}, w.copied)
			require.Equal(t, 3, report.PrimaryKeys.Success)
			require.Equal(t, 3, report.Indexes.Success)
			require.Equal(t, 3, report.Sequences.Success)
		})
	}
}

func TestDamengPGSequenceMetadataFailureIsIsolatedAndReported(t *testing.T) {
	for _, dst := range []string{"postgres", "gaussdb", "seabox", "highgo", "vastbase", "gbase", "kingbase"} {
		for _, lower := range []bool{false, true} {
			for _, capture := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/lower=%t/capture=%t", dst, lower, capture), func(t *testing.T) {
					r, w, m := partialMigrationFixture("dameng", dst, "", false)
					m.cfg.CaptureRepair, m.cfg.LowerCaseNames, m.cfg.TargetSchema = capture, lower, "TargetSchema"
					r.sequences[0].Unsupported = `读取自增列 "Source"."A"."Id" 状态失败: Error -7019: 无效的表名`
					r.sequences[0].FailureStage = "metadata"
					r.sequences[0].StartValue = 0
					report := m.Run(context.Background())
					require.True(t, report.HasFailures())
					require.Equal(t, 3, report.Tables.Success)
					require.Equal(t, 3, report.Data.Success)
					require.Equal(t, 3, report.PrimaryKeys.Success)
					require.Equal(t, 3, report.Indexes.Success)
					require.Equal(t, 3, report.Sequences.Total)
					require.False(t, report.Sequences.TotalUnknown)
					require.Equal(t, 2, report.Sequences.Success)
					require.Equal(t, 1, report.Sequences.Failed)
					require.Len(t, w.sequences, 2)
					table, column := "A", "Id"
					if lower {
						table, column = "a", "id"
					}
					for _, seq := range w.sequences {
						require.NotEqual(t, table, seq.TableName, "failed metadata must never reach the writer")
					}
					require.Len(t, report.Sequences.Items, 1)
					item := report.Sequences.Items[0]
					require.Equal(t, "metadata", item.Stage)
					require.Equal(t, "A", item.Table)
					require.Equal(t, table, item.TargetTable)
					require.Equal(t, "TargetSchema", item.TargetSchema)
					require.Equal(t, "seq_"+table+"_"+column, item.TargetName)
					require.Equal(t, r.sequences[0].Unsupported, item.Error)
					require.Empty(t, item.DDL)
					if capture {
						require.NotNil(t, report.RepairManifest)
						found := false
						for _, obj := range report.RepairManifest.Objects {
							if obj.Kind == "sequence" && obj.Table == table {
								found = true
								require.Equal(t, "failed", obj.Status)
								require.Empty(t, obj.DDL)
								require.Empty(t, obj.Units)
								require.NotNil(t, obj.Sequence)
								require.Equal(t, "metadata", obj.Sequence.FailureStage)
							}
						}
						require.True(t, found)
					}
				})
			}
		}
	}
}

func TestDamengPGFirstOrMiddleTableFailureContinues(t *testing.T) {
	for _, first := range []bool{false, true} {
		for _, dst := range []string{"postgres", "gaussdb", "seabox", "highgo", "vastbase", "gbase", "kingbase"} {
			t.Run(fmt.Sprintf("%s/first=%t", dst, first), func(t *testing.T) {
				_, w, m := partialMigrationFixture("dameng", dst, "create", first)
				report := m.Run(context.Background())
				require.Equal(t, 2, report.Tables.Success)
				require.Equal(t, 1, report.Tables.Failed)
				require.Equal(t, 2, report.Data.Success)
				require.ElementsMatch(t, []string{"a", "z"}, w.copied)
				for _, category := range []CategoryReport{report.PrimaryKeys, report.Indexes, report.Sequences} {
					require.Equal(t, 2, category.Success)
					require.Equal(t, 1, category.Failed)
					require.Equal(t, "dependency", category.Items[0].Stage)
				}
			})
		}
	}
}

func TestDamengPGRecoveryRenameDoesNotInventSequenceDDL(t *testing.T) {
	table := repair.Object{Kind: "table", Table: "t", ColumnMapping: map[string]string{"Id": "id"}}
	obj := repair.Object{Kind: "sequence", Table: "t", Status: "failed", Sequence: &source.SequenceInfo{
		TableName: "t", ColumnName: "id", Unsupported: "自增列当前值为空", FailureStage: "metadata",
	}}
	got := regenerateRepairCandidate(&mockWriter{}, Config{}, table, obj, map[string]string{"Id": "new_id"})
	require.Empty(t, got.SuggestedDDL)
	require.Empty(t, got.DDL)
	require.Empty(t, got.Units)
	require.Equal(t, obj.Sequence, got.Sequence)
	require.Equal(t, "failed", got.Status)
}
