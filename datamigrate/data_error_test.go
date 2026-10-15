package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	highgopq "dbgold/internal/highgopq"
	ogpq "gitee.com/opengauss/openGauss-connector-go-pq"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestFormatDataWriteErrorPreservesPostgresDiagnostics(t *testing.T) {
	const detail = "A field with precision 5, scale 2 must round to an absolute value less than 10^3."
	const where = `COPY Orders, line 2, column BudgetAmount: "1000.00"`
	for name, err := range map[string]error{
		"postgres": &pq.Error{Code: "22003", Message: "numeric field overflow", Detail: detail, Where: where},
		"gaussdb":  &ogpq.Error{Code: "22003", Message: "numeric field overflow", Detail: detail, Where: where},
		"highgo":   &highgopq.Error{Code: "22003", Message: "numeric field overflow", Detail: detail, Where: where},
	} {
		t.Run(name, func(t *testing.T) {
			// Driver errors may already be wrapped by a target writer.
			got := formatDataWriteError(fmt.Errorf("copy failed: %w", err))
			require.Contains(t, got, "copy failed: pq: numeric field overflow")
			require.Contains(t, got, "SQLSTATE：22003")
			require.Contains(t, got, "详细原因："+detail)
			require.Contains(t, got, where)
			require.Contains(t, got, "line 为本批次内行号，column 为目标列")
		})
	}
}

func TestFormatDataWriteErrorDoesNotInventColumnOrRow(t *testing.T) {
	t.Run("no server context", func(t *testing.T) {
		got := formatDataWriteError(&pq.Error{Code: "22003", Message: "numeric field overflow", Detail: "precision 5, scale 2"})
		require.Contains(t, got, "详细原因：precision 5, scale 2")
		require.NotContains(t, got, "目标列")
		require.NotContains(t, got, "上下文")
	})
	t.Run("explicit column and hint", func(t *testing.T) {
		got := formatDataWriteError(&pq.Error{
			Message: "invalid value", Column: "MixedCaseColumn", Hint: "check input",
			Where: "trigger function, line 7", Line: "99", File: "numeric.c",
		})
		require.Contains(t, got, "目标列：MixedCaseColumn")
		require.Contains(t, got, "提示：check input")
		require.Contains(t, got, "数据库上下文：trigger function, line 7")
		require.NotContains(t, got, "批次内行号")
		require.NotContains(t, got, "99") // Driver Line is a server source-code line.
	})
	t.Run("plain error", func(t *testing.T) {
		require.Equal(t, "connection lost", formatDataWriteError(errors.New("connection lost")))
	})
}

type diagnosticPagingReader struct{ *mockReader }

func (r *diagnosticPagingReader) ReadPage(_ context.Context, table string, _ []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	rows := r.rows[table]
	if offset >= int64(len(rows)) {
		return []string{"Id", "BudgetAmount"}, []string{"INT", "DECIMAL"}, nil, nil
	}
	return []string{"Id", "BudgetAmount"}, []string{"INT", "DECIMAL"}, rows[offset:min(offset+limit, int64(len(rows)))], nil
}

func (r *diagnosticPagingReader) CountRows(_ context.Context, table string) (int64, error) {
	return int64(len(r.rows[table])), nil
}

type diagnosticPagingWriter struct {
	*mockWriter
	err error
}

func (w *diagnosticPagingWriter) CopyData(_ context.Context, _ string, _, _ []string, rows [][]interface{}) error {
	if rows[0][0] == 3 {
		return w.err
	}
	return nil
}

func TestDataWriteDiagnosticsReachReportForSerialAndParallelPages(t *testing.T) {
	for _, parallel := range []int{1, 3} {
		t.Run(fmt.Sprintf("parallel=%d", parallel), func(t *testing.T) {
			const where = `COPY orders, line 2, column budgetamount: "1000.00"`
			reader := &diagnosticPagingReader{&mockReader{
				tables: []string{"Orders"},
				ddl: map[string]*source.TableDDLInfo{"Orders": {
					TableName: "Orders", Columns: []source.ColumnInfo{
						{Name: "Id", DataType: "int"},
						{Name: "BudgetAmount", DataType: "decimal", Precision: 5, Scale: 2},
					},
				}},
				pk: map[string][]string{"Orders": {"Id"}},
				rows: map[string][][]interface{}{"Orders": {
					{1, "1.00"}, {2, "2.00"}, {3, "3.00"}, {4, "1000.00"}, {5, "5.00"}, {6, "6.00"},
				}},
			}}
			writer := &diagnosticPagingWriter{mockWriter: &mockWriter{}, err: &pq.Error{
				Code: "22003", Message: "numeric field overflow", Where: where,
				Detail: "A field with precision 5, scale 2 must round to an absolute value less than 10^3.",
			}}
			m, job := newTestMigrator(reader, writer)
			m.cfg.PageSize, m.cfg.IntraTableParallel, m.cfg.LowerCaseNames = 2, parallel, true
			report := m.Run(context.Background())
			logs := strings.Join(drainLogs(job), "\n")
			require.Equal(t, 1, report.Data.Failed)
			require.Len(t, report.Data.Items, 1)
			item := report.Data.Items[0]
			require.Equal(t, "Orders", item.Name)
			require.Contains(t, item.Error, "表 Orders 第 2 页写入失败")
			require.Contains(t, item.Error, where)
			require.Contains(t, logs, item.Error)
			require.Contains(t, logs, "迁移 Orders: 第 3 页 (2 行) ... OK")
			require.NotContains(t, logs, "迁移 Orders: 第 2 页 (2 行) ... OK")
			// The persisted report must retain all diagnostics for later viewing.
			encoded, err := json.Marshal(report)
			require.NoError(t, err)
			var restored MigrationReport
			require.NoError(t, json.Unmarshal(encoded, &restored))
			require.Equal(t, item.Error, restored.Data.Items[0].Error)
		})
	}
}
