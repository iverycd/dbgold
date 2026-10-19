package target

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// These field lists are taken from the supplied CreatePVI procedure, not from
// its production dependency map. A missing production field must fail the test.
var createPVIFixtureTables = []struct{ name, columns string }{
	{"workflow_processversion", "ProcessVersionGuid ProcessVersionName Status:int ProcessGuid"},
	{"workflow_process", "ProcessGuid ProcessName"},
	{"workflow_activity", "ActivityGuid ActivityName ProcessVersionGuid ActivityType:int HandleURL Priority:int"},
	{"workflow_transition", "TransitionGuid FromActivityGuid ToActivityGuid WorkItemModeName Is_ShowAsOperationButton:int"},
	{"workflow_context", "ProcessVersionGuid FieldName FieldType FieldValue"},
	{"workflow_pv_material", "MaterialGuid ProcessVersionGuid MaterialName Source Type:int PageUrl_ReadAndWrite PageUrl_Read PageUrl_Print Status:int Is_ShowInGrid:int ClientTag Is_InitMisTable:int MainPVActivityGuid"},
	{"workflow_pv_mistableset", "MaterialGuid TableID SQL_TableName"},
	{"workflow_pvi", "ProcessVersionInstanceGuid ProcessVersionInstanceName ProcessVersionGuid Initiator StartDate:timestamp Status:int Is_SubProcessVersionInstance:int ActivityGuid_Callme ActivityInstanceGuid_Callme SyncType:int Note ProcessGuid Is_NeedSendEarlyWarning:int Is_NeedSendOverTimeNotify:int MainPVIGuid MainPVActivityGuid LockStatus"},
	{"workflow_context_value", "FieldGuid ProcessVersionInstanceGuid FieldName FieldType FieldValue"},
	{"workflow_pvi_material", "MaterialInstanceGuid MaterialInstanceName AddDate:timestamp Is_Defined:int ProcessVersionInstanceGuid MaterialGuid MaterialName Source Type:int AttachStorageInfoGroupGuid PageUrl_ReadAndWrite PageUrl_Read PageUrl_Print Status:int Is_ShowInGrid:int ClientTag Is_InitMisTable:int"},
	{"workflow_pvi_mistablerow", "MaterialInstanceGuid TableID SQL_TableName PVIGuid MisTableSetInstanceGuid"},
	{"workflow_activity_instance", "ActivityInstanceGuid ProcessVersionGuid ProcessVersionInstanceGuid SubProcessVersionInstanceGuid ActivityGuid StartDate:timestamp Status:int RollBackTag:int Note"},
	{"workflow_transition_instance", "TransitionInstanceGuid TransitionGuid ProcessVersionInstanceGuid SourceActiviryGuid SourceActiviryName SrcActInstGuid SrcActInstName TargetActivityGuid TargetActivityName TgtActInstGuid TgtActInstName Type:int TransitDate:timestamp Direction:int"},
	{"workflow_workitem", "WorkItemGuid ActivityGuid ActivityName ActivityInstanceGuid WorkItemName WorkItemType WorkItemModeName HandleURL Status:int Priority:int SenderGuid SenderName Transactor TransactorName JobGuid WaitHandleGuid CreateDate:timestamp LimitMinutes:int TimeLimit:int TimeLimitUnit:int Is_NeedSendEarlyWarning:int Is_NeedSendOverTimeNotify:int ProcessGuid ProcessVersionGuid ProcessVersionName ProcessVersionInstanceGuid ProcessVersionInstanceName GroupGuid AgentForUserGuid AgentForWorkItemGuid OperatorForDisplayGuid OperatorForDisplayName Note"},
	{"messages_center", "MessageItemGuid Title MessageType Content SendMode:int GenerateDate:timestamp IsSchedule:int ScheduleSendDate:timestamp MessageTarget TargetUser TargetDispName FromUser FromDispName FromMobile HandleUrl BeiZhu OuGuid BaseOuGuid IsShow:int JobGuid HandleType ClientIdentifier ClientTag IsDel:int IsNoHandle:int PVIGuid OverTimePoint:timestamp EarlyWarningPoint:timestamp"},
}

