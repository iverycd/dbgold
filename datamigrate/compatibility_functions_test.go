package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type compatibilityTestWriter struct {
	mockWriter
	calls   []string
	fail    bool
	product string
}

func (w *compatibilityTestWriter) DBType() string {
	if w.product != "" {
		return w.product
	}
	return "postgres"
}
func (w *compatibilityTestWriter) EnsureCompatibilityFunctions(context.Context) (*target.CompatibilityFunctionsReport, error) {
	w.calls = append(w.calls, "prepare")
	r := &target.CompatibilityFunctionsReport{Total: 1, Success: 1, Created: 1, Items: []target.CompatibilityFunctionResult{{Name: "get_uuid", Schema: "app", Status: "created", DDL: "CREATE FUNCTION get_uuid"}}}
	if w.fail {
		r.Success, r.Created, r.Reused, r.Failed = 0, 0, 0, 1
		for i := range r.Items {
			r.Items[i].Status = "failed"
			r.Items[i].Error = "no UUID"
		}
		return r, errors.New("no UUID")
	}
	return r, nil
}
func (w *compatibilityTestWriter) CreateTable(context.Context, []dialect.Statement) error {
	w.calls = append(w.calls, "table")
	return nil
}
func (w *compatibilityTestWriter) CreateView(_ context.Context, v source.ViewInfo) error {
	w.calls = append(w.calls, "view")
	if w.fail && strings.Contains(strings.ToLower(v.Definition), "get_uuid(") {
		return errors.New("function get_uuid does not exist")
	}
	return nil
}
func (w *compatibilityTestWriter) CopyData(context.Context, string, []string, []string, [][]interface{}) error {
	w.calls = append(w.calls, "data")
	return nil
}

type compatibilityViewReader struct {
	mockReader
	views []source.ViewInfo
}

func (r *compatibilityViewReader) GetViews(context.Context) ([]source.ViewInfo, error) {
	if r.views != nil {
		return r.views, nil
	}
	return []source.ViewInfo{{ViewName: "v", Definition: "SELECT get_uuid()"}}, nil
}

