package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// A DSN opts into disposable schemas in a dedicated test database. The source,
// when supplied, is only queried for constant expression results, never changed.
func TestSQLServerPGViewsIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_TEST_KINGBASE_DSN")
	if dsn == "" {
		t.Skip("DBGOLD_TEST_KINGBASE_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	schema := fmt.Sprintf("dbgold_s2pg_views_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+source.QuotePGIdent(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := db.ExecContext(cleanup, "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
		require.NoError(t, err)
	})
	w, err := target.NewKingbase(dsn, schema, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	m, _, _ := pgViewTestMigrator()
	m.writer, m.cfg.TargetSchema = w, schema
	qn := func(name string) string { return w.Dialect().QualifyTable(schema, name) }
	for _, ddl := range []string{
		"CREATE TABLE " + qn("frame_user") + " (row_id integer,userguid varchar(50))",
		"CREATE TABLE " + qn("frame_user_secondou") + " (row_id integer,userguid varchar(50))",
		"CREATE TABLE " + qn("portal_item") + " (rowguid varchar(50),disabled varchar(50))",
		"CREATE TABLE " + qn("portal_myitem") + " (portaletguid varchar(50),disabled integer)",
		"INSERT INTO " + qn("frame_user") + " VALUES(7,'a')",
		"INSERT INTO " + qn("frame_user_secondou") + " VALUES(12,'a')",
	} {
		_, err = db.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
	view := source.ViewInfo{ViewName: "secondou", Definition: `CREATE VIEW dbo.secondou AS SELECT Row_id FROM Frame_User UNION SELECT 1000000000+a.ROW_ID AS row_id FROM FRAME_USER_SECONDOU a LEFT JOIN Frame_User b ON a.UserGuid=b.UserGuid`}
	_, err = m.createOneView(ctx, view)
	require.NoError(t, err)
	rows, err := db.QueryContext(ctx, "SELECT row_id FROM "+qn("secondou")+" ORDER BY row_id")
	require.NoError(t, err)
	var actual []int64
	for rows.Next() {
		var value int64
		require.NoError(t, rows.Scan(&value))
		actual = append(actual, value)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []int64{7, 1000000012}, actual)

	var src *sql.DB
	if sourceDSN := os.Getenv("DBGOLD_SQLSERVER_PG_SOURCE_DSN"); sourceDSN != "" {
		src, err = sql.Open("sqlserver", sourceDSN)
		require.NoError(t, err)
		defer src.Close()
	}
	for _, tt := range []struct {
		raw  string
		want string
		null bool
	}{
		{raw: `'1'+'2'+3`, want: "15"},
		{raw: `'1'+2+3`, want: "6"},
		{raw: `CAST(1 AS nvarchar(10))+N'AbC'`, want: "1AbC"},
		{raw: `(CASE WHEN 1=1 THEN 1 ELSE '2' END)+'3'`, want: "4"},
		{raw: `COALESCE(NULL,1,'2')+'3'`, want: "4"},
		{raw: `LEN(123)`, want: "3"},
		{raw: `N'AbC'+NULL`, null: true},
		{raw: `NULL+N'AbC'`, null: true},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			v, err := m.prepareView(ctx, source.ViewInfo{ViewName: "expression", Definition: "SELECT " + tt.raw + " AS result"})
			require.NoError(t, err)
			var value sql.NullString
			require.NoError(t, db.QueryRowContext(ctx, "SELECT CAST(result AS text) FROM ("+v.Definition+") q").Scan(&value))
			require.Equal(t, !tt.null, value.Valid)
			require.Equal(t, tt.want, value.String)
			if src != nil {
				var sourceValue sql.NullString
				require.NoError(t, src.QueryRowContext(ctx, "SELECT CAST(("+tt.raw+") AS nvarchar(100))").Scan(&sourceValue))
				require.Equal(t, sourceValue, value)
			}
		})
	}
	view = source.ViewInfo{ViewName: "myitem", Definition: `CREATE VIEW dbo.myitem AS SELECT p.RowGuid FROM portal_item p INNER JOIN Portal_MyItem m ON p.RowGuid=m.PortaletGuid WHERE NOT p.disabled<>0 AND m.disabled=0`}
	_, err = m.createOneView(ctx, view)
	require.NoError(t, err)
	for _, input := range []any{"0", "00", " 0 ", "", "   ", "+0", "1", nil, "bad", "2147483648"} {
		t.Run(fmt.Sprintf("disabled_%v", input), func(t *testing.T) {
			_, err := db.ExecContext(ctx, "TRUNCATE "+qn("portal_item")+", "+qn("portal_myitem"))
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, "INSERT INTO "+qn("portal_item")+" VALUES('a',$1)", input)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, "INSERT INTO "+qn("portal_myitem")+" VALUES('a',0)")
			require.NoError(t, err)
			var count int
			targetErr := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qn("myitem")).Scan(&count)
			if src != nil {
				var expected int
				sourceErr := src.QueryRowContext(ctx, "SELECT CASE WHEN NOT CAST(@p1 AS nvarchar(50))<>0 THEN 1 ELSE 0 END", input).Scan(&expected)
				require.Equal(t, sourceErr != nil, targetErr != nil)
				if sourceErr == nil {
					require.Equal(t, expected, count)
				}
			} else if input == "bad" || input == "2147483648" {
				require.Error(t, targetErr)
			} else {
				require.NoError(t, targetErr)
				if input == nil || input == "1" {
					require.Zero(t, count)
				} else {
					require.Equal(t, 1, count)
				}
			}
		})
	}
	// The converter's metadata source is a fixture; only the isolated Kingbase
	// schema is mutated. Real source SQL above consists entirely of SELECTs.
}
