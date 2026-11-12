package target

import "strings"

func demoCalcBJInfoDDL(schema, catalog string, native bool) string {
	tables := []pgProcedureTable{
		{"detail", "pb_tb_baojiahistroydetail", strings.Fields("bjguid totalprice unitprice geshu jiegou")},
		{"history", "pingbiao_baojiahistroy", strings.Fields("bjguid bjtotal finalbaojia")},
	}
	return pgBusinessProcedureDDL(schema, catalog, native, "proc_pingbiao_calcbjinfo", "IN p_bjguid varchar", tables, "", "", `
 EXECUTE {{LINE_TOTALS}} USING p_bjguid;
 EXECUTE {{GROUP_TOTALS}} USING p_bjguid;
 EXECUTE {{HISTORY_TOTAL}} USING p_bjguid;
`, map[string]string{
		"LINE_TOTALS": `UPDATE {{detail}}
 SET {{detail.totalprice}}=coalesce({{detail.unitprice}}::numeric,0)*coalesce({{detail.geshu}}::numeric,0)
 WHERE {{detail.bjguid}}=$1`,
		// MySQL CONCAT_WS(parent.JieGou,'.%') has a single value argument.
		// The parent JieGou is the unused separator, so the pattern is '.%'.
		"GROUP_TOTALS": `UPDATE {{detail}} AS parent SET {{detail.totalprice}}=coalesce((
 SELECT {{catalog}}.sum(coalesce(child.{{detail.totalprice}}::numeric,0)) FROM {{detail}} AS child
 WHERE child.{{detail.jiegou}}::text LIKE '.%' AND child.{{detail.bjguid}}=$1
 ),parent.{{detail.totalprice}}::numeric)
 WHERE parent.{{detail.bjguid}}=$1 AND {{catalog}}.strpos(parent.{{detail.jiegou}}::text,'.')=0`,
		"HISTORY_TOTAL": `UPDATE {{history}} AS h SET {{history.bjtotal}}=totals.amount,{{history.finalbaojia}}=totals.amount
 FROM (SELECT {{catalog}}.sum(coalesce({{detail.totalprice}}::numeric,0)) AS amount FROM {{detail}}
 WHERE {{catalog}}.strpos({{detail.jiegou}}::text,'.')=0 AND {{detail.bjguid}}=$1) AS totals
 WHERE h.{{history.bjguid}}=$1`,
	})
}

func demoGetBJDetailsDDL(schema, catalog string, native bool) string {
	tables := []pgProcedureTable{
		{"lot", "pingbiao_biaoduan", strings.Fields("biaoduanguid baojiaround")},
		{"bidder", "pingbiao_kaibiaotoubiao", strings.Fields("rowguid danweiguid upr_date biaoduanguid toubiaototal danweiname iscanyupb")},
		{"history", "pingbiao_baojiahistroy", strings.Fields("bjguid bjnum danweiguid biaoduanguid bjtotal finalbaojia status isqianzhang attachconname bjr_name bjr_date danweiname")},
	}
	return pgBusinessProcedureDDLWithDeferredTables(schema, catalog, native, "pro_getbjdetails", demoLotParameters+", INOUT p_result refcursor DEFAULT NULL",
		tables, "", " max_round integer;\n", `
 BEGIN
  EXECUTE {{ROUND_COUNT}} INTO STRICT max_round USING p_biaoduanguid;
 EXCEPTION WHEN no_data_found THEN
  max_round := NULL;
 END;
 IF max_round>1 THEN
  {{RESOLVE_DEFERRED_TABLES}}
  OPEN p_result FOR EXECUTE {{MULTI_ROUND}} USING p_biaoduanguid,max_round;
 ELSE
  OPEN p_result FOR EXECUTE {{SINGLE_ROUND}} USING p_biaoduanguid;
 END IF;
`, map[string]string{
			"ROUND_COUNT": `SELECT coalesce({{lot.baojiaround}}::integer,1) FROM {{lot}} WHERE {{lot.biaoduanguid}}=$1`,
			"SINGLE_ROUND": `SELECT 1 AS "BJNum",b.{{bidder.rowguid}} AS "BJGuid",b.{{bidder.danweiguid}} AS "DanWeiGuid",
 '标书报价'::varchar AS "BJR_Name",b.{{bidder.upr_date}} AS "BJR_Date",b.{{bidder.biaoduanguid}} AS "BiaoDuanGuid",
 b.{{bidder.toubiaototal}} AS "BJTotal",b.{{bidder.toubiaototal}} AS "FinalBaoJia",b.{{bidder.danweiname}} AS "DanWeiName",
 '1'::varchar AS "Status",'1'::varchar AS "IsQianZhang",''::varchar AS "AttachGuid",''::varchar AS "AttachConName"
 FROM {{bidder}} AS b WHERE b.{{bidder.biaoduanguid}}=$1`,
			"MULTI_ROUND": demoBJDetailsMultipleRounds,
		}, 1)
}