var createPVIOutputTables = []string{
	"workflow_pvi", "workflow_context_value", "workflow_pvi_material", "workflow_pvi_mistablerow",
	"workflow_activity_instance", "workflow_transition_instance", "workflow_workitem", "messages_center",
}

const createPVIContextValue = "中文 ' \\"

type createPVIFixture struct {
	db      *sql.DB
	schema  string
	tables  map[string]string
	columns map[string]map[string]string
}

type createPVIRecord map[string]sql.NullString

func TestCompatibilityCreatePVISQLIntegration(t *testing.T) {
	for _, product := range []struct{ name, variable, driver string }{
		{"postgres", "DBGOLD_TEST_POSTGRES_DSN", "postgres"},
		{"kingbase", "DBGOLD_TEST_KINGBASE_DSN", "postgres"},
		{"highgo", "DBGOLD_TEST_HIGHGO_DSN", "highgo"},
		{"gaussdb", "DBGOLD_TEST_GAUSSDB_DSN", "opengauss"},
		{"vastbase", "DBGOLD_TEST_VASTBASE_DSN", "postgres"},
		{"gbase", "DBGOLD_TEST_GBASE_DSN", "postgres"},
		{"seabox", "DBGOLD_TEST_SEABOX_DSN", "postgres"},
	} {
		t.Run(product.name, func(t *testing.T) {
			dsn := os.Getenv(product.variable)
			if dsn == "" {
				t.Skip(product.variable + " is not set")
			}
			db, err := sql.Open(product.driver, dsn)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			require.NoError(t, db.PingContext(ctx))
			for _, tc := range []struct {
				name string
				run  func(*testing.T, context.Context, *createPVIFixture)
			}{
				{"all_fields_and_query_string", checkCreatePVIFields},
				{"seven_arguments_and_query_string", checkCreatePVIExplicitOutput},
				{"character_configuration_statuses", checkCreatePVICharacterStatuses},
				{"null_handle_url", checkCreatePVINullURL},
				{"no_optional_context_or_materials", checkCreatePVIEmptyOptional},
				{"configuration_errors_are_atomic", checkCreatePVIConfiguration},
				{"last_insert_conflict_is_atomic", checkCreatePVIMessageConflict},
				{"caller_can_rollback", checkCreatePVICallerTransaction},
				{"dependency_validation", checkCreatePVIDependencies},
			} {
				t.Run(tc.name, func(t *testing.T) {
					schema := newBusinessIntegrationSchema(t, ctx, db)
					// Creating/reusing the process must not execute it or require
					// its business tables, which are created afterward.
					report, err := ensurePGProcedureSpecCompatibility(ctx, db, schema, product.name, compatibilityProcedureSpecs[2])
					require.NoError(t, err)
					require.Equal(t, 1, report.Created)
					report, err = ensurePGProcedureSpecCompatibility(ctx, db, schema, product.name, compatibilityProcedureSpecs[2])
					require.NoError(t, err)
					require.Equal(t, 1, report.Reused)
					f := newCreatePVIFixture(t, ctx, db, schema)
					tc.run(t, ctx, f)
				})
			}
		})
	}
}

