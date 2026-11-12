package target

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type demo2VersionProjectionCheck struct {
	table, guidColumns, listColumns, uuidColumn, emptyColumns, nullColumns, preservedColumn string
}

var demo2VersionProjectionChecks = []demo2VersionProjectionCheck{
	{"workflow_activity", "activityguid copyfromactivityguid", "", "", "addattachfilesource", "is_passiftransctorispresenter is_passwhennotransactor", "callsubprocessguid"},
	{"workflow_activity_fieldaccess", "activitymaterialguid materialguid activityguid", "", "rowguid", "", "", "fieldchinesename"},
	{"workflow_activity_linkinfo", "activityguid", "", "linkguid", "", "", "linkurl"},
	{"workflow_activity_material", "activitymaterialguid materialguid activityguid", "", "", "", "", "mainpvactivityguid"},
	{"workflow_activity_operation", "operationguid activityguid transitionguid", "targetactivity", "", "", "", "operationpageurl"},
	{"workflow_config", "sourceguid", "", "rowguid", "", "", "configvalue"},
	{"workflow_context", "activityguid frommaterialguid", "", "fieldguid", "", "", "fieldvalue"},
	{"workflow_event", "sourceguid eventmethodguid", "", "eventguid", "", "", "eventtype"},
	{"workflow_invokemethod", "invokegroupguid methodguid", "", "invokeguid", "", "", "whenthrowexception"},
	{"workflow_method", "methodguid activityguid", "", "", "", "", "returnvaluetype"},
	{"workflow_method_parameter", "methodguid", "", "mpguid", "", "", "mpvalue"},
	{"workflow_participator", "sourceguid methodguid", "", "participatorguid", "", "", "dataid"},
	{"workflow_pv_material", "materialguid", "", "", "template_asinfogroupguid", "", "frommainpvmatguid"},
	{"workflow_pv_mistableset", "materialguid", "", "mistablesetguid", "", "", "sql_tablename"},
	{"workflow_pv_printadapter", "", "frommaterialguidlist", "rowguid", "", "", "toprinttempleteguid"},
	{"workflow_transition", "transitionguid fromactivityguid toactivityguid selecttransctormethodguid", "", "", "", "", "targetactivitytransactorsource"},
	{"workflow_transition_condition", "transitionguid methodguid", "", "conditionguid", "", "", "conditionexpression"},
}

