package datamigrate

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"testing"

	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

type mysqlDMStrategyWriter struct {
	*fullScanTestWriter
	product string
	batches atomic.Int32
}

func (w *mysqlDMStrategyWriter) DBType() string { return w.product }

func (w *mysqlDMStrategyWriter) CopyMySQLBatchData(ctx context.Context, table string, cols, types []string, rows [][]any) error {
	w.batches.Add(1)
	// The ordinary reader path must pass the identical metadata and row values.
	return w.fullScanTestWriter.CopyData(ctx, table, cols, types, rows)
}

type nonMySQLStrategyReader struct{ *fullScanTestReader }

func (r *nonMySQLStrategyReader) DBType() string { return "postgres" }

func TestMySQLDamengWriteStrategyOrdinaryTable(t *testing.T) {
	for _, tc := range []struct {
		name, read, write, dst, purpose string
		readers                         int
		mysql                           bool
		batches                         int32
	}{
		{"serial offset batch", "offset", "batch", "dameng", "", 1, true, 3},
		{"parallel offset batch", "offset", "batch", "dameng", "", 2, true, 3},
		{"stream batch", "stream", "batch", "dameng", "", 2, true, 3},
		{"default row", "offset", "", "dameng", "", 2, true, 0},
		{"explicit row", "stream", "row", "dameng", "", 2, true, 0},
		{"cdc", "stream", "batch", "dameng", "cdc", 1, true, 0},
		{"another target", "stream", "batch", "gaussdb", "", 2, true, 0},
		{"another source", "offset", "batch", "dameng", "", 2, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, r, w := newFullScanTest(tc.read, "MixedCase")
			m.cfg.IntraTableParallel = tc.readers
			m.cfg.DamengWriteStrategy, m.cfg.MigrationPurpose = tc.write, tc.purpose
			if !tc.mysql {
				m.reader = &nonMySQLStrategyReader{r}
			}
			observed := &mysqlDMStrategyWriter{fullScanTestWriter: w, product: tc.dst}
			m.writer = observed
			ok, why := m.migrateTableData(context.Background(), "MixedCase")
			require.True(t, ok, why)
			require.Equal(t, tc.batches, observed.batches.Load())
			sort.Ints(w.data["mixedcase"])
			require.Equal(t, []int{1, 2, 3, 4, 5, 6}, w.data["mixedcase"])
		})
	}
}

type mysqlDMPartitionWriter struct{ *partitionFlowWriter }

func (w *mysqlDMPartitionWriter) DBType() string { return "dameng" }
func (w *mysqlDMPartitionWriter) CopyMySQLBatchData(context.Context, string, []string, []string, [][]any) error {
	return errors.New("ordinary batch writer used on a partition")
}

func TestMySQLDamengWriteStrategySkipsPartitionCopy(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Root")
	r.specs["Root"] = partitionFlowSpec()
	p := m.PreflightPartitions(context.Background())
	require.True(t, p.Ready)
	m.writer = &mysqlDMPartitionWriter{w}
	m.cfg.DamengWriteStrategy, m.cfg.MySQLReadStrategy = "batch", "stream"
	ok, why := m.migrateTableData(context.Background(), "Root")
	require.True(t, ok, why)
	require.Equal(t, []string{"Root"}, r.streamed)
	rows, err := w.CountRows(context.Background(), "Root")
	require.NoError(t, err)
	require.EqualValues(t, 2, rows)
}

type dmCaseValidationWriter struct{ *contentTestWriter }

func (w *dmCaseValidationWriter) DBType() string { return "dameng" }

func TestMySQLDamengContentValidationUsesPhysicalTargetNames(t *testing.T) {
	r, w, m := contentFixture()
	// The target uppercase table also has a reserved source column remapped by
	// the normal Dameng column planner.
	r.metadata.Columns[1].Name = "RowId"
	r.ddl["Users"].Columns[1].Name = "RowId"
	w.metadata.Columns[0].Name, w.metadata.Columns[1].Name = "ID", "ROW_ID"
	w.metadata.PrimaryKey = []string{"ID"}
	w.rows["USERS"] = r.rows["Users"]
	m.writer = &dmCaseValidationWriter{w}
	ctx := context.Background()
	_, err := m.planDamengTable(ctx, "Users")
	require.NoError(t, err)
	m.freezeDataValidation(ctx, []string{"Users"})
	m.markDataValidationCopied("Users")
	report := MigrationReport{DataValidation: &validation.Report{}}
	m.validateDataContent(ctx, &report)
	require.Equal(t, validation.StatusPassed, report.DataValidation.Status, "%+v", report.DataValidation)
	require.Len(t, report.DataValidation.Tables, 1)
	row := report.DataValidation.Tables[0]
	require.Equal(t, "Users", row.SourceTable)
	require.Equal(t, "USERS", row.TargetTable)
	require.EqualValues(t, 1, row.SourceRows)
	require.Equal(t, row.SourceRows, row.TargetRows)
}
