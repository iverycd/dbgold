package datamigrate

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

type recoveryContentReader struct {
	*partitionRecoveryReader
	describes, scans   int
	drift, unsupported bool
}

func (r *recoveryContentReader) DescribeValidationTable(context.Context, string) (validation.Table, error) {
	r.describes++
	meta := partitionContentMetadata()
	if r.drift && r.describes > 1 {
		meta.Columns[0].DatabaseType = "VARCHAR"
	}
	if r.unsupported {
		meta.Columns[0].Kind = validation.Unsupported
	}
	return meta, nil
}
func (r *recoveryContentReader) OpenValidationScan(_ context.Context, table string, _ []validation.Column) (validation.RowStream, error) {
	r.scans++
	return &contentTestStream{rows: r.rows[table]}, nil
}
func (r *recoveryContentReader) OpenPartitionValidationScan(_ context.Context, table string, location validation.PartitionLocation, _ []validation.Column) (validation.RowStream, error) {
	r.scans++
	return &contentTestStream{rows: partitionContentLeaf(r.rows[table], location.Name)}, nil
}

type recoveryContentWriter struct {
	*partitionRecoveryWriter
	rows                          map[string][][]any
	scans                         int
	corrupt, wrongLeaf, scanError bool
	cancel                        context.CancelFunc
}

func (w *recoveryContentWriter) DescribeValidationTable(context.Context, string) (validation.Table, error) {
	return partitionContentMetadata(), nil
}
func (w *recoveryContentWriter) OpenValidationScan(_ context.Context, table string, _ []validation.Column) (validation.RowStream, error) {
	w.scans++
	if w.scanError {
		return nil, errors.New("validation cursor denied")
	}
	return &contentTestStream{rows: w.rows[table]}, nil
}
func (w *recoveryContentWriter) OpenPartitionValidationScan(_ context.Context, table string, location validation.PartitionLocation, _ []validation.Column) (validation.RowStream, error) {
	w.scans++
	if w.cancel != nil {
		w.cancel()
	}
	name := location.Name
	if w.wrongLeaf && name == "Low" {
		name = "High"
	}
	return &contentTestStream{rows: partitionContentLeaf(w.rows[table], name)}, nil
}
func (w *recoveryContentWriter) CopyData(ctx context.Context, table string, columns, types []string, rows [][]any) error {
	if err := w.partitionRecoveryWriter.CopyData(ctx, table, columns, types, rows); err != nil {
		return err
	}
	for _, row := range rows {
		w.rows[table] = append(w.rows[table], slices.Clone(row))
	}
	if w.corrupt {
		w.rows[table][0][0] = 2
	}
	return nil
}

