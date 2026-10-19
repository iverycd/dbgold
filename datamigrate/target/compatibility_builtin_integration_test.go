package target

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// These tests only use explicitly configured, disposable databases and create
// isolated schemas. Each available PG-family product runs the same SQL checks.
func TestCompatibilityBuiltinsSQLIntegration(t *testing.T) {
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
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			require.NoError(t, db.PingContext(ctx))
			schema := fmt.Sprintf("dbgold builtins_\"'\\_%d", time.Now().UnixNano())
			_, err = db.ExecContext(ctx, "CREATE SCHEMA "+source.QuotePGIdent(schema))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := db.ExecContext(context.Background(), "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
				require.NoError(t, err)
			})
			w := builtinIntegrationWriter(db, schema, product.name)
			report, err := EnsureCompatibilityFunctions(ctx, w)
			require.NoError(t, err, "initialize before code_items exists")
			require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Total)
			require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Created)
			require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Success)
			require.Zero(t, report.Failed)
			for _, item := range report.Items {
				require.NotEmpty(t, item.DDL)
				require.Equal(t, schema, item.Schema)
			}
			t.Run("auto_increase_itemid", func(t *testing.T) { checkAutoIncreaseSQL(t, ctx, db, schema) })
			t.Run("count_rows", func(t *testing.T) { checkCountRowsSQL(t, ctx, db, schema) })
			t.Run("fun_getpy", func(t *testing.T) { checkPinyinSQL(t, ctx, db, schema) })
			t.Run("invoker_permissions", func(t *testing.T) { checkCountRowsInvokerSQL(t, ctx, db, schema) })
			report, err = EnsureCompatibilityFunctions(ctx, w)
			require.NoError(t, err)
			require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Reused)
			require.Zero(t, report.Created)
			t.Run("preserve_incompatible_function", func(t *testing.T) {
				name := qualifiedFunction(schema, "count_rows")
				_, err := db.ExecContext(ctx, "DROP FUNCTION "+name+"(varchar,varchar)")
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, "CREATE FUNCTION "+name+"(table_name varchar, owner varchar) RETURNS numeric LANGUAGE sql AS $$SELECT 999::numeric$$")
				require.NoError(t, err)
				var before, after string
				query := "SELECT pg_get_functiondef($1::regprocedure::oid)"
				require.NoError(t, db.QueryRowContext(ctx, query, name+"(varchar,varchar)").Scan(&before))
				report, err := EnsureCompatibilityFunctions(ctx, w)
				require.Error(t, err)
				require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Total)
				require.Equal(t, len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Reused)
				require.Equal(t, 1, report.Failed)
				require.NoError(t, db.QueryRowContext(ctx, query, name+"(varchar,varchar)").Scan(&after))
				require.Equal(t, before, after)
			})
		})
	}
}

func builtinIntegrationWriter(db *sql.DB, schema, product string) Writer {
	w := &PostgresWriter{db: db, schema: schema, dbType: product}
	switch product {
	case "kingbase":
		return &KingbaseWriter{db: db, schema: schema}
	case "highgo":
		return &HighGoWriter{db: db, schema: schema}
	case "gaussdb":
		return &GaussDBWriter{db: db, schema: schema}
	case "vastbase":
		return &VastbaseWriter{PostgresWriter: w}
	case "gbase":
		return &GBaseWriter{PostgresWriter: w}
	default:
		return w
	}
}

func checkAutoIncreaseSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	t.Helper()
	query := "SELECT " + qualifiedFunction(schema, "auto_increase_itemid") + "()"
	var value string
	require.Error(t, db.QueryRowContext(ctx, query).Scan(&value), "missing table must fail at call time")
	table := qualifiedFunction(schema, "code_items")
	_, err := db.ExecContext(ctx, "CREATE TABLE "+table+"(itemid numeric)")
	require.NoError(t, err)
	for _, tc := range []struct{ name, values, want string }{
		{"empty", "", "1"},
		{"continuous", "(1),(2),(3)", "4"},
		{"gap_and_minimum_above_one", "(5),(6),(8)", "7"},
		{"single_minimum_above_one", "(5)", "6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, "TRUNCATE "+table)
			require.NoError(t, err)
			if tc.values != "" {
				_, err = db.ExecContext(ctx, "INSERT INTO "+table+" VALUES "+tc.values)
				require.NoError(t, err)
			}
			require.NoError(t, db.QueryRowContext(ctx, query).Scan(&value))
			require.Equal(t, tc.want, value)
		})
	}
	_, err = db.ExecContext(ctx, "DROP TABLE "+table)
	require.NoError(t, err)
	table = qualifiedFunction(schema, "CoDe_ItEmS")
	_, err = db.ExecContext(ctx, "CREATE TABLE "+table+`("ItEmId" integer)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO "+table+" VALUES (5),(6),(8)")
	require.NoError(t, err)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO pg_catalog")
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(ctx, query).Scan(&value), "table lookup must use captured target schema")
	require.Equal(t, "7", value)
	require.NoError(t, tx.Rollback())
	_, err = db.ExecContext(ctx, "CREATE TABLE "+qualifiedFunction(schema, "code_items")+"(itemid integer)")
	require.NoError(t, err)
	require.Error(t, db.QueryRowContext(ctx, query).Scan(&value), "case-equivalent tables are ambiguous")
	_, err = db.ExecContext(ctx, "DROP TABLE "+qualifiedFunction(schema, "code_items"))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "ALTER TABLE "+table+" ADD itemid integer")
	require.NoError(t, err)
	require.Error(t, db.QueryRowContext(ctx, query).Scan(&value), "case-equivalent columns are ambiguous")
	_, err = db.ExecContext(ctx, "ALTER TABLE "+table+` DROP COLUMN "ItEmId"`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "ALTER TABLE "+table+" DROP COLUMN itemid")
	require.NoError(t, err)
	require.Error(t, db.QueryRowContext(ctx, query).Scan(&value), "missing itemid column must fail clearly")
}

func checkCountRowsSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	t.Helper()
	tableName := `odd.table"'\\name`
	_, err := db.ExecContext(ctx, "CREATE TABLE "+qualifiedFunction(schema, tableName)+"(id int)")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO "+qualifiedFunction(schema, tableName)+" VALUES (1),(2),(3)")
	require.NoError(t, err)
	function := qualifiedFunction(schema, "count_rows")
	var count int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT "+function+"($1::varchar,$2::varchar)", tableName, schema).Scan(&count))
	require.EqualValues(t, 3, count)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+source.QuotePGIdent(schema)+", pg_catalog")
	require.NoError(t, err)
	for _, query := range []string{"SELECT " + function + "($1::varchar)", "SELECT " + function + "($1::varchar,NULL::varchar)"} {
		require.NoError(t, tx.QueryRowContext(ctx, query, tableName).Scan(&count))
		require.EqualValues(t, 3, count)
	}
	require.NoError(t, tx.Rollback())
	// A table name containing a dot remains one identifier; it cannot become
	// an unquoted schema path or executable SQL.
	require.Error(t, db.QueryRowContext(ctx, "SELECT "+function+"($1::varchar,$2::varchar)", tableName+"; SELECT 1", schema).Scan(&count))
}

func checkPinyinSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	t.Helper()
	query := "SELECT " + qualifiedFunction(schema, "fun_getpy") + "($1::varchar)"
	for _, tc := range []struct {
		input any
		want  string
	}{
		{"中国", "zg"},
		{"张三A1", "zs"},
		{"重庆", "zq"},
		{"繁體", "ft"},
		{"龘㐀㐁𠀀", "dqth"},
		{"\U00032389", "m"},
		{"𠮷〇", ""},
		{nil, ""},
		{"", ""},
		{"ABC123😀", ""},
	} {
		var value sql.NullString
		require.NoError(t, db.QueryRowContext(ctx, query, tc.input).Scan(&value), fmt.Sprint(tc.input))
		require.Equal(t, tc.want != "", value.Valid)
		require.Equal(t, tc.want, value.String)
	}
}

func checkCountRowsInvokerSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	t.Helper()
	role := fmt.Sprintf("dbgold_reader_%d", time.Now().UnixNano())
	_, err := db.ExecContext(ctx, "CREATE ROLE "+source.QuotePGIdent(role))
	if compatibilitySQLState(err) == "42501" {
		t.Skip("test user lacks permission to create a temporary role")
	}
	require.NoError(t, err)
	defer func() {
		_, err := db.ExecContext(context.Background(), "DROP OWNED BY "+source.QuotePGIdent(role))
		require.NoError(t, err)
		_, err = db.ExecContext(context.Background(), "DROP ROLE "+source.QuotePGIdent(role))
		require.NoError(t, err)
	}()
	table := qualifiedFunction(schema, "private_rows")
	_, err = db.ExecContext(ctx, "CREATE TABLE "+table+"(id int)")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "GRANT USAGE ON SCHEMA "+source.QuotePGIdent(schema)+" TO "+source.QuotePGIdent(role))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "GRANT EXECUTE ON FUNCTION "+qualifiedFunction(schema, "count_rows")+"(varchar,varchar) TO "+source.QuotePGIdent(role))
	require.NoError(t, err)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "SET LOCAL ROLE "+source.QuotePGIdent(role))
	require.NoError(t, err)
	var count int64
	err = tx.QueryRowContext(ctx, "SELECT "+qualifiedFunction(schema, "count_rows")+"($1::varchar,$2::varchar)", "private_rows", schema).Scan(&count)
	require.Error(t, err, "caller without table SELECT must not inherit initializer permissions")
	require.Equal(t, "42501", compatibilitySQLState(err))
	require.NoError(t, tx.Rollback())
	_, err = db.ExecContext(ctx, "GRANT SELECT ON TABLE "+table+" TO "+source.QuotePGIdent(role))
	require.NoError(t, err)
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "SET LOCAL ROLE "+source.QuotePGIdent(role))
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT "+qualifiedFunction(schema, "count_rows")+"($1::varchar,$2::varchar)", "private_rows", schema).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, tx.Rollback())
}

func TestCompatibilityBuiltinsConcurrentSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	schema := fmt.Sprintf("dbgold_builtin_race_%d", time.Now().UnixNano())
	_, err := db.ExecContext(ctx, "CREATE SCHEMA "+source.QuotePGIdent(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.ExecContext(context.Background(), "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
		require.NoError(t, err)
	})
	type outcome struct {
		report *CompatibilityFunctionsReport
		err    error
	}
	outcomes := make(chan outcome, 6)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			report, err := EnsureCompatibilityFunctions(ctx, builtinIntegrationWriter(db, schema, "postgres"))
			outcomes <- outcome{report, err}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)
	created := 0
	for outcome := range outcomes {
		require.NoError(t, outcome.err)
		require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), outcome.report.Total)
		require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), outcome.report.Success)
		created += outcome.report.Created
	}
	require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), created, "each routine must only be created once")
}

func TestCompatibilityBuiltinsDefaultSchemaAndIsolationSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	// A single connection makes the session-level default schema deterministic.
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := fmt.Sprintf("dbgold_builtin_default_%d", time.Now().UnixNano())
	_, err := db.ExecContext(ctx, "CREATE SCHEMA "+source.QuotePGIdent(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.ExecContext(context.Background(), "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
		require.NoError(t, err)
	})
	_, err = db.ExecContext(ctx, "SET search_path TO "+source.QuotePGIdent(schema)+", pg_catalog")
	require.NoError(t, err)
	// A pre-existing UUID must not short-circuit the other functions. Nor may
	// initialization call it to decide whether it can be reused.
	_, err = db.ExecContext(ctx, "CREATE FUNCTION "+qualifiedFunction(schema, "get_uuid")+"() RETURNS varchar LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'must not execute existing function'; END;$$")
	require.NoError(t, err)
	count := qualifiedFunction(schema, "count_rows")
	_, err = db.ExecContext(ctx, "CREATE FUNCTION "+count+"(table_name varchar, owner varchar) RETURNS numeric LANGUAGE sql AS $$SELECT 999::numeric$$")
	require.NoError(t, err)
	var before string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT pg_get_functiondef($1::regprocedure::oid)", count+"(varchar,varchar)").Scan(&before))
	report, err := EnsureCompatibilityFunctions(ctx, builtinIntegrationWriter(db, "", "postgres"))
	require.ErrorContains(t, err, "count_rows")
	require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Total)
	require.Equal(t, 1, report.Reused)
	require.Equal(t, len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs)-1, report.Created)
	require.Equal(t, 1, report.Failed)
	for _, item := range report.Items {
		require.Equal(t, schema, item.Schema)
	}
	var after, pinyin, itemid string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT pg_get_functiondef($1::regprocedure::oid)", count+"(varchar,varchar)").Scan(&after))
	require.Equal(t, before, after)
	_, err = db.ExecContext(ctx, "CREATE TABLE "+qualifiedFunction(schema, "code_items")+"(itemid integer)")
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT "+qualifiedFunction(schema, "auto_increase_itemid")+"(), "+qualifiedFunction(schema, "fun_getpy")+"('中国')").Scan(&itemid, &pinyin))
	require.Equal(t, "1", itemid)
	require.Equal(t, "zg", pinyin)
	// Replace the newly installed pinyin function with an application-owned
	// implementation that raises if called. Metadata discovery must retain it.
	_, err = db.ExecContext(ctx, "CREATE OR REPLACE FUNCTION "+qualifiedFunction(schema, "fun_getpy")+"(p_name varchar) RETURNS varchar LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'must not execute existing function'; END;$$")
	require.NoError(t, err)
	report, err = EnsureCompatibilityFunctions(ctx, builtinIntegrationWriter(db, "", "postgres"))
	require.ErrorContains(t, err, "count_rows")
	require.Equal(t, len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Reused)
	require.Zero(t, report.Created)
	require.Equal(t, 1, report.Failed)
}

