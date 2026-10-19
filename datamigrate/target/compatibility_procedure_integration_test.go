package target

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// Independent fixture copied from the supplied MySQL field lists. Do not derive
// these tables from the production mapping: that would hide an omitted field.
var archiveFixtureTables = []archiveTableSpec{
	{"workflow_activity_instance", "workflow_activity_instance_his", "processversioninstanceguid", fixtureColumns(`
Row_ID ActivityGuid ActivityInstanceGuid SUBPROCESSVERSIONINSTANCEGUID Status StartDate ENDTIME TERMINATEDATE
IS_OVERTIME ROLLBACKTAG ACTIVITYINSTANCENAME ProcessVersionGuid ProcessVersionInstanceGuid Note`)},
	{"workflow_pvi_mistablerow", "workflow_pvi_mistablerow_his", "pviguid", fixtureColumns(`
Row_ID MATERIALINSTANCEGUID TABLEID SQL_TABLENAME MISROWGUID PVIGUID MISTABLESETINSTANCEGUID`)},
	{"workflow_pvi_material", "workflow_pvi_material_his", "processversioninstanceguid", fixtureColumns(`
Row_ID MATERIALINSTANCEGUID MATERIALINSTANCENAME ADDDATE IS_DEFINED ADDINWORKITEMGUID ADDUSERGUID
PROCESSVERSIONINSTANCEGUID MATERIALGUID MATERIALNAME SOURCE TYPE ATTACHSTORAGEINFOGROUPGUID
PAGEURL_READANDWRITE PAGEURL_READ PAGEURL_PRINT STATUS IS_SHOWINGRID CLIENTTAG IS_INITMISTABLE FROMMAINPVIMATINSTANCEGUID`)},
	{"workflow_transition_instance", "workflow_transition_i_history", "processversioninstanceguid", fixtureColumns(`
TRANSITIONINSTANCEGUID TRANSITIONGUID PROCESSVERSIONINSTANCEGUID SOURCEACTIVIRYGUID SOURCEACTIVIRYNAME
SRCACTINSTGUID SRCACTINSTNAME TARGETACTIVITYGUID TARGETACTIVITYNAME TGTACTINSTGUID TGTACTINSTNAME TYPE TRANSITDATE DIRECTION`)},
	{"workflow_workitem", "workflow_workitem_history", "processversioninstanceguid", fixtureColumns(`
Row_ID WorkItemGuid ActivityGuid ActivityName ActivityInstanceGuid WorkItemName WorkItemType WorkItemModeName
HandleURL Status Priority SenderGuid SenderName Transactor TransactorName JobGuid OperationGuid ReadDate
OperationDate OperatorGuid OperatorName Opinion WaitHandleGuid CreateDate StartDate EndDate TerminateDate
StatusBeforeTerminate LimitMinutes ProcessGuid ProcessVersionGuid ProcessVersionName ProcessVersionInstanceGuid
ProcessVersionInstanceName GroupGuid Is_Locked LockerUserGuid LockerDispName LockDate AgentForUserGuid
OperationType LockerWindowGuid ABAuditOpinion TimeLimit TimeLimitUnit OverTimePoint EarlyWarningPoint
Is_NeedSendEarlyWarning Is_NeedSendOverTimeNotify AgentForWorkItemGuid OperatorForDisplayGuid OperatorForDisplayName Note TRANSITIONINSTANCEGUID`)},
	{"workflow_pvi_opinion", "workflow_pvi_opinion_his", "pviguid", fixtureColumns(`
OPINIONGUID WORKITEMGUID ACTIVITYGUID ACTIVITYNAME ACTIVITYINSTANCEGUID PVIGUID OPINIONTEXT ADDDATE ADDUSERGUID ADDUSERNAME`)},
	{"workflow_context_value", "workflow_context_value_history", "processversioninstanceguid", fixtureColumns(`
FIELDGUID PROCESSVERSIONINSTANCEGUID ACTIVITYINSTANCEGUID FIELDNAME FIELDTYPE FIELDVALUE`)},
}

func fixtureColumns(columns string) []string { return strings.Fields(strings.ToLower(columns)) }

func TestChangeDataToHistoryMapping(t *testing.T) {
	require.Equal(t, archiveFixtureTables, changeDataToHistoryTables, "preserve all seven mappings, field lists and original order")
	ddl := changeDataToHistoryDDL(`schema "quote'`, "pg_catalog", false)
	require.Equal(t, 1, strings.Count(ddl, "COMMIT;"))
	require.Contains(t, ddl, "SECURITY INVOKER")
	require.NotContains(t, ddl, "SET search_path")
	require.NotContains(t, ddl, "LOCK TABLE")
}

