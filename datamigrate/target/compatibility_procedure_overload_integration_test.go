package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Only inspect catalog candidates: these deliberately failing user routines
// must never be called while deciding which compatibility object to create.
func TestCompatibilityProcedureLiteralOverloadsSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, spec := range compatibilityProcedureSpecs {
		if !spec.writableCTE {
			continue
		}
		t.Run(spec.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, args        string
				canonical, domain bool
				conflicts         bool
			}{
				{name: "text", args: "p text", conflicts: true},
				{name: "text_with_canonical", args: "p text", canonical: true, conflicts: true},
				{name: "name", args: "p name", conflicts: true},
				{name: "bpchar", args: "p character", conflicts: true},
				{name: "string_domain", domain: true, conflicts: true},
				{name: "trailing_default", args: "p text, extra integer DEFAULT 0", conflicts: true},
				{name: "variadic", args: "VARIADIC p text[]", conflicts: true},
				{name: "out_only_string", args: "OUT response text", conflicts: true},
				{name: "required_second_argument", args: "p text, extra integer"},
				{name: "required_out_argument", args: "p text, OUT response integer"},
				{name: "integer", args: "p integer"},
				{name: "integer_trailing_default", args: "p integer, extra text DEFAULT ''"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					schema := newBusinessIntegrationSchema(t, ctx, db)
					name := qualifiedFunction(schema, spec.name)
					if tc.canonical {
						businessExec(t, ctx, db, "CREATE PROCEDURE "+name+"(p varchar) LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'canonical routine must not execute'; END; $$")
					}
					args := tc.args
					if tc.domain {
						domain := qualifiedFunction(schema, "string_domain")
						businessExec(t, ctx, db, "CREATE DOMAIN "+domain+" AS text")
						args = "p " + domain
					}
					businessExec(t, ctx, db, "CREATE PROCEDURE "+name+"("+args+") LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'overload must not execute'; END; $$")
					before := procedureOverloadDefinitions(t, ctx, db, schema, spec.name)
					report, err := ensurePGProcedureSpecCompatibility(ctx, db, schema, "postgres", spec)
					after := procedureOverloadDefinitions(t, ctx, db, schema, spec.name)
					if tc.conflicts {
						require.ErrorContains(t, err, "字符串字面量")
						require.Equal(t, 1, report.Failed)
						require.Equal(t, before, after, "existing definitions and candidate set must remain untouched")
					} else {
						require.NoError(t, err)
						require.Equal(t, 1, report.Created)
						require.Len(t, after, len(before)+1)
						for oid, definition := range before {
							require.Equal(t, definition, after[oid], "unrelated overload must be preserved")
						}
					}
				})
			}
		})
	}
}

