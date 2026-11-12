package target

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestDemoCompatibilityProceduresSQLIntegration(t *testing.T) {
	testPGCompatibilityProducts(t, func(t *testing.T, ctx context.Context, db *sql.DB, product string) {
		for _, tc := range []struct {
			name       string
			procedures []string
			check      func(*testing.T, context.Context, *sql.DB, string)
		}{
			{"fixed_deletes", []string{"DeleteTouBiaoInfo", "DeleteTouBiaoInfo_SXF", "DeleteZhaoBiaoInfo"}, checkDemoFixedDeletes},
			{"dynamic_deletes", []string{"PROC_PINGBIAO_DELETEBIAODUAN", "PROC_PINGBIAO_DELETETOUBIAODW", "P_GET_CLASS_NAME"}, checkDemoDynamicDeletes},
			{"calc_quote", []string{"PROC_PINGBIAO_CALCBJINFO"}, checkDemoCalcQuote},
			{"quote_details", []string{"Pro_GetBJDetails"}, checkDemoQuoteDetails},
			{"history", []string{"PROC_PB_MOVEDATA2HISTORYDB", "Proc_PingBiao_MoveAllData2HistoryDB"}, checkDemoHistoryMove},
		} {
			t.Run(tc.name, func(t *testing.T) {
				schema := newBusinessIntegrationSchema(t, ctx, db)
				report, err := ensurePGProcedureCompatibility(ctx, db, schema, product, tc.procedures)
				require.NoError(t, err)
				require.Equal(t, len(tc.procedures), report.Created, "initialization runs before business tables exist")
				tc.check(t, ctx, db, schema)
				report, err = ensurePGProcedureCompatibility(ctx, db, schema, product, tc.procedures)
				require.NoError(t, err)
				require.Equal(t, len(tc.procedures), report.Reused)
			})
		}
		if product == "postgres" {
			t.Run("reject_cross_schema_inheritance", func(t *testing.T) {
				checkDemoDeleteInheritanceScope(t, ctx, db, product)
			})
		}
	})
}

func demoCount(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(ctx, query, args...).Scan(&count))
	return count
}

func checkDemoFixedDeletes(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	q := func(name string) string { return qualifiedFunction(schema, name) }
	bidderTables := strings.Fields(demoDeleteTouBiaoTables + " pingbiao_qtxinxi pingbiao_tb_cbrgyclmx jsgc_pb_ecofile")
	for _, table := range bidderTables {
		businessExec(t, ctx, db, "CREATE TABLE "+q(table)+`("BiaoDuanGuid" varchar,"DanWeiGuid" varchar)`)
		businessExec(t, ctx, db, "INSERT INTO "+q(table)+" VALUES ('A','D'),('A','other'),('B','D')")
	}
	for _, table := range strings.Fields(demoDeleteZhaoBiaoLotTables) {
		businessExec(t, ctx, db, "CREATE TABLE "+q(table)+"(biaoduanguid varchar)")
		businessExec(t, ctx, db, "INSERT INTO "+q(table)+" VALUES ('A'),('B')")
	}

	businessExec(t, ctx, db, "CALL "+q("deletetoubiaoinfo")+"('A','D')")
	for i, table := range bidderTables {
		want := 2
		if i >= len(bidderTables)-3 {
			want = 3
		}
		require.Equal(t, want, demoCount(t, ctx, db, "SELECT count(*) FROM "+q(table)), table)
	}
	techFile := q("pingbiao_tb_techfile")
	businessExec(t, ctx, db, "INSERT INTO "+techFile+" VALUES ('A','D')")
	businessExec(t, ctx, db, "CALL "+q("deletetoubiaoinfo_sxf")+"('A','D')")
	for _, table := range bidderTables {
		want := 2
		if table == "pingbiao_tb_techfile" {
			want = 3
		}
		require.Equal(t, want, demoCount(t, ctx, db, "SELECT count(*) FROM "+q(table)), table)
	}

	businessExec(t, ctx, db, "INSERT INTO "+q("jsgc_pb_ecofile")+" VALUES ('A','ZB001'),('B','ZB001')")
	businessExec(t, ctx, db, "CALL "+q("deletezhaobiaoinfo")+"('A','other')")
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("pingbiao_zb_danweigc")))
	require.Equal(t, 3, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("jsgc_pb_ecofile")))
	for _, table := range strings.Fields(demoDeleteZhaoBiaoLotTables) {
		require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q(table)+" WHERE biaoduanguid='B'"), table)
		require.Zero(t, demoCount(t, ctx, db, "SELECT count(*) FROM "+q(table)+" WHERE biaoduanguid='A'"), table)
	}
}

