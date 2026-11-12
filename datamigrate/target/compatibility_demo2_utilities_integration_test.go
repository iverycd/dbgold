package target

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDemo2DateDiffAndCategorySQLIntegration(t *testing.T) {
	testPGCompatibilityProducts(t, func(t *testing.T, ctx context.Context, db *sql.DB, product string) {
		schema := newBusinessIntegrationSchema(t, ctx, db)
		w := builtinIntegrationWriter(db, schema, product)
		names := CompatibilityRoutineNames{Procedures: []string{"ABS_DATE_DIFF", "Table_Category_Add", "abs_date_diff"}}
		report, err := EnsureCompatibilityFunctions(ctx, w, names)
		require.NoError(t, err)
		require.Equal(t, 2, report.Created)

		t.Run("whole_months", func(t *testing.T) {
			call := "CALL " + qualifiedFunction(schema, "abs_date_diff") + "($1::timestamp,$2::timestamp"
			for _, tc := range []struct {
				name        string
				start, end  any
				years       int64
				months      int64
				placeholder bool
			}{
				{name: "year_and_months", start: "2023-01-05 12:00:00", end: "2024-03-05 12:00:00", years: 1, months: 2},
				{name: "reverse", start: "2024-03-05 12:00:00", end: "2023-01-05 12:00:00", years: 1, months: 2, placeholder: true},
				{name: "short_month", start: "2024-01-31 00:00:00", end: "2024-02-29 00:00:00"},
				{name: "time_before_boundary", start: "2024-01-05 12:00:00", end: "2024-02-05 11:59:59"},
				{name: "time_at_boundary", start: "2024-01-05 12:00:00", end: "2024-02-05 12:00:00", months: 1},
				{name: "equal", start: "2024-01-05 12:00:00", end: "2024-01-05 12:00:00"},
				{name: "null", end: "2024-02-05", placeholder: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					statement := call
					if tc.placeholder {
						statement += ",NULL::integer,NULL::integer,NULL::integer"
					}
					var year, month, day sql.NullInt64
					require.NoError(t, db.QueryRowContext(ctx, statement+")", tc.start, tc.end).Scan(&year, &month, &day))
					wantValid := tc.start != nil && tc.end != nil
					require.Equal(t, sql.NullInt64{Int64: tc.years, Valid: wantValid}, year)
					require.Equal(t, sql.NullInt64{Int64: tc.months, Valid: wantValid}, month)
					require.Equal(t, sql.NullInt64{Valid: wantValid}, day)
				})
			}
		})

		t.Run("category_numbers", func(t *testing.T) {
			table := qualifiedFunction(schema, "TaBlE_CaTeGoRy")
			businessExec(t, ctx, db, "CREATE TABLE "+table+`("CategoryNum" varchar PRIMARY KEY,"CategoryName" varchar,"HomePageUrl" varchar)`)
			businessExec(t, ctx, db, "INSERT INTO "+table+` VALUES
 ('AA','legacy',NULL),('0105','child',NULL),('01x9','legacy child',NULL),('P%08','literal prefix',NULL),('P_98','other prefix',NULL)`)
			call := "CALL " + qualifiedFunction(schema, "table_category_add") + "($1::varchar,$2::varchar,$3::varchar)"
			for _, tc := range []struct {
				name, code string
				parent     any
			}{
				{"root 'one'", "01", nil},
				{"root two", "02", ""},
				{"child of 01", "0106", "01"},
				{"literal prefix child", "P%09", "P%"},
			} {
				businessExec(t, ctx, db, call, tc.name, tc.parent, "/page")
				var code, url string
				require.NoError(t, db.QueryRowContext(ctx, "SELECT \"CategoryNum\",\"HomePageUrl\" FROM "+table+` WHERE "CategoryName"=$1`, tc.name).Scan(&code, &url))
				require.Equal(t, tc.code, code)
				require.Equal(t, "/page", url)
			}
			businessExec(t, ctx, db, "INSERT INTO "+table+" VALUES ('0199','last child',NULL)")
			_, err := db.ExecContext(ctx, call, "overflow", "01", "/ignored")
			require.ErrorContains(t, err, "two-digit category numbers exhausted")
			require.Zero(t, demoCount(t, ctx, db, "SELECT count(*) FROM "+table+` WHERE "CategoryName"='overflow'`))

			if product == "postgres" {
				checkDemo2CategoryConcurrency(t, ctx, db, call, table)
			}
		})

		report, err = EnsureCompatibilityFunctions(ctx, w, names)
		require.NoError(t, err)
		require.Equal(t, 2, report.Reused)
	})
}

