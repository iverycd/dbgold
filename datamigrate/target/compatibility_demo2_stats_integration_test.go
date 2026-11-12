package target

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestCompatibilityDemo2StatsIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("DBGOLD_TEST_POSTGRES_DSN is not set")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	t.Run("score_and_publication_count", func(t *testing.T) {
		schema := newBusinessIntegrationSchema(t, ctx, db)
		q := func(name string) string { return qualifiedFunction(schema, name) }
		w := builtinIntegrationWriter(db, schema, "postgres")
		inventory := CompatibilityRoutineNames{Functions: []string{"GetExtraScore", "Getpainfocount"}}
		report, err := EnsureCompatibilityFunctions(ctx, w, inventory)
		require.NoError(t, err)
		require.Equal(t, 2, report.Created)
		report, err = EnsureCompatibilityFunctions(ctx, w, inventory)
		require.NoError(t, err)
		require.Equal(t, 2, report.Reused)

		var value int
		count := "SELECT " + q("getpainfocount") + "($1,$2,$3,$4,$5,'2026-10-01','2026-10-08','2026-10-01','2026-10-31')"
		require.NoError(t, db.QueryRowContext(ctx, count, "u1", "o1", "A_%", nil, 1).Scan(&value))
		require.Zero(t, value, "unsupported limits do not access the view")
		businessExec(t, ctx, db, "CREATE TABLE "+q("WebDB_PA_Score")+`("UserGuid" varchar,"PostDate" timestamp,"Score" numeric)`)
		businessExec(t, ctx, db, "INSERT INTO "+q("WebDB_PA_Score")+` VALUES
 ('u1','2026-10-01',2),('u1','2026-10-08',3.5),('u1','2026-10-08 12:00:00',40),
 ('u2','2026-10-08',200),('u1','2026-10-08',NULL),('u3','2026-10-01',NULL)`)
		score := "SELECT " + q("getextrascore") + "($1,'2026-10-01','2026-10-08')"
		for _, tc := range []struct {
			user any
			want int
		}{{"u1", 6}, {"u2", 200}, {"u3", 0}, {nil, 0}} {
			require.NoError(t, db.QueryRowContext(ctx, score, tc.user).Scan(&value))
			require.Equal(t, tc.want, value, "user %v", tc.user)
		}

		createDemo2PAInfoView(t, ctx, db, schema)
		for _, tc := range []struct {
			user, ou, category any
			limit, dtype       any
			want               int
		}{
			{"u1", "o1", "A_%", 0, 1, 2}, {"u1", "o1", "A_%", 2, 1, 2},
			{"u1", "o1", "A_%", 3, 2, 3}, {"u1", "o1", "A_%", 3, nil, 3},
			{"u1", "o1", "A_%", 1, 1, 0}, {"u1", "o1", nil, 0, 1, 0},
			{"u1", nil, "A_%", 0, 1, 0}, {"u1", "o1", "", 0, 1, 3},
			{"u2", "", "A_%", 0, 1, 1}, {nil, "o1", "A_%", 0, 1, 0},
		} {
			require.NoError(t, db.QueryRowContext(ctx, count, tc.user, tc.ou, tc.category, tc.limit, tc.dtype).Scan(&value))
			require.Equal(t, tc.want, value, "%+v", tc)
		}
	})

	t.Run("pastat_inline", func(t *testing.T) {
		checkDemo2PAStat(t, ctx, db, false)
	})
	t.Run("pastat_target_function", func(t *testing.T) {
		checkDemo2PAStat(t, ctx, db, true)
	})
}

func createDemo2PAInfoView(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	t.Helper()
	q := func(name string) string { return qualifiedFunction(schema, name) }
	businessExec(t, ctx, db, "CREATE TABLE "+q("InfoRows")+`("InfoID" varchar,"CategoryNum" varchar,"OUGuid" varchar,"UserGuid" varchar,"PubInWebDate" timestamp)`)
	businessExec(t, ctx, db, "INSERT INTO "+q("InfoRows")+` VALUES
 ('first','A_%','o1','u1','2026-10-01'),('end','A_%child','o1','u1','2026-10-08'),
 ('next','A_%child','o1','u1','2026-10-09'),('different','AXZchild','o1','u1','2026-10-02'),
 (NULL,'A_%child','o1','u1','2026-10-02'),('other-ou','A_%child','o2','u1','2026-10-02'),
 ('other-user','A_%child','o1','u2','2026-10-02'),('empty-ou','A_%child','','u2','2026-10-02')`)
	businessExec(t, ctx, db, "CREATE VIEW "+q("ViEw_InFoMaIn")+" AS SELECT * FROM "+q("InfoRows"))
}

