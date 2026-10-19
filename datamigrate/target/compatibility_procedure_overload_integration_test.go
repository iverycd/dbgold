package target

import (
	"context"
	"database/sql"
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