func checkDemo2CategoryConcurrency(t *testing.T, ctx context.Context, db *sql.DB, call, table string) {
	t.Helper()
	first, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer first.Rollback()
	_, err = first.ExecContext(ctx, call, "concurrent first", "C", nil)
	require.NoError(t, err)

	second, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() {
		_ = first.Rollback()
		_ = second.Close()
	}()
	var pid int
	require.NoError(t, second.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid))
	done := make(chan error, 1)
	go func() {
		_, err := second.ExecContext(ctx, call, "concurrent second", "C", nil)
		done <- err
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid=$1 AND mode='ShareRowExclusiveLock' AND NOT granted)", pid).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, first.Commit())
	select {
	case err = <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var codes string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT string_agg(\"CategoryNum\",',' ORDER BY \"CategoryNum\") FROM "+table+` WHERE "CategoryName" LIKE 'concurrent %'`).Scan(&codes))
	require.Equal(t, "C01,C02", codes)
}

func TestDemo2RoutineInventorySQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema := newBusinessIntegrationSchema(t, ctx, db)
	w := builtinIntegrationWriter(db, schema, "postgres")
	names := CompatibilityRoutineNames{
		Functions: []string{"ConvertGuid", "CONVERTGUIDLIST", "GETCATEGORYINFOPUBCOUNT", "GetCategoryScore", "getChildOuList",
			"GetExtraScore", "Getpainfocount", "GetScore", "selectAllUserCount"},
		Procedures: []string{"abs_date_diff", "ClearAllPVI", "ClearDirtyData", "cms_generate_dbuser", "copyprocessversion",
			"DeleteProcessVersion", "DeleteProcessVersionInstance", "Delete_ReMail", "PAStat", "Table_Category_Add"},
	}
	for _, reuse := range []bool{false, true} {
		report, err := EnsureCompatibilityFunctions(ctx, w, names)
		require.NoError(t, err)
		require.Equal(t, 18, report.Total)
		if reuse {
			require.Equal(t, 18, report.Reused)
		} else {
			require.Equal(t, 18, report.Created)
		}
		var got []string
		for _, item := range report.Items {
			kind := "function"
			if item.Kind == "procedure" {
				kind = item.Kind
			}
			got = append(got, kind+" "+item.Name)
		}
		var want []string
		for _, name := range names.Functions {
			want = append(want, "function "+strings.ToLower(name))
		}
		for _, name := range names.Procedures {
			if name != "cms_generate_dbuser" {
				want = append(want, "procedure "+strings.ToLower(name))
			}
		}
		require.ElementsMatch(t, want, got)
	}
}

func TestDemo2DateLiteralOverloadSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema := newBusinessIntegrationSchema(t, ctx, db)
	w := builtinIntegrationWriter(db, schema, "postgres")
	name := qualifiedFunction(schema, "abs_date_diff")
	businessExec(t, ctx, db, "CREATE PROCEDURE "+name+"(text,text) LANGUAGE plpgsql AS $$BEGIN NULL; END$$")
	before := procedureOverloadDefinitions(t, ctx, db, schema, "abs_date_diff")
	require.Len(t, before, 1)
	report, err := EnsureCompatibilityFunctions(ctx, w, CompatibilityRoutineNames{Procedures: []string{"abs_date_diff"}})
	require.ErrorContains(t, err, "字符串字面量")
	require.Equal(t, 1, report.Failed)
	require.Equal(t, before, procedureOverloadDefinitions(t, ctx, db, schema, "abs_date_diff"))
}
