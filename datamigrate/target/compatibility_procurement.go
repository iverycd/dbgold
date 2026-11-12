package target

import "strings"

const procurementProcedureParameters = "IN p_gonggaoguid varchar"

func procCGAfterZBGSDDL(schema, catalog string, native bool) string {
	tables := []pgProcedureTable{
		{"lot", "cg_biaoduaninfo", strings.Fields("biaoduanguid isweizbrtuibzj weizbrtuibzjdate")},
		{"link", "cg_zbgsandbd", strings.Fields("gonggaoguid biaoduanguid")},
		{"award", "cg_zhongbiaodetail", strings.Fields("biaoduanguid danweiguid")},
		{"bidder", "cg_toubiaodanwei", strings.Fields("biaoduanguid danweiguid iszhongbiao bzjisback")},
	}
	return pgBusinessProcedureDDL(schema, catalog, native, "proc_cg_afterzbgs", procurementProcedureParameters, tables, "", "", `
 EXECUTE {{MARK_LOTS}} USING p_gonggaoguid;
 EXECUTE {{MARK_WINNERS}} USING p_gonggaoguid;
 EXECUTE {{MARK_REFUNDS}} USING p_gonggaoguid;
`, map[string]string{
		"MARK_LOTS": `UPDATE {{lot}} AS lot
 SET {{lot.isweizbrtuibzj}}='1', {{lot.weizbrtuibzjdate}}={{catalog}}.statement_timestamp()
 WHERE EXISTS (
  SELECT 1 FROM {{link}} AS link WHERE link.{{link.gonggaoguid}}=$1
  AND link.{{link.biaoduanguid}}=lot.{{lot.biaoduanguid}}
 )`,
		"MARK_WINNERS": `UPDATE {{bidder}} AS bidder SET {{bidder.iszhongbiao}}='1'
 WHERE {{catalog}}.char_length(bidder.{{bidder.danweiguid}}::text)>0 AND EXISTS (
  SELECT 1 FROM {{award}} AS award
  JOIN {{link}} AS link ON link.{{link.biaoduanguid}}=award.{{award.biaoduanguid}}
  WHERE link.{{link.gonggaoguid}}=$1 AND award.{{award.biaoduanguid}}=bidder.{{bidder.biaoduanguid}}
  AND award.{{award.danweiguid}}=bidder.{{bidder.danweiguid}}
 )`,
		// A bidder can win one lot and qualify for a refund on another. A
		// second winner on the same lot does not make either winner eligible.
		"MARK_REFUNDS": `UPDATE {{bidder}} AS bidder SET {{bidder.bzjisback}}='1'
 WHERE {{catalog}}.char_length(bidder.{{bidder.danweiguid}}::text)>0
 AND EXISTS (
  SELECT 1 FROM {{award}} AS award
  JOIN {{link}} AS link ON link.{{link.biaoduanguid}}=award.{{award.biaoduanguid}}
  WHERE link.{{link.gonggaoguid}}=$1 AND award.{{award.biaoduanguid}}=bidder.{{bidder.biaoduanguid}}
  AND {{catalog}}.char_length(award.{{award.danweiguid}}::text)>0
 )
 AND NOT EXISTS (
  SELECT 1 FROM {{award}} AS award WHERE award.{{award.biaoduanguid}}=bidder.{{bidder.biaoduanguid}}
  AND award.{{award.danweiguid}}=bidder.{{bidder.danweiguid}}
 )`,
	})
}

func procCGGongGaoInfoDeleteDDL(schema, catalog string, native bool) string {
	tables := []pgProcedureTable{
		{"notice", "cg_zhaobiaogginfo", strings.Fields("gonggaoguid pvi_guid")},
		{"lot", "cg_biaoduaninfo", strings.Fields("biaoduanguid gonggaoisselected")},
		{"link", "cg_zbggandbd", strings.Fields("gonggaoguid biaoduanguid")},
		{"qualification", "cg_zhaobiaogg_zizhireq", []string{"gonggaoguid"}},
	}
	return pgBusinessProcedureDDL(schema, catalog, native, "proc_cg_gonggaoinfo_delete", procurementProcedureParameters, tables, "", ` pvi_guid varchar;
 has_workflow_procedure boolean;
`, `
 BEGIN
  EXECUTE {{NOTICE_WORKFLOW}} INTO STRICT pvi_guid USING p_gonggaoguid;
 EXCEPTION WHEN NO_DATA_FOUND THEN
  pvi_guid := NULL;
 END;
 EXECUTE {{RESET_LOTS}} USING p_gonggaoguid;
 EXECUTE {{DELETE_LINKS}} USING p_gonggaoguid;
 EXECUTE {{DELETE_QUALIFICATIONS}} USING p_gonggaoguid;
 EXECUTE {{DELETE_NOTICE}} USING p_gonggaoguid;
 IF {{catalog}}.char_length(pvi_guid)>0 THEN
  EXECUTE {{CHECK_WORKFLOW}} INTO has_workflow_procedure;
  IF NOT has_workflow_procedure THEN
   RAISE EXCEPTION 'proc_cg_gonggaoinfo_delete: expected deleteprocessversioninstance(varchar) with one input parameter in the target schema';
  END IF;
  CALL {{schema}}."deleteprocessversioninstance"(pvi_guid);
 END IF;
`, map[string]string{
		"NOTICE_WORKFLOW": `SELECT notice.{{notice.pvi_guid}} FROM {{notice}} AS notice
 WHERE notice.{{notice.gonggaoguid}}=$1 FOR UPDATE`,
		"RESET_LOTS": `UPDATE {{lot}} AS lot SET {{lot.gonggaoisselected}}='0'
 WHERE EXISTS (
  SELECT 1 FROM {{link}} AS link WHERE link.{{link.gonggaoguid}}=$1
  AND link.{{link.biaoduanguid}}=lot.{{lot.biaoduanguid}}
 )`,
		"DELETE_LINKS":          `DELETE FROM {{link}} WHERE {{link.gonggaoguid}}=$1`,
		"DELETE_QUALIFICATIONS": `DELETE FROM {{qualification}} WHERE {{qualification.gonggaoguid}}=$1`,
		"DELETE_NOTICE":         `DELETE FROM {{notice}} WHERE {{notice.gonggaoguid}}=$1`,
		// CALL can coerce varchar to a text overload. Require the exact
		// procedure identity before letting it delete the referenced workflow.
		"CHECK_WORKFLOW": `SELECT EXISTS (
 SELECT 1 FROM {{catalog}}.pg_proc AS p
 WHERE p.oid=({{catalog}}.quote_ident({{schema_name}})||'."deleteprocessversioninstance"(character varying)')::regprocedure::oid
 AND p.prokind='p' AND p.provariadic=0
 AND (p.proargmodes IS NULL OR {{catalog}}.array_to_string(p.proargmodes,',')='i')
)`,
	})
}