// At every round, use that bidder's latest submitted quote or carry forward
// the initial quote. The history row's null values must also carry forward.
const demoBJDetailsMultipleRounds = `SELECT round.number AS "BJNum",
 (CASE WHEN h.bjnum IS NULL THEN b.{{bidder.rowguid}}::varchar ELSE h.bjguid END)::varchar(50) AS "BJGuid",
 b.{{bidder.danweiguid}}::varchar(50) AS "DanWeiGuid",
 (CASE WHEN h.bjnum IS NULL THEN '标书报价' ELSE h.bidder_name END)::varchar(256) AS "BJR_Name",
 CASE WHEN h.bjnum IS NULL THEN {{catalog}}.date_trunc('second',b.{{bidder.upr_date}}::timestamp) ELSE h.bid_date END AS "BJR_Date",
 b.{{bidder.biaoduanguid}}::varchar(50) AS "BiaoDuanGuid",
 (CASE WHEN h.bjnum IS NULL THEN coalesce(b.{{bidder.toubiaototal}}::numeric,0) ELSE h.amount END)::numeric(18,2) AS "BJTotal",
 (CASE WHEN h.bjnum IS NULL THEN coalesce(b.{{bidder.toubiaototal}}::numeric,0) ELSE h.final_amount END)::numeric(18,2) AS "FinalBaoJia",
 (CASE WHEN h.bjnum IS NULL THEN b.{{bidder.danweiname}} ELSE h.company_name END)::varchar(50) AS "DanWeiName",
 (CASE WHEN h.bjnum IS NULL THEN '1' ELSE h.signed END)::varchar(50) AS "IsQianZhang",
 (CASE WHEN h.bjnum IS NULL THEN '1' ELSE h.bid_status END)::varchar(50) AS "Status",
 (CASE WHEN h.bjnum IS NULL THEN '' ELSE h.bjguid END)::varchar(50) AS "AttachGuid",
 (CASE WHEN h.bjnum IS NULL THEN '' ELSE h.attachment_name END)::varchar(256) AS "AttachConName"
 FROM {{bidder}} AS b CROSS JOIN {{catalog}}.generate_series(1,$2) AS round(number)
 LEFT JOIN LATERAL (
  SELECT bh.{{history.bjnum}} AS bjnum,bh.{{history.bjguid}}::varchar AS bjguid,
   bh.{{history.bjr_name}} AS bidder_name,{{catalog}}.date_trunc('second',bh.{{history.bjr_date}}::timestamp) AS bid_date,
   CASE WHEN bh.{{history.bjtotal}}::text !~ '[^0-9.]' THEN coalesce(nullif(bh.{{history.bjtotal}}::text,'')::numeric,0) ELSE 0 END AS amount,
   CASE WHEN bh.{{history.finalbaojia}}::text !~ '[^0-9.]' THEN coalesce(nullif(bh.{{history.finalbaojia}}::text,'')::numeric,0) ELSE 0 END AS final_amount,
   bh.{{history.danweiname}} AS company_name,bh.{{history.isqianzhang}}::text AS signed,
   bh.{{history.status}}::text AS bid_status,bh.{{history.attachconname}} AS attachment_name
  FROM {{history}} AS bh
  WHERE bh.{{history.biaoduanguid}}=$1 AND bh.{{history.danweiguid}}=b.{{bidder.danweiguid}}
   AND bh.{{history.bjnum}}::integer BETWEEN 2 AND round.number AND bh.{{history.status}}::text='1'
  ORDER BY bh.{{history.bjnum}}::integer DESC,bh.{{history.bjguid}} LIMIT 1
 ) AS h ON true
 WHERE b.{{bidder.biaoduanguid}}=$1 AND b.{{bidder.iscanyupb}}::text='1'
 ORDER BY "BJTotal" ASC NULLS FIRST`
