package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func oracleDMFlowSequence() source.SequenceInfo {
	return source.SequenceInfo{TableName: "Mixed", ColumnName: "RowId", StartValueText: "1",
		Native: &source.NativeSequenceInfo{Name: "SharedSeq", IncrementBy: "5", MinValue: "1", MaxValue: "9999999999999999999999999999", CacheSize: "20",
			Bindings: []source.SequenceBinding{
				{TableName: "Mixed", ColumnName: "RowId", Kind: "trigger", TriggerName: "MixedTrigger", OnlyWhenNull: true},
			},
		},
	}
}

func oracleDMFlowSnapshot(s source.SequenceInfo) source.SequenceInfo {
	n := *s.Native
	n.Bindings = slices.Clone(n.Bindings)
	s.Native = &n
	return s
}

func TestOracleDamengSequenceBoundsAcrossMigrationFlows(t *testing.T) {
	for _, content := range []string{"schema_only", "both", "data_only"} {
		for _, limit := range []struct{ name, value string }{
			{"28_digit_default", "9999999999999999999999999999"},
			{"27_digit_limit", "999999999999999999999999999"},
			{"24_digit_custom", "123456789012345678901234"},
		} {
			t.Run(content+"/"+limit.name, func(t *testing.T) {
				r, w, m := oracleDMFixture(content)
				r.tables = append(r.tables, "Other")
				r.ddl["Other"] = &source.TableDDLInfo{TableName: "Other", Columns: slices.Clone(r.ddl["Mixed"].Columns)}
				r.sequences = []source.SequenceInfo{oracleDMFlowSequence()}
				r.sequences[0].Native.MaxValue = limit.value
				r.sequences[0].Native.Bindings = append(r.sequences[0].Native.Bindings, source.SequenceBinding{
					TableName: "Other", ColumnName: "RowId", Kind: "trigger", TriggerName: "OtherTrigger", OnlyWhenNull: true,
				})
				original := oracleDMFlowSnapshot(r.sequences[0])
				m.cfg.CaptureRepair = true
				report := m.Run(context.Background())
				require.False(t, report.HasFailures(), "%+v", report)
				require.Equal(t, 1, report.Sequences.Total)
				require.Equal(t, 1, report.Sequences.Success)
				written := w.sequences
				if content == "data_only" {
					require.Empty(t, written)
					written = w.calibrated
				}
				require.Len(t, written, 1, "shared sequence is processed once")
				require.Equal(t, "9223372036854775807", written[0].Native.MaxValue)
				require.Equal(t, "42", written[0].StartValueText)
				require.Equal(t, "5", written[0].Native.IncrementBy)
				require.Len(t, written[0].Native.Bindings, 2)
				require.Equal(t, original, r.sequences[0], "migration must preserve reader metadata")
				require.NotNil(t, report.RepairManifest)
				objects := 0
				for _, object := range report.RepairManifest.Objects {
					if object.Kind != "sequence" {
						continue
					}
					objects++
					require.Equal(t, "9223372036854775807", object.Sequence.Native.MaxValue)
					if content != "data_only" {
						require.Contains(t, object.DDL, "MAXVALUE 9223372036854775807")
						require.NotContains(t, object.DDL, original.Native.MaxValue)
					}
				}
				require.Equal(t, 1, objects)
				adaptations := 0
				for len(m.job.LogCh) > 0 {
					line := <-m.job.LogCh
					if strings.Contains(line, "Oracle MAXVALUE 已适配") {
						adaptations++
						require.Contains(t, line, "SHAREDSEQ")
						require.Contains(t, line, original.Native.MaxValue+" → 9223372036854775807")
					}
				}
				require.Equal(t, 1, adaptations, "inventory and execution should not duplicate the adaptation warning")
			})
		}
	}
}

type oracleDMSequenceFailureWriter struct {
	*oracleDMWriter
}

func (w *oracleDMSequenceFailureWriter) CreateSequence(context.Context, source.SequenceInfo, string) error {
	return fmt.Errorf("create sequence denied")
}

func (w *oracleDMSequenceFailureWriter) CalibrateOracleDameng(context.Context, source.SequenceInfo) error {
	return fmt.Errorf("alter sequence denied")
}

func TestOracleDamengSequenceFailureReportsStageAndContext(t *testing.T) {
	for _, content := range []string{"schema_only", "data_only"} {
		for _, stage := range []string{"metadata", "preflight", "execute"} {
			t.Run(content+"/"+stage, func(t *testing.T) {
				r, w, m := oracleDMFixture(content)
				seq := oracleDMFlowSequence()
				switch stage {
				case "metadata":
					seq.Unsupported = "源序列 SRC.SharedSeq 不存在或当前账号不可见；引用 Mixed.RowId (MixedTrigger)"
					seq.FailureStage = "metadata"
				case "preflight":
					seq.Native.IncrementBy = "0"
				case "execute":
					m.writer = &oracleDMSequenceFailureWriter{w}
				}
				r.sequences = []source.SequenceInfo{seq}
				report := m.Run(context.Background())
				require.Equal(t, 1, report.Sequences.Failed)
				require.Len(t, report.Sequences.Items, 1)
				item := report.Sequences.Items[0]
				require.Equal(t, stage, item.Stage)
				require.Equal(t, "SharedSeq", item.SourceName)
				require.Equal(t, "SHAREDSEQ", item.TargetName)
				require.Equal(t, "Mixed", item.Table)
				require.Equal(t, "MIXED", item.TargetTable)
				require.Equal(t, "APP", item.TargetSchema)
				if stage == "execute" && content == "schema_only" {
					require.Contains(t, item.DDL, `CREATE SEQUENCE "APP"."SHAREDSEQ"`)
					require.Contains(t, item.DDL, "MAXVALUE 9223372036854775807")
					require.Contains(t, item.DDL, `CREATE TRIGGER "APP"."MIXEDTRIGGER"`)
				} else {
					require.Empty(t, item.DDL, "metadata/preflight failures and calibration must not fabricate CREATE DDL")
				}
			})
		}
	}
}

func TestOracleDamengRecoveryReturnsNormalizedSequenceMetadata(t *testing.T) {
	r, w, m := oracleDMFixture("data_only")
	r.sequences = []source.SequenceInfo{oracleDMFlowSequence()}
	original := oracleDMFlowSnapshot(r.sequences[0])
	values, err := RecoverySequences(context.Background(), r, w, m.cfg, repair.RecoveryPreview{
		SourceTable: "Mixed", TargetTable: "MIXED", ColumnMapping: map[string]string{"RowId": "ROW_ID", "Txt": "TXT"},
	})
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Equal(t, "9223372036854775807", values[0].Native.MaxValue)
	require.Equal(t, "SHAREDSEQ", values[0].Native.Name)
	require.Equal(t, "ROW_ID", values[0].Native.Bindings[0].ColumnName)
	require.Equal(t, original, r.sequences[0])
}
