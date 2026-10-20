package datamigrate

import (
	"context"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type oracleDMSequenceRecoveryReader struct{ *recoveryTestReader }

func (*oracleDMSequenceRecoveryReader) DBType() string { return "oracle" }

type oracleDMSequenceRecoveryWriter struct{ *recoveryTestWriter }

func (*oracleDMSequenceRecoveryWriter) DBType() string           { return "dameng" }
func (*oracleDMSequenceRecoveryWriter) Dialect() dialect.Dialect { return dialect.NewDaMeng() }

func TestOracleDamengRecoveryDefersInvalidNativeSequenceUntilRepair(t *testing.T) {
	for _, invalid := range []string{"missing metadata", "invalid step", "empty intersection"} {
		for _, deferred := range []bool{false, true} {
			name := invalid + "/calibrate"
			if deferred {
				name = invalid + "/defer"
			}
			t.Run(name, func(t *testing.T) {
				baseReader, baseWriter, cfg, table, ops, _ := recoveryFixture(t)
				r := &oracleDMSequenceRecoveryReader{baseReader}
				w := &oracleDMSequenceRecoveryWriter{baseWriter}
				seq := source.SequenceInfo{
					TableName: table.SourceTable, ColumnName: "ID", StartValueText: "1",
					Native: &source.NativeSequenceInfo{
						Name: "S", IncrementBy: "1", MinValue: "1", MaxValue: "1000", CacheSize: "0",
						Bindings: []source.SequenceBinding{{TableName: table.SourceTable, ColumnName: "ID", Kind: "trigger", TriggerName: "TRG"}},
					},
				}
				var wantError string
				switch invalid {
				case "missing metadata":
					seq.Unsupported, seq.FailureStage = "源序列 S 元数据不可见", "metadata"
					wantError = seq.Unsupported
				case "invalid step":
					seq.Native.IncrementBy = "0"
					wantError = "起值、步长或边界无效"
				case "empty intersection":
					seq.Native.MinValue = "9223372036854775808"
					seq.Native.MaxValue = "999999999999999999999999999"
					wantError = "最小值超出达梦 int64"
				}
				r.sequences = []source.SequenceInfo{seq}
				original := seq
				native := *seq.Native
				native.Bindings = append([]source.SequenceBinding(nil), native.Bindings...)
				original.Native = &native

				var related []repair.Object
				if deferred {
					pending := seq
					n := *seq.Native
					n.Bindings = []source.SequenceBinding{{TableName: table.Table, ColumnName: "renamed", SourceColumnName: "ID", Kind: "trigger", TriggerName: "TRG"}}
					pending.Native, pending.TableName, pending.ColumnName = &n, table.Table, "renamed"
					related = []repair.Object{{Kind: "sequence", Name: "S", Table: table.Table, TargetName: "S", Status: "pending", Sequence: &pending}}
				}

				calibrateCalls := 0
				ops.calibrate = func(_ context.Context, _ target.Writer, targetTable string, sequences []source.SequenceInfo) error {
					calibrateCalls++
					require.Equal(t, table.Table, targetTable)
					if deferred {
						require.Empty(t, sequences, "pending sequence repair must be deferred before validating calibration metadata")
						return nil
					}
					require.Len(t, sequences, 1, "invalid nondeferred metadata must reach calibration")
					require.Equal(t, seq.Unsupported, sequences[0].Unsupported)
					require.Equal(t, seq.Native.MaxValue, sequences[0].Native.MaxValue)
					_, err := dialect.NormalizeOracleDamengSequence(sequences[0])
					return err
				}
				preview, err := previewRecovery(context.Background(), r, w, cfg, table, related, nil, ops)
				require.NoError(t, err)
				result, err := runRecovery(context.Background(), r, w, cfg, preview, false, nil, ops)
				require.Equal(t, 1, calibrateCalls)
				require.Equal(t, "succeeded", result.DataStatus)
				require.Equal(t, int64(5), result.CommittedRows)
				require.Equal(t, original, r.sequences[0], "recovery must preserve source sequence metadata")
				if deferred {
					require.NoError(t, err)
					require.Equal(t, "succeeded", result.Status)
				} else {
					require.ErrorContains(t, err, wantError)
					require.Equal(t, "failed", result.Status)
				}
			})
		}
	}
}
