package target

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompatibilityProcurementSQLIntegration(t *testing.T) {
	testPGCompatibilityProducts(t, func(t *testing.T, ctx context.Context, db *sql.DB, product string) {
		t.Run("award_notice", func(t *testing.T) {
			schema := newBusinessIntegrationSchema(t, ctx, db)
			report, err := ensurePGProcedureCompatibility(ctx, db, schema, product, []string{"Proc_CG_AfterZBGS"})
			require.NoError(t, err, "initialization precedes business tables")
			require.Equal(t, 1, report.Created)
			checkProcurementAward(t, ctx, db, schema)
		})
		t.Run("missing_workflow_rolls_back_notice", func(t *testing.T) {
			schema := newBusinessIntegrationSchema(t, ctx, db)
			report, err := ensurePGProcedureCompatibility(ctx, db, schema, product, []string{"PROC_CG_GONGGAOINFO_DELETE"})
			require.NoError(t, err, "initialization precedes business tables and any workflow dependency")
			require.Equal(t, 1, report.Created)
			checkProcurementNoticeDelete(t, ctx, db, schema, product)
		})
	})
}

func checkProcurementAward(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	t.Helper()
	lots := qualifiedFunction(schema, "CG_BiaoDuanInfo")
	links := qualifiedFunction(schema, "cg_zbgsandbd")
	awards := qualifiedFunction(schema, "CG_ZhongBiaoDetail")
	bidders := qualifiedFunction(schema, "CG_TouBiaoDanWei")
	businessExec(t, ctx, db, "CREATE TABLE "+lots+`("BiaoDuanGuid" varchar PRIMARY KEY, "IsWeiZBRTuiBZJ" integer, "WeiZBRTuiBZJDate" timestamp)`)
	businessExec(t, ctx, db, "CREATE TABLE "+links+`("GongGaoGuid" varchar, biaoduanguid varchar)`)
	businessExec(t, ctx, db, "CREATE TABLE "+awards+`("BiaoDuanGuid" varchar, "DanWeiGuid" varchar)`)
	businessExec(t, ctx, db, "CREATE TABLE "+bidders+`("BiaoDuanGuid" varchar, "DanWeiGuid" varchar, "IsZhongBiao" integer, "BZJIsBack" varchar)`)
	businessExec(t, ctx, db, "INSERT INTO "+lots+` VALUES ('a',0,NULL),('b',0,NULL),('empty',0,NULL),('other',0,NULL)`)
	businessExec(t, ctx, db, "INSERT INTO "+links+` VALUES ('notice','a'),('notice','a'),('notice','b'),('notice','empty'),('unrelated','other')`)
	businessExec(t, ctx, db, "INSERT INTO "+awards+` VALUES
 ('a','one'),('a','two'),('a','two'),('b','loser'),('empty',NULL),('empty',''),('other','one')`)
	businessExec(t, ctx, db, "INSERT INTO "+bidders+` VALUES
 ('a','one',0,'0'),('a','two',0,'0'),('a','loser',0,'0'),('b','one',0,'0'),('b','loser',0,'0'),
 ('empty','one',0,'0'),('empty','',0,'0'),('other','one',0,'0'),('other','loser',0,'0')`)

	withBusinessCatalogPath(t, ctx, db, func(tx *sql.Tx) {
		_, err := tx.ExecContext(ctx, "CALL "+qualifiedFunction(schema, "proc_cg_afterzbgs")+"('notice')")
		require.NoError(t, err)
		for _, lot := range []string{"a", "b", "empty", "other"} {
			var flag int
			var hasDate bool
			err = tx.QueryRowContext(ctx, `SELECT "IsWeiZBRTuiBZJ", "WeiZBRTuiBZJDate" IS NOT NULL FROM `+lots+` WHERE "BiaoDuanGuid"=$1`, lot).Scan(&flag, &hasDate)
			require.NoError(t, err)
			if lot == "other" {
				require.Zero(t, flag)
				require.False(t, hasDate)
			} else {
				require.Equal(t, 1, flag)
				require.True(t, hasDate)
			}
		}
		for _, tc := range []struct {
			lot, bidder string
			won         int
			refund      string
		}{
			{"a", "one", 1, "0"}, {"a", "two", 1, "0"}, {"a", "loser", 0, "1"},
			{"b", "one", 0, "1"}, {"b", "loser", 1, "0"},
			{"empty", "one", 0, "0"}, {"empty", "", 0, "0"},
			{"other", "one", 0, "0"}, {"other", "loser", 0, "0"},
		} {
			var won int
			var refund string
			err = tx.QueryRowContext(ctx, `SELECT "IsZhongBiao","BZJIsBack" FROM `+bidders+
				` WHERE "BiaoDuanGuid"=$1 AND ("DanWeiGuid"=$2 OR (COALESCE(char_length($2::varchar),0)=0 AND COALESCE(char_length("DanWeiGuid"),0)=0))`,
				tc.lot, tc.bidder).Scan(&won, &refund)
			require.NoError(t, err)
			require.Equal(t, tc.won, won, tc)
			require.Equal(t, tc.refund, refund, tc)
		}
	})
	var changed int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM `+bidders+` WHERE "IsZhongBiao"<>0 OR "BZJIsBack"<>'0'`).Scan(&changed))
	require.Zero(t, changed, "caller rollback restores bidder flags")
}

func seedProcurementNoticeFixture(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	t.Helper()
	notices := qualifiedFunction(schema, "CG_ZhaoBiaoGGInfo")
	lots := qualifiedFunction(schema, "CG_BiaoDuanInfo")
	links := qualifiedFunction(schema, "CG_ZBGGAndBD")
	qualifications := qualifiedFunction(schema, "cg_zhaobiaogg_zizhireq")
	businessExec(t, ctx, db, "CREATE TABLE "+notices+`("GongGaoGuid" varchar PRIMARY KEY, "PVI_GUID" varchar)`)
	businessExec(t, ctx, db, "CREATE TABLE "+lots+`("BiaoDuanGuid" varchar PRIMARY KEY, "GongGaoIsSelected" varchar)`)
	businessExec(t, ctx, db, "CREATE TABLE "+links+`("GongGaoGuid" varchar REFERENCES `+notices+`("GongGaoGuid"), "BiaoDuanGuid" varchar REFERENCES `+lots+`("BiaoDuanGuid"))`)
	businessExec(t, ctx, db, "CREATE TABLE "+qualifications+`("GongGaoGuid" varchar REFERENCES `+notices+`("GongGaoGuid"))`)
	businessExec(t, ctx, db, "INSERT INTO "+notices+` VALUES ('notice','root'),('untouched','other'),('no-workflow',NULL)`)
	businessExec(t, ctx, db, "INSERT INTO "+lots+` VALUES ('lot-a','1'),('lot-b','1'),('lot-c','1')`)
	businessExec(t, ctx, db, "INSERT INTO "+links+` VALUES ('notice','lot-a'),('untouched','lot-b'),('no-workflow','lot-c')`)
	businessExec(t, ctx, db, "INSERT INTO "+qualifications+` VALUES ('notice'),('untouched'),('no-workflow')`)
}

func checkProcurementNoticeDelete(t *testing.T, ctx context.Context, db *sql.DB, schema, product string) {
	t.Helper()
	seedProcurementNoticeFixture(t, ctx, db, schema)
	call := "CALL " + qualifiedFunction(schema, "proc_cg_gonggaoinfo_delete") + "($1::varchar)"
	_, err := db.ExecContext(ctx, call, "notice")
	require.ErrorContains(t, err, "deleteprocessversioninstance")
	if product == "postgres" {
		businessExec(t, ctx, db, pgProcedureDDL(schema, "deleteprocessversioninstance", "IN p_guid text",
			`BEGIN RAISE EXCEPTION 'wrong workflow overload was executed'; END;`, false))
		_, err = db.ExecContext(ctx, call, "notice")
		require.ErrorContains(t, err, "deleteprocessversioninstance")
		require.NotContains(t, err.Error(), "wrong workflow overload was executed")
	}

	businessExec(t, ctx, db, call, "no-workflow")
	businessExec(t, ctx, db, call, "no-workflow")
	for _, tc := range []struct {
		guid, lot, selected string
		remaining           int
	}{
		{"notice", "lot-a", "1", 1}, {"untouched", "lot-b", "1", 1}, {"no-workflow", "lot-c", "0", 0},
	} {
		for _, table := range []string{"CG_ZhaoBiaoGGInfo", "CG_ZBGGAndBD", "cg_zhaobiaogg_zizhireq"} {
			var remaining int
			query := "SELECT count(*) FROM " + qualifiedFunction(schema, table) + ` WHERE "GongGaoGuid"=$1`
			require.NoError(t, db.QueryRowContext(ctx, query, tc.guid).Scan(&remaining))
			require.Equal(t, tc.remaining, remaining, "%s %s", table, tc.guid)
		}
		var selected string
		query := `SELECT "GongGaoIsSelected" FROM ` + qualifiedFunction(schema, "CG_BiaoDuanInfo") + ` WHERE "BiaoDuanGuid"=$1`
		require.NoError(t, db.QueryRowContext(ctx, query, tc.lot).Scan(&selected))
		require.Equal(t, tc.selected, selected, tc.lot)
	}
}