func TestPartitionRecoveryContentValidationControlsCompletion(t *testing.T) {
	for _, scenario := range []string{"passed", "root_mismatch", "leaf_mismatch", "source_drift", "scan_error", "cancelled", "unsupported", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			reader, writer, cfg, object, ops, truncates := partitionRecoveryFixture(t)
			writer.counts["Root"] = map[string]int64{"Dst.Low": 4, "Dst.High": 3}
			r := &recoveryContentReader{partitionRecoveryReader: reader}
			w := &recoveryContentWriter{partitionRecoveryWriter: writer, rows: map[string][][]any{"Root": {{2}, {2}, {2}, {2}, {12}, {12}, {12}}}}
			truncate := ops.truncate
			ops.truncate = func(ctx context.Context, target target.Writer, table string) error {
				if err := truncate(ctx, target, table); err != nil {
					return err
				}
				w.rows[table] = nil
				return nil
			}
			cfg.ValidateData = scenario != "disabled"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "root_mismatch", "disabled":
				w.corrupt = true
			case "leaf_mismatch":
				w.wrongLeaf = true
			case "source_drift":
				r.drift = true
			case "scan_error":
				w.scanError = true
			case "cancelled":
				w.cancel = cancel
			case "unsupported":
				r.unsupported = true
			}
			preview, err := previewRecovery(ctx, r, w, cfg, object, nil, nil, ops)
			require.NoError(t, err)
			var persisted []repair.RecoveryResult
			result, err := runRecovery(ctx, r, w, cfg, preview, true, func(result repair.RecoveryResult) error { persisted = append(persisted, result); return nil }, ops)
			require.NotEmpty(t, persisted)
			require.Equal(t, result, persisted[len(persisted)-1], "terminal validation diagnostics must be durable")
			if scenario == "passed" || scenario == "disabled" {
				require.NoError(t, err)
				require.Equal(t, "succeeded", result.Status)
				require.Equal(t, "succeeded", result.DataStatus)
				if scenario == "passed" {
					require.Equal(t, 3, w.scans, "root plus both leaves")
				} else {
					require.Zero(t, w.scans)
					require.Zero(t, r.scans)
				}
			} else {
				require.Error(t, err)
				require.NotEqual(t, "succeeded", result.Status)
				require.NotEqual(t, "succeeded", result.DataStatus)
				require.NotEmpty(t, result.Error)
				if scenario == "unsupported" {
					require.Zero(t, *truncates)
					require.Zero(t, w.attempts)
					require.Len(t, w.rows["Root"], 7)
				} else {
					require.EqualValues(t, 2, result.CommittedRows, "failed validation must retain completed copy evidence")
					if scenario == "leaf_mismatch" {
						require.True(t, strings.Contains(result.Error, "PARTITION Src.Low"), result.Error)
					}
				}
			}
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestPartitionRecoveryEnabledValidationRequiresScannersBeforeTruncate(t *testing.T) {
	r, w, cfg, object, ops, truncates := partitionRecoveryFixture(t)
	cfg.ValidateData = true
	w.counts["Root"] = map[string]int64{"Dst.Low": 4, "Dst.High": 3}
	preview, err := previewRecovery(context.Background(), r, w, cfg, object, nil, nil, ops)
	require.NoError(t, err)
	result, err := runRecovery(context.Background(), r, w, cfg, preview, true, nil, ops)
	require.ErrorContains(t, err, "支持根表及逐叶扫描")
	require.NotEqual(t, "succeeded", result.DataStatus)
	require.Zero(t, *truncates)
	require.Zero(t, w.attempts)
}

type recoveryCancelCountReader struct {
	*partitionRecoveryReader
	afterCount func()
}

func (r *recoveryCancelCountReader) CountRows(ctx context.Context, table string) (int64, error) {
	n, err := r.partitionRecoveryReader.CountRows(ctx, table)
	r.afterCount()
	return n, err
}

func TestPartitionRecoveryLateCancellationCannotFinishSuccessfully(t *testing.T) {
	for _, stage := range []string{"final_count", "calibration"} {
		t.Run(stage, func(t *testing.T) {
			reader, w, cfg, object, ops, _ := partitionRecoveryFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &recoveryCancelCountReader{partitionRecoveryReader: reader, afterCount: func() {}}
			if stage == "final_count" {
				r.afterCount = func() {
					if w.attempts > 0 {
						cancel()
					}
				}
			} else {
				ops.calibrate = func(context.Context, target.Writer, string, []source.SequenceInfo) error { cancel(); return nil }
			}
			preview, err := previewRecovery(ctx, r, w, cfg, object, nil, nil, ops)
			require.NoError(t, err)
			var terminal repair.RecoveryResult
			result, err := runRecovery(ctx, r, w, cfg, preview, false, func(progress repair.RecoveryResult) error { terminal = progress; return nil }, ops)
			require.ErrorIs(t, err, context.Canceled)
			require.Equal(t, "unknown", result.Status)
			require.Equal(t, result, terminal)
			if stage == "final_count" {
				require.NotEqual(t, "succeeded", result.DataStatus)
			}
		})
	}
}
