package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"

	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

func partitionContentMetadata() validation.Table {
	return validation.Table{Columns: []validation.Column{{Name: "Id", DatabaseType: "BIGINT", Kind: validation.Number}}}
}

type partitionContentReader struct{ *partitionFlowReader }

func (r *partitionContentReader) DescribeValidationTable(context.Context, string) (validation.Table, error) {
	return partitionContentMetadata(), nil
}
func (r *partitionContentReader) OpenValidationScan(_ context.Context, table string, _ []validation.Column) (validation.RowStream, error) {
	return &contentTestStream{rows: r.rows[table]}, nil
}
func (r *partitionContentReader) OpenPartitionValidationScan(_ context.Context, table string, location validation.PartitionLocation, _ []validation.Column) (validation.RowStream, error) {
	return &contentTestStream{rows: partitionContentLeaf(r.rows[table], location.Name)}, nil
}

type partitionContentWriter struct {
	*partitionFlowWriter
	rows            map[string][][]any
	corruptExisting bool
	scans           int
	scanError       error
}

func (w *partitionContentWriter) DescribeValidationTable(context.Context, string) (validation.Table, error) {
	return partitionContentMetadata(), nil
}
func (w *partitionContentWriter) OpenValidationScan(_ context.Context, table string, _ []validation.Column) (validation.RowStream, error) {
	w.scans++
	if w.scanError != nil {
		return nil, w.scanError
	}
	return &contentTestStream{rows: w.rows[table]}, nil
}
func (w *partitionContentWriter) OpenPartitionValidationScan(_ context.Context, table string, location validation.PartitionLocation, _ []validation.Column) (validation.RowStream, error) {
	w.scans++
	return &contentTestStream{rows: partitionContentLeaf(w.rows[table], location.Name)}, nil
}
func (w *partitionContentWriter) CopyData(ctx context.Context, table string, cols, types []string, rows [][]any) error {
	if err := w.partitionFlowWriter.CopyData(ctx, table, cols, types, rows); err != nil {
		return err
	}
	for _, row := range rows {
		w.rows[table] = append(w.rows[table], slices.Clone(row))
	}
	if w.corruptExisting {
		w.rows[table][0][0] = 3 // unchanged root/leaf counts; changed pre-existing value
	}
	return nil
}

func partitionContentLeaf(rows [][]any, name string) [][]any {
	var result [][]any
	for _, row := range rows {
		low := row[0].(int) < 10
		if low && name == "Low" || !low && name == "High" {
			result = append(result, row)
		}
	}
	return result
}

func TestPartitionContentAppendValidatesRootAndLeavesAgainstFrozenValues(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			m, reader, writer := newPartitionFlow(t, "Root")
			reader.specs["Root"] = partitionFlowSpec()
			// Duplicates are legitimate when no unique constraint exists.
			reader.rows["Root"] = [][]any{{1}, {1}, {11}}
			writer.counts["Root"] = map[string]int64{"Dst.Low": 1, "Dst.High": 1}
			r := &partitionContentReader{reader}
			w := &partitionContentWriter{partitionFlowWriter: writer, rows: map[string][][]any{"Root": {{2}, {12}}}, corruptExisting: corrupt}
			m.reader, m.writer = r, w
			m.cfg.Content, m.cfg.ValidateData = "data_only", true
			report := m.Run(context.Background())
			require.Equal(t, "succeeded", report.PartitionMigration.Tables[0].Status)
			require.Len(t, report.DataValidation.Tables, 3, "root plus every leaf must be checked")
			if corrupt {
				require.Equal(t, validation.StatusMismatch, report.DataValidation.Status)
				require.True(t, report.HasFailures())
			} else {
				require.Equal(t, validation.StatusPassed, report.DataValidation.Status, "%+v", report.DataValidation)
				require.False(t, report.HasFailures(), "%+v", report)
				require.EqualValues(t, 3, report.DataValidation.Tables[0].TargetRows)
			}
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries, "all root and leaf fingerprint files must be removed")
		})
	}
}

func TestPartitionsMetadataSnapshotKeepsOptionalPartitionCapabilities(t *testing.T) {
	m, reader, writer := newPartitionFlow(t, "Root")
	reader.specs["Root"] = partitionFlowSpec()
	r := &partitionContentReader{reader}
	w := &partitionContentWriter{partitionFlowWriter: writer, rows: map[string][][]any{}}
	// SQLServer→Dameng freezes ordinary metadata in this wrapper after partition
	// planning. Nested snapshots must still forward optional I/O to the original.
	inner := &sqlServerDamengSnapshot{Reader: r, tables: reader.ddl}
	m.reader = &sqlServerDamengSnapshot{Reader: inner, tables: reader.ddl}
	m.writer = w
	m.cfg.ValidateData = true
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, []string{"Root"}, reader.streamed)
	require.Zero(t, reader.pageReads["Root"])
	require.Equal(t, "succeeded", report.PartitionMigration.Tables[0].Status)
	require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
	require.Len(t, report.DataValidation.Tables, 3)
}

func TestPartitionAppendBaselineFailureStopsBeforeCopy(t *testing.T) {
	for _, failure := range []string{"unreadable", "changed_count"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			m, reader, writer := newPartitionFlow(t, "Root")
			reader.specs["Root"] = partitionFlowSpec()
			writer.counts["Root"] = map[string]int64{"Dst.Low": 1, "Dst.High": 1}
			w := &partitionContentWriter{partitionFlowWriter: writer, rows: map[string][][]any{"Root": {{2}, {12}}}}
			if failure == "unreadable" {
				w.scanError = errors.New("baseline permission denied")
			} else {
				w.rows["Root"] = append(w.rows["Root"], []any{4})
			}
			before := len(w.rows["Root"])
			m.reader, m.writer = &partitionContentReader{reader}, w
			m.cfg.Content, m.cfg.ValidateData = "data_only", true
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.Equal(t, before, len(w.rows["Root"]))
			require.Empty(t, reader.streamed, "failed baseline must prevent the first source copy cursor")
			require.Empty(t, writer.copied)
			require.Contains(t, report.Data.Items[0].Error, "未写入该表")
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}
