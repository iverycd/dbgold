package datamigrate

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type crossRecoveryWriter struct {
	*oracleRecordingWriter
	configured map[string]*source.TableDDLInfo
	checked    []source.SequenceInfo
	allowed    map[string]string
}

func (*crossRecoveryWriter) SetSourceType(string) {}
func (w *crossRecoveryWriter) ConfigureCrossOracle(tables map[string]*source.TableDDLInfo) {
	w.configured = tables
}
func (w *crossRecoveryWriter) InspectCrossOracleSequence(_ context.Context, s source.SequenceInfo, required bool) error {
	if !required {
		return errors.New("recovery must require every binding")
	}
	w.checked = append(w.checked, s)
	return nil
}
func (w *crossRecoveryWriter) ValidatePGOracleDataTable(_ context.Context, _ string, _ []source.ColumnInfo, allowed map[string]string) error {
	w.allowed = allowed
	return nil
}

func TestCrossOracleRecoveryRechecksSharedGeneratorIdentity(t *testing.T) {
	for _, product := range []string{"dameng", "sqlserver"} {
		t.Run(product, func(t *testing.T) {
			r := crossFixture(product)
			w := &crossRecoveryWriter{oracleRecordingWriter: &oracleRecordingWriter{dia: dialect.NewOracle()}}
			cfg := Config{TargetDBType: "oracle", TargetSchema: "APP"}
			raw := source.SequenceInfo{StartValueText: "20", Native: &source.NativeSequenceInfo{Name: "Shared", Schema: "Src", IncrementBy: "-5", MinValue: "-1000", MaxValue: "1000", CacheSize: "0", Bindings: []source.SequenceBinding{{Schema: "Src", TableName: "A", ColumnName: "Id", Kind: "default"}, {Schema: "Src", TableName: "B", ColumnName: "Id", Kind: "default"}}}}
			r.sequences = []source.SequenceInfo{raw}
			m := NewMigrator(r, w, &Job{}, cfg)
			saved := m.crossOracleSequence(raw, map[string]bool{"A": true, "B": true})
			saved.Native.OracleObjectID = 321
			ctx := WithCrossOracleEvidence(context.Background(), []source.SequenceInfo{saved})
			preview := func() repair.RecoveryPreview {
				return repair.RecoveryPreview{SourceTable: "A", TargetTable: "A", SourceColumns: r.ddl["A"].Columns, ColumnMapping: map[string]string{"Id": "ID"}}
			}
			p := preview()
			require.NoError(t, prepareCrossOracleRecovery(ctx, r, w, cfg, &p, nil))
			require.Len(t, p.CrossOracleSequences, 1)
			require.EqualValues(t, 321, w.checked[0].Native.OracleObjectID)
			require.Len(t, w.checked[0].Native.Bindings, 2)
			require.Len(t, w.allowed, 1)
			require.Equal(t, "ID", w.configured["A"].Columns[0].Name)
			// Source progression may advance, but must be captured by a new preview.
			r.sequences[0].StartValueText = "15"
			p = preview()
			require.NoError(t, prepareCrossOracleRecovery(ctx, r, w, cfg, &p, nil))
			require.Equal(t, "15", p.CrossOracleSequences[0].StartValueText)
			// Removing another consumer changes the full shared binding set.
			r.sequences[0].Native.Bindings = r.sequences[0].Native.Bindings[:1]
			p = preview()
			require.ErrorContains(t, prepareCrossOracleRecovery(ctx, r, w, cfg, &p, nil), "冻结源定义或绑定")
			r.sequences = nil
			p = preview()
			require.ErrorContains(t, prepareCrossOracleRecovery(ctx, r, w, cfg, &p, nil), "冻结源定义或绑定")
		})
	}
}

type crossRecoveryStream struct {
	*oracleFixtureReader
	columns []string
}

func (r *crossRecoveryStream) StreamCrossRows(_ context.Context, _ string, _ int, consume func([]string, []string, [][]any) error) error {
	for _, row := range [][]any{{1}, {2}, {3}} {
		if err := consume(r.columns, []string{"INT"}, [][]any{row}); err != nil {
			return err
		}
	}
	return nil
}
func TestCrossRecoveryChecksColumnsBeforeTruncateAndCountsCommittedPages(t *testing.T) {
	for _, badColumns := range []bool{false, true} {
		t.Run(map[bool]string{false: "page_failure", true: "changed_columns"}[badColumns], func(t *testing.T) {
			r := &crossRecoveryStream{oracleFixtureReader: crossFixture("sqlserver"), columns: []string{"Id"}}
			if badColumns {
				r.columns = []string{"Different"}
			}
			w := &recoveryTestWriter{mockWriter: &mockWriter{}, failWrite: 2}
			truncated := 0
			ops := recoveryOps{truncate: func(context.Context, target.Writer, string) error { truncated++; return nil }}
			p := repair.RecoveryPreview{SourceTable: "A", TargetTable: "A", SourceRows: 3, SourceColumns: r.ddl["A"].Columns, ColumnMapping: map[string]string{"Id": "ID"}}
			result := repair.RecoveryResult{}
			err := recoverCrossRows(context.Background(), r, w, Config{PageSize: 1}, p, true, nil, ops, &result)
			if badColumns {
				require.ErrorContains(t, err, "列顺序")
				require.Zero(t, truncated)
				require.Zero(t, w.writes)
			} else {
				require.ErrorContains(t, err, "已确认提交 1 行")
				require.Equal(t, 1, truncated)
				require.Equal(t, 2, w.writes)
				require.EqualValues(t, 1, result.CommittedRows)
				require.Equal(t, []string{"ID"}, w.columns)
			}
		})
	}
}