func procedureOverloadDefinitions(t *testing.T, ctx context.Context, db *sql.DB, schema, name string) map[int64]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT p.oid,pg_catalog.pg_get_functiondef(p.oid) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname=$2`, schema, name)
	require.NoError(t, err)
	defer rows.Close()
	definitions := make(map[int64]string)
	for rows.Next() {
		var oid int64
		var definition string
		require.NoError(t, rows.Scan(&oid, &definition))
		definitions[oid] = definition
	}
	require.NoError(t, rows.Err())
	return definitions
}

// The optional INOUT value adds a second documented CALL arity. Protect both
// arities and preserve every existing candidate without executing its body.
func TestCreatePVIProcedureOverloadsSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	spec := compatibilityProcedureSpecs[2]
	varcharInputs := createPVIInputArguments("varchar")
	textInputs := createPVIInputArguments("text")
	for _, tc := range []struct {
		name, args string
		conflicts  bool
	}{
		{"six_varchar", varcharInputs, true},
		{"six_text", textInputs, true},
		{"mixed_string_positions", strings.Replace(varcharInputs, "p_username varchar", "p_username text", 1), true},
		{"seven_text_inout_default", textInputs + ", INOUT p_handleurl text DEFAULT NULL", true},
		{"seven_required_strings", textInputs + ", p_handleurl text", true},
		{"trailing_integer_default", varcharInputs + ", extra integer DEFAULT 0", true},
		{"variadic", varcharInputs + ", VARIADIC extra text[]", true},
		{"required_string_out", varcharInputs + ", OUT response text", true},
		{"six_integers", createPVIInputArguments("integer"), false},
		{"required_eighth_argument", textInputs + ", p_handleurl text, extra integer", false},
		{"required_integer_out", textInputs + ", OUT response integer", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := newBusinessIntegrationSchema(t, ctx, db)
			name := qualifiedFunction(schema, spec.name)
			businessExec(t, ctx, db, "CREATE PROCEDURE "+name+"("+tc.args+") LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'overload must not execute'; END$$")
			before := procedureOverloadDefinitions(t, ctx, db, schema, spec.name)
			report, err := ensurePGProcedureSpecCompatibility(ctx, db, schema, "postgres", spec)
			after := procedureOverloadDefinitions(t, ctx, db, schema, spec.name)
			if tc.conflicts {
				require.ErrorContains(t, err, "字符串字面量")
				require.Equal(t, 1, report.Failed)
				require.Equal(t, before, after)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, report.Created)
				require.Len(t, after, len(before)+1)
				for oid, definition := range before {
					require.Equal(t, definition, after[oid])
				}
			}
		})
	}
}

func TestCreatePVIIncompatibleDefinitionsSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	spec := compatibilityProcedureSpecs[2]
	inputs := createPVIInputArguments("varchar")
	for _, tc := range []struct {
		name, kind, args, returns string
	}{
		{"function_identity", "FUNCTION", inputs + ", p_handleurl varchar", " RETURNS varchar"},
		{"all_in_no_output", "PROCEDURE", inputs + ", p_handleurl varchar DEFAULT NULL", ""},
		{"missing_output_default", "PROCEDURE", inputs + ", INOUT p_handleurl varchar", ""},
		{"extra_input_default", "PROCEDURE", strings.Replace(inputs, "p_ouguid varchar", "p_ouguid varchar DEFAULT NULL", 1) + ", INOUT p_handleurl varchar DEFAULT NULL", ""},
		{"extra_out_argument", "PROCEDURE", inputs + ", OUT extra varchar, INOUT p_handleurl varchar DEFAULT NULL", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := newBusinessIntegrationSchema(t, ctx, db)
			name := qualifiedFunction(schema, spec.name)
			businessExec(t, ctx, db, "CREATE "+tc.kind+" "+name+"("+tc.args+")"+tc.returns+" LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'existing definition must not execute'; END$$")
			before := procedureOverloadDefinitions(t, ctx, db, schema, spec.name)
			report, err := ensurePGProcedureSpecCompatibility(ctx, db, schema, "postgres", spec)
			require.ErrorContains(t, err, "已保留原定义")
			require.Equal(t, 1, report.Failed)
			require.Equal(t, before, procedureOverloadDefinitions(t, ctx, db, schema, spec.name))
		})
	}
}

func TestCreatePVIReusesDefaultOutputProcedureSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	spec := compatibilityProcedureSpecs[2]
	schema := newBusinessIntegrationSchema(t, ctx, db)
	name := qualifiedFunction(schema, spec.name)
	businessExec(t, ctx, db, "CREATE PROCEDURE "+name+"("+createPVIInputArguments("varchar")+", INOUT p_handleurl varchar DEFAULT NULL) LANGUAGE plpgsql AS $$BEGIN p_handleurl := COALESCE(p_handleurl, '/default') || '?pvi=' || p_processversioninstanceguid; END$$")
	before := procedureOverloadDefinitions(t, ctx, db, schema, spec.name)
	report, err := ensurePGProcedureSpecCompatibility(ctx, db, schema, "postgres", spec)
	require.NoError(t, err)
	require.Equal(t, 1, report.Reused)
	require.Equal(t, before, procedureOverloadDefinitions(t, ctx, db, schema, spec.name))
	for _, count := range []int{6, 7} {
		t.Run(fmt.Sprintf("arguments_%d", count), func(t *testing.T) {
			var url string
			args := "'process','pvi','user','username','workitem','ou'"
			want := "/default?pvi=pvi"
			if count == 7 {
				args += ",'/supplied'"
				want = "/supplied?pvi=pvi"
			}
			require.NoError(t, db.QueryRowContext(ctx, "CALL "+name+"("+args+")").Scan(&url))
			require.Equal(t, want, url)
		})
	}
}

func createPVIInputArguments(typ string) string {
	var arguments []string
	for _, name := range []string{"p_processguid", "p_processversioninstanceguid", "p_userguid", "p_username", "p_workitemguid", "p_ouguid"} {
		arguments = append(arguments, name+" "+typ)
	}
	return strings.Join(arguments, ", ")
}