// The same archive cases are available to every supported product. No DSN
// defaults to a user's database: these tests require an explicit disposable DB.
func TestCompatibilityProcedureSQLIntegration(t *testing.T) {
	type archiveCase struct {
		name string
		run  func(*testing.T, context.Context, *archiveFixture)
	}
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
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			require.NoError(t, db.PingContext(ctx))
			cases := []archiveCase{
				{"scope_fields_and_repeat", checkArchiveScope},
				{"missing_root_record", checkArchiveMissingRoot},
				{"conflict_rolls_back_every_move", checkArchiveConflict},
				{"dependency_validation", checkArchiveDependencies},
				{"destination_type_error_rolls_back", checkArchiveTypeConflict},
				{"existing_routine_identity", checkArchiveRoutineIdentity},
			}
			if product.name == "postgres" {
				cases = append(cases,
					archiveCase{"caller_transaction_is_rejected", checkArchiveCallerTransaction},
					archiveCase{"concurrent_insert_and_repeat", checkArchiveConcurrency},
					archiveCase{"invoker_permissions", checkArchivePermissions},
					archiveCase{"trigger_skipped_insert_rolls_back", checkArchiveSkippedInsert},
					archiveCase{"catalog_definition_not_shadowed", checkArchiveCatalogShadow},
				)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					schema := newBusinessIntegrationSchema(t, ctx, db)
					// Create before any dependencies, then reuse the exact definition.
					report, err := ensurePGProcedureSpecCompatibility(ctx, db, schema, product.name, compatibilityProcedureSpecs[0])
					require.NoError(t, err)
					require.Equal(t, 1, report.Created)
					ddl := report.Items[0].DDL
					report, err = ensurePGProcedureSpecCompatibility(ctx, db, schema, product.name, compatibilityProcedureSpecs[0])
					require.NoError(t, err)
					require.Equal(t, 1, report.Reused)
					require.Contains(t, strings.ToLower(report.Items[0].DDL), "commit")
					f := newArchiveFixture(t, ctx, db, schema)
					f.product, f.ddl = product.name, ddl
					tc.run(t, ctx, f)
				})
			}
		})
	}
}

type archiveFixture struct {
	db      *sql.DB
	schema  string
	product string
	ddl     string
}

func (f *archiveFixture) table(name string, history bool) string {
	if !history {
		name = strings.ToUpper(name)
	}
	return qualifiedFunction(f.schema, name)
}

func archiveColumn(name string, history bool) string {
	if !history {
		name = strings.ToUpper(name)
	}
	return source.QuotePGIdent(name)
}

func (f *archiveFixture) call() string {
	return "CALL " + qualifiedFunction(f.schema, "changedatatohistory") + "($1::varchar)"
}

func newArchiveFixture(t *testing.T, ctx context.Context, db *sql.DB, schema string) *archiveFixture {
	t.Helper()
	f := &archiveFixture{db: db, schema: schema}
	businessExec(t, ctx, db, "CREATE TABLE "+f.table("workflow_pvi", false)+`("PROCESSVERSIONINSTANCEGUID" varchar,"MAINPVIGUID" varchar,"TAG" varchar)`)
	for _, pair := range archiveFixtureTables {
		for _, history := range []bool{false, true} {
			name := pair.source
			if history {
				name = pair.history
			}
			var fields []string
			for i, col := range pair.columns {
				definition := archiveColumn(col, history) + " varchar"
				if i == 0 {
					definition += " PRIMARY KEY"
				}
				fields = append(fields, definition)
			}
			businessExec(t, ctx, db, "CREATE TABLE "+f.table(name, history)+"("+strings.Join(fields, ",")+")")
		}
	}
	return f
}

func (f *archiveFixture) seed(t *testing.T, ctx context.Context, guids ...string) {
	t.Helper()
	for _, guid := range guids {
		for _, pair := range archiveFixtureTables {
			f.insert(t, ctx, pair, guid, "original")
		}
	}
}