func checkDemoDeleteInheritanceScope(t *testing.T, ctx context.Context, db *sql.DB, product string) {
	schema := newBusinessIntegrationSchema(t, ctx, db)
	otherSchema := newBusinessIntegrationSchema(t, ctx, db)
	parent := qualifiedFunction(schema, "lot_parent")
	child := qualifiedFunction(otherSchema, "lot_child")
	report, err := ensurePGProcedureCompatibility(ctx, db, schema, product, []string{"P_GET_CLASS_NAME"})
	require.NoError(t, err)
	require.Equal(t, 1, report.Created)
	businessExec(t, ctx, db, "CREATE TABLE "+parent+"(biaoduanguid varchar)")
	businessExec(t, ctx, db, "CREATE TABLE "+child+"() INHERITS ("+parent+")")
	businessExec(t, ctx, db, "INSERT INTO "+parent+" VALUES ('A')")
	businessExec(t, ctx, db, "INSERT INTO "+child+" VALUES ('A')")
	call := "CALL " + qualifiedFunction(schema, "p_get_class_name") + "('A')"
	businessExec(t, ctx, db, call)
	require.Zero(t, demoCount(t, ctx, db, "SELECT count(*) FROM ONLY "+parent))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+child))

	partitioned := qualifiedFunction(schema, "lot_partitioned")
	partition := qualifiedFunction(otherSchema, "lot_partition")
	businessExec(t, ctx, db, "CREATE TABLE "+partitioned+"(biaoduanguid varchar) PARTITION BY LIST(biaoduanguid)")
	businessExec(t, ctx, db, "CREATE TABLE "+partition+" PARTITION OF "+partitioned+" FOR VALUES IN ('A')")
	businessExec(t, ctx, db, "INSERT INTO "+partitioned+" VALUES ('A')")
	_, err = db.ExecContext(ctx, call)
	require.ErrorContains(t, err, "partitions outside the target schema")
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+partition))
}

func checkDemoDynamicDeletes(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	q := func(name string) string { return qualifiedFunction(schema, name) }
	other := qualifiedFunction(newBusinessIntegrationSchema(t, ctx, db), "BiddingData")
	for _, table := range []string{q("BiddingData"), q("TEMP_Save"), q("PingBiao_BiaoDuan"), q("PingBiao_Error"), q("PingBiao_KaiBiaoTouBiao"), other} {
		businessExec(t, ctx, db, "CREATE TABLE "+table+`("BiaoDuanGuid" varchar,"DanWeiGuid" varchar)`)
		businessExec(t, ctx, db, "INSERT INTO "+table+" VALUES ('A','D'),('A','other'),('B','D')")
	}
	businessExec(t, ctx, db, "CREATE VIEW "+q("ordinary_view")+" AS SELECT * FROM "+q("BiddingData"))
	businessExec(t, ctx, db, "CREATE TABLE "+q("OneKey")+"(biaoduanguid varchar)")
	businessExec(t, ctx, db, "INSERT INTO "+q("OneKey")+" VALUES ('A'),('B')")
	businessExec(t, ctx, db, "CALL "+q("proc_pingbiao_deletetoubiaodw")+"('A','D')")
	require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("BiddingData")))
	require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("OneKey")))
	for _, table := range []string{q("TEMP_Save"), q("PingBiao_Error"), q("PingBiao_KaiBiaoTouBiao"), other} {
		require.Equal(t, 3, demoCount(t, ctx, db, "SELECT count(*) FROM "+table))
	}

	// Every mutating statement remains part of the caller's transaction.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "CALL "+q("p_get_class_name")+"('A')")
	require.NoError(t, err)
	var count int
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM "+q("OneKey")).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, tx.Rollback())
	require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("OneKey")))

	businessExec(t, ctx, db, "CALL "+q("proc_pingbiao_deletebiaoduan")+"('A')")
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("OneKey")))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("BiddingData")))
	require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("PingBiao_BiaoDuan")))
	require.Equal(t, 3, demoCount(t, ctx, db, "SELECT count(*) FROM "+other))
}

