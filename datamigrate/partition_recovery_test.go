package datamigrate

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type partitionRecoveryReader struct {
	*partitionFlowReader
	stream func(context.Context, string, int, func([]string, []string, [][]any) error) error
}

func (r *partitionRecoveryReader) StreamPartitionRows(ctx context.Context, table string, batch int, consume func([]string, []string, [][]any) error) error {
	if r.stream != nil {
		return r.stream(ctx, table, batch, consume)
	}
	return r.partitionFlowReader.StreamPartitionRows(ctx, table, batch, consume)
}

type partitionRecoveryWriter struct {
	*partitionFlowWriter
	loseAcknowledgement bool
	attempts            int
}

func (w *partitionRecoveryWriter) CopyData(ctx context.Context, table string, cols, types []string, rows [][]any) error {
	w.attempts++
	if err := w.partitionFlowWriter.CopyData(ctx, table, cols, types, rows); err != nil {
		return err
	}
	if w.loseAcknowledgement {
		return errors.New("connection lost after server commit")
	}
	return nil
}

func partitionRecoveryFixture(t *testing.T) (*partitionRecoveryReader, *partitionRecoveryWriter, Config, repair.Object, recoveryOps, *int) {
	t.Helper()
	m, reader, writer := newPartitionFlow(t, "Root")
	reader.specs["Root"] = partitionFlowSpec()
	preflight := m.PreflightPartitions(context.Background())
	require.True(t, preflight.Ready, "%+v", preflight.Issues)
	require.Len(t, preflight.Tables, 1)
	writer.counts["Root"] = map[string]int64{"Dst.Low": 0, "Dst.High": 0}
	obj := repair.Object{ID: "partition-root", Kind: "table", SourceTable: "Root", SourceName: "Root", Table: "Root", TargetName: "Root", SourceColumns: reader.ddl["Root"].Columns, ColumnMapping: map[string]string{"Id": "Id"}, DataStatus: "pending", PartitionPlan: preflight.Tables[0].Plan}
	truncates := new(int)
	ops := recoveryOps{
		schema: func(context.Context, target.Writer) (string, error) { return "Dst", nil },
		inspect: func(context.Context, target.Writer, string) ([]source.ColumnInfo, error) {
			return []source.ColumnInfo{{Name: "Id", DataType: "bigint", IsNullable: false}}, nil
		},
		prepare: func(context.Context, target.Writer, string, []source.ColumnInfo) error { return nil },
		truncate: func(context.Context, target.Writer, string) error {
			*truncates++
			writer.counts["Root"] = map[string]int64{"Dst.Low": 0, "Dst.High": 0}
			return nil
		},
		calibrate: func(context.Context, target.Writer, string, []source.SequenceInfo) error { return nil },
	}
	return &partitionRecoveryReader{partitionFlowReader: reader}, &partitionRecoveryWriter{partitionFlowWriter: writer}, m.cfg, obj, ops, truncates
}

func TestPartitionRecoveryOldLedgerCannotAdoptCurrentPartitionDefinition(t *testing.T) {
	r, w, cfg, obj, ops, truncates := partitionRecoveryFixture(t)
	obj.PartitionPlan = nil
	_, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.ErrorContains(t, err, "旧账本缺少分区快照")
	require.Zero(t, *truncates)
	require.Zero(t, w.attempts)
}

func TestPartitionRecoverySavedSourceDriftIsRejectedBeforeMutation(t *testing.T) {
	r, w, cfg, obj, ops, truncates := partitionRecoveryFixture(t)
	r.specs["Root"].Partitions[0].Upper[0].Text = "9"
	_, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.ErrorContains(t, err, "源分区定义已漂移")
	require.Zero(t, *truncates)
	require.Zero(t, w.attempts)
}

func TestPartitionRecoveryMissingTargetLeafStopsBeforeTruncate(t *testing.T) {
	r, w, cfg, obj, ops, truncates := partitionRecoveryFixture(t)
	w.counts["Root"] = map[string]int64{"Dst.Low": 2, "Dst.High": 1}
	preview, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.NoError(t, err)
	missing := source.ClonePartitionSpec(obj.PartitionPlan.Target)
	missing.Partitions = missing.Partitions[:1]
	w.actual = func(string) *source.PartitionSpec { return missing }
	_, err = runRecovery(context.Background(), r, w, cfg, preview, true, nil, ops)
	require.ErrorContains(t, err, "先补齐目标分区结构")
	require.Zero(t, *truncates)
	require.Zero(t, w.attempts)
	require.Equal(t, int64(2), w.counts["Root"]["Dst.Low"])
}