func (f *archiveFixture) insert(t *testing.T, ctx context.Context, pair archiveTableSpec, guid, suffix string) {
	t.Helper()
	var values []any
	var placeholders []string
	for i, col := range pair.columns {
		var value any = fmt.Sprintf("%s:%s:%s:中文 ' \\", guid, suffix, col)
		if i == 0 {
			value = guid + ":" + suffix
		} else if col == pair.key {
			value = guid
		} else if i%9 == 0 {
			value = nil
		}
		values = append(values, value)
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
	}
	businessExec(t, ctx, f.db, "INSERT INTO "+f.table(pair.source, false)+" VALUES ("+strings.Join(placeholders, ",")+")", values...)
}

func (f *archiveFixture) rows(t *testing.T, ctx context.Context, pair archiveTableSpec, history bool, guid string) [][]sql.NullString {
	t.Helper()
	name := pair.source
	if history {
		name = pair.history
	}
	var cols []string
	for _, col := range pair.columns {
		cols = append(cols, archiveColumn(col, history))
	}
	rows, err := f.db.QueryContext(ctx, "SELECT "+strings.Join(cols, ",")+" FROM "+f.table(name, history)+" WHERE "+archiveColumn(pair.key, history)+"=$1 ORDER BY "+cols[0], guid)
	require.NoError(t, err)
	defer rows.Close()
	var result [][]sql.NullString
	for rows.Next() {
		values := make([]sql.NullString, len(cols))
		pointers := make([]any, len(cols))
		for i := range values {
			pointers[i] = &values[i]
		}
		require.NoError(t, rows.Scan(pointers...))
		result = append(result, values)
	}
	require.NoError(t, rows.Err())
	return result
}

func (f *archiveFixture) tag(t *testing.T, ctx context.Context, guid string) string {
	t.Helper()
	var value string
	require.NoError(t, f.db.QueryRowContext(ctx, "SELECT min(\"TAG\") FROM "+f.table("workflow_pvi", false)+` WHERE "PROCESSVERSIONINSTANCEGUID"=$1`, guid).Scan(&value))
	return value
}

func checkArchiveScope(t *testing.T, ctx context.Context, f *archiveFixture) {
	// Duplicates, root-as-child and grandchildren must not broaden the scope.
	businessExec(t, ctx, f.db, "INSERT INTO "+f.table("workflow_pvi", false)+" VALUES ('root','root','root-tag'),('c2','root','old'),('c1','root','old'),('c1','root','old'),('grandchild','c1','old'),('unrelated',NULL,'old')")
	f.seed(t, ctx, "root", "c1", "c2", "grandchild", "unrelated")
	want := map[string][][]sql.NullString{}
	for _, pair := range archiveFixtureTables {
		for _, guid := range []string{"root", "c1", "c2", "grandchild", "unrelated"} {
			want[pair.source+guid] = f.rows(t, ctx, pair, false, guid)
		}
	}
	// Empty caller search_path and a quoted schema exercise captured resolution.
	conn, err := f.db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(ctx, "SET search_path TO pg_catalog")
	require.NoError(t, err)
	for repeat := 0; repeat < 2; repeat++ {
		if repeat == 0 {
			// Exercise the documented standalone CALL with an uncast literal.
			_, err = conn.ExecContext(ctx, "CALL "+qualifiedFunction(f.schema, "changedatatohistory")+"('root')")
		} else {
			_, err = conn.ExecContext(ctx, f.call(), "root")
		}
		require.NoError(t, err)
		for _, pair := range archiveFixtureTables {
			for _, guid := range []string{"root", "c1", "c2"} {
				require.Empty(t, f.rows(t, ctx, pair, false, guid))
				require.Equal(t, want[pair.source+guid], f.rows(t, ctx, pair, true, guid), pair.source+guid)
			}
			for _, guid := range []string{"grandchild", "unrelated"} {
				require.Equal(t, want[pair.source+guid], f.rows(t, ctx, pair, false, guid))
				require.Empty(t, f.rows(t, ctx, pair, true, guid))
			}
		}
	}
	require.Equal(t, "root-tag", f.tag(t, ctx, "root"))
	require.Equal(t, "1", f.tag(t, ctx, "c1"))
	require.Equal(t, "1", f.tag(t, ctx, "c2"))
	require.Equal(t, "old", f.tag(t, ctx, "grandchild"))
}

func checkArchiveMissingRoot(t *testing.T, ctx context.Context, f *archiveFixture) {
	f.seed(t, ctx, "no-pvi-row")
	businessExec(t, ctx, f.db, f.call(), "no-pvi-row")
	for _, pair := range archiveFixtureTables {
		require.Empty(t, f.rows(t, ctx, pair, false, "no-pvi-row"))
		require.Len(t, f.rows(t, ctx, pair, true, "no-pvi-row"), 1)
	}
}

