package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	names          target.CompatibilityRoutineNames
	product        string
}

func (w *compatibilityTestWriter) DBType() string {
	if w.product != "" {
		return w.product
	}
	return "postgres"
}
func compatibilityTestItems(names target.CompatibilityRoutineNames) []target.CompatibilityFunctionResult {
	items := []target.CompatibilityFunctionResult{
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
		{Name: "inter_arry", Signature: "inter_arry(p_seta varchar, p_setb varchar)", Schema: "app", Status: "created", DDL: "CREATE FUNCTION inter_arry"},
		{Name: "sec_to_dhm", Signature: "sec_to_dhm(p_sec bigint)", Schema: "app", Status: "created", DDL: "CREATE FUNCTION sec_to_dhm"},
		{Name: "selectallusercount", Signature: "selectallusercount()", Schema: "app", Status: "created", DDL: "CREATE FUNCTION selectallusercount"},
		{Name: "stuff", Signature: "stuff(p_old varchar, p_start integer, p_length integer, p_replace varchar)", Schema: "app", Status: "created", DDL: "CREATE FUNCTION stuff"},
		{Name: "getdate", Signature: "getdate()", Schema: "app", Status: "created", DDL: "CREATE FUNCTION getdate"},
		{Name: "getrowid", Signature: "getrowid()", Schema: "app", Status: "created", DDL: "CREATE FUNCTION getrowid"},
		{Name: "isnull", Signature: "isnull(i_obj varchar, i_obj2 varchar)", Schema: "app", Status: "created", DDL: "CREATE FUNCTION isnull"},
		{Name: "changedatatohistory", Kind: "procedure", Signature: "changedatatohistory(p_processversioninstanceguid varchar)", Schema: "app", Status: "created", DDL: "CREATE PROCEDURE changedatatohistory"},
		{Name: "changedatatomain", Kind: "procedure", Signature: "changedatatomain(p_processversioninstanceguid varchar)", Schema: "app", Status: "created", DDL: "CREATE PROCEDURE changedatatomain"},
		{Name: "createpvi", Kind: "procedure", Signature: "createpvi(p_processguid varchar, p_processversioninstanceguid varchar, p_userguid varchar, p_username varchar, p_workitemguid varchar, p_ouguid varchar, INOUT p_handleurl varchar DEFAULT NULL)", Schema: "app", Status: "created", DDL: "CREATE PROCEDURE createpvi"},
	}
	return slices.DeleteFunc(items, func(item target.CompatibilityFunctionResult) bool {
		selected := names.Functions
		if item.Kind == "procedure" {
			selected = names.Procedures
		}
		return !slices.ContainsFunc(selected, func(name string) bool {
			return strings.EqualFold(name, item.Name)
		})
	})
}
func (w *compatibilityTestWriter) EnsureCompatibilityFunctions(_ context.Context, names target.CompatibilityRoutineNames) (*target.CompatibilityFunctionsReport, error) {
	w.calls = append(w.calls, "prepare")
	w.names = target.CompatibilityRoutineNames{Functions: slices.Clone(names.Functions), Procedures: slices.Clone(names.Procedures)}
	items := compatibilityTestItems(names)
	r := &target.CompatibilityFunctionsReport{Total: len(items), Success: len(items), Created: len(items), Items: items}
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

type compatibilityTestReader struct {
	mockReader
	views          []source.ViewInfo
	functionNames  []string
	functionErr    error
	functionCalls  int
	procedureNames []string
	procedureErr   error
	procedureCalls int
}

func (r *compatibilityTestReader) GetViews(context.Context) ([]source.ViewInfo, error) {
	if r.views != nil {
		return r.views, nil
	}
	return []source.ViewInfo{{ViewName: "v", Definition: "SELECT get_uuid()"}}, nil
}

func (r *compatibilityTestReader) ListFunctionNames(context.Context) ([]string, error) {
	r.functionCalls++
	return r.functionNames, r.functionErr
}

func (r *compatibilityTestReader) ListProcedureNames(context.Context) ([]string, error) {
	r.procedureCalls++
	return r.procedureNames, r.procedureErr
}

func TestCompatibilityInitializedForEveryMigrationMode(t *testing.T) {
	names := target.CompatibilityRoutineNames{
		Functions:  []string{"FuN_GeTpY", "fun_getpy", "CreatePVI", "test"},
		Procedures: []string{"ChangeDataToHistory", "COUNT_ROWS"},
	}
	expectedItems := compatibilityTestItems(names)
	require.Len(t, expectedItems, 2)
	for _, mode := range []string{"both", "schema_only", "data_only", "objects", "include", "cdc"} {
		t.Run(mode, func(t *testing.T) {
			r := &compatibilityTestReader{mockReader: mockReader{
				tables: []string{"t"}, ddl: map[string]*source.TableDDLInfo{"t": {TableName: "t", Columns: []source.ColumnInfo{{Name: "id", DataType: "int"}}}}, rows: map[string][][]interface{}{"t": {{1}}},
			}, views: []source.ViewInfo{}, functionNames: names.Functions, procedureNames: names.Procedures}
			w := &compatibilityTestWriter{}
			m, _ := newTestMigrator(r, w)
			m.cfg.Content = "both"
			switch mode {
			case "objects":
				m.cfg.Objects = []string{"indexes"}
			case "include":
				m.cfg.Mode = "include"
				m.cfg.Filter = "t"
			case "cdc":
				m.cfg.MigrationPurpose = "cdc"
			default:
				m.cfg.Content = mode
			}
			report := m.Run(context.Background())
			require.False(t, report.HasFailures())
			require.NotNil(t, report.CompatibilityFunctions)
			require.Equal(t, 1, r.functionCalls)
			require.Equal(t, 1, r.procedureCalls)
			require.Equal(t, names, w.names)
			require.Equal(t, "prepare", w.calls[0])
			require.Equal(t, len(expectedItems), report.CompatibilityFunctions.Total)
			require.Equal(t, len(expectedItems), report.CompatibilityFunctions.Success)
			require.Equal(t, len(expectedItems), report.CompatibilityFunctions.Created)
			require.Zero(t, report.CompatibilityFunctions.Reused)
			require.Len(t, report.CompatibilityFunctions.Items, len(expectedItems))
			itemsByName := make(map[string]target.CompatibilityFunctionResult, len(expectedItems))
			for _, item := range report.CompatibilityFunctions.Items {
				itemsByName[item.Name] = item
			}
			for _, expected := range expectedItems {
				item, found := itemsByName[expected.Name]
				require.True(t, found, expected.Name)
				require.Equal(t, expected.Kind, item.Kind, expected.Name)
				require.Equal(t, expected.Signature, item.Signature, expected.Name)
			}
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
			{"inter_arry", "INTER_ARRY('甲,乙', '甲,乙')"},
			{"sec_to_dhm", "sec_to_DHM(90061)"},
			{"selectallusercount", "selectAllUserCount()"},
			{"stuff", "stuff('abcabc', 1, 3, 'x')"},
			{"getdate", "GETDATE()"},
			{"getrowid", "GETROWID()"},
			{"isnull", "app.ISNULL(NULL, '默认值')"},
		} {
			function, call := sample.name, sample.call
			t.Run(fmt.Sprintf("%t/%s", capture, function), func(t *testing.T) {
				r := &compatibilityTestReader{mockReader: mockReader{
					tables: []string{"t"}, ddl: map[string]*source.TableDDLInfo{"t": {TableName: "t", Columns: []source.ColumnInfo{{Name: "id", DataType: "int"}}}}, rows: map[string][][]interface{}{"t": {{1}}},
				}, functionNames: []string{function, "getscore"}, views: []source.ViewInfo{{ViewName: "dependent", Definition: "SELECT " + call}, {ViewName: "independent", Definition: "SELECT 1"}}}
				w := &compatibilityTestWriter{failedFunction: function}
				m, _ := newTestMigrator(r, w)
				m.cfg.CaptureRepair = capture
				report := m.Run(context.Background())
				require.True(t, report.HasFailures())
				total := len(compatibilityTestItems(w.names))
				require.Equal(t, total, report.CompatibilityFunctions.Total)
				require.Equal(t, total-1, report.CompatibilityFunctions.Success)
				require.Equal(t, total-1, report.CompatibilityFunctions.Created)
				require.Equal(t, 1, report.CompatibilityFunctions.Failed)
				for _, item := range report.CompatibilityFunctions.Items {
					if item.Name == function {
						require.Equal(t, "failed", item.Status)
						require.Contains(t, item.Error, function+" unavailable")
					} else {
						require.Equal(t, "created", item.Status)
					}
				}
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
		r := &compatibilityTestReader{functionNames: []string{"GET_UUID"}, procedureNames: []string{"CreatePVI"}}
		m, _ := newTestMigrator(r, w)
		if standalone {
			results := m.MigrateViews(context.Background(), []string{"v"})
			require.Len(t, results, 1)
			require.Empty(t, results[0].Error)
		} else {
			report := m.Run(context.Background())
			require.Equal(t, 1, report.Views.Success)
		}
		require.Equal(t, []string{"prepare", "view"}, w.calls)
		require.Equal(t, 1, r.functionCalls)
		require.Equal(t, 1, r.procedureCalls)
		require.Equal(t, r.functionNames, w.names.Functions)
		require.Equal(t, r.procedureNames, w.names.Procedures)
		require.NotNil(t, m.CompatibilityFunctions())
		require.Equal(t, 2, m.CompatibilityFunctions().Success)
	}
}
func TestCompatibilityNonPGAndOldReportsStayUnchanged(t *testing.T) {
	w := &compatibilityTestWriter{product: "mysql"}
	r := &compatibilityTestReader{views: []source.ViewInfo{}, functionNames: []string{"GET_UUID"}, procedureNames: []string{"CreatePVI"}}
	m, _ := newTestMigrator(r, w)
	report := m.Run(context.Background())
	require.Nil(t, report.CompatibilityFunctions)
	require.Zero(t, r.functionCalls)
	require.Zero(t, r.procedureCalls)
	require.Empty(t, w.calls)
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "compatibility_functions")
	require.NoError(t, json.Unmarshal(raw, &report))
	require.False(t, report.HasFailures())
}

func TestCompatibilityEmptySourceInventoryDoesNotSelectRoutines(t *testing.T) {
	for _, reader := range []source.Reader{&compatibilityTestReader{}, &mockReader{}} {
		w := &compatibilityTestWriter{}
		m, _ := newTestMigrator(reader, w)
		require.NoError(t, m.prepareCompatibilityFunctions(context.Background()))
		require.Empty(t, w.names.Functions)
		require.Empty(t, w.names.Procedures)
		require.Zero(t, m.CompatibilityFunctions().Total)
	}
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
	reader := &compatibilityTestReader{functionNames: []string{"Fun_GetPY", "count_rows"}, views: []source.ViewInfo{{ViewName: "dependent", Definition: "SELECT fun_getpy('中国')"}, {ViewName: "independent", Definition: "SELECT 1"}}}
	m, _ := newTestMigrator(reader, w)
	results := m.MigrateViews(context.Background(), []string{"dependent", "independent"})
	require.Len(t, results, 2)
	require.Equal(t, "dependent", results[0].Name)
	require.Contains(t, results[0].Error, "fun_getpy does not exist")
	require.Equal(t, "independent", results[1].Name)
	require.Empty(t, results[1].Error)
	require.Equal(t, 1, m.CompatibilityFunctions().Failed)
	require.Equal(t, 1, m.CompatibilityFunctions().Success)
	require.Equal(t, []string{"prepare", "view", "view"}, w.calls)
}

func TestCompatibilityDiscoveryFailureContinuesMigration(t *testing.T) {
	for _, tc := range []struct {
		mode, failedKind, label string
	}{
		{"full", "function", "源库函数列表"},
		{"full", "procedure", "源库存储过程列表"},
		{"standalone_views", "function", "源库函数列表"},
		{"standalone_views", "procedure", "源库存储过程列表"},
	} {
		t.Run(tc.mode+"/"+tc.failedKind, func(t *testing.T) {
			r := &compatibilityTestReader{
				mockReader: mockReader{
					tables: []string{"t"},
					ddl:    map[string]*source.TableDDLInfo{"t": {TableName: "t", Columns: []source.ColumnInfo{{Name: "id", DataType: "int"}}}},
					rows:   map[string][][]interface{}{"t": {{1}}},
				},
				functionNames:  []string{"GET_UUID"},
				procedureNames: []string{"ChangeDataToMain"},
			}
			catalogErr := errors.New("catalog access denied")
			want := target.CompatibilityRoutineNames{}
			if tc.failedKind == "function" {
				r.functionErr = catalogErr
				want.Procedures = r.procedureNames
			} else {
				r.procedureErr = catalogErr
				want.Functions = r.functionNames
			}
			w := &compatibilityTestWriter{}
			m, _ := newTestMigrator(r, w)
			if tc.mode == "standalone_views" {
				results := m.MigrateViews(context.Background(), []string{"v"})
				require.Len(t, results, 1)
				require.Equal(t, "v", results[0].Name)
				require.Empty(t, results[0].Error)
				require.Equal(t, []string{"prepare", "view"}, w.calls)
			} else {
				report := m.Run(context.Background())
				require.True(t, report.HasFailures())
				require.Equal(t, 1, report.Tables.Success)
				require.Equal(t, 1, report.Data.Success)
				require.Equal(t, 1, report.Views.Success)
				require.Equal(t, []string{"prepare", "table", "data", "view"}, w.calls)
			}
			require.Equal(t, 1, r.functionCalls)
			require.Equal(t, 1, r.procedureCalls)
			require.Equal(t, want, w.names)
			report := m.CompatibilityFunctions()
			require.NotNil(t, report)
			require.Equal(t, 2, report.Total)
			require.Equal(t, 1, report.Success)
			require.Equal(t, 1, report.Failed)
			issue := report.Items[len(report.Items)-1]
			require.Equal(t, tc.label, issue.Name)
			require.Equal(t, tc.failedKind, issue.Kind)
			require.Equal(t, "failed", issue.Status)
			require.Contains(t, issue.Error, "catalog access denied")
		})
	}
}

func TestCompatibilityProcedureFailureContinuesMigration(t *testing.T) {
	for _, procedure := range []string{"changedatatohistory", "changedatatomain", "createpvi"} {
		t.Run(procedure, func(t *testing.T) {
			reader := &compatibilityTestReader{mockReader: mockReader{
				tables: []string{"t"}, ddl: map[string]*source.TableDDLInfo{"t": {TableName: "t", Columns: []source.ColumnInfo{{Name: "id", DataType: "int"}}}}, rows: map[string][][]interface{}{"t": {{1}}},
			}, views: []source.ViewInfo{{ViewName: "independent", Definition: "SELECT 1"}}, procedureNames: []string{procedure}}
			w := &compatibilityTestWriter{failedFunction: procedure}
			m, _ := newTestMigrator(reader, w)
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			total := len(compatibilityTestItems(w.names))
			require.Equal(t, total, report.CompatibilityFunctions.Total)
			require.Equal(t, total-1, report.CompatibilityFunctions.Success)
			require.Equal(t, 1, report.CompatibilityFunctions.Failed)
			require.Equal(t, 1, report.Tables.Success)
			require.Equal(t, 1, report.Data.Success)
			require.Equal(t, 1, report.Views.Success)
			for _, item := range report.CompatibilityFunctions.Items {
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