func newCreatePVIFixture(t *testing.T, ctx context.Context, db *sql.DB, schema string) *createPVIFixture {
	t.Helper()
	f := &createPVIFixture{db: db, schema: schema, tables: make(map[string]string), columns: make(map[string]map[string]string)}
	for i, spec := range createPVIFixtureTables {
		name := spec.name
		if i%2 == 0 {
			name = strings.ToUpper(name)
		}
		f.tables[spec.name] = qualifiedFunction(schema, name)
		f.columns[spec.name] = make(map[string]string)
		var fields []string
		for j, field := range strings.Fields(spec.columns) {
			column, kind, _ := strings.Cut(field, ":")
			if kind == "" {
				kind = "varchar"
			}
			key := strings.ToLower(column)
			if j%2 == 0 {
				column = strings.ToUpper(column)
			}
			f.columns[spec.name][key] = source.QuotePGIdent(column)
			fields = append(fields, source.QuotePGIdent(column)+" "+kind)
		}
		businessExec(t, ctx, db, "CREATE TABLE "+f.tables[spec.name]+"("+strings.Join(fields, ",")+")")
	}
	sequence := qualifiedFunction(schema, "create_pvi_uuid_sequence")
	businessExec(t, ctx, db, "CREATE SEQUENCE "+sequence)
	// A sequence-backed UUID wrapper avoids extension installation and makes
	// per-row generation observable. Nothing outside this disposable schema runs.
	body := "SELECT ('00000000-0000-0000-0000-' || pg_catalog.lpad(pg_catalog.nextval(" + sqlFunctionBody(sequence) + "::regclass)::text,12,'0'))::varchar"
	businessExec(t, ctx, db, "CREATE FUNCTION "+qualifiedFunction(schema, "get_uuid")+"() RETURNS varchar LANGUAGE sql VOLATILE AS "+sqlFunctionBody(body))
	f.insert(t, ctx, "workflow_processversion", map[string]any{"processversionguid": "version", "processversionname": "流程版一", "status": 10, "processguid": "process"})
	f.insert(t, ctx, "workflow_process", map[string]any{"processguid": "process", "processname": "请假流程"})
	f.insert(t, ctx, "workflow_activity", map[string]any{"activityguid": "start", "activityname": "开始", "processversionguid": "version", "activitytype": 10})
	f.insert(t, ctx, "workflow_activity", map[string]any{"activityguid": "first", "activityname": "审批", "processversionguid": "version", "activitytype": 20, "handleurl": "/approve?mode=edit", "priority": 7})
	f.insert(t, ctx, "workflow_transition", map[string]any{"transitionguid": "transition", "fromactivityguid": "start", "toactivityguid": "first", "workitemmodename": "审批模式", "is_showasoperationbutton": 20})
	f.insert(t, ctx, "workflow_context", map[string]any{"processversionguid": "version", "fieldname": "first-field", "fieldtype": "varchar", "fieldvalue": createPVIContextValue})
	f.insert(t, ctx, "workflow_context", map[string]any{"processversionguid": "version", "fieldname": "second-field", "fieldtype": "nullable", "fieldvalue": nil})
	f.insert(t, ctx, "workflow_context", map[string]any{"processversionguid": "other-version", "fieldname": "excluded", "fieldtype": "varchar", "fieldvalue": "not copied"})
	for _, item := range []struct{ guid, version, name string }{
		{"material-one", "version", "材料一"},
		{"material-two", "version", "材料二"},
		{"material-one", "other-version", "其他版本同GUID材料"},
	} {
		f.insert(t, ctx, "workflow_pv_material", map[string]any{
			"materialguid": item.guid, "processversionguid": item.version, "materialname": item.name,
			"source": "source:" + item.name, "type": 3, "pageurl_readandwrite": "/edit/" + item.name,
			"pageurl_read": "/read/" + item.name, "pageurl_print": "/print/" + item.name,
			"status": 10, "is_showingrid": 20, "clienttag": "tag:" + item.name, "is_initmistable": 10,
		})
	}
	for _, item := range []struct {
		guid, version string
		status        int
		main          any
	}{
		{"inactive", "version", 20, nil},
		{"subprocess-material", "version", 10, "child-activity"},
		{"another-version", "other-version", 10, nil},
	} {
		f.insert(t, ctx, "workflow_pv_material", map[string]any{"materialguid": item.guid, "processversionguid": item.version, "status": item.status, "mainpvactivityguid": item.main})
	}
	for _, item := range []struct{ material, table, sql string }{
		{"material-one", "table-one", "MIS_ONE"}, {"material-two", "table-two", "MIS_TWO"}, {"inactive", "excluded", "MIS_EXCLUDED"},
	} {
		f.insert(t, ctx, "workflow_pv_mistableset", map[string]any{"materialguid": item.material, "tableid": item.table, "sql_tablename": item.sql})
	}
	return f
}