func checkArchiveConflict(t *testing.T, ctx context.Context, f *archiveFixture) {
	businessExec(t, ctx, f.db, "INSERT INTO "+f.table("workflow_pvi", false)+" VALUES ('root',NULL,'root-tag'),('child','root','child-tag')")
	f.seed(t, ctx, "root", "child")
	// Conflict in the main workflow happens after ALL child tables and tag update.
	pair := archiveFixtureTables[4]
	businessExec(t, ctx, f.db, "INSERT INTO "+f.table(pair.history, true)+" SELECT * FROM "+f.table(pair.source, false)+` WHERE "PROCESSVERSIONINSTANCEGUID"='root'`)
	_, err := f.db.ExecContext(ctx, f.call(), "root")
	require.Error(t, err)
	require.Equal(t, "23505", compatibilitySQLState(err))
	for i, pair := range archiveFixtureTables {
		for _, guid := range []string{"root", "child"} {
			require.Len(t, f.rows(t, ctx, pair, false, guid), 1)
			if i == 4 && guid == "root" {
				require.Len(t, f.rows(t, ctx, pair, true, guid), 1)
			} else {
				require.Empty(t, f.rows(t, ctx, pair, true, guid))
			}
		}
	}
	require.Equal(t, "child-tag", f.tag(t, ctx, "child"))
	require.Equal(t, "root-tag", f.tag(t, ctx, "root"))
}

func checkArchiveDependencies(t *testing.T, ctx context.Context, f *archiveFixture) {
	f.seed(t, ctx, "root")
	for _, value := range []any{nil, ""} {
		_, err := f.db.ExecContext(ctx, f.call(), value)
		require.ErrorContains(t, err, "must not be NULL or empty")
	}
	last := archiveFixtureTables[6]
	businessExec(t, ctx, f.db, "ALTER TABLE "+f.table(last.history, true)+" RENAME COLUMN fieldvalue TO missing")
	_, err := f.db.ExecContext(ctx, f.call(), "root")
	require.ErrorContains(t, err, "column fieldvalue is missing")
	businessExec(t, ctx, f.db, "ALTER TABLE "+f.table(last.history, true)+" RENAME COLUMN missing TO fieldvalue")
	businessExec(t, ctx, f.db, "ALTER TABLE "+f.table(last.history, true)+` ADD "FieldValue" varchar`)
	_, err = f.db.ExecContext(ctx, f.call(), "root")
	require.ErrorContains(t, err, "column fieldvalue is ambiguous")
	businessExec(t, ctx, f.db, "ALTER TABLE "+f.table(last.history, true)+` DROP COLUMN "FieldValue"`)
	businessExec(t, ctx, f.db, "CREATE TABLE "+f.table(last.source, true)+"(dummy integer)")
	_, err = f.db.ExecContext(ctx, f.call(), "root")
	require.ErrorContains(t, err, "table workflow_context_value is ambiguous")
	businessExec(t, ctx, f.db, "DROP TABLE "+f.table(last.source, true))
	businessExec(t, ctx, f.db, "DROP TABLE "+f.table(last.history, true))
	// An identically named object in another schema must never fill this gap.
	otherSchema := newBusinessIntegrationSchema(t, ctx, f.db)
	businessExec(t, ctx, f.db, "CREATE TABLE "+qualifiedFunction(otherSchema, last.history)+"(fieldvalue varchar)")
	_, err = f.db.ExecContext(ctx, f.call(), "root")
	require.ErrorContains(t, err, "table workflow_context_value_history is missing")
	for _, pair := range archiveFixtureTables {
		require.Len(t, f.rows(t, ctx, pair, false, "root"), 1)
	}
}

func checkArchiveCallerTransaction(t *testing.T, ctx context.Context, f *archiveFixture) {
	f.seed(t, ctx, "root")
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, f.call(), "root")
	require.Error(t, err)
	require.Equal(t, "2D000", compatibilitySQLState(err))
	require.NoError(t, tx.Rollback())
	for _, pair := range archiveFixtureTables {
		require.Len(t, f.rows(t, ctx, pair, false, "root"), 1)
		require.Empty(t, f.rows(t, ctx, pair, true, "root"))
	}
}