func TestCompatibilityInitializedForEveryMigrationMode(t *testing.T) {
	for _, mode := range []string{"both", "schema_only", "data_only", "objects", "include"} {
		t.Run(mode, func(t *testing.T) {
			r := &mockReader{tables: []string{"t"}, ddl: map[string]*source.TableDDLInfo{"t": {TableName: "t", Columns: []source.ColumnInfo{{Name: "id", DataType: "int"}}}}, rows: map[string][][]interface{}{"t": {{1}}}}
			w := &compatibilityTestWriter{}
			m, _ := newTestMigrator(r, w)
			m.cfg.Content = mode
			if mode == "objects" {
				m.cfg.Objects = []string{"indexes"}
			}
			if mode == "include" {
				m.cfg.Mode = "include"
				m.cfg.Filter = "t"
			}
			report := m.Run(context.Background())
			require.False(t, report.HasFailures())
			require.NotNil(t, report.CompatibilityFunctions)
			require.Equal(t, "prepare", w.calls[0])
			require.Equal(t, 1, report.CompatibilityFunctions.Total)
			require.Equal(t, 1, report.CompatibilityFunctions.Success)
			require.Equal(t, 1, report.CompatibilityFunctions.Created)
			require.Zero(t, report.CompatibilityFunctions.Reused)
			require.Len(t, report.CompatibilityFunctions.Items, 1)
			require.Zero(t, report.Views.Total)
		})
	}
}
func TestCompatibilityFailureContinuesTablesDataAndIndependentViews(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprint(capture), func(t *testing.T) {
			r := &compatibilityViewReader{mockReader: mockReader{
				tables: []string{"t"}, ddl: map[string]*source.TableDDLInfo{"t": {TableName: "t", Columns: []source.ColumnInfo{{Name: "id", DataType: "int"}}}}, rows: map[string][][]interface{}{"t": {{1}}},
			}, views: []source.ViewInfo{{ViewName: "dependent", Definition: "SELECT get_uuid()"}, {ViewName: "independent", Definition: "SELECT 1"}}}
			w := &compatibilityTestWriter{fail: true}
			m, _ := newTestMigrator(r, w)
			m.cfg.CaptureRepair = capture
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.Equal(t, []string{"prepare", "table", "data", "view", "view"}, w.calls)
			require.Equal(t, 1, report.Tables.Total)
			require.Equal(t, 1, report.Tables.Success)
			require.Equal(t, 1, report.Data.Success)
			require.Equal(t, 1, report.Views.Success)
			require.Equal(t, 1, report.Views.Failed)
			require.Equal(t, "dependent", report.Views.Items[0].Name)
			require.Contains(t, report.Views.Items[0].Error, "get_uuid does not exist")
			if capture {
				require.NotNil(t, report.RepairManifest)
				statuses := map[string]string{}
				for _, o := range report.RepairManifest.Objects {
					statuses[o.Name] = o.Status
				}
				require.Equal(t, "succeeded", statuses["t"])
				require.Equal(t, "succeeded", statuses["independent"])
				require.Equal(t, "unknown", statuses["dependent"])
			}
			encoded, err := json.Marshal(report)
			require.NoError(t, err)
			var saved MigrationReport
			require.NoError(t, json.Unmarshal(encoded, &saved))
			require.True(t, saved.HasFailures())
			require.Equal(t, report.CompatibilityFunctions, saved.CompatibilityFunctions)
		})
	}
}
func TestCompatibilityInitializedBeforeStandaloneAndFullViews(t *testing.T) {
	for _, standalone := range []bool{true, false} {
		w := &compatibilityTestWriter{}
		m, _ := newTestMigrator(&compatibilityViewReader{}, w)
		if standalone {
			results := m.MigrateViews(context.Background(), []string{"v"})
			require.Len(t, results, 1)
			require.Empty(t, results[0].Error)
		} else {
			report := m.Run(context.Background())
			require.Equal(t, 1, report.Views.Success)
		}
		require.Equal(t, []string{"prepare", "view"}, w.calls)
		require.NotNil(t, m.CompatibilityFunctions())
	}
}
func TestCompatibilityNonPGAndOldReportsStayUnchanged(t *testing.T) {
	w := &compatibilityTestWriter{product: "mysql"}
	m, _ := newTestMigrator(&mockReader{}, w)
	report := m.Run(context.Background())
	require.Nil(t, report.CompatibilityFunctions)
	require.Empty(t, w.calls)
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "compatibility_functions")
	require.NoError(t, json.Unmarshal(raw, &report))
	require.False(t, report.HasFailures())
}

func TestCompatibilityHistoricalTwoFunctionReportRoundTrip(t *testing.T) {
	legacy := &target.CompatibilityFunctionsReport{
		Total: 2, Success: 2, Created: 1, Reused: 1,
		Items: []target.CompatibilityFunctionResult{
			{Name: "get_uuid", Schema: "app", Status: "created", DDL: "CREATE FUNCTION get_uuid"},
			{Name: "GET_UUID", Schema: "app", Status: "reused", DDL: "existing definition"},
		},
	}
	report := MigrationReport{CompatibilityFunctions: legacy}
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	var saved MigrationReport
	require.NoError(t, json.Unmarshal(raw, &saved))
	require.Equal(t, legacy, saved.CompatibilityFunctions)
	require.False(t, saved.HasFailures())
}

func TestCompatibilityStandaloneFailureStillExecutesAllViews(t *testing.T) {
	w := &compatibilityTestWriter{fail: true}
	reader := &compatibilityViewReader{views: []source.ViewInfo{{ViewName: "dependent", Definition: "SELECT get_uuid()"}, {ViewName: "independent", Definition: "SELECT 1"}}}
	m, _ := newTestMigrator(reader, w)
	results := m.MigrateViews(context.Background(), []string{"dependent", "independent"})
	require.Len(t, results, 2)
	require.Equal(t, "dependent", results[0].Name)
	require.Contains(t, results[0].Error, "get_uuid does not exist")
	require.Equal(t, "independent", results[1].Name)
	require.Empty(t, results[1].Error)
	require.Equal(t, 1, m.CompatibilityFunctions().Failed)
	require.Equal(t, []string{"prepare", "view", "view"}, w.calls)
}
