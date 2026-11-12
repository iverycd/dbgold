package target

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCompatibilityDependencySelectionSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	t.Run("inline_logic_without_extra_functions", func(t *testing.T) {
		schema := newBusinessIntegrationSchema(t, ctx, db)
		w := builtinIntegrationWriter(db, schema, "postgres")
		q := func(name string) string { return qualifiedFunction(schema, name) }
		businessExec(t, ctx, db, "CREATE FUNCTION "+q("getscore")+"(text) RETURNS integer LANGUAGE sql AS $$SELECT 999$$")
		report, err := EnsureCompatibilityFunctions(ctx, w, CompatibilityRoutineNames{
			Functions: []string{"ConvertGuidList", "GetCategoryScore", "NEWID"},
		})
		require.NoError(t, err)
		require.Equal(t, 3, report.Created)
		for _, signature := range []string{q("convertguid") + "(varchar,varchar)", q("getscore") + "(varchar)", q("get_uuid") + "()"} {
			var missing bool
			require.NoError(t, db.QueryRowContext(ctx, "SELECT to_regprocedure($1) IS NULL", signature).Scan(&missing))
			require.True(t, missing, signature)
		}
		var converted, id string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT "+q("convertguidlist")+"('new-123456789012','abc;def'), "+q("newid")+"()").Scan(&converted, &id))
		require.Equal(t, "123;def", converted)
		require.Regexp(t, `^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`, id)
		businessExec(t, ctx, db, "CREATE TABLE "+q("WeBdB_Information")+`("InfoID" varchar, "UserGuid" varchar, "PubInWebDate" timestamp)`)
		businessExec(t, ctx, db, "CREATE TABLE "+q("View_InfoMain")+`("InfoID" varchar, "Score" integer)`)
		businessExec(t, ctx, db, "INSERT INTO "+q("WeBdB_Information")+" VALUES ('a','user','2026-01-01'),('b','user','2026-01-01'),('missing','user','2026-01-01')")
		businessExec(t, ctx, db, "INSERT INTO "+q("View_InfoMain")+" VALUES ('a',1),('a',4),('b',9)")
		var score int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT "+q("getcategoryscore")+"('user','2026-01-01','2026-01-02')").Scan(&score))
		require.Equal(t, 13, score)
	})

	t.Run("reuse_existing_exact_dependency", func(t *testing.T) {
		schema := newBusinessIntegrationSchema(t, ctx, db)
		w := builtinIntegrationWriter(db, schema, "postgres")
		businessExec(t, ctx, db, "CREATE FUNCTION "+qualifiedFunction(schema, "convertguid")+"(varchar,varchar) RETURNS varchar LANGUAGE sql AS $$SELECT 'existing'::varchar$$")
		report, err := EnsureCompatibilityFunctions(ctx, w, CompatibilityRoutineNames{Functions: []string{"ConvertGuidList"}})
		require.NoError(t, err)
		require.Equal(t, 1, report.Created)
		var converted string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT "+qualifiedFunction(schema, "convertguidlist")+"('new','abc;def')").Scan(&converted))
		require.Equal(t, "existing;def", converted)
	})

	t.Run("uuid_procedure_conflict_is_preserved", func(t *testing.T) {
		schema := newBusinessIntegrationSchema(t, ctx, db)
		w := builtinIntegrationWriter(db, schema, "postgres")
		q := qualifiedFunction(schema, "get_uuid")
		businessExec(t, ctx, db, "CREATE PROCEDURE "+q+"() LANGUAGE plpgsql AS $$BEGIN NULL; END$$")
		report, err := EnsureCompatibilityFunctions(ctx, w, CompatibilityRoutineNames{Functions: []string{"Get_UUID"}})
		require.ErrorContains(t, err, "已保留原定义")
		require.Equal(t, 1, report.Failed)
		var kind string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT prokind FROM pg_catalog.pg_proc WHERE oid=$1::regprocedure", q+"()").Scan(&kind))
		require.Equal(t, "p", kind)
	})

	t.Run("createpvi_with_no_get_uuid", func(t *testing.T) {
		schema := newBusinessIntegrationSchema(t, ctx, db)
		w := builtinIntegrationWriter(db, schema, "postgres")
		report, err := EnsureCompatibilityFunctions(ctx, w, CompatibilityRoutineNames{Procedures: []string{"CreatePVI"}})
		require.NoError(t, err)
		require.Len(t, report.Items, 1)
		require.Equal(t, "created", report.Items[0].Status)
		f := newCreatePVIFixture(t, ctx, db, schema)
		businessExec(t, ctx, db, "DROP FUNCTION "+qualifiedFunction(schema, "get_uuid")+"()")
		checkCreatePVIFields(t, ctx, f)
		var oid sql.NullInt64
		require.NoError(t, db.QueryRowContext(ctx, "SELECT to_regprocedure($1)::oid", qualifiedFunction(schema, "get_uuid")+"()").Scan(&oid))
		require.False(t, oid.Valid)
	})
}