func (f *createPVIFixture) insert(t *testing.T, ctx context.Context, table string, values map[string]any) {
	t.Helper()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var columns, placeholders []string
	var args []any
	for i, key := range keys {
		require.Contains(t, f.columns[table], key)
		columns = append(columns, f.columns[table][key])
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		args = append(args, values[key])
	}
	businessExec(t, ctx, f.db, "INSERT INTO "+f.tables[table]+"("+strings.Join(columns, ",")+") VALUES ("+strings.Join(placeholders, ",")+")", args...)
}

func (f *createPVIFixture) call(explicitOutput bool) string {
	args := "$1::varchar,$2::varchar,$3::varchar,$4::varchar,$5::varchar,$6::varchar"
	if explicitOutput {
		args += ",$7::varchar"
	}
	return "CALL " + qualifiedFunction(f.schema, "createpvi") + "(" + args + ")"
}

func createPVIInputs() []any { return []any{"process", "pvi", "user", "李 四", "work", "ou"} }

func (f *createPVIFixture) rows(t *testing.T, ctx context.Context, table string) []createPVIRecord {
	t.Helper()
	var keys, columns []string
	for key := range f.columns[table] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		columns = append(columns, f.columns[table][key]+"::text")
	}
	rows, err := f.db.QueryContext(ctx, "SELECT "+strings.Join(columns, ",")+" FROM "+f.tables[table])
	require.NoError(t, err)
	defer rows.Close()
	var result []createPVIRecord
	for rows.Next() {
		values := make([]sql.NullString, len(keys))
		pointers := make([]any, len(keys))
		for i := range values {
			pointers[i] = &values[i]
		}
		require.NoError(t, rows.Scan(pointers...))
		record := make(createPVIRecord)
		for i, key := range keys {
			record[key] = values[i]
		}
		result = append(result, record)
	}
	require.NoError(t, rows.Err())
	return result
}

func requireCreatePVIRecord(t *testing.T, row createPVIRecord, expected map[string]any) {
	t.Helper()
	require.Len(t, expected, len(row), "assert every field in the independent fixture")
	for key, want := range expected {
		got, ok := row[key]
		require.True(t, ok, key)
		if want == nil {
			require.False(t, got.Valid, key)
		} else {
			require.True(t, got.Valid, key)
			require.Equal(t, fmt.Sprint(want), got.String, key)
		}
	}
}

func requireCreatePVIEmpty(t *testing.T, ctx context.Context, f *createPVIFixture) {
	t.Helper()
	for _, table := range createPVIOutputTables {
		var count int
		require.NoError(t, f.db.QueryRowContext(ctx, "SELECT count(*) FROM "+f.tables[table]).Scan(&count))
		require.Zero(t, count, table)
	}
}

