package datamigrate

import (
	"context"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

type mvValidationScanner struct {
	metadata validation.Table
	rows     [][]any
}

func (s *mvValidationScanner) DescribeValidationTable(context.Context, string) (validation.Table, error) {
	return s.metadata, nil
}
func (s *mvValidationScanner) OpenValidationScan(_ context.Context, _ string, columns []validation.Column) (validation.RowStream, error) {
	var rows [][]any
	for _, row := range s.rows {
		var projected []any
		for _, want := range columns {
			for i, actual := range s.metadata.Columns {
				if actual.Name == want.Name {
					projected = append(projected, row[i])
					break
				}
			}
		}
		rows = append(rows, projected)
	}
	return &contentTestStream{rows: rows}, nil
}

func TestNativeRowIDProjectionRetainsAllOrdinaryValuesAndDuplicates(t *testing.T) {
	columns := []validation.Column{{Name: "Value", Kind: validation.Number, DatabaseType: "INT"}, {Name: "Rid", Kind: validation.Text, DatabaseType: "ROWID"}}
	metadata := validation.Table{Columns: columns, PrimaryKey: []string{"Rid"}}
	src := &mvValidationScanner{metadata: metadata, rows: [][]any{{int64(10), "old1"}, {int64(10), "old2"}}}
	dst := &mvValidationScanner{metadata: metadata, rows: [][]any{{int64(10), "new1"}, {int64(10), "new2"}}}
	m, _, _ := nativeTestMigrator(Config{Mode: "all", Content: "both", ValidateData: true})
	frozen := frozenValidationTable{name: "SourceStorage", target: "TargetStorage", metadata: metadata, rebasedRowIDs: map[string]bool{"Rid": true}}
	plan, err := m.dataValidationPlan(context.Background(), dst, frozen)
	require.NoError(t, err)
	require.Len(t, plan.SourceColumns, 1)
	require.Equal(t, "Value", plan.SourceColumns[0].Name)
	require.Empty(t, plan.PrimaryKey)
	ps := &nativeRowIDProjectionScanner{Scanner: src, expected: metadata, ignored: frozen.rebasedRowIDs}
	pt := &nativeRowIDProjectionScanner{Scanner: dst, expected: metadata, ignored: frozen.rebasedRowIDs}
	result := validation.Validate(context.Background(), ps, pt, plan)
	require.Equal(t, validation.StatusPassed, result.Status)
	require.EqualValues(t, 2, result.SourceRows)
	dst.rows[1][0] = int64(11)
	result = validation.Validate(context.Background(), ps, pt, plan)
	require.Equal(t, validation.StatusMismatch, result.Status)
	dst.rows = dst.rows[:1]
	result = validation.Validate(context.Background(), ps, pt, plan)
	require.Equal(t, validation.StatusMismatch, result.Status)
	dst.metadata.Columns = append(dst.metadata.Columns, validation.Column{Name: "Extra", Kind: validation.Number, DatabaseType: "INT"})
	result = validation.Validate(context.Background(), ps, pt, plan)
	require.Equal(t, validation.StatusError, result.Status)
}

func TestNativeRowIDValidationExceptionRequiresSuccessfulBaselineLedger(t *testing.T) {
	m, _, _ := nativeTestMigrator(Config{Mode: "all", Content: "both", ValidateData: true})
	o := damengnative.Object{ID: "mv", Kind: "materialized_view", Name: "Mv", Table: "TargetStorage", Schema: "Dst", Materialized: &damengnative.MaterializedState{Baseline: true, SnapshotColumns: []string{"Value", "Rid"}, RowIDColumns: []int{1}}}
	p := &damengnative.Preflight{Objects: []damengnative.Object{o}}
	m.validationTables = []frozenValidationTable{{name: "SourceStorage", target: "TargetStorage"}}
	m.repairs = &repairRecorder{objects: []repair.Object{{ID: o.ID, Units: []repair.Unit{{SQL: damengnative.MaterializedBaselineSQL("Dst", "Mv"), Status: "pending"}}}}}
	m.markNativeRowIDValidation(p, map[string]error{})
	require.Empty(t, m.validationTables[0].rebasedRowIDs)
	m.repairs.objects[0].Units[0].Status = "succeeded"
	m.markNativeRowIDValidation(p, map[string]error{})
	require.Equal(t, map[string]bool{"Rid": true}, m.validationTables[0].rebasedRowIDs)
	require.Contains(t, m.validationTables[0].note, "普通列与重复行数量")
	// An ordinary table ROWID column is never excluded by datatype alone.
	require.NotContains(t, m.validationTables[0].rebasedRowIDs, "UnrelatedRid")
}