func checkArchiveTypeConflict(t *testing.T, ctx context.Context, f *archiveFixture) {
	f.seed(t, ctx, "root")
	businessExec(t, ctx, f.db, "ALTER TABLE "+f.table("workflow_context_value_history", true)+" ALTER COLUMN fieldvalue TYPE integer USING fieldvalue::integer")
	_, err := f.db.ExecContext(ctx, f.call(), "root")
	require.Error(t, err)
	for _, pair := range archiveFixtureTables {
		require.Len(t, f.rows(t, ctx, pair, false, "root"), 1)
		require.Empty(t, f.rows(t, ctx, pair, true, "root"))
	}
}

func checkArchiveConcurrency(t *testing.T, ctx context.Context, f *archiveFixture) {
	businessExec(t, ctx, f.db, "INSERT INTO "+f.table("workflow_pvi", false)+" VALUES ('root',NULL,'root-tag')")
	f.seed(t, ctx, "root")
	lockKey := time.Now().UnixNano()
	gate, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey)
	require.NoError(t, err)
	triggerFunction := qualifiedFunction(f.schema, "archive_test_gate")
	businessExec(t, ctx, f.db, "CREATE FUNCTION "+triggerFunction+"() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM pg_catalog.pg_advisory_xact_lock("+fmt.Sprint(lockKey)+"); RETURN NEW; END$$")
	first := archiveFixtureTables[0]
	businessExec(t, ctx, f.db, "CREATE TRIGGER archive_test_gate BEFORE INSERT ON "+f.table(first.history, true)+" FOR EACH ROW EXECUTE FUNCTION "+triggerFunction+"()")
	conn, err := f.db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	var pid int
	// Release the test barrier before closing a connection that could still be
	// executing CALL, including when an assertion below aborts the test.
	defer gate.Rollback()
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid))
	done := make(chan error, 1)
	go func() { _, callErr := conn.ExecContext(ctx, f.call(), "root"); done <- callErr }()
	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := f.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event='advisory')", pid).Scan(&waiting)
		return queryErr == nil && waiting
	}, 10*time.Second, 20*time.Millisecond)
	// The DELETE snapshot already ran; this new row must survive the first call.
	f.insert(t, ctx, first, "root", "late")
	require.NoError(t, gate.Commit())
	require.NoError(t, <-done)
	remaining := f.rows(t, ctx, first, false, "root")
	require.Len(t, remaining, 1)
	require.Equal(t, "root:late", remaining[0][0].String)
	require.Len(t, f.rows(t, ctx, first, true, "root"), 1)
	// Concurrent repeats serialize on workflow_pvi and move the late row once.
	for i := 0; i < 2; i++ {
		go func() { _, callErr := f.db.ExecContext(ctx, f.call(), "root"); done <- callErr }()
	}
	require.NoError(t, <-done)
	require.NoError(t, <-done)
	require.Empty(t, f.rows(t, ctx, first, false, "root"))
	require.Len(t, f.rows(t, ctx, first, true, "root"), 2)
}

func checkArchivePermissions(t *testing.T, ctx context.Context, f *archiveFixture) {
	var canCreateRole bool
	require.NoError(t, f.db.QueryRowContext(ctx, "SELECT rolsuper OR rolcreaterole FROM pg_roles WHERE rolname=current_user").Scan(&canCreateRole))
	if !canCreateRole {
		t.Skip("temporary invoker role requires CREATEROLE")
	}
	role := source.QuotePGIdent(fmt.Sprintf("dbgold_archive_%d", time.Now().UnixNano()))
	businessExec(t, ctx, f.db, "CREATE ROLE "+role)
	defer businessExec(t, ctx, f.db, "DROP ROLE "+role)
	businessExec(t, ctx, f.db, "GRANT USAGE ON SCHEMA "+source.QuotePGIdent(f.schema)+" TO "+role)
	defer businessExec(t, ctx, f.db, "REVOKE USAGE ON SCHEMA "+source.QuotePGIdent(f.schema)+" FROM "+role)
	f.seed(t, ctx, "root")
	conn, err := f.db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(ctx, "SET ROLE "+role)
	require.NoError(t, err)
	defer conn.ExecContext(ctx, "RESET ROLE")
	_, err = conn.ExecContext(ctx, f.call(), "root")
	require.Error(t, err)
	require.Equal(t, "42501", compatibilitySQLState(err))
	for _, pair := range archiveFixtureTables {
		require.Len(t, f.rows(t, ctx, pair, false, "root"), 1)
		require.Empty(t, f.rows(t, ctx, pair, true, "root"))
	}
}

