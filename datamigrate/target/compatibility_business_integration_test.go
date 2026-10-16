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

// Like the existing builtin suite, this runs only against explicitly supplied
// disposable databases. Every subtest owns its schema, including dependencies.
func TestCompatibilityBusinessSQLIntegration(t *testing.T) {
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
			for _, tc := range []struct {
				name  string
				check func(*testing.T, context.Context, *sql.DB, string)
			}{
				{"guid_strings", checkBusinessGUIDSQL},
				{"publication_count", checkBusinessPublicationCountSQL},
				{"score", checkBusinessScoreSQL},
				{"category_score", checkBusinessCategoryScoreSQL},
				{"child_ou_list", checkBusinessChildOUListSQL},
			} {
				t.Run(tc.name, func(t *testing.T) {
					schema := newBusinessIntegrationSchema(t, ctx, db)
					report, err := EnsureCompatibilityFunctions(ctx, builtinIntegrationWriter(db, schema, product.name))
					require.NoError(t, err, "initialization must not require business tables")
					require.Equal(t, 10, report.Total)
					require.Equal(t, 10, report.Created)
					require.Zero(t, report.Failed)
					tc.check(t, ctx, db, schema)
				})
			}
		})
	}
}

func newBusinessIntegrationSchema(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	schema := fmt.Sprintf("dbgold business_\"'\\_%d", time.Now().UnixNano())
	businessExec(t, ctx, db, "CREATE SCHEMA "+source.QuotePGIdent(schema))
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := db.ExecContext(cleanupCtx, "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
		require.NoError(t, err)
	})
	return schema
}

func businessExec(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := db.ExecContext(ctx, query, args...)
	require.NoError(t, err)
}

func checkBusinessGUIDSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	const guid = "abcdefghijklmnopqrstuvwx123456789012"
	for _, tc := range []struct {
		name, function string
		newGUID, input any
		want           string
		null           bool
	}{
		{"null_input", "convertguid", guid, nil, "", false},
		{"both_null", "convertguid", nil, nil, "", false},
		{"null_guid", "convertguid", nil, "abc", "", true},
		{"null_guid_empty_input", "convertguid", nil, "", "", true},
		{"empty", "convertguid", guid, "", "", false},
		{"spaces", "convertguid", guid, "   ", "", false},
		{"short_old_guid", "convertguid", guid, "abc", "123", false},
		{"twelve_characters", "convertguid", guid, "abcdefghijkl", "123456789012", false},
		{"long_old_guid", "convertguid", guid, "old-abcdefghijkl", "old-123456789012", false},
		{"trim_old_length", "convertguid", guid, "abc   ", "123", false},
		{"trim_new_length", "convertguid", guid + "   ", "abc", "123", false},
		{"tab_not_trimmed", "convertguid", guid, "a\t", "12", false},
		{"negative_start", "convertguid", "abcdefghij", "abc", "j", false},
		{"negative_start_in_middle", "convertguid", "abcdef", "abc", "bcd", false},
		{"negative_start_before_string", "convertguid", "abc", "abc", "", false},
		{"zero_start", "convertguid", "abcdefghijk", "abc", "", false},
		{"negative_start_uses_untrimmed_string", "convertguid", "abcdefghij  ", "abc", " ", false},
		{"multibyte_twelve_bytes", "convertguid", guid, "中文测试", "1234", false},
		{"multibyte_negative_prefix_length", "convertguid", guid, "中文测试字", "123456789012", false},
		{"list_null", "convertguidlist", guid, nil, "", true},
		{"list_empty", "convertguidlist", guid, "", "", false},
		{"list_unterminated_only", "convertguidlist", guid, "abc", "abc", false},
		{"list_unterminated_tail", "convertguidlist", guid, "abc;def", "123;def", false},
		{"list_terminated_tail", "convertguidlist", guid, "abc;def;", "123;123;", false},
		{"list_empty_items", "convertguidlist", guid, ";;abc;", ";;123;", false},
		{"list_null_guid_without_separator", "convertguidlist", nil, "abc", "abc", false},
		{"list_null_guid_with_separator", "convertguidlist", nil, "abc;", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value sql.NullString
			query := "SELECT " + qualifiedFunction(schema, tc.function) + "($1::varchar,$2::varchar)"
			require.NoError(t, db.QueryRowContext(ctx, query, tc.newGUID, tc.input).Scan(&value))
			require.Equal(t, !tc.null, value.Valid)
			require.Equal(t, tc.want, value.String)
		})
	}
	// convertguidlist must resolve convertguid in its own schema, regardless of
	// the caller's path. This also verifies execution after initial compilation.
	withBusinessCatalogPath(t, ctx, db, func(tx *sql.Tx) {
		var value string
		require.NoError(t, tx.QueryRowContext(ctx, "SELECT "+qualifiedFunction(schema, "convertguidlist")+"($1::varchar,'abc;'::varchar)", guid).Scan(&value))
		require.Equal(t, "123;", value)
	})
}

func checkBusinessPublicationCountSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	function := qualifiedFunction(schema, "getcategoryinfopubcount")
	query := "SELECT " + function + "($1::varchar,$2::integer,$3::timestamp,$4::timestamp)"
	var value int
	err := db.QueryRowContext(ctx, query, "A", 1, "2024-01-02", "2024-01-03").Scan(&value)
	require.Error(t, err, "missing View_InfoMain must fail only when called")
	require.Contains(t, strings.ToLower(err.Error()), "view_infomain")
	rows := qualifiedFunction(schema, "publication_rows")
	view := qualifiedFunction(schema, "ViEw_InFoMaIn")
	businessExec(t, ctx, db, "CREATE TABLE "+rows+`("InFoId" varchar,"CaTeGoRyNuM" varchar,"InFoStAtUsCoDe" integer,"PuBiNwEbDaTe" timestamp)`)
	businessExec(t, ctx, db, "INSERT INTO "+rows+` VALUES
 ('one','A',1,'2024-01-02 00:00:00'),
 ('two','A',1,'2024-01-02 12:00:00'),
 ('three','A',1,'2024-01-03 00:00:00'),
 ('wrong-status','A',2,'2024-01-02 12:00:00'),
 ('wrong-category','AB',1,'2024-01-02 12:00:00'),
 (NULL,'A',1,'2024-01-02 12:00:00'),
 ('before','A',1,'2024-01-01 23:59:59'),
 ('after','A',1,'2024-01-03 00:00:01')`)
	businessExec(t, ctx, db, "CREATE VIEW "+view+" AS SELECT * FROM "+rows)
	for _, tc := range []struct {
		name             string
		category, status any
		start, end       any
		want             int
	}{
		{"filters_and_inclusive_bounds", "A", 1, "2024-01-02", "2024-01-03", 3},
		{"different_status", "A", 2, "2024-01-02", "2024-01-03", 1},
		{"full_category_not_prefix", "AB", 1, "2024-01-02", "2024-01-03", 1},
		{"null_category", nil, 1, "2024-01-02", "2024-01-03", 0},
		{"null_status", "A", nil, "2024-01-02", "2024-01-03", 0},
		{"null_date", "A", 1, nil, "2024-01-03", 0},
		{"reverse_dates", "A", 1, "2024-01-03", "2024-01-02", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, db.QueryRowContext(ctx, query, tc.category, tc.status, tc.start, tc.end).Scan(&value))
			require.Equal(t, tc.want, value)
		})
	}
	withBusinessCatalogPath(t, ctx, db, func(tx *sql.Tx) {
		require.NoError(t, tx.QueryRowContext(ctx, query, "A", 1, "2024-01-02", "2024-01-03").Scan(&value))
		require.Equal(t, 3, value)
	})
	businessExec(t, ctx, db, "CREATE TABLE "+qualifiedFunction(schema, "view_infomain")+"(infoid varchar)")
	require.Error(t, db.QueryRowContext(ctx, query, "A", 1, "2024-01-02", "2024-01-03").Scan(&value), "case-equivalent relation names must not be selected arbitrarily")
	businessExec(t, ctx, db, "DROP TABLE "+qualifiedFunction(schema, "view_infomain"))
	businessExec(t, ctx, db, "CREATE OR REPLACE VIEW "+view+` AS SELECT *,"InFoId" AS infoid FROM `+rows)
	require.Error(t, db.QueryRowContext(ctx, query, "A", 1, "2024-01-02", "2024-01-03").Scan(&value), "case-equivalent columns must fail clearly")
	businessExec(t, ctx, db, "DROP VIEW "+view)
	businessExec(t, ctx, db, "CREATE VIEW "+view+` AS SELECT "InFoId","CaTeGoRyNuM","PuBiNwEbDaTe" FROM `+rows)
	require.Error(t, db.QueryRowContext(ctx, query, "A", 1, "2024-01-02", "2024-01-03").Scan(&value), "missing status column must fail clearly")
}

func checkBusinessScoreSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	query := "SELECT " + qualifiedFunction(schema, "getscore") + "($1::varchar)"
	var value int
	require.Error(t, db.QueryRowContext(ctx, query, "A").Scan(&value), "missing View_InfoMain")
	view := qualifiedFunction(schema, "ViEw_InFoMaIn")
	businessExec(t, ctx, db, "CREATE TABLE "+view+`("InFoId" varchar,"ScOrE" integer)`)
	businessExec(t, ctx, db, "INSERT INTO "+view+" VALUES ('A',2),('A',7),('A',NULL),('B',99),('null-score',NULL),(NULL,88)")
	for _, tc := range []struct {
		input any
		want  int
	}{{"A", 7}, {"B", 99}, {"null-score", 0}, {"missing", 0}, {nil, 0}} {
		require.NoError(t, db.QueryRowContext(ctx, query, tc.input).Scan(&value), fmt.Sprint(tc.input))
		require.Equal(t, tc.want, value, fmt.Sprint(tc.input))
	}
	withBusinessCatalogPath(t, ctx, db, func(tx *sql.Tx) {
		require.NoError(t, tx.QueryRowContext(ctx, query, "A").Scan(&value))
		require.Equal(t, 7, value)
	})
	businessExec(t, ctx, db, "ALTER TABLE "+view+" ADD score integer")
	require.Error(t, db.QueryRowContext(ctx, query, "A").Scan(&value), "ambiguous score column")
	businessExec(t, ctx, db, "ALTER TABLE "+view+" DROP COLUMN score")
	businessExec(t, ctx, db, "ALTER TABLE "+view+` DROP COLUMN "ScOrE"`)
	require.Error(t, db.QueryRowContext(ctx, query, "A").Scan(&value), "missing score column")
}

func checkBusinessCategoryScoreSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	query := "SELECT " + qualifiedFunction(schema, "getcategoryscore") + "($1::varchar,$2::date,$3::date)"
	var value int
	require.Error(t, db.QueryRowContext(ctx, query, "alice", "2024-01-02", "2024-01-03").Scan(&value), "missing information table")
	table := qualifiedFunction(schema, "WeBdB_InFoRmAtIoN")
	view := qualifiedFunction(schema, "ViEw_InFoMaIn")
	businessExec(t, ctx, db, "CREATE TABLE "+table+`("InFoId" varchar,"UsErGuId" varchar,"PuBiNwEbDaTe" timestamp)`)
	businessExec(t, ctx, db, "INSERT INTO "+table+` VALUES
 ('A1','alice','2024-01-02 00:00:00'),
 ('A2','alice','2024-01-02 12:00:00'),
 ('B1','bob','2024-01-02 12:00:00'),
 ('A2','alice','2024-01-03 00:00:00'),
 ('A1','alice','2024-01-03 12:00:00'),
 ('missing','alice','2024-01-02 12:00:00'),
 (NULL,'alice','2024-01-02 12:00:00')`)
	require.Error(t, db.QueryRowContext(ctx, query, "alice", "2024-01-02", "2024-01-03").Scan(&value), "GetScore dependency must not silently become zero when its relation is missing")
	businessExec(t, ctx, db, "CREATE TABLE "+view+`("InFoId" varchar,"ScOrE" integer)`)
	businessExec(t, ctx, db, "INSERT INTO "+view+" VALUES ('A1',2),('A1',7),('A2',4),('B1',90)")
	for _, tc := range []struct {
		name       string
		user       any
		start, end any
		want       int
	}{
		{"user_filter_and_midnight_upper_bound", "alice", "2024-01-02", "2024-01-03", 15},
		{"other_user", "bob", "2024-01-02", "2024-01-03", 90},
		{"same_day_means_midnight_only", "alice", "2024-01-02", "2024-01-02", 7},
		{"no_rows", "nobody", "2024-01-02", "2024-01-03", 0},
		{"null_user", nil, "2024-01-02", "2024-01-03", 0},
		{"null_date", "alice", nil, "2024-01-03", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, db.QueryRowContext(ctx, query, tc.user, tc.start, tc.end).Scan(&value))
			require.Equal(t, tc.want, value)
		})
	}
	withBusinessCatalogPath(t, ctx, db, func(tx *sql.Tx) {
		require.NoError(t, tx.QueryRowContext(ctx, query, "alice", "2024-01-02", "2024-01-03").Scan(&value))
		require.Equal(t, 15, value, "GetScore must be schema-qualified and receive varchar")
	})
	businessExec(t, ctx, db, "CREATE FUNCTION "+qualifiedFunction(schema, "getscore")+"(text) RETURNS integer LANGUAGE sql AS $$SELECT 999$$")
	businessExec(t, ctx, db, "ALTER TABLE "+table+` ALTER COLUMN "InFoId" TYPE text`)
	require.NoError(t, db.QueryRowContext(ctx, query, "alice", "2024-01-02", "2024-01-03").Scan(&value))
	require.Equal(t, 15, value, "a text InfoID column must still call the canonical varchar overload")
	businessExec(t, ctx, db, "DROP FUNCTION "+qualifiedFunction(schema, "getscore")+"(text)")
	businessExec(t, ctx, db, "DROP FUNCTION "+qualifiedFunction(schema, "getscore")+"(varchar)")
	require.Error(t, db.QueryRowContext(ctx, query, "alice", "2024-01-02", "2024-01-03").Scan(&value), "missing GetScore function must not be hidden")
}

func checkBusinessChildOUListSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	query := "SELECT " + qualifiedFunction(schema, "getchildoulist") + "($1::varchar)"
	var value string
	require.Error(t, db.QueryRowContext(ctx, query, "r").Scan(&value), "missing frame_ou")
	table := qualifiedFunction(schema, "FrAmE_Ou")
	businessExec(t, ctx, db, "CREATE TABLE "+table+`("OuGuId" varchar,"PaReNtOuGuId" varchar)`)
	for _, input := range []any{nil, ""} {
		require.NoError(t, db.QueryRowContext(ctx, query, input).Scan(&value))
		require.Empty(t, value)
	}
	require.NoError(t, db.QueryRowContext(ctx, query, "ghost").Scan(&value))
	require.Equal(t, "ghost", value, "missing root is still included")
	require.NoError(t, db.QueryRowContext(ctx, query, "b,a").Scan(&value))
	require.Equal(t, "a,b", value, "roots are sorted as the first breadth level")
	businessExec(t, ctx, db, "INSERT INTO "+table+" VALUES ('z','r'),('a','r'),('c','a'),('b','z')")
	require.NoError(t, db.QueryRowContext(ctx, query, "r").Scan(&value))
	require.Equal(t, "r,a,z,b,c", value, "breadth first, then identifier order within a level")
	withBusinessCatalogPath(t, ctx, db, func(tx *sql.Tx) {
		require.NoError(t, tx.QueryRowContext(ctx, query, "r").Scan(&value))
		require.Equal(t, "r,a,z,b,c", value)
	})
	businessExec(t, ctx, db, "TRUNCATE "+table)
	businessExec(t, ctx, db, "INSERT INTO "+table+" VALUES ('a','r'),('b','r'),('c','a'),('c','b')")
	require.NoError(t, db.QueryRowContext(ctx, query, "r").Scan(&value))
	require.Equal(t, "r,a,b,c", value, "a shared descendant on independent branches is not a cycle")
	businessExec(t, ctx, db, "TRUNCATE "+table)
	businessExec(t, ctx, db, "INSERT INTO "+table+" VALUES ('a','r'),('b','a')")
	require.NoError(t, db.QueryRowContext(ctx, query, "r,a").Scan(&value))
	require.Equal(t, "a,r,b", value, "overlapping roots are not a path cycle; deduplicate using minimum depth")
	businessExec(t, ctx, db, "INSERT INTO "+table+" VALUES ('a','b')")
	checkCycle := func(root string) {
		cycleCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		err := db.QueryRowContext(cycleCtx, query, root).Scan(&value)
		require.Error(t, err, "cycles must be rejected, including paths reached through overlapping roots")
		require.NotErrorIs(t, err, context.DeadlineExceeded)
		require.NotEqual(t, "57014", compatibilitySQLState(err), "cycle detection must fail explicitly, not time out")
	}
	checkCycle("r,a")
	businessExec(t, ctx, db, "TRUNCATE "+table)
	businessExec(t, ctx, db, "INSERT INTO "+table+" VALUES ('r','r')")
	checkCycle("r")
	businessExec(t, ctx, db, "TRUNCATE "+table)
	// One level exceeds the source's group_concat_max_len=102400 limit. The
	// target contract has no silent truncation, including the last descendant.
	const children = 2600
	values := make([]string, children)
	args := make([]any, children)
	for i := range values {
		values[i] = fmt.Sprintf("($%d,'r')", i+1)
		args[i] = fmt.Sprintf("child-%034d", i)
	}
	businessExec(t, ctx, db, "INSERT INTO "+table+" VALUES "+strings.Join(values, ","), args...)
	require.NoError(t, db.QueryRowContext(ctx, query, "r").Scan(&value))
	require.Greater(t, len(value), 102400)
	require.Len(t, strings.Split(value, ","), children+1)
	require.True(t, strings.HasSuffix(value, args[children-1].(string)))
}

func withBusinessCatalogPath(t *testing.T, ctx context.Context, db *sql.DB, check func(*sql.Tx)) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO pg_catalog")
	require.NoError(t, err)
	check(tx)
	require.NoError(t, tx.Rollback())
}