func TestDemo2WorkflowVersionSQLIntegration(t *testing.T) {
	testPGCompatibilityProducts(t, func(t *testing.T, ctx context.Context, db *sql.DB, product string) {
		schema := newBusinessIntegrationSchema(t, ctx, db)
		w := builtinIntegrationWriter(db, schema, product)
		names := CompatibilityRoutineNames{Procedures: []string{"CopyProcessVersion"}}
		report, err := EnsureCompatibilityFunctions(ctx, w, names)
		require.NoError(t, err)
		require.Equal(t, 1, report.Created, "the procedure initializes before workflow tables exist")
		require.Len(t, report.Items, 1, "GUID helpers are not installed without source matches")
		require.Equal(t, "procedure", report.Items[0].Kind)
		require.Equal(t, "copyprocessversion", report.Items[0].Name)

		q := func(name string) string { return qualifiedFunction(schema, strings.ToUpper(name)) }
		const sourceVersion = "source-version"
		const targetVersion = "target-version"
		const newGUID = "99999999-aaaa-bbbb-cccc-abcdefghijkl"
		const sourceGUID = "long-prefix-123456789012"
		const targetGUID = "long-prefix-abcdefghijkl"
		const sourceList = "node-123456789012;tail-original"
		const targetList = "node-abcdefghijkl;tail-original"
		columnsForTable := make(map[string][]string, len(demo2WorkflowVersionCopyTables))
		for _, table := range demo2WorkflowVersionCopyTables {
			for _, field := range strings.Fields(table.projection) {
				column, _, _ := strings.Cut(field, "=")
				columnsForTable[table.name] = append(columnsForTable[table.name], column)
			}
		}
		for _, check := range demo2VersionProjectionChecks {
			columns := columnsForTable[check.table]
			var declarations, identifiers, parameters []string
			var values []any
			for _, column := range columns {
				identifier := source.QuotePGIdent(strings.ToUpper(column))
				declarations = append(declarations, identifier+" varchar")
				identifiers = append(identifiers, identifier)
				parameters = append(parameters, fmt.Sprintf("$%d", len(parameters)+1))
				var value any = "source-" + column
				if column == "processversionguid" {
					value = sourceVersion
				}
				for _, guid := range strings.Fields(check.guidColumns) {
					if guid == column {
						value = sourceGUID
					}
				}
				for _, list := range strings.Fields(check.listColumns) {
					if list == column {
						value = sourceList
					}
				}
				values = append(values, value)
			}
			businessExec(t, ctx, db, "CREATE TABLE "+q(check.table)+" ("+strings.Join(declarations, ", ")+")")
			businessExec(t, ctx, db, "INSERT INTO "+q(check.table)+" ("+strings.Join(identifiers, ", ")+") VALUES ("+
				strings.Join(parameters, ", ")+")", values...)
			businessExec(t, ctx, db, "INSERT INTO "+q(check.table)+` ("PROCESSVERSIONGUID") VALUES ('unrelated')`)
		}
		businessExec(t, ctx, db, "CREATE TABLE "+q("workflow_processversion")+` ("PROCESSVERSIONGUID" varchar)`)
		businessExec(t, ctx, db, "INSERT INTO "+q("workflow_processversion")+" VALUES ($1),($2)", targetVersion, "unrelated")
		copyCall := "CALL " + qualifiedFunction(schema, "copyprocessversion") + "($1::varchar,$2::varchar,$3::varchar)"
		businessExec(t, ctx, db, copyCall, sourceVersion, targetVersion, newGUID)

		for _, check := range demo2VersionProjectionChecks {
			var result []byte
			require.NoError(t, db.QueryRowContext(ctx, "SELECT pg_catalog.row_to_json(r)::text FROM "+q(check.table)+
				` AS r WHERE r."PROCESSVERSIONGUID"=$1`, targetVersion).Scan(&result))
			var row map[string]*string
			require.NoError(t, json.Unmarshal(result, &row))
			checkValue := func(column, expected string) {
				actual, exists := row[strings.ToUpper(column)]
				require.True(t, exists, "%s.%s", check.table, column)
				require.NotNil(t, actual, "%s.%s", check.table, column)
				require.Equal(t, expected, *actual, "%s.%s", check.table, column)
			}
			for _, guid := range strings.Fields(check.guidColumns) {
				checkValue(guid, targetGUID)
			}
			for _, list := range strings.Fields(check.listColumns) {
				checkValue(list, targetList)
			}
			if check.uuidColumn != "" {
				actual := row[strings.ToUpper(check.uuidColumn)]
				require.NotNil(t, actual, check.table)
				require.Regexp(t, `^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$`, *actual, check.table)
			}
			for _, column := range strings.Fields(check.emptyColumns) {
				checkValue(column, "")
			}
			for _, column := range strings.Fields(check.nullColumns) {
				actual, exists := row[strings.ToUpper(column)]
				require.True(t, exists, "%s.%s", check.table, column)
				require.Nil(t, actual, "%s.%s", check.table, column)
			}
			checkValue(check.preservedColumn, "source-"+check.preservedColumn)
			checkValue("processversionguid", targetVersion)
		}

		// An error in the final table leaves the earlier copies rolled back.
		businessExec(t, ctx, db, "ALTER TABLE "+q("workflow_transition_condition")+
			` ADD CONSTRAINT copy_rollback CHECK ("PROCESSVERSIONGUID"<>'bad-version')`)
		_, err = db.ExecContext(ctx, copyCall, sourceVersion, "bad-version", newGUID)
		require.ErrorContains(t, err, "copy_rollback")
		for _, check := range demo2VersionProjectionChecks {
			require.Zero(t, demoCount(t, ctx, db, "SELECT count(*) FROM "+q(check.table)+` WHERE "PROCESSVERSIONGUID"='bad-version'`), check.table)
		}

		report, err = EnsureCompatibilityFunctions(ctx, w, CompatibilityRoutineNames{Procedures: []string{"DeleteProcessVersion"}})
		require.NoError(t, err)
		require.Equal(t, 1, report.Created)
		require.Len(t, report.Items, 1)
		businessExec(t, ctx, db, "CALL "+qualifiedFunction(schema, "deleteprocessversion")+"($1::varchar)", targetVersion)
		for _, check := range demo2VersionProjectionChecks {
			require.Zero(t, demoCount(t, ctx, db, "SELECT count(*) FROM "+q(check.table)+` WHERE "PROCESSVERSIONGUID"=$1`, targetVersion), check.table)
			require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+q(check.table)), check.table)
		}
		require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("workflow_processversion")+` WHERE "PROCESSVERSIONGUID"='unrelated'`))
		require.Zero(t, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("workflow_processversion")+` WHERE "PROCESSVERSIONGUID"=$1`, targetVersion))

		report, err = EnsureCompatibilityFunctions(ctx, w, CompatibilityRoutineNames{Procedures: []string{"CopyProcessVersion", "DeleteProcessVersion"}})
		require.NoError(t, err)
		require.Equal(t, 2, report.Reused)

		if product == "postgres" {
			t.Run("preinstalled_helpers", func(t *testing.T) {
				checkDemo2VersionUsesPreinstalledHelpers(t, ctx, db, schema, w, copyCall, sourceVersion, newGUID, sourceGUID, sourceList)
			})
		}
	})
}