func TestPartitionRecoveryLeafCountsAreBoundEvenWhenTotalDoesNotChange(t *testing.T) {
	for _, side := range []string{"source", "target"} {
		t.Run(side, func(t *testing.T) {
			r, w, cfg, obj, ops, truncates := partitionRecoveryFixture(t)
			w.counts["Root"] = map[string]int64{"Dst.Low": 1, "Dst.High": 1}
			first, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
			require.NoError(t, err)
			if side == "source" {
				r.rows["Root"] = [][]any{{11}, {12}}
			} else {
				w.counts["Root"] = map[string]int64{"Dst.Low": 0, "Dst.High": 2}
			}
			fresh, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
			require.NoError(t, err)
			require.Equal(t, first.SourceRows, fresh.SourceRows)
			require.Equal(t, first.TargetRows, fresh.TargetRows)
			require.NotEqual(t, first.Fingerprint, fresh.Fingerprint)
			_, err = runRecovery(context.Background(), r, w, cfg, first, true, nil, ops)
			require.ErrorContains(t, err, "逐分区行数已变化")
			require.Zero(t, *truncates)
			require.Zero(t, w.attempts)
		})
	}
}

func TestPartitionRecoveryReadsAndValidatesFirstBatchBeforeTruncating(t *testing.T) {
	for _, failure := range []string{"cursor open", "column drift", "empty cursor", "progress persistence"} {
		t.Run(failure, func(t *testing.T) {
			r, w, cfg, obj, ops, truncates := partitionRecoveryFixture(t)
			w.counts["Root"] = map[string]int64{"Dst.Low": 2, "Dst.High": 1}
			preview, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
			require.NoError(t, err)
			var progress func(repair.RecoveryResult) error
			switch failure {
			case "cursor open":
				r.stream = func(context.Context, string, int, func([]string, []string, [][]any) error) error {
					return errors.New("cursor denied")
				}
			case "column drift":
				r.stream = func(_ context.Context, _ string, _ int, consume func([]string, []string, [][]any) error) error {
					return consume([]string{"Other"}, []string{"BIGINT"}, [][]any{{1}})
				}
			case "empty cursor":
				r.stream = func(context.Context, string, int, func([]string, []string, [][]any) error) error { return nil }
			case "progress persistence":
				progress = func(repair.RecoveryResult) error { return errors.New("ledger unavailable") }
			}
			_, err = runRecovery(context.Background(), r, w, cfg, preview, true, progress, ops)
			require.Error(t, err)
			require.Zero(t, *truncates, "failure before a validated first batch must preserve old target data")
			require.Zero(t, w.attempts)
			require.Equal(t, int64(2), w.counts["Root"]["Dst.Low"])
		})
	}
}

func TestPartitionRecoveryUnknownCommittedWriteCannotBeBlindlyAppended(t *testing.T) {
	r, w, cfg, obj, ops, truncates := partitionRecoveryFixture(t)
	w.loseAcknowledgement = true
	preview, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.NoError(t, err)
	result, err := runRecovery(context.Background(), r, w, cfg, preview, false, nil, ops)
	require.ErrorContains(t, err, "禁止直接追加重试")
	require.Equal(t, "unknown", result.Status)
	require.Equal(t, int64(0), result.CommittedRows, "lost acknowledgement is not proven committed progress")
	require.Equal(t, 1, w.attempts)
	require.Equal(t, int64(1), w.counts["Root"]["Dst.Low"], "server may already have committed the first batch")
	w.loseAcknowledgement = false
	newPreview, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.NoError(t, err)
	_, err = runRecovery(context.Background(), r, w, cfg, newPreview, false, nil, ops)
	require.ErrorContains(t, err, "目标表非空")
	require.Equal(t, 1, w.attempts)
	require.Zero(t, *truncates)
}

func TestPartitionRecoveryStreamsOneRootAndVerifiesEveryLeafAfterReload(t *testing.T) {
	r, w, cfg, obj, ops, truncates := partitionRecoveryFixture(t)
	w.counts["Root"] = map[string]int64{"Dst.Low": 4, "Dst.High": 3}
	preview, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.NoError(t, err)
	result, err := runRecovery(context.Background(), r, w, cfg, preview, true, nil, ops)
	require.NoError(t, err)
	require.Equal(t, "succeeded", result.Status)
	require.Equal(t, "succeeded", result.DataStatus)
	require.Equal(t, int64(2), result.CommittedRows)
	require.Equal(t, 1, *truncates)
	require.Equal(t, []string{"Root"}, r.streamed)
	require.Zero(t, r.pageReads["Root"])
	require.Equal(t, map[string]int64{"Dst.Low": 1, "Dst.High": 1}, w.counts["Root"])
}
