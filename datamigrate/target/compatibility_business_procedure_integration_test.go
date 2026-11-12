package target

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testPGCompatibilityProducts(t *testing.T, check func(*testing.T, context.Context, *sql.DB, string)) {
	t.Helper()
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
			check(t, ctx, db, product.name)
		})
	}
}

func TestCompatibilityAutoMarkCountOverloadSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, tc := range []struct {
		name, parameters string
		conflicts        bool
	}{
		{"text_overload", "text, text, integer", true},
		{"one_text_input", "varchar, text, integer", true},
		{"integer_overload", "integer, integer, integer", false},
		{"bigint_passcode", "text, text, bigint", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := newBusinessIntegrationSchema(t, ctx, db)
			name := qualifiedFunction(schema, "automarkcount")
			businessExec(t, ctx, db, "CREATE PROCEDURE "+name+"("+tc.parameters+") LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'overload must not execute'; END; $$")
			before := procedureOverloadDefinitions(t, ctx, db, schema, "automarkcount")
			// A mixed-case source inventory selects only the two named procedures.
			inventory := []string{"AutoMarkCount", "PROC_CG_GONGGAOINFO_DELETE", "unknown", "AUTOMARKCOUNT"}
			report, err := ensurePGProcedureCompatibility(ctx, db, schema, "postgres", inventory)
			require.Len(t, report.Items, 2)
			require.Equal(t, "automarkcount", report.Items[0].Name)
			require.Equal(t, "proc_cg_gonggaoinfo_delete", report.Items[1].Name)
			require.Equal(t, "created", report.Items[1].Status)
			after := procedureOverloadDefinitions(t, ctx, db, schema, "automarkcount")
			if tc.conflicts {
				require.ErrorContains(t, err, "字符串字面量")
				require.Equal(t, 1, report.Failed)
				require.Equal(t, before, after)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 2, report.Created)
			require.Len(t, after, len(before)+1)
			for oid, definition := range before {
				require.Equal(t, definition, after[oid])
			}
			report, err = ensurePGProcedureCompatibility(ctx, db, schema, "postgres", inventory)
			require.NoError(t, err)
			require.Equal(t, 2, report.Reused)
			_, err = db.ExecContext(ctx, "CALL "+name+"('exam', 'user', 1)")
			require.ErrorContains(t, err, "table examuserlist is missing")
		})
	}
}
