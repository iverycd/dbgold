package damengnative

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func nativeMVReader(db *sql.DB) *catalogReader {
	mv := Object{ID: ObjectID("App", "materialized_view", "Mv"), Kind: "materialized_view", Name: "Mv", Schema: "App"}
	store := Object{ID: ObjectID("App", "table", "Storage"), Kind: "table", Name: "Storage", Schema: "App"}
	return &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{mv, store}}, raw: map[int64]catalogObject{1: {id: 1, name: "Mv", sub: "VIEW", info3: 2}, 2: {id: 2, name: "Storage", sub: "UTAB"}}, byRaw: map[int64][]int{1: {0}, 2: {1}}, index: map[string]int{"materialized_view\x00Mv": 0, "table\x00Storage": 1}}
}

func expectNativeMVProof(t *testing.T, m sqlmock.Sqlmock, query string, columns []string, differences int64) {
	m.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "App"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectBegin()
	m.ExpectExec("SET TRANSACTION READ ONLY").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT * FROM "App"."Mv" WHERE 1=0`)).WillReturnRows(sqlmock.NewRows(columns))
	rowids, err := materializedDirectRowIDs(query, len(columns))
	require.NoError(t, err)
	if len(rowids) > 0 {
		types := sqlmock.NewRows([]string{"name", "type"})
		for i, c := range columns {
			kind := "INT"
			for _, id := range rowids {
				if id == i {
					kind = "ROWID"
				}
			}
			types.AddRow(c, kind)
		}
		m.ExpectQuery(`SELECT COLUMN_NAME,DATA_TYPE FROM ALL_TAB_COLUMNS`).WithArgs("App", "Mv").WillReturnRows(types)
	}
	bag, err := MaterializedBagSQL("App", "Mv", query, columns, nil)
	require.NoError(t, err)
	m.ExpectQuery(regexp.QuoteMeta(bag)).WillReturnRows(sqlmock.NewRows([]string{"differences"}).AddRow(differences))
	m.ExpectRollback()
}

func TestMaterializedFastPrimaryKeyEstablishesExplicitProvenBaseline(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	query := `SELECT "Id","Value" FROM "App"."T"`
	expectNativeMVProof(t, m, query, []string{"Id", "Value"}, 0)
	err = r.materializedSnapshot(0, `CREATE MATERIALIZED VIEW "App"."Mv" FOR "App"."Storage" ON PREBUILT TABLE REFRESH WITH PRIMARY KEY FAST ON COMMIT START WITH SYSDATE+1 NEXT SYSDATE+2 AS `+query)
	require.NoError(t, err)
	o := r.snapshot.Objects[0]
	require.NotNil(t, o.Materialized)
	require.True(t, o.Materialized.Baseline)
	require.Equal(t, "PROVEN_CURRENT", o.Materialized.Staleness)
	require.Equal(t, "FAST", o.Materialized.RefreshMethod)
	require.Equal(t, "PRIMARY KEY", o.Materialized.RefreshRule)
	require.Empty(t, o.Materialized.RowIDColumns)
	require.Len(t, o.Units, 4)
	require.Contains(t, o.Units[0].SQL, "NEVER REFRESH AS")
	require.NotContains(t, o.Units[0].SQL, "ON COMMIT")
	require.Equal(t, Unit{SQL: `ALTER MATERIALIZED VIEW "App"."Mv" REFRESH COMPLETE ON DEMAND`, Phase: "mv_prepare"}, o.Units[1])
	require.Equal(t, Unit{SQL: `REFRESH MATERIALIZED VIEW "App"."Mv" COMPLETE`, Phase: "mv_rebase"}, o.Units[2])
	require.Equal(t, Unit{SQL: `ALTER MATERIALIZED VIEW "App"."Mv" REFRESH WITH PRIMARY KEY FAST ON COMMIT START WITH SYSDATE+1 NEXT SYSDATE+2`, Phase: "mv_restore"}, o.Units[3])
	require.Len(t, r.snapshot.Issues, 1)
	require.Equal(t, "warning", r.snapshot.Issues[0].Severity)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedDefaultForceAlsoRequiresSnapshotProof(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	query := `SELECT "Id" FROM "App"."T"`
	expectNativeMVProof(t, m, query, []string{"Id"}, 0)
	require.NoError(t, r.materializedSnapshot(0, `CREATE MATERIALIZED VIEW "App"."Mv" AS `+query))
	require.Equal(t, "FORCE", r.snapshot.Objects[0].Materialized.RefreshMethod)
	require.Equal(t, `ALTER MATERIALIZED VIEW "App"."Mv" REFRESH FORCE ON DEMAND`, r.snapshot.Objects[0].Units[3].SQL)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedStaleFastRetainsSnapshotForHistoryPlanner(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	query := `SELECT "Id" FROM "App"."T"`
	expectNativeMVProof(t, m, query, []string{"Id"}, 2)
	err = r.materializedSnapshot(0, `CREATE MATERIALIZED VIEW "App"."Mv" REFRESH FAST AS `+query)
	require.NoError(t, err)
	require.Equal(t, "STALE", r.snapshot.Objects[0].Materialized.Staleness)
	require.Contains(t, r.snapshot.Objects[0].Units[0].SQL, "NEVER REFRESH AS")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedDirectRowIDsAreMappedByOutputOrdinal(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	query := `SELECT T."Id",T.ROWID AS "SourceRid" FROM "App"."T" T`
	expectNativeMVProof(t, m, query, []string{"Id", "Rid"}, 0)
	require.NoError(t, r.materializedSnapshot(0, `CREATE MATERIALIZED VIEW "App"."Mv"("Id","Rid") REFRESH FAST WITH ROWID AS `+query))
	require.Equal(t, []int{1}, r.snapshot.Objects[0].Materialized.RowIDColumns)
	require.Contains(t, r.snapshot.Issues[0].Reason, "目标物理 ROWID")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedRowIDExpressionsCannotBeMasked(t *testing.T) {
	for _, query := range []string{`SELECT ROWIDTOCHAR(T.ROWID) R FROM T`, `SELECT T.ROWID+1 R FROM T`, `SELECT T.*,T.ROWID R FROM T`, `WITH C AS (SELECT ROWID R FROM T) SELECT R FROM C`, `SELECT T.ROWID R FROM T WHERE T.ROWID=CHARTOROWID('AAAAAAAAAAAAAAAAAB')`} {
		_, err := materializedDirectRowIDs(query, 3)
		require.Error(t, err, query)
	}
	ids, err := materializedDirectRowIDs(`SELECT "Id",'ROWID' AS "Literal" FROM T`, 2)
	require.NoError(t, err)
	require.Empty(t, ids)
	ids, err = materializedDirectRowIDs(`WITH C AS (SELECT "Id" FROM T) SELECT T.ROWID R,T."Id" FROM T`, 2)
	require.NoError(t, err)
	require.Equal(t, []int{0}, ids)
}

func TestMaterializedBagComparisonPreservesDuplicatesAndOrdinalAliases(t *testing.T) {
	full, err := MaterializedBagSQL("App", "Mv", `SELECT A."x",B."x" FROM A,B`, []string{"One", "Two"}, nil)
	require.NoError(t, err)
	require.Contains(t, full, `AS "DBGOLD_Q" ("DBGOLD_C0","DBGOLD_C1")`)
	require.Contains(t, full, `COUNT(*) AS "DBGOLD_N"`)
	require.Contains(t, full, `GROUP BY "DBGOLD_C0","DBGOLD_C1"`)
	ordinary, err := MaterializedBagSQL("App", "Mv", `SELECT "x",ROWID FROM T`, []string{"One", "Rid"}, []int{1})
	require.NoError(t, err)
	require.Contains(t, ordinary, `GROUP BY "DBGOLD_C0" MINUS`)
	require.NotContains(t, ordinary, `GROUP BY "DBGOLD_C0","DBGOLD_C1"`)
	_, err = MaterializedBagSQL("App", "Mv", `SELECT 1; DELETE FROM T`, []string{"One"}, nil)
	require.Error(t, err)
}