func checkDemo2PAStat(t *testing.T, ctx context.Context, db *sql.DB, targetFunction bool) {
	t.Helper()
	schema := newBusinessIntegrationSchema(t, ctx, db)
	q := func(name string) string { return qualifiedFunction(schema, name) }
	if targetFunction {
		businessExec(t, ctx, db, "CREATE FUNCTION "+q("getpainfocount")+`(
 varchar,varchar,varchar,integer,integer,timestamp,timestamp,timestamp,timestamp)
 RETURNS integer LANGUAGE sql AS $$SELECT 42$$`)
	}
	report, err := EnsureCompatibilityFunctions(ctx, builtinIntegrationWriter(db, schema, "postgres"), CompatibilityRoutineNames{Procedures: []string{"PAStat"}})
	require.NoError(t, err)
	require.Equal(t, 1, report.Created)
	if !targetFunction {
		var installed bool
		err = db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname='getpainfocount')`, schema).Scan(&installed)
		require.NoError(t, err)
		require.False(t, installed)
		createDemo2PAInfoView(t, ctx, db, schema)
	}
	businessExec(t, ctx, db, "CREATE TABLE "+q("WebDB_PA_Count")+`("CategoryNum" varchar,"DType" smallint,"MinCount" integer,"LimitType" integer,"BelongTo" varchar)`)
	businessExec(t, ctx, db, "INSERT INTO "+q("WebDB_PA_Count")+` VALUES ('A_%',1,1,0,'All'),('A_%',2,2,2,'u1'),('missing',1,3,1,'All'),('unused',1,4,0,'u3')`)
	businessExec(t, ctx, db, "CREATE TABLE "+q("Frame_User")+`("UserGuid" varchar,"DisplayName" varchar)`)
	businessExec(t, ctx, db, "INSERT INTO "+q("Frame_User")+` VALUES ('u1','User One')`)
	businessExec(t, ctx, db, "CREATE TABLE "+q("WebDB_Category")+`("CategoryNum" varchar,"CategoryName" varchar)`)
	businessExec(t, ctx, db, "INSERT INTO "+q("WebDB_Category")+` VALUES ('A_%','Literal prefix')`)
	businessExec(t, ctx, db, "CREATE TABLE "+q("temp_tb")+`(marker integer)`)
	businessExec(t, ctx, db, "INSERT INTO "+q("temp_tb")+` VALUES (7)`)

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO pg_catalog,public")
	require.NoError(t, err)
	call := "CALL " + q("pastat") + "($1::text,$2::text,'2026-10-01','2026-10-08','2026-10-01','2026-10-31')"
	var cursor string
	require.NoError(t, tx.QueryRowContext(ctx, call, "u1;u2;ignored", "o1;unpaired").Scan(&cursor))
	rows, err := tx.QueryContext(ctx, "FETCH ALL FROM "+source.QuotePGIdent(cursor))
	require.NoError(t, err)
	defer rows.Close()
	columns, err := rows.Columns()
	require.NoError(t, err)
	require.Equal(t, []string{"SYSID", "DISPLAYNAME", "USERGUID", "OUGUID", "CATEGORYNUM", "CATEGORYNAME", "DTYPE", "MINCOUNT", "PUBCOUNT"}, columns)

	type stat struct {
		id, dateType, minCount, pubCount int
		user, ou, category               string
		displayName, categoryName        sql.NullString
	}
	var actual []stat
	for rows.Next() {
		var item stat
		require.NoError(t, rows.Scan(&item.id, &item.displayName, &item.user, &item.ou, &item.category, &item.categoryName, &item.dateType, &item.minCount, &item.pubCount))
		actual = append(actual, item)
	}
	require.NoError(t, rows.Err())
	name := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	want := []stat{
		{1, 1, 1, 2, "u1", "o1", "A_%", name("User One"), name("Literal prefix")},
		{1, 2, 2, 3, "u1", "o1", "A_%", name("User One"), name("Literal prefix")},
		{1, 1, 3, 0, "u1", "o1", "missing", name("User One"), sql.NullString{}},
		{2, 1, 1, 1, "u2", "", "A_%", sql.NullString{}, name("Literal prefix")},
		{2, 1, 3, 0, "u2", "", "missing", sql.NullString{}, sql.NullString{}},
	}
	if targetFunction {
		for i := range want {
			want[i].pubCount = 42
		}
	}
	require.ElementsMatch(t, want, actual)
	require.NoError(t, tx.Commit())

	var count, marker int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM pg_temp.temp_tb").Scan(&count))
	require.Equal(t, 5, count)
	require.NoError(t, conn.QueryRowContext(ctx, call, "no-delimiter", nil).Scan(&cursor))
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM pg_temp.temp_tb").Scan(&count))
	require.Zero(t, count, "a second call refreshes this session's result table")
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT marker FROM "+q("temp_tb")).Scan(&marker))
	require.Equal(t, 7, marker)
}
