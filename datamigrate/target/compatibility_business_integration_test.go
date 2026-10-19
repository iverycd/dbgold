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
				{"inter_array", checkBusinessInterArraySQL},
				{"seconds_dhm", checkBusinessSecondsDHMSQL},
				{"all_user_count", checkBusinessAllUserCountSQL},
				{"stuff", checkBusinessStuffSQL},
			} {
				t.Run(tc.name, func(t *testing.T) {
					schema := newBusinessIntegrationSchema(t, ctx, db)
					report, err := EnsureCompatibilityFunctions(ctx, builtinIntegrationWriter(db, schema, product.name))
					require.NoError(t, err, "initialization must not require business tables")
					require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Total)
					require.Equal(t, 1+len(builtinCompatibilitySpecs)+len(compatibilityProcedureSpecs), report.Created)
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

func checkBusinessInterArraySQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	query := "SELECT " + qualifiedFunction(schema, "inter_arry") + "($1::varchar,$2::varchar)"
	for _, tc := range []struct {
		name       string
		setA, setB any
		want       int
	}{
		{"single_ascii_match_before_length_check", "a", "a", 1},
		{"ascii_list_preserves_early_return", "a,b", "a,b", 0},
		{"chinese_list", "甲,乙", "乙,甲", 1},
		{"missing_item", "甲,乙", "甲,丙", 0},
		{"whole_item_match", "甲", "甲乙", 0},
		{"duplicates_are_counted", "甲,甲", "甲", 1},
		{"two_byte_rounding", "é,甲", "é,甲", 1},
		{"four_byte_preserves_original_indexing", "😀,甲", "😀,甲", 0},
		{"single_four_byte_character", "😀", "😀", 1},
		{"mixed_ascii_before_chinese", "a,甲", "a,甲", 0},
		{"mixed_ascii_after_chinese", "甲,a", "甲,a", 0},
		{"case_sensitive", "A", "a", 0},
		{"accent_sensitive", "é", "e", 0},
		{"whitespace_is_significant", "甲", " 甲", 0},
		{"trailing_whitespace_is_significant", "甲", "甲 ", 0},
		{"empty_source", "", "甲", 0},
		{"empty_target", "甲", "", 0},
		{"both_empty", "", "", 0},
		{"leading_empty_source_item", ",甲", "甲", 0},
		{"middle_empty_source_item", "甲,,乙", "甲,乙", 0},
		{"trailing_empty_source_item", "甲,", "甲", 0},
		{"empty_target_items", "甲,乙", ",甲,,乙,", 1},
		{"null_source", nil, "甲", 0},
		{"null_target", "甲", nil, 0},
		{"both_null", nil, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value int
			require.NoError(t, db.QueryRowContext(ctx, query, tc.setA, tc.setB).Scan(&value))
			require.Equal(t, tc.want, value)
		})
	}
	withBusinessCatalogPath(t, ctx, db, func(tx *sql.Tx) {
		var value int
		require.NoError(t, tx.QueryRowContext(ctx, query, "é,甲", "é,甲").Scan(&value))
		require.Equal(t, 1, value)
	})
}

func checkBusinessSecondsDHMSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	query := "SELECT " + qualifiedFunction(schema, "sec_to_dhm") + "($1::bigint)"
	for _, tc := range []struct {
		name string
		sec  any
		want string
		null bool
	}{
		{"zero", int64(0), "0d 0h 0m ", false},
		{"seconds_discarded", int64(59), "0d 0h 0m ", false},
		{"minute_boundary", int64(60), "0d 0h 1m ", false},
		{"before_hour", int64(3599), "0d 0h 59m ", false},
		{"hour_boundary", int64(3600), "0d 1h 0m ", false},
		{"before_day", int64(86399), "0d 23h 59m ", false},
		{"day_boundary", int64(86400), "1d 0h 0m ", false},
		{"days_hours_minutes", int64(90061), "1d 1h 1m ", false},
		{"negative_seconds_truncate_toward_zero", int64(-59), "0d 0h 0m ", false},
		{"negative_minute", int64(-60), "0d 0h -1m ", false},
		{"negative_days_hours_minutes", int64(-90061), "-1d -1h -1m ", false},
		{"large_value_before_day_boundary", int64(9223372036854719999), "106751991167299d 23h 59m ", false},
		{"large_negative_value_before_day_boundary", int64(-9223372036854719999), "-106751991167299d -23h -59m ", false},
		{"maximum_bigint", int64(9223372036854775807), "106751991167300d 15h 30m ", false},
		{"minimum_bigint", int64(-9223372036854775808), "-106751991167300d -15h -30m ", false},
		{"null", nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value sql.NullString
			require.NoError(t, db.QueryRowContext(ctx, query, tc.sec).Scan(&value))
			require.Equal(t, !tc.null, value.Valid)
			require.Equal(t, tc.want, value.String, "the final space is part of the source result")
		})
	}
	withBusinessCatalogPath(t, ctx, db, func(tx *sql.Tx) {
		var value string
		require.NoError(t, tx.QueryRowContext(ctx, query, int64(90061)).Scan(&value))
		require.Equal(t, "1d 1h 1m ", value)
	})
}

func checkBusinessStuffSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	query := "SELECT " + qualifiedFunction(schema, "stuff") + "($1::varchar,$2::integer,$3::integer,$4::varchar)"
	for _, tc := range []struct {
		name               string
		old, start, length any
		replacement        any
		want               string
		null               bool
	}{
		{"global_replacement", "abcabc", 2, 2, "X", "aXaX", false},
		{"negative_start", "abcabc", -3, 2, "X", "XcXc", false},
		{"negative_start_at_first_character", "abcabc", -6, 2, "X", "XcXc", false},
		{"negative_start_before_string", "abc", -4, 2, "X", "abc", false},
		{"minimum_start", "abc", -2147483648, 2, "X", "abc", false},
		{"zero_start", "abc", 0, 2, "X", "abc", false},
		{"start_after_string", "abc", 4, 2, "X", "abc", false},
		{"maximum_start", "abc", 2147483647, 2, "X", "abc", false},
		{"zero_length", "abc", 2, 0, "X", "abc", false},
		{"negative_length", "abc", 2, -1, "X", "abc", false},
		{"minimum_length", "abc", 2, -2147483648, "X", "abc", false},
		{"length_beyond_string", "abc", 2, 2147483647, "X", "aX", false},
		{"empty_replacement", "abcabc", 2, 2, "", "aa", false},
		{"replacement_contains_match", "abab", 1, 2, "abab", "abababab", false},
		{"case_sensitive", "AaA", 1, 1, "x", "xax", false},
		{"multibyte_positions", "甲乙甲乙", 2, 1, "x", "甲x甲x", false},
		{"four_byte_positions", "😀甲😀甲", -1, 1, "x", "😀x😀x", false},
		{"empty_source", "", 1, 1, "X", "", false},
		{"null_source", nil, 1, 1, "X", "", true},
		{"null_start", "abc", nil, 1, "X", "", true},
		{"null_length", "abc", 1, nil, "X", "", true},
		{"null_replacement", "abc", 1, 1, nil, "", true},
		{"null_replacement_with_empty_match", "abc", 0, 0, nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value sql.NullString
			require.NoError(t, db.QueryRowContext(ctx, query, tc.old, tc.start, tc.length, tc.replacement).Scan(&value))
			require.Equal(t, !tc.null, value.Valid)
			require.Equal(t, tc.want, value.String)
		})
	}
	withBusinessCatalogPath(t, ctx, db, func(tx *sql.Tx) {
		var value string
		require.NoError(t, tx.QueryRowContext(ctx, query, "abcabc", 2, 2, "X").Scan(&value))
		require.Equal(t, "aXaX", value)
	})
}

func checkBusinessAllUserCountSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	function := qualifiedFunction(schema, "selectallusercount")
	query := "SELECT " + function + "()"
	var value string
	err := db.QueryRowContext(ctx, query).Scan(&value)
	require.Error(t, err, "missing frame_user must fail only when called")
	require.Contains(t, strings.ToLower(err.Error()), "frame_user")
	table := qualifiedFunction(schema, "FrAmE_UsEr")
	businessExec(t, ctx, db, "CREATE TABLE "+table+"(id integer)")
	require.NoError(t, db.QueryRowContext(ctx, query).Scan(&value))
	require.Equal(t, "mysql count :0", value)
	businessExec(t, ctx, db, "INSERT INTO "+table+" VALUES (1),(2),(NULL)")
	require.NoError(t, db.QueryRowContext(ctx, query).Scan(&value))
	require.Equal(t, "mysql count :3", value, "COUNT(*) includes rows whose columns are NULL")
	withBusinessCatalogPath(t, ctx, db, func(tx *sql.Tx) {
		require.NoError(t, tx.QueryRowContext(ctx, query).Scan(&value))
		require.Equal(t, "mysql count :3", value)
	})
	decoySchema := newBusinessIntegrationSchema(t, ctx, db)
	businessExec(t, ctx, db, "CREATE TABLE "+qualifiedFunction(decoySchema, "frame_user")+"(id integer)")
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+source.QuotePGIdent(decoySchema)+",pg_catalog")
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(ctx, query).Scan(&value))
	require.Equal(t, "mysql count :3", value, "caller search_path must not select the decoy frame_user")
	require.NoError(t, tx.Rollback())
	ambiguousTable := qualifiedFunction(schema, "frame_user")
	businessExec(t, ctx, db, "CREATE TABLE "+ambiguousTable+"(id integer)")
	err = db.QueryRowContext(ctx, query).Scan(&value)
	require.Error(t, err, "case-equivalent frame_user names must not be selected arbitrarily")
	require.Contains(t, strings.ToLower(err.Error()), "ambiguous")
	businessExec(t, ctx, db, "DROP TABLE "+ambiguousTable)
	require.NoError(t, db.QueryRowContext(ctx, query).Scan(&value))
	require.Equal(t, "mysql count :3", value)
	t.Run("caller_permissions", func(t *testing.T) {
		role := fmt.Sprintf("dbgold_user_counter_%d", time.Now().UnixNano())
		quotedRole := source.QuotePGIdent(role)
		_, err := db.ExecContext(ctx, "CREATE ROLE "+quotedRole)
		if compatibilitySQLState(err) == "42501" {
			t.Skip("test user lacks permission to create a temporary role")
		}
		require.NoError(t, err)
		defer func() {
			_, err := db.ExecContext(context.Background(), "DROP OWNED BY "+quotedRole)
			require.NoError(t, err)
			_, err = db.ExecContext(context.Background(), "DROP ROLE "+quotedRole)
			require.NoError(t, err)
		}()
		businessExec(t, ctx, db, "REVOKE ALL ON TABLE "+table+" FROM PUBLIC")
		businessExec(t, ctx, db, "GRANT USAGE ON SCHEMA "+source.QuotePGIdent(schema)+" TO "+quotedRole)
		businessExec(t, ctx, db, "GRANT EXECUTE ON FUNCTION "+function+"() TO "+quotedRole)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer tx.Rollback()
		_, err = tx.ExecContext(ctx, "SET LOCAL ROLE "+quotedRole)
		require.NoError(t, err)
		err = tx.QueryRowContext(ctx, query).Scan(&value)
		require.Error(t, err, "caller without table SELECT must not inherit initializer permissions")
		require.Equal(t, "42501", compatibilitySQLState(err))
		require.NoError(t, tx.Rollback())
		businessExec(t, ctx, db, "GRANT SELECT ON TABLE "+table+" TO "+quotedRole)
		tx, err = db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer tx.Rollback()
		_, err = tx.ExecContext(ctx, "SET LOCAL ROLE "+quotedRole)
		require.NoError(t, err)
		require.NoError(t, tx.QueryRowContext(ctx, query).Scan(&value))
		require.Equal(t, "mysql count :3", value)
		require.NoError(t, tx.Rollback())
	})
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