func checkArchiveSkippedInsert(t *testing.T, ctx context.Context, f *archiveFixture) {
	f.seed(t, ctx, "root")
	triggerFunction := qualifiedFunction(f.schema, "skip_archive_row")
	businessExec(t, ctx, f.db, "CREATE FUNCTION "+triggerFunction+"() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NULL; END$$")
	businessExec(t, ctx, f.db, "CREATE TRIGGER skip_archive_row BEFORE INSERT ON "+f.table("workflow_context_value_history", true)+" FOR EACH ROW EXECUTE FUNCTION "+triggerFunction+"()")
	_, err := f.db.ExecContext(ctx, f.call(), "root")
	require.ErrorContains(t, err, "archive row count mismatch")
	for _, pair := range archiveFixtureTables {
		require.Len(t, f.rows(t, ctx, pair, false, "root"), 1)
		require.Empty(t, f.rows(t, ctx, pair, true, "root"))
	}
}

func checkArchiveCatalogShadow(t *testing.T, ctx context.Context, f *archiveFixture) {
	// The captured schema may legitimately contain same-named application code.
	// Initializing/reusing the procedure must never call it to read metadata.
	for _, spec := range []string{"pg_get_functiondef(oid)", "array_to_string(anyarray,text)"} {
		name, args, _ := strings.Cut(spec, "(")
		businessExec(t, ctx, f.db, "CREATE FUNCTION "+qualifiedFunction(f.schema, name)+"("+args+" RETURNS text LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'application function must not execute'; END$$")
	}
	report, err := ensurePGProcedureSpecCompatibility(ctx, f.db, f.schema, f.product, compatibilityProcedureSpecs[0])
	require.NoError(t, err)
	require.Equal(t, 1, report.Reused)
	require.Empty(t, report.Items[0].DefinitionWarning)
	require.Contains(t, strings.ToLower(report.Items[0].DDL), "commit")
}

func checkArchiveRoutineIdentity(t *testing.T, ctx context.Context, f *archiveFixture) {
	name := qualifiedFunction(f.schema, "changedatatohistory")
	businessExec(t, ctx, f.db, "DROP PROCEDURE "+name+"(varchar)")
	// Function and procedure identities share a namespace but must not be reused
	// interchangeably. Read the function afterward to prove it was preserved.
	businessExec(t, ctx, f.db, "CREATE FUNCTION "+name+"(varchar) RETURNS varchar LANGUAGE sql AS $$SELECT 'preserved'::varchar$$")
	report, err := ensurePGProcedureSpecCompatibility(ctx, f.db, f.schema, f.product, compatibilityProcedureSpecs[0])
	require.Error(t, err)
	require.Equal(t, 1, report.Failed)
	var value string
	require.NoError(t, f.db.QueryRowContext(ctx, "SELECT "+name+"('root'::varchar)").Scan(&value))
	require.Equal(t, "preserved", value)
	businessExec(t, ctx, f.db, "DROP FUNCTION "+name+"(varchar)")
	// Native headers on derived engines are covered by their initialization
	// matrix. Use the already accepted DDL header to construct these variants.
	ddl := f.ddl
	for _, args := range []string{"INOUT p_processversioninstanceguid varchar", "IN p_processversioninstanceguid varchar, IN extra integer DEFAULT 0"} {
		variant := strings.Replace(ddl, "IN p_processversioninstanceguid varchar", args, 1)
		businessExec(t, ctx, f.db, variant)
		report, err = ensurePGProcedureSpecCompatibility(ctx, f.db, f.schema, f.product, compatibilityProcedureSpecs[0])
		require.Error(t, err)
		require.Equal(t, 1, report.Failed)
		signature := "varchar"
		if strings.Contains(args, "integer") {
			signature += ",integer"
		}
		businessExec(t, ctx, f.db, "DROP PROCEDURE "+name+"("+signature+")")
	}
	// A recognized user implementation is reused without ever executing it.
	custom := strings.Replace(ddl, "\n COMMIT;", "\n RAISE EXCEPTION 'user implementation preserved';", 1)
	businessExec(t, ctx, f.db, custom)
	report, err = ensurePGProcedureSpecCompatibility(ctx, f.db, f.schema, f.product, compatibilityProcedureSpecs[0])
	require.NoError(t, err)
	require.Equal(t, 1, report.Reused)
	require.Contains(t, report.Items[0].DDL, "user implementation preserved")
}
