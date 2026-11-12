package target

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

const demoDeleteTouBiaoTables = `
pingbiao_zb_danweigc
pingbiao_tb_feiyongwj
pingbiao_tb_fywjmx
pingbiao_tb_qdfb
pingbiao_tb_qingdanitem
pingbiao_tb_qingdanitemdezj
pingbiao_tb_qingdanitemrcjhl
pingbiao_tb_csxm1bt
pingbiao_tb_csxm1mx
pingbiao_tb_csxm_main
pingbiao_tb_measureitem
pingbiao_tb_measureitemdezj
pingbiao_tb_measureitemrcjhl
pingbiao_tb_otheritem
pingbiao_tb_zanliejemx
pingbiao_tb_clzgbt
pingbiao_tb_cailiaozgmx
pingbiao_tb_zhuanyegczgjmx
pingbiao_tb_jirigbt
pingbiao_tb_jirigmx
pingbiao_tb_zongchengbfwfmx
pingbiao_tb_fbrgyclmx
pingbiao_tb_resource
pingbiao_toubiao_detailbaojia
pingbiao_tb_techfile
pingbiao_tb_techcsbt
pingbiao_tb_techcsdata
pingbiao_tb_qdcsbt
pingbiao_tb_qdcsdata
pingbiao_tb_dxgcfhz
pingbiao_extend
pb_tb_baojiahistroydetail
pingbiao_baojiahistroy
pingbiao_tbinfomx
`

const demoDeleteZhaoBiaoLotTables = `
pingbiao_zb_qingdanitem_biaoti
pingbiao_zb_qingdanitem
pingbiao_zb_measureitem_main
pingbiao_zb_measureitem
pingbiao_zb_otheritem
pingbiao_zb_zanliejemx
pingbiao_zb_cailiaozgmx
pingbiao_zb_zhuanyegczgj
pingbiao_zb_jirigbt
pingbiao_zb_jirigmx
pingbiao_zb_zongchengbfwfmx
pingbiao_zb_jiagongcl
pingbiao_eval_guifeilist
pingbiao_eval_guifeiprojectset
pingbiao_zb_techcsbt
pingbiao_zb_techcsdata
pingbiao_zb_qdcsbt
pingbiao_zb_qdcsdata
pingbiao_pfd
pingbiao_zb_techfile
pingbiao_parameter
pingbiao_zb_economicpbbf
pingbiao_evak_pbdetail
pingbiao_zb_netfile
pingbiao_zbinfomx
`

func demoDeleteTouBiaoInfoDDL(schema, catalog string, native bool) string {
	return demoFixedDeletesDDL(schema, catalog, native, "deletetoubiaoinfo", nil, strings.Fields(demoDeleteTouBiaoTables), "")
}

func demoDeleteTouBiaoInfoSXFDDL(schema, catalog string, native bool) string {
	tables := strings.Replace(demoDeleteTouBiaoTables, "\npingbiao_tb_techfile\n", "\n", 1)
	extraTables := " pingbiao_qtxinxi pingbiao_tb_cbrgyclmx jsgc_pb_ecofile"
	return demoFixedDeletesDDL(schema, catalog, native, "deletetoubiaoinfo_sxf", nil, strings.Fields(tables+extraTables), "")
}

func demoDeleteZhaoBiaoInfoDDL(schema, catalog string, native bool) string {
	return demoFixedDeletesDDL(schema, catalog, native, "deletezhaobiaoinfo",
		strings.Fields(demoDeleteZhaoBiaoLotTables), []string{"pingbiao_zb_danweigc"}, "jsgc_pb_ecofile")
}

func demoFixedDeletesDDL(schema, catalog string, native bool, name string, lotTables, bidderTables []string, fixedBidderTable string) string {
	var tables []pgProcedureTable
	var statements strings.Builder
	queries := make(map[string]string)
	addDelete := func(table, bidder string) {
		columns := []string{"biaoduanguid"}
		query := "DELETE FROM {{" + table + "}} WHERE {{" + table + ".biaoduanguid}}=$1"
		parameters := "p_biaoduanguid"
		if bidder != "" {
			columns = append(columns, "danweiguid")
			query += " AND {{" + table + ".danweiguid}}=" + bidder
		}
		if bidder == "$2" {
			parameters += ", p_danweiguid"
		}
		tables = append(tables, pgProcedureTable{table, table, columns})
		key := fmt.Sprintf("DELETE_%d", len(tables))
		queries[key] = query
		fmt.Fprintf(&statements, " EXECUTE {{%s}} USING %s;\n", key, parameters)
	}
	for _, table := range bidderTables {
		addDelete(table, "$2")
	}
	for _, table := range lotTables {
		addDelete(table, "")
	}
	if fixedBidderTable != "" {
		addDelete(fixedBidderTable, "'ZB001'")
	}
	return pgBusinessProcedureDDL(schema, catalog, native, name, demoLotBidderParameters, tables, "", "", statements.String(), queries)
}

const demoDeleteLotExcludedViews = "'view_pingbiao_eval_measureitem','view_pingbiao_eval_qingdanfx','view_pb_eval_qingdanfydbmx'"

func demoDeleteBiaoDuanDDL(schema, catalog string, native bool) string {
	return demoDynamicDeletesDDL(schema, catalog, native, "proc_pingbiao_deletebiaoduan",
		"'pingbiao_biaoduan','view_pingbiao_toubiao',"+demoDeleteLotExcludedViews, false)
}