func checkCreatePVIFields(t *testing.T, ctx context.Context, f *createPVIFixture) {
	var before, after time.Time
	require.NoError(t, f.db.QueryRowContext(ctx, "SELECT CURRENT_TIMESTAMP").Scan(&before))
	var handleURL string
	require.NoError(t, f.db.QueryRowContext(ctx, f.call(false), createPVIInputs()...).Scan(&handleURL))
	require.NoError(t, f.db.QueryRowContext(ctx, "SELECT CURRENT_TIMESTAMP").Scan(&after))
	require.Equal(t, "/approve?mode=edit&ProcessVersionInstanceGuid=pvi&WorkItemGuid=work", handleURL)
	pvis := f.rows(t, ctx, "workflow_pvi")
	require.Len(t, pvis, 1)
	date := pvis[0]["startdate"].String
	requireCreatePVIRecord(t, pvis[0], map[string]any{
		"processversioninstanceguid": "pvi", "processversioninstancename": "请假流程", "processversionguid": "version", "initiator": "user",
		"startdate": date, "status": 10, "is_subprocessversioninstance": 20, "activityguid_callme": "", "activityinstanceguid_callme": "",
		"synctype": 10, "note": "", "processguid": "process", "is_needsendearlywarning": 20, "is_needsendovertimenotify": 20,
		"mainpviguid": "", "mainpvactivityguid": "", "lockstatus": "20",
	})
	var dateWithinCall bool
	require.NoError(t, f.db.QueryRowContext(ctx, "SELECT "+f.columns["workflow_pvi"]["startdate"]+" BETWEEN $1 AND $2 FROM "+f.tables["workflow_pvi"], before, after).Scan(&dateWithinCall))
	require.True(t, dateWithinCall)
	var generated []string
	contexts := f.rows(t, ctx, "workflow_context_value")
	require.Len(t, contexts, 2)
	for _, row := range contexts {
		generated = append(generated, row["fieldguid"].String)
		fieldType, value := "varchar", any(createPVIContextValue)
		if row["fieldname"].String == "second-field" {
			fieldType, value = "nullable", nil
		} else {
			require.Equal(t, "first-field", row["fieldname"].String)
		}
		requireCreatePVIRecord(t, row, map[string]any{"fieldguid": row["fieldguid"].String, "processversioninstanceguid": "pvi", "fieldname": row["fieldname"].String, "fieldtype": fieldType, "fieldvalue": value})
	}
	materials := f.rows(t, ctx, "workflow_pvi_material")
	// Keep the original outer IN semantics: another version's row with an
	// eligible MaterialGuid is copied too; unrelated/inactive GUIDs are omitted.
	require.Len(t, materials, 3)
	materialID := materials[0]["materialinstanceguid"].String
	generated = append(generated, materialID)
	names := make(map[string]bool)
	for _, row := range materials {
		name := row["materialname"].String
		names[name] = true
		guid := "material-one"
		if name == "材料二" {
			guid = "material-two"
		}
		requireCreatePVIRecord(t, row, map[string]any{
			"materialinstanceguid": materialID, "materialinstancename": name, "adddate": date, "is_defined": 10, "processversioninstanceguid": "pvi",
			"materialguid": guid, "materialname": name, "source": "source:" + name, "type": 3, "attachstorageinfogroupguid": "",
			"pageurl_readandwrite": "/edit/" + name, "pageurl_read": "/read/" + name, "pageurl_print": "/print/" + name,
			"status": 10, "is_showingrid": 20, "clienttag": "tag:" + name, "is_initmistable": 10,
		})
	}
	require.Equal(t, map[string]bool{"材料一": true, "材料二": true, "其他版本同GUID材料": true}, names)
	misRows := f.rows(t, ctx, "workflow_pvi_mistablerow")
	require.Len(t, misRows, 2)
	for _, row := range misRows {
		generated = append(generated, row["mistablesetinstanceguid"].String)
		sqlName := "MIS_ONE"
		if row["tableid"].String == "table-two" {
			sqlName = "MIS_TWO"
		} else {
			require.Equal(t, "table-one", row["tableid"].String)
		}
		requireCreatePVIRecord(t, row, map[string]any{"materialinstanceguid": materialID, "tableid": row["tableid"].String, "sql_tablename": sqlName, "pviguid": "pvi", "mistablesetinstanceguid": row["mistablesetinstanceguid"].String})
	}
	activities := f.rows(t, ctx, "workflow_activity_instance")
	require.Len(t, activities, 2)
	activityIDs := make(map[string]string)
	for _, row := range activities {
		guid := row["activityguid"].String
		status := 20
		if guid == "start" {
			status = 80
		} else {
			require.Equal(t, "first", guid)
		}
		activityIDs[guid] = row["activityinstanceguid"].String
		generated = append(generated, row["activityinstanceguid"].String)
		requireCreatePVIRecord(t, row, map[string]any{
			"activityinstanceguid": row["activityinstanceguid"].String, "processversionguid": "version", "processversioninstanceguid": "pvi",
			"subprocessversioninstanceguid": "", "activityguid": guid, "startdate": date, "status": status, "rollbacktag": 20, "note": "",
		})
	}
	transitions := f.rows(t, ctx, "workflow_transition_instance")
	require.Len(t, transitions, 1)
	transitionID := transitions[0]["transitioninstanceguid"].String
	generated = append(generated, transitionID)
	requireCreatePVIRecord(t, transitions[0], map[string]any{
		"transitioninstanceguid": transitionID, "transitionguid": "transition", "processversioninstanceguid": "pvi",
		"sourceactiviryguid": "start", "sourceactiviryname": "开始", "srcactinstguid": activityIDs["start"], "srcactinstname": "开始",
		"targetactivityguid": "first", "targetactivityname": "审批", "tgtactinstguid": activityIDs["first"], "tgtactinstname": "审批",
		"type": 20, "transitdate": date, "direction": 10,
	})
	workItems := f.rows(t, ctx, "workflow_workitem")
	require.Len(t, workItems, 1)
	waitID := workItems[0]["waithandleguid"].String
	generated = append(generated, waitID)
	requireCreatePVIRecord(t, workItems[0], map[string]any{
		"workitemguid": "work", "activityguid": "first", "activityname": "审批", "activityinstanceguid": activityIDs["first"], "workitemname": "【审批】请假流程",
		"workitemtype": "FirstWorkItem", "workitemmodename": "审批模式", "handleurl": handleURL, "status": 10, "priority": 7,
		"senderguid": "user", "sendername": "李 四", "transactor": "user", "transactorname": "李 四", "jobguid": "", "waithandleguid": waitID,
		"createdate": date, "limitminutes": -1, "timelimit": -1, "timelimitunit": 50, "is_needsendearlywarning": 20, "is_needsendovertimenotify": 20,
		"processguid": "process", "processversionguid": "version", "processversionname": "流程版一", "processversioninstanceguid": "pvi",
		"processversioninstancename": "请假流程", "groupguid": "", "agentforuserguid": "", "agentforworkitemguid": "",
		"operatorfordisplayguid": "user", "operatorfordisplayname": "李 四", "note": "",
	})
	messages := f.rows(t, ctx, "messages_center")
	require.Len(t, messages, 1)
	requireCreatePVIRecord(t, messages[0], map[string]any{
		"messageitemguid": waitID, "title": "【审批】请假流程", "messagetype": "审批模式", "content": "", "sendmode": 4, "generatedate": date,
		"isschedule": 0, "schedulesenddate": nil, "messagetarget": "", "targetuser": "user", "targetdispname": "李 四", "fromuser": "user",
		"fromdispname": "李 四", "frommobile": "", "handleurl": handleURL, "beizhu": "审批", "ouguid": "ou", "baseouguid": "", "isshow": 0,
		"jobguid": "", "handletype": "审批模式", "clientidentifier": "process", "clienttag": "", "isdel": 0, "isnohandle": 0,
		"pviguid": "pvi", "overtimepoint": nil, "earlywarningpoint": nil,
	})
	seen := make(map[string]bool)
	for _, guid := range generated {
		require.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`, guid)
		require.False(t, seen[guid], "every independently generated identifier must be new: %s", guid)
		seen[guid] = true
	}
}

func checkCreatePVIExplicitOutput(t *testing.T, ctx context.Context, f *createPVIFixture) {
	businessExec(t, ctx, f.db, "UPDATE "+f.tables["workflow_activity"]+" SET "+f.columns["workflow_activity"]["handleurl"]+"='/approve' WHERE "+f.columns["workflow_activity"]["activityguid"]+"='first'")
	inputs := append(createPVIInputs(), "ignored-input-value")
	var handleURL string
	require.NoError(t, f.db.QueryRowContext(ctx, f.call(true), inputs...).Scan(&handleURL))
	require.Equal(t, "/approve?ProcessVersionInstanceGuid=pvi&WorkItemGuid=work", handleURL)
	for _, table := range []string{"workflow_workitem", "messages_center"} {
		rows := f.rows(t, ctx, table)
		require.Len(t, rows, 1)
		require.Equal(t, handleURL, rows[0]["handleurl"].String)
	}
}

func checkCreatePVICharacterStatuses(t *testing.T, ctx context.Context, f *createPVIFixture) {
	for _, item := range []struct{ table, column string }{
		{"workflow_processversion", "status"}, {"workflow_activity", "activitytype"},
		{"workflow_transition", "is_showasoperationbutton"}, {"workflow_pv_material", "status"},
	} {
		column := f.columns[item.table][item.column]
		businessExec(t, ctx, f.db, "ALTER TABLE "+f.tables[item.table]+" ALTER COLUMN "+column+" TYPE varchar USING "+column+"::varchar")
	}
	checkCreatePVIFields(t, ctx, f)
}

func checkCreatePVINullURL(t *testing.T, ctx context.Context, f *createPVIFixture) {
	businessExec(t, ctx, f.db, "UPDATE "+f.tables["workflow_activity"]+" SET "+f.columns["workflow_activity"]["handleurl"]+"=NULL WHERE "+f.columns["workflow_activity"]["activityguid"]+"='first'")
	var handleURL sql.NullString
	require.NoError(t, f.db.QueryRowContext(ctx, f.call(false), createPVIInputs()...).Scan(&handleURL))
	require.False(t, handleURL.Valid)
	for _, table := range []string{"workflow_workitem", "messages_center"} {
		rows := f.rows(t, ctx, table)
		require.Len(t, rows, 1)
		require.False(t, rows[0]["handleurl"].Valid, table)
	}
}

func checkCreatePVIEmptyOptional(t *testing.T, ctx context.Context, f *createPVIFixture) {
	for _, table := range []string{"workflow_context", "workflow_pv_material", "workflow_pv_mistableset"} {
		businessExec(t, ctx, f.db, "DELETE FROM "+f.tables[table])
	}
	var handleURL string
	require.NoError(t, f.db.QueryRowContext(ctx, f.call(false), createPVIInputs()...).Scan(&handleURL))
	require.Equal(t, "/approve?mode=edit&ProcessVersionInstanceGuid=pvi&WorkItemGuid=work", handleURL)
	for _, table := range []string{"workflow_context_value", "workflow_pvi_material", "workflow_pvi_mistablerow"} {
		require.Empty(t, f.rows(t, ctx, table), table)
	}
	for _, table := range []string{"workflow_pvi", "workflow_transition_instance", "workflow_workitem", "messages_center"} {
		require.Len(t, f.rows(t, ctx, table), 1, table)
	}
	require.Len(t, f.rows(t, ctx, "workflow_activity_instance"), 2)
}

func checkCreatePVIConfiguration(t *testing.T, ctx context.Context, f *createPVIFixture) {
	type mutation struct{ name, change, restore string }
	activity, version, transition := f.tables["workflow_activity"], f.tables["workflow_processversion"], f.tables["workflow_transition"]
	ac, vc, tc := f.columns["workflow_activity"], f.columns["workflow_processversion"], f.columns["workflow_transition"]
	for _, test := range []mutation{
		{"missing_active_version", "UPDATE " + version + " SET " + vc["status"] + "=20", "UPDATE " + version + " SET " + vc["status"] + "=10"},
		{"multiple_active_versions", "INSERT INTO " + version + "(" + vc["processguid"] + "," + vc["processversionguid"] + "," + vc["processversionname"] + "," + vc["status"] + ") VALUES ('process','another-active','另一版',10)", "DELETE FROM " + version + " WHERE " + vc["processversionguid"] + "='another-active'"},
		{"missing_start", "UPDATE " + activity + " SET " + ac["activitytype"] + "=11 WHERE " + ac["activityguid"] + "='start'", "UPDATE " + activity + " SET " + ac["activitytype"] + "=10 WHERE " + ac["activityguid"] + "='start'"},
		{"missing_successor", "UPDATE " + transition + " SET " + tc["is_showasoperationbutton"] + "=10", "UPDATE " + transition + " SET " + tc["is_showasoperationbutton"] + "=20"},
		{"missing_first_activity", "UPDATE " + transition + " SET " + tc["toactivityguid"] + "='missing'", "UPDATE " + transition + " SET " + tc["toactivityguid"] + "='first'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			businessExec(t, ctx, f.db, test.change)
			var handleURL sql.NullString
			err := f.db.QueryRowContext(ctx, f.call(false), createPVIInputs()...).Scan(&handleURL)
			require.ErrorContains(t, err, "createpvi: expected exactly one")
			require.NotEqual(t, sql.ErrNoRows, err, "must return a configuration error, not silently omit a result")
			requireCreatePVIEmpty(t, ctx, f)
			businessExec(t, ctx, f.db, test.restore)
		})
	}
}

func checkCreatePVIMessageConflict(t *testing.T, ctx context.Context, f *createPVIFixture) {
	target := f.columns["messages_center"]["targetuser"]
	businessExec(t, ctx, f.db, "ALTER TABLE "+f.tables["messages_center"]+" ADD UNIQUE ("+target+")")
	f.insert(t, ctx, "messages_center", map[string]any{"targetuser": "user", "title": "existing message"})
	var output sql.NullString
	err := f.db.QueryRowContext(ctx, f.call(false), createPVIInputs()...).Scan(&output)
	require.Error(t, err)
	require.Equal(t, "23505", compatibilitySQLState(err))
	for _, table := range createPVIOutputTables[:len(createPVIOutputTables)-1] {
		require.Empty(t, f.rows(t, ctx, table), table)
	}
	messages := f.rows(t, ctx, "messages_center")
	require.Len(t, messages, 1)
	require.Equal(t, "existing message", messages[0]["title"].String)
}

func checkCreatePVICallerTransaction(t *testing.T, ctx context.Context, f *createPVIFixture) {
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	var output string
	require.NoError(t, tx.QueryRowContext(ctx, f.call(false), createPVIInputs()...).Scan(&output))
	for _, table := range createPVIOutputTables {
		var count int
		require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM "+f.tables[table]).Scan(&count))
		require.Positive(t, count, table)
	}
	require.NoError(t, tx.Rollback())
	requireCreatePVIEmpty(t, ctx, f)
}

func checkCreatePVIDependencies(t *testing.T, ctx context.Context, f *createPVIFixture) {
	table := f.tables["messages_center"]
	column := f.columns["messages_center"]["earlywarningpoint"]
	businessExec(t, ctx, f.db, "ALTER TABLE "+table+" RENAME COLUMN "+column+" TO missing")
	var output sql.NullString
	err := f.db.QueryRowContext(ctx, f.call(false), createPVIInputs()...).Scan(&output)
	require.ErrorContains(t, err, "missing")
	requireCreatePVIEmpty(t, ctx, f)
	businessExec(t, ctx, f.db, "ALTER TABLE "+table+" RENAME COLUMN missing TO "+column)
	businessExec(t, ctx, f.db, "ALTER TABLE "+table+" ADD earlywarningpoint timestamp")
	err = f.db.QueryRowContext(ctx, f.call(false), createPVIInputs()...).Scan(&output)
	require.ErrorContains(t, err, "ambiguous")
	requireCreatePVIEmpty(t, ctx, f)
}
