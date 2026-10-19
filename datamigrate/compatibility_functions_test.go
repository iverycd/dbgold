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
	calls          []string
	failedFunction string
	product        string
}

func (w *compatibilityTestWriter) DBType() string {
	if w.product != "" {
		return w.product
	}
	return "postgres"
}
func (w *compatibilityTestWriter) EnsureCompatibilityFunctions(context.Context) (*target.CompatibilityFunctionsReport, error) {
	w.calls = append(w.calls, "prepare")
	r := &target.CompatibilityFunctionsReport{Total: 13, Success: 13, Created: 13, Items: []target.CompatibilityFunctionResult{
		{Name: "get_uuid", Signature: "get_uuid()", Schema: "app", Status: "created", DDL: "CREATE FUNCTION get_uuid"},
		{Name: "auto_increase_itemid", Signature: "auto_increase_itemid()", Schema: "app", Status: "created", DDL: "CREATE FUNCTION auto_increase_itemid"},
		{Name: "count_rows", Signature: "count_rows(table_name varchar, owner varchar DEFAULT NULL)", Schema: "app", Status: "created", DDL: "CREATE FUNCTION count_rows"},
		{Name: "fun_getpy", Signature: "fun_getpy(p_name varchar)", Schema: "app", Status: "created", DDL: "CREATE FUNCTION fun_getpy"},
		{Name: "convertguid", Signature: "convertguid(p_newguid varchar, p_instr varchar)", Schema: "app", Status: "created", DDL: "CREATE FUNCTION convertguid"},
		{Name: "convertguidlist", Signature: "convertguidlist(p_newguid varchar, p_instr varchar)", Schema: "app", Status: "created", DDL: "CREATE FUNCTION convertguidlist"},
		{Name: "getcategoryinfopubcount", Signature: "getcategoryinfopubcount(p_categorynum varchar, p_infostatuscode integer, p_startdate timestamp without time zone, p_enddate timestamp without time zone)", Schema: "app", Status: "created", DDL: "CREATE FUNCTION getcategoryinfopubcount"},
		{Name: "getscore", Signature: "getscore(p_infoid varchar)", Schema: "app", Status: "created", DDL: "CREATE FUNCTION getscore"},
		{Name: "getcategoryscore", Signature: "getcategoryscore(p_userguid varchar, p_fromdate date, p_todate date)", Schema: "app", Status: "created", DDL: "CREATE FUNCTION getcategoryscore"},
		{Name: "getchildoulist", Signature: "getchildoulist(p_rootid varchar)", Schema: "app", Status: "created", DDL: "CREATE FUNCTION getchildoulist"},
		{Name: "changedatatohistory", Kind: "procedure", Signature: "changedatatohistory(p_processversioninstanceguid varchar)", Schema: "app", Status: "created", DDL: "CREATE PROCEDURE changedatatohistory"},
		{Name: "changedatatomain", Kind: "procedure", Signature: "changedatatomain(p_processversioninstanceguid varchar)", Schema: "app", Status: "created", DDL: "CREATE PROCEDURE changedatatomain"},
		{Name: "createpvi", Kind: "procedure", Signature: "createpvi(p_processguid varchar, p_processversioninstanceguid varchar, p_userguid varchar, p_username varchar, p_workitemguid varchar, p_ouguid varchar, INOUT p_handleurl varchar DEFAULT NULL)", Schema: "app", Status: "created", DDL: "CREATE PROCEDURE createpvi"},
	}}
	for i := range r.Items {
		if r.Items[i].Name != w.failedFunction {
			continue
		}
		r.Success--
		r.Created--
		r.Failed++
		r.Items[i].Status = "failed"
		r.Items[i].Error = "function " + w.failedFunction + " unavailable"
		return r, errors.New(r.Items[i].Error)
	}
	return r, nil
}
func (w *compatibilityTestWriter) CreateTable(context.Context, []dialect.Statement) error {
	w.calls = append(w.calls, "table")
	return nil
}
func (w *compatibilityTestWriter) CreateView(_ context.Context, v source.ViewInfo) error {
	w.calls = append(w.calls, "view")
	if w.failedFunction != "" && strings.Contains(strings.ToLower(v.Definition), w.failedFunction+"(") {
		return fmt.Errorf("function %s does not exist", w.failedFunction)
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
			require.Equal(t, 13, report.CompatibilityFunctions.Total)
			require.Equal(t, 13, report.CompatibilityFunctions.Success)
			require.Equal(t, 13, report.CompatibilityFunctions.Created)
			require.Zero(t, report.CompatibilityFunctions.Reused)
			require.Len(t, report.CompatibilityFunctions.Items, 13)
			require.Equal(t, "count_rows(table_name varchar, owner varchar DEFAULT NULL)", report.CompatibilityFunctions.Items[2].Signature)
			require.Equal(t, "procedure", report.CompatibilityFunctions.Items[10].Kind)
			require.Equal(t, "changedatatohistory(p_processversioninstanceguid varchar)", report.CompatibilityFunctions.Items[10].Signature)
			require.Equal(t, "procedure", report.CompatibilityFunctions.Items[11].Kind)
			require.Equal(t, "changedatatomain(p_processversioninstanceguid varchar)", report.CompatibilityFunctions.Items[11].Signature)
			require.Equal(t, "procedure", report.CompatibilityFunctions.Items[12].Kind)
			require.Equal(t, "createpvi(p_processguid varchar, p_processversioninstanceguid varchar, p_userguid varchar, p_username varchar, p_workitemguid varchar, p_ouguid varchar, INOUT p_handleurl varchar DEFAULT NULL)", report.CompatibilityFunctions.Items[12].Signature)
			require.Zero(t, report.Views.Total)
		})
	}
}
func TestCompatibilityFailureContinuesTablesDataAndIndependentViews(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, sample := range []struct{ name, call string }{
			{"get_uuid", "get_uuid()"},
			{"auto_increase_itemid", "auto_increase_itemid()"},
			{"count_rows", "count_rows('t')"},
			{"fun_getpy", "fun_getpy('中国')"},
			{"convertguid", "ConvertGuid('new-guid', 'old-guid')"},
			{"convertguidlist", "CONVERTGUIDLIST('new-guid', 'old-guid;tail')"},
			{"getcategoryinfopubcount", "GETCATEGORYINFOPUBCOUNT('category', 1, timestamp '2026-01-01', timestamp '2026-02-01')"},
			{"getscore", "GetScore('info-id')"},
			{"getcategoryscore", "GetCategoryScore('user-id', date '2026-01-01', date '2026-02-01')"},
			{"getchildoulist", "getChildOuList('root-id')"},
		} {
			function, call := sample.name, sample.call
			t.Run(fmt.Sprintf("%t/%s", capture, function), func(t *testing.T) {
				r := &compatibilityViewReader{mockReader: mockReader{
					tables: []string{"t"}, ddl: map[string]*source.TableDDLInfo{"t": {TableName: "t", Columns: []source.ColumnInfo{{Name: "id", DataType: "int"}}}}, rows: map[string][][]interface{}{"t": {{1}}},
				}, views: []source.ViewInfo{{ViewName: "dependent", Definition: "SELECT " + call}, {ViewName: "independent", Definition: "SELECT 1"}}}
				w := &compatibilityTestWriter{failedFunction: function}
				m, _ := newTestMigrator(r, w)
				m.cfg.CaptureRepair = capture
				report := m.Run(context.Background())
				require.True(t, report.HasFailures())
				require.Equal(t, 13, report.CompatibilityFunctions.Total)
				require.Equal(t, 12, report.CompatibilityFunctions.Success)
				require.Equal(t, 12, report.CompatibilityFunctions.Created)
				require.Equal(t, 1, report.CompatibilityFunctions.Failed)
				require.Equal(t, []string{"prepare", "table", "data", "view", "view"}, w.calls)
				require.Equal(t, 1, report.Tables.Total)
				require.Equal(t, 1, report.Tables.Success)
				require.Equal(t, 1, report.Data.Success)
				require.Equal(t, 1, report.Views.Success)
				require.Equal(t, 1, report.Views.Failed)
				require.Equal(t, "dependent", report.Views.Items[0].Name)
				require.Contains(t, report.Views.Items[0].Error, function+" does not exist")
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
		require.Equal(t, 13, m.CompatibilityFunctions().Success)
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
	w := &compatibilityTestWriter{failedFunction: "fun_getpy"}
	reader := &compatibilityViewReader{views: []source.ViewInfo{{ViewName: "dependent", Definition: "SELECT fun_getpy('中国')"}, {ViewName: "independent", Definition: "SELECT 1"}}}
	m, _ := newTestMigrator(reader, w)
	results := m.MigrateViews(context.Background(), []string{"dependent", "independent"})
	require.Len(t, results, 2)
	require.Equal(t, "dependent", results[0].Name)
	require.Contains(t, results[0].Error, "fun_getpy does not exist")
	require.Equal(t, "independent", results[1].Name)
	require.Empty(t, results[1].Error)
	require.Equal(t, 1, m.CompatibilityFunctions().Failed)
	require.Equal(t, 12, m.CompatibilityFunctions().Success)
	require.Equal(t, []string{"prepare", "view", "view"}, w.calls)
}

func TestCompatibilityProcedureFailureContinuesMigration(t *testing.T) {
	for _, procedure := range []string{"changedatatohistory", "changedatatomain", "createpvi"} {
		t.Run(procedure, func(t *testing.T) {
			reader := &compatibilityViewReader{mockReader: mockReader{
				tables: []string{"t"}, ddl: map[string]*source.TableDDLInfo{"t": {TableName: "t", Columns: []source.ColumnInfo{{Name: "id", DataType: "int"}}}}, rows: map[string][][]interface{}{"t": {{1}}},
			}, views: []source.ViewInfo{{ViewName: "independent", Definition: "SELECT 1"}}}
			w := &compatibilityTestWriter{failedFunction: procedure}
			m, _ := newTestMigrator(reader, w)
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.Equal(t, 13, report.CompatibilityFunctions.Total)
			require.Equal(t, 12, report.CompatibilityFunctions.Success)
			require.Equal(t, 1, report.CompatibilityFunctions.Failed)
			require.Equal(t, 1, report.Tables.Success)
			require.Equal(t, 1, report.Data.Success)
			require.Equal(t, 1, report.Views.Success)
			for _, item := range report.CompatibilityFunctions.Items[10:] {
				if item.Name == procedure {
					require.Equal(t, "failed", item.Status)
				} else {
					require.Equal(t, "created", item.Status)
				}
			}
			require.Equal(t, []string{"prepare", "table", "data", "view"}, w.calls)
		})
	}
}