func demoDeleteTouBiaoDWDDL(schema, catalog string, native bool) string {
	return demoDynamicDeletesDDL(schema, catalog, native, "proc_pingbiao_deletetoubiaodw",
		"'pingbiao_kaibiaotoubiao','pingbiao_error',"+demoDeleteLotExcludedViews, true)
}

func demoGetClassNameDDL(schema, catalog string, native bool) string {
	return demoDynamicDeletesDDL(schema, catalog, native, "p_get_class_name", "'pingbiao_biaoduan'", false)
}

func demoDynamicDeletesDDL(schema, catalog string, native bool, name, excluded string, bidderScoped bool) string {
	parameters := demoLotParameters
	tableFilter := ""
	columnList := "'biaoduanguid'"
	deleteTail := " USING p_biaoduanguid;"
	requiredColumns := "1"
	if bidderScoped {
		parameters = demoLotBidderParameters
		tableFilter = " AND {{CAT}}.lower(c.relname::text) NOT LIKE 'temp%'"
		columnList += ",'danweiguid'"
		deleteTail = "||' AND '||{{CAT}}.quote_ident(target.danwei)||'=$2' USING p_biaoduanguid,p_danweiguid;"
		requiredColumns = "2"
	}
	body := strings.NewReplacer(
		"{{CAT}}", source.QuotePGIdent(catalog), "{{SCHEMA}}", sqlFunctionBody(schema), "{{NAME}}", name,
		"{{EXCLUDED}}", excluded, "{{TABLE_FILTER}}", tableFilter, "{{COLUMNS}}", columnList,
		"{{REQUIRED_COLUMNS}}", requiredColumns, "{{DELETE_TAIL}}", deleteTail,
		"{{CHECK_INHERITANCE}}", demoCrossSchemaDescendants("target.relation_oid", "target.schema_oid", "{{CAT}}"),
	).Replace(demoDynamicDeletesBody)
	// Replacer does not process placeholders introduced by a replacement.
	body = strings.ReplaceAll(body, "{{CAT}}", source.QuotePGIdent(catalog))
	return pgProcedureDDL(schema, name, parameters, body, native)
}

const demoDynamicDeletesBody = `DECLARE
 target record;
 relation_sql text;
BEGIN
 FOR target IN
  SELECT c.oid AS relation_oid,c.relnamespace AS schema_oid,c.relname::text AS relation_name,c.relkind::text AS relation_kind,
   {{CAT}}.min(CASE WHEN {{CAT}}.lower(a.attname::text)='biaoduanguid' THEN a.attname::text END) AS lot,
   {{CAT}}.min(CASE WHEN {{CAT}}.lower(a.attname::text)='danweiguid' THEN a.attname::text END) AS danwei,
   {{CAT}}.count(*) AS column_count
  FROM {{CAT}}.pg_class c
  JOIN {{CAT}}.pg_namespace n ON n.oid=c.relnamespace
  JOIN {{CAT}}.pg_attribute a ON a.attrelid=c.oid
  WHERE n.nspname={{SCHEMA}} AND c.relkind IN ('r','p')
   AND NOT EXISTS (
    SELECT 1 FROM {{CAT}}.pg_inherits i JOIN {{CAT}}.pg_class parent ON parent.oid=i.inhparent
    WHERE i.inhrelid=c.oid AND parent.relkind='p'
   )
   AND {{CAT}}.lower(c.relname::text) NOT IN ({{EXCLUDED}}){{TABLE_FILTER}}
   AND a.attnum>0 AND NOT a.attisdropped AND {{CAT}}.lower(a.attname::text) IN ({{COLUMNS}})
  GROUP BY c.oid,c.relnamespace,c.relname,c.relkind
  ORDER BY c.relname
 LOOP
  IF target.lot IS NULL OR ({{REQUIRED_COLUMNS}}=2 AND target.danwei IS NULL) THEN
   CONTINUE;
  END IF;
  IF target.column_count<>{{REQUIRED_COLUMNS}} THEN
   RAISE EXCEPTION '{{NAME}}: GUID columns are ambiguous in %',target.relation_name;
  END IF;
  relation_sql := {{CAT}}.quote_ident({{SCHEMA}})||'.'||{{CAT}}.quote_ident(target.relation_name);
  EXECUTE 'LOCK TABLE ONLY '||relation_sql||' IN SHARE ROW EXCLUSIVE MODE';
  IF target.relation_kind='p' THEN
   IF EXISTS ({{CHECK_INHERITANCE}}) THEN
    RAISE EXCEPTION '{{NAME}}: table % has partitions outside the target schema',target.relation_name;
   END IF;
  ELSE
   relation_sql := 'ONLY '||relation_sql;
  END IF;
  EXECUTE 'DELETE FROM '||relation_sql||' WHERE '||{{CAT}}.quote_ident(target.lot)||'=$1'{{DELETE_TAIL}}
 END LOOP;
END;`

func demoCrossSchemaDescendants(relation, schemaOID, catalog string) string {
	return strings.NewReplacer("{{RELATION}}", relation, "{{SCHEMA_OID}}", schemaOID, "{{CATALOG}}", catalog).Replace(`
 WITH RECURSIVE descendants(oid) AS (
  SELECT inhrelid FROM {{CATALOG}}.pg_inherits WHERE inhparent={{RELATION}}
  UNION
  SELECT i.inhrelid FROM {{CATALOG}}.pg_inherits i JOIN descendants d ON i.inhparent=d.oid
 )
 SELECT 1 FROM descendants d JOIN {{CATALOG}}.pg_class c ON c.oid=d.oid WHERE c.relnamespace<>{{SCHEMA_OID}}
`)
}
