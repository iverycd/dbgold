package damengnative

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMaterializedVerificationDoesNotInventSourceHistory(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	r.verification = true
	require.NoError(t, r.materializedSnapshot(0, `CREATE MATERIALIZED VIEW "App"."Mv" FOR "App"."Storage" ON PREBUILT TABLE REFRESH FAST AS SELECT "Id" FROM "App"."Base"`))
	require.False(t, r.snapshot.Objects[0].Materialized.Baseline)
	require.Len(t, r.snapshot.Objects[0].Units, 2)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedNeverRefreshedSnapshotRetainsUnusableBaseline(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	r.ctx = context.Background()
	r.snapshot.Objects[0].Materialized = &MaterializedState{Baseline: true, Staleness: "STALE"}
	r.snapshot.Objects[0].Units = []Unit{{SQL: "create", Phase: "after_data"}, {SQL: "prepare", Phase: "mv_prepare"}, {SQL: "refresh", Phase: "mv_rebase"}, {SQL: "restore", Phase: "mv_restore"}}
	m.ExpectQuery(`SELECT TO_CHAR\(LAST_REFRESH_DATE`).WithArgs("App", "Mv").WillReturnRows(sqlmock.NewRows([]string{"date", "state"}).AddRow("1900-01-01 00:00:00.000000", "UNUSEABLE"))
	require.NoError(t, r.prepareMaterializedHistory())
	require.False(t, r.snapshot.Objects[0].Materialized.Baseline)
	require.Equal(t, "UNUSEABLE", r.snapshot.Objects[0].Materialized.Staleness)
	require.Nil(t, r.snapshot.Objects[0].Materialized.History)
	require.Len(t, r.snapshot.Objects[0].Units, 2)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedPartitionBufferKeepsNativeColumnAndLiteralSemantics(t *testing.T) {
	ddl := `CREATE TABLE "App"."MTAB$_MiXeD" ("Id" INT,"Text" VARCHAR(80) DEFAULT 'PARTITION BY / App.X') STORAGE(ON "MAIN",CLUSTERBTR);`
	out, err := MaterializedPartitionBufferDDL(ddl, "App", "Buffer", "P", []Column{{Name: "Id", DataType: "INT"}, {Name: "Text", DataType: "VARCHAR"}})
	require.NoError(t, err)
	require.Contains(t, out, `CREATE TABLE "App"."Buffer"`)
	require.Contains(t, out, `DEFAULT 'PARTITION BY / App.X'`)
	require.Contains(t, out, `PARTITION BY RANGE("Id") (PARTITION "P" VALUES LESS THAN(MAXVALUE))`)
	require.Contains(t, out, `STORAGE(ON "MAIN",CLUSTERBTR)`)
}

func TestSyntheticTableConstraintsAreDistinctAndIdempotent(t *testing.T) {
	source := `CREATE TABLE "App"."Store"("Id" INT,"(" VARCHAR(20) DEFAULT '(',CONSTRAINT "KeepPK" PRIMARY KEY("Id"),CONSTRAINT "KeepCK" CHECK("Id">0))`
	a, err := renameDDLHead(source, "TABLE", "App", "Stage")
	require.NoError(t, err)
	b, err := renameDDLHead(source, "TABLE", "App", "Buffer")
	require.NoError(t, err)
	require.NotContains(t, a, `CONSTRAINT "KeepPK"`)
	require.NotEqual(t, a, b)
	again, err := renameDDLHead(a, "TABLE", "App", "Stage")
	require.NoError(t, err)
	require.Equal(t, a, again)
	require.Contains(t, a, `CHECK("Id">0)`)
}

func TestMaterializedLexingKeepsQuotedPunctuationInData(t *testing.T) {
	ddl := `CREATE TABLE "App"."Store"("Id" INT,"(" VARCHAR(20) DEFAULT '(',")" VARCHAR(20) DEFAULT ')') PARTITION BY HASH("Id")(PARTITION "P1",PARTITION "P2") STORAGE(ON "MAIN",CLUSTERBTR)`
	require.True(t, materializedHashPartitioned(ddl))
	plain, partitioned, err := MaterializedUnpartitionedBufferDDL(ddl, "App", "Buffer")
	require.NoError(t, err)
	require.True(t, partitioned)
	require.Contains(t, plain, `"(" VARCHAR(20) DEFAULT '('`)
	require.NotContains(t, plain, "PARTITION BY HASH")
	partition, err := MaterializedPartitionBufferDDL(plain, "App", "Buffer", "P", []Column{{Name: "Id", DataType: "INT"}})
	require.NoError(t, err)
	require.Contains(t, partition, `DEFAULT ')') PARTITION BY RANGE`)
	query := `SELECT ROWID, ';', ',', '(', ')' FROM "App"."Base"`
	_, err = MaterializedBagSQL("App", "Mv", query, []string{"Rid", "Semi", "Comma", "Open", "Close"}, nil)
	require.NoError(t, err)
	ids, err := materializedDirectRowIDs(query, 5)
	require.NoError(t, err)
	require.Equal(t, []int{0}, ids)
}

func TestMaterializedHistoryDescriptorFreezesContentAndSourceCutoff(t *testing.T) {
	p := &MaterializedHistoryPlan{Version: 1, Schema: "App", Views: []MaterializedHistoryView{{Name: "Mv", SourceRefresh: "2026-10-22 12:01:02.123456"}}}
	before := MaterializedHistorySQL("App", "Mv", p)
	p.Views[0].SourceRefresh = "2026-10-22 12:01:02.123457"
	require.NotEqual(t, before, MaterializedHistorySQL("App", "Mv", p))
	require.NotEqual(t, before, MaterializedHistorySQL("Target", "Mv", p))
}

func TestMaterializedStaleHashCannotAccidentallyUsePartitionExchange(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	m.ExpectQuery(`SELECT PARTITIONING_TYPE,SUBPARTITIONING_TYPE FROM ALL_PART_TABLES`).WithArgs("App", "Storage").WillReturnRows(sqlmock.NewRows([]string{"type", "subtype"}).AddRow("HASH", "NONE"))
	_, err = r.historyViewBuffers(Object{}, "Storage", `CREATE TABLE "App"."Storage"("Id" INT) PARTITION BY HASH("Id")(PARTITION "P1",PARTITION "P2")`, []Column{{Name: "Id", DataType: "INT", Writable: true}}, "Buffer")
	require.ErrorContains(t, err, "必须使用已冻结的原生在线重定义通路")
	require.NoError(t, m.ExpectationsWereMet())
}