func checkDemoCalcQuote(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	q := func(name string) string { return qualifiedFunction(schema, name) }
	detail, history := q("PB_TB_BaoJiaHistroyDetail"), q("PingBiao_BaoJiaHistroy")
	businessExec(t, ctx, db, "CREATE TABLE "+detail+"(bjguid varchar,jiegou varchar,unitprice numeric,geshu numeric,totalprice numeric)")
	businessExec(t, ctx, db, "CREATE TABLE "+history+"(bjguid varchar,bjtotal numeric,finalbaojia numeric)")
	businessExec(t, ctx, db, "INSERT INTO "+detail+" VALUES ('A','1',2,3,0),('A','.2',5,2,0),('A','1.2',20,1,0),('A','2',NULL,1,0),('B','1',1,1,7)")
	businessExec(t, ctx, db, "INSERT INTO "+history+" VALUES ('A',0,0),('B',7,7)")
	businessExec(t, ctx, db, "CALL "+q("proc_pingbiao_calcbjinfo")+"('A')")
	require.Equal(t, 3, demoCount(t, ctx, db, "SELECT count(*) FROM "+detail+" WHERE bjguid='A' AND totalprice=10"))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+detail+" WHERE bjguid='A' AND jiegou='1.2' AND totalprice=20"))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+history+" WHERE bjguid='A' AND bjtotal=20 AND finalbaojia=20"))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+history+" WHERE bjguid='B' AND bjtotal=7 AND finalbaojia=7"))
}

func checkDemoQuoteDetails(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	q := func(name string) string { return qualifiedFunction(schema, name) }
	lots, bidders, history := q("PingBiao_BiaoDuan"), q("PingBiao_KaiBiaoTouBiao"), q("PingBiao_BaoJiaHistroy")
	businessExec(t, ctx, db, "CREATE TABLE "+lots+"(biaoduanguid varchar,baojiaround integer)")
	businessExec(t, ctx, db, "CREATE TABLE "+bidders+`(rowguid varchar,danweiguid varchar,upr_date timestamp,
 biaoduanguid varchar,toubiaototal numeric,danweiname varchar,iscanyupb varchar)`)
	businessExec(t, ctx, db, "INSERT INTO "+lots+" VALUES ('one',1),('multi',3)")
	businessExec(t, ctx, db, "INSERT INTO "+bidders+` VALUES ('single','D','2026-01-01 12:05:30','one',42,'single bidder','0'),
 ('start','D','2026-01-01 12:05:30','multi',100,'first bidder','1'),
 ('excluded','E',NULL,'multi',5,'ineligible','0')`)
	call := "CALL " + q("pro_getbjdetails") + "($1::varchar)"
	type quote struct {
		round         int
		id            string
		amount, final float64
		date          string
		reviewer      sql.NullString
	}
	readQuotes := func(lot string, flags []string) []quote {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer tx.Rollback()
		var cursor string
		require.NoError(t, tx.QueryRowContext(ctx, call, lot).Scan(&cursor))
		rows, err := tx.QueryContext(ctx, "FETCH ALL FROM "+source.QuotePGIdent(cursor))
		require.NoError(t, err)
		defer rows.Close()
		columns, err := rows.Columns()
		require.NoError(t, err)
		require.Equal(t, []string{"BJNum", "BJGuid", "DanWeiGuid", "BJR_Name", "BJR_Date", "BiaoDuanGuid", "BJTotal", "FinalBaoJia", "DanWeiName", flags[0], flags[1], "AttachGuid", "AttachConName"}, columns)
		var quotes []quote
		for rows.Next() {
			var item quote
			var bidder, lotID, company, firstFlag, secondFlag, attachment, attachmentName string
			require.NoError(t, rows.Scan(&item.round, &item.id, &bidder, &item.reviewer, &item.date, &lotID, &item.amount, &item.final, &company, &firstFlag, &secondFlag, &attachment, &attachmentName))
			require.Equal(t, lot, lotID)
			quotes = append(quotes, item)
		}
		require.NoError(t, rows.Err())
		return quotes
	}

	one := readQuotes("one", []string{"Status", "IsQianZhang"})
	require.Len(t, one, 1, "one round returns even an ineligible bidder and needs no history table")
	require.Equal(t, "single", one[0].id)
	require.Equal(t, float64(42), one[0].amount)

	businessExec(t, ctx, db, "CREATE TABLE "+history+`(bjguid varchar,bjnum integer,danweiguid varchar,biaoduanguid varchar,
 bjtotal varchar,finalbaojia varchar,status varchar,isqianzhang varchar,attachconname varchar,bjr_name varchar,bjr_date timestamp,danweiname varchar)`)
	businessExec(t, ctx, db, "INSERT INTO "+history+` VALUES
 ('submitted',2,'D','multi','20.50',NULL,'1','0','file',NULL,'2026-01-02 13:00:01.123','company'),
 ('not-submitted',3,'D','multi','1','1','0','0','file','reviewer',NULL,'company')`)
	multi := readQuotes("multi", []string{"IsQianZhang", "Status"})
	require.Len(t, multi, 3)
	require.Equal(t, "submitted", multi[0].id)
	require.Equal(t, "submitted", multi[1].id)
	require.ElementsMatch(t, []int{2, 3}, []int{multi[0].round, multi[1].round})
	require.Equal(t, float64(20.5), multi[0].amount)
	require.Zero(t, multi[0].final)
	require.False(t, multi[0].reviewer.Valid)
	require.False(t, multi[1].reviewer.Valid)
	require.NotContains(t, multi[0].date, ".123")
	require.Equal(t, 1, multi[2].round)
	require.Equal(t, float64(100), multi[2].amount)
	businessExec(t, ctx, db, "UPDATE "+history+" SET finalbaojia='invalid' WHERE bjguid='submitted'")
	require.Zero(t, readQuotes("multi", []string{"IsQianZhang", "Status"})[0].final)
}