func checkDemo2VersionUsesPreinstalledHelpers(t *testing.T, ctx context.Context, db *sql.DB, schema string, w Writer, copyCall, sourceVersion, newGUID, sourceGUID, sourceList string) {
	t.Helper()
	businessExec(t, ctx, db, "DROP PROCEDURE "+qualifiedFunction(schema, "copyprocessversion")+"(varchar,varchar,varchar)")
	const uuid = "01234567-89ab-cdef-0123-456789abcdef"
	helperDefinitions := []struct{ name, args, result string }{
		{"convertguid", "varchar,varchar", "'existing-guid:' || $2"},
		{"convertguidlist", "varchar,varchar", "'existing-list:' || $2"},
		{"get_uuid", "", "'" + uuid + "'"},
	}
	definitions := make(map[string]string, len(helperDefinitions))
	const definitionSQL = "SELECT pg_catalog.pg_get_functiondef($1::regprocedure::oid)"
	for _, helper := range helperDefinitions {
		identity := qualifiedFunction(schema, helper.name) + "(" + helper.args + ")"
		businessExec(t, ctx, db, "CREATE FUNCTION "+identity+" RETURNS varchar LANGUAGE sql AS $$SELECT ("+helper.result+")::varchar$$")
		var definition string
		require.NoError(t, db.QueryRowContext(ctx, definitionSQL, identity).Scan(&definition))
		definitions[identity] = definition
	}

	report, err := EnsureCompatibilityFunctions(ctx, w, CompatibilityRoutineNames{Procedures: []string{"CopyProcessVersion"}})
	require.NoError(t, err)
	require.Equal(t, 1, report.Created)
	require.Len(t, report.Items, 1)
	require.Equal(t, "copyprocessversion", report.Items[0].Name)
	for identity, expected := range definitions {
		var definition string
		require.NoError(t, db.QueryRowContext(ctx, definitionSQL, identity).Scan(&definition))
		require.Equal(t, expected, definition)
	}
	var inventory string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT pg_catalog.string_agg(p.proname::text,',' ORDER BY p.proname)
 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1`, schema).Scan(&inventory))
	require.Equal(t, "convertguid,convertguidlist,copyprocessversion,deleteprocessversion,get_uuid", inventory)

	const targetVersion = "preinstalled-version"
	businessExec(t, ctx, db, copyCall, sourceVersion, targetVersion, newGUID)
	for _, check := range []struct{ table, column, expected string }{
		{"WORKFLOW_ACTIVITY", "ACTIVITYGUID", "existing-guid:" + sourceGUID},
		{"WORKFLOW_ACTIVITY_OPERATION", "TARGETACTIVITY", "existing-list:" + sourceList},
		{"WORKFLOW_ACTIVITY_LINKINFO", "LINKGUID", uuid},
	} {
		var value string
		query := "SELECT " + source.QuotePGIdent(check.column) + " FROM " + qualifiedFunction(schema, check.table) + ` WHERE "PROCESSVERSIONGUID"=$1`
		require.NoError(t, db.QueryRowContext(ctx, query, targetVersion).Scan(&value))
		require.Equal(t, check.expected, value, check.column)
	}
}

func TestDemo2VersionGUIDFallbackMatchesBuiltinSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx := context.Background()
	schema := newBusinessIntegrationSchema(t, ctx, db)
	for _, name := range []string{"convertguid", "convertguidlist"} {
		businessExec(t, ctx, db, businessFunctionDDL(schema, "pg_catalog", name))
	}
	render := strings.NewReplacer("{{catalog}}", "pg_catalog", "{{schema}}", source.QuotePGIdent(schema))
	for _, kind := range []struct {
		name        string
		expressions []string
	}{
		{"convertguid", []string{demo2VersionConvertGUID("$1::varchar", false)}},
		{"convertguidlist", []string{demo2VersionConvertGUIDList("$1::varchar", false), demo2VersionConvertGUIDList("$1::varchar", true)}},
	} {
		t.Run(kind.name, func(t *testing.T) {
			for _, expression := range kind.expressions {
				for _, input := range []any{nil, "", ";", ";;", "abcdefghijklmnop", "中文字符尾巴  ", "a;中文字符尾巴;tail  ", "a;中文字符尾巴;;"} {
					for _, guid := range []any{nil, "", "short", "1234567890123", "1234567890123456 ", "99999999-aaaa-bbbb-cccc-abcdefghijkl"} {
						var want, actual sql.NullString
						var unused int
						query := "SELECT " + qualifiedFunction(schema, kind.name) + "($3::varchar,$1::varchar)," +
							render.Replace(expression) + ",$2::integer"
						require.NoError(t, db.QueryRowContext(ctx, query, input, 0, guid).Scan(&want, &actual, &unused))
						require.Equal(t, want, actual, "input=%q, guid=%q", input, guid)
					}
				}
			}
		})
	}
}