func TestCompatibilityBuiltinsRoutineKindSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, kind := range []string{"aggregate", "window"} {
		t.Run(kind, func(t *testing.T) {
			schema := fmt.Sprintf("dbgold_builtin_kind_%d", time.Now().UnixNano())
			_, err := db.ExecContext(ctx, "CREATE SCHEMA "+source.QuotePGIdent(schema))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := db.ExecContext(context.Background(), "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
				require.NoError(t, err)
			})
			name := qualifiedFunction(schema, "fun_getpy")
			if kind == "aggregate" {
				transition := qualifiedFunction(schema, "concat_step")
				_, err = db.ExecContext(ctx, "CREATE FUNCTION "+transition+"(state varchar, input varchar) RETURNS varchar LANGUAGE sql AS $$SELECT (coalesce(state,'')||coalesce(input,''))::varchar$$")
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, "CREATE AGGREGATE "+name+"(varchar)(SFUNC="+transition+",STYPE=varchar,INITCOND='')")
			} else {
				_, err = db.ExecContext(ctx, "CREATE FUNCTION "+name+"(p_name varchar) RETURNS varchar LANGUAGE internal WINDOW AS 'window_first_value'")
				if compatibilitySQLState(err) == "42501" {
					t.Skip("test user lacks permission to define an internal window function")
				}
			}
			require.NoError(t, err)
			var before, after int64
			require.NoError(t, db.QueryRowContext(ctx, "SELECT $1::regprocedure::oid", name+"(varchar)").Scan(&before))
			report, err := EnsureCompatibilityFunctions(ctx, builtinIntegrationWriter(db, schema, "postgres"))
			require.Error(t, err, "aggregate/window object cannot satisfy a scalar compatibility function")
			require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Total)
			require.Equal(t, len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Created)
			require.Equal(t, 1, report.Failed)
			require.Equal(t, "failed", report.Items[3].Status)
			require.NoError(t, db.QueryRowContext(ctx, "SELECT $1::regprocedure::oid", name+"(varchar)").Scan(&after))
			require.Equal(t, before, after, "existing object must be retained")
		})
	}
}

func TestCompatibilityCountRowsOverloadSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, exactExists := range []bool{false, true} {
		t.Run(fmt.Sprintf("two_argument_function_exists_%t", exactExists), func(t *testing.T) {
			schema := fmt.Sprintf("dbgold_builtin_overload_%d", time.Now().UnixNano())
			_, err := db.ExecContext(ctx, "CREATE SCHEMA "+source.QuotePGIdent(schema))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := db.ExecContext(context.Background(), "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
				require.NoError(t, err)
			})
			name := qualifiedFunction(schema, "count_rows")
			_, err = db.ExecContext(ctx, "CREATE FUNCTION "+name+"(table_name varchar) RETURNS numeric LANGUAGE sql AS $$SELECT 999::numeric$$")
			require.NoError(t, err)
			if exactExists {
				_, err = db.ExecContext(ctx, "CREATE FUNCTION "+name+"(table_name varchar,owner varchar DEFAULT NULL) RETURNS numeric LANGUAGE sql AS $$SELECT 888::numeric$$")
				require.NoError(t, err)
			}
			report, err := EnsureCompatibilityFunctions(ctx, builtinIntegrationWriter(db, schema, "postgres"))
			require.Error(t, err, "one-argument call would be ambiguous despite a matching two-argument signature")
			require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Total)
			require.Equal(t, len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Created)
			require.Equal(t, 1, report.Failed)
			require.Equal(t, "failed", report.Items[2].Status)
			var count int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname='count_rows'", schema).Scan(&count))
			if exactExists {
				require.Equal(t, 2, count)
			} else {
				require.Equal(t, 1, count, "failed initialization must not create a new ambiguous overload")
			}
		})
	}
}