func checkDemoHistoryMove(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	q := func(name string) string { return qualifiedFunction(schema, name) }
	archive := newBusinessIntegrationSchema(t, ctx, db)
	h := func(name string) string { return qualifiedFunction(archive, name) }
	lots, config, log, errLog := q("PingBiao_BiaoDuan"), q("PingBiao_HistoryDBConfig"), q("PingBiao_MoveDataLog"), q("PingBiao_MoveDataError")
	businessExec(t, ctx, db, "CREATE TABLE "+lots+"(biaoduanguid varchar PRIMARY KEY,pingbiaoisend varchar,historydb_name varchar,if_moved varchar,kaibiaodate date)")
	businessExec(t, ctx, db, "CREATE TABLE "+config+"(historydb_name varchar,isnowuse integer,dbtype varchar)")
	businessExec(t, ctx, db, "CREATE TABLE "+log+"(logcontent varchar,logtime timestamp,error_line varchar,biaoduanguid varchar)")
	businessExec(t, ctx, db, "CREATE TABLE "+errLog+"(errorcontent varchar,errortime timestamp,tablename varchar,biaoduanguid varchar)")
	businessExec(t, ctx, db, "INSERT INTO "+config+" VALUES ($1,1,'2')", archive)
	businessExec(t, ctx, db, "INSERT INTO "+lots+` VALUES
 ('move','1',NULL,'0',CURRENT_DATE-29),('keep','1',NULL,'0',CURRENT_DATE-28),
 ('old-unfinished','0',NULL,'0',CURRENT_DATE-91),('boundary-unfinished','0',NULL,'0',CURRENT_DATE-90),
 ('failure','1',NULL,'0',CURRENT_DATE),('suppressed','1',NULL,'0',CURRENT_DATE),('same-schema','1',NULL,'0',CURRENT_DATE)`)

	copyOnly := "PingBiao_KaiBiaoTouBiao"
	for _, pair := range [][2]string{{"A_Source", "a_source"}, {copyOnly, "pingbiao_kaibiaotoubiao"}} {
		businessExec(t, ctx, db, "CREATE TABLE "+q(pair[0])+`("Payload" varchar,"BiaoDuanGuid" varchar)`)
		businessExec(t, ctx, db, "CREATE TABLE "+h(pair[1])+"(biaoduanguid varchar,payload varchar)")
		businessExec(t, ctx, db, "INSERT INTO "+q(pair[0])+" VALUES ('row','move'),('other','keep'),('unchanged','failure'),('not copied','suppressed')")
	}
	businessExec(t, ctx, db, "ALTER TABLE "+q("A_Source")+` ADD PRIMARY KEY ("BiaoDuanGuid")`)
	businessExec(t, ctx, db, "CREATE TABLE "+q("B_Cascade")+`(biaoduanguid varchar REFERENCES `+q("A_Source")+`("BiaoDuanGuid") ON DELETE CASCADE)`)
	businessExec(t, ctx, db, "CREATE TABLE "+h("b_cascade")+"(biaoduanguid varchar)")
	businessExec(t, ctx, db, "INSERT INTO "+q("B_Cascade")+" VALUES ('move')")
	businessExec(t, ctx, db, "CREATE TABLE "+q("C_InheritedParent")+"(biaoduanguid varchar)")
	businessExec(t, ctx, db, "CREATE TABLE "+q("D_InheritedChild")+"(child_info varchar) INHERITS ("+q("C_InheritedParent")+")")
	businessExec(t, ctx, db, "CREATE TABLE "+h("c_inheritedparent")+"(biaoduanguid varchar)")
	businessExec(t, ctx, db, "CREATE TABLE "+h("d_inheritedchild")+"(biaoduanguid varchar,child_info varchar)")
	businessExec(t, ctx, db, "INSERT INTO "+q("C_InheritedParent")+" VALUES ('move')")
	businessExec(t, ctx, db, "INSERT INTO "+q("D_InheritedChild")+" VALUES ('move','kept')")
	businessExec(t, ctx, db, "CREATE TABLE "+q("PingBiao_Meeting")+"(biaoduanguid varchar)")
	businessExec(t, ctx, db, "INSERT INTO "+q("PingBiao_Meeting")+" VALUES ('move')")
	businessExec(t, ctx, db, "CALL "+q("proc_pingbiao_movealldata2historydb")+"()")
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+lots+" WHERE biaoduanguid='move' AND historydb_name=$1 AND if_moved='1'", archive))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+h("a_source")+" WHERE biaoduanguid='move' AND payload='row'"))
	require.Equal(t, 0, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("A_Source")+` WHERE "BiaoDuanGuid"='move'`))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+h("b_cascade")+" WHERE biaoduanguid='move'"))
	require.Zero(t, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("B_Cascade")))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+h("c_inheritedparent")+" WHERE biaoduanguid='move'"))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+h("d_inheritedchild")+" WHERE biaoduanguid='move' AND child_info='kept'"))
	require.Zero(t, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("C_InheritedParent")))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q(copyOnly)+` WHERE "BiaoDuanGuid"='move'`))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+h("pingbiao_kaibiaotoubiao")+" WHERE biaoduanguid='move'"))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("PingBiao_Meeting")+" WHERE biaoduanguid='move'"))
	require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+log+" WHERE biaoduanguid IN ('move','old-unfinished') AND logcontent='迁移成功'"))
	require.Equal(t, 0, demoCount(t, ctx, db, "SELECT count(*) FROM "+log+" WHERE biaoduanguid IN ('keep','boundary-unfinished')"))

	suppress := h("suppress_archive_row")
	businessExec(t, ctx, db, "CREATE FUNCTION "+suppress+"() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NULL; END;$$")
	businessExec(t, ctx, db, "CREATE TRIGGER suppress_insert BEFORE INSERT ON "+h("a_source")+" FOR EACH ROW EXECUTE PROCEDURE "+suppress+"()")
	businessExec(t, ctx, db, "CALL "+q("proc_pb_movedata2historydb")+"('suppressed')")
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("A_Source")+` WHERE "BiaoDuanGuid"='suppressed'`))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+errLog+" WHERE biaoduanguid='suppressed' AND errorcontent LIKE '%copied 0 of 1 rows%'"))
	businessExec(t, ctx, db, "DROP TRIGGER suppress_insert ON "+h("a_source"))

	businessExec(t, ctx, db, "CREATE TABLE "+q("Z_MissingArchive")+"(biaoduanguid varchar)")
	businessExec(t, ctx, db, "CALL "+q("proc_pb_movedata2historydb")+"('failure')")
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("A_Source")+` WHERE "BiaoDuanGuid"='failure'`))
	require.Equal(t, 0, demoCount(t, ctx, db, "SELECT count(*) FROM "+h("a_source")+" WHERE biaoduanguid='failure'"))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+errLog+" WHERE biaoduanguid='failure' AND tablename='Z_MissingArchive'"))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+log+" WHERE biaoduanguid='failure' AND logcontent='迁移失败'"))
	businessExec(t, ctx, db, "UPDATE "+config+" SET historydb_name=$1", schema)
	businessExec(t, ctx, db, "CALL "+q("proc_pb_movedata2historydb")+"('same-schema')")
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+errLog+" WHERE biaoduanguid='same-schema' AND errorcontent LIKE '%must differ%'"))
}
