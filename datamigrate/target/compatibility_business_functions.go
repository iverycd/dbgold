package target

import (
	"strings"

	"dbgold/datamigrate/source"
)

// These are reviewed implementations of specific business functions, not a
// translator for arbitrary source routines. Dependencies are resolved only when
// invoked, because compatibility preparation runs before tables and views exist.
func businessFunctionDDL(schema, catalog, name string) string {
	var signature, result, body, lookup string
	volatility := "VOLATILE"
	switch name {
	case "convertguid":
		signature, result, body = "(p_newguid varchar, p_instr varchar)", "varchar", convertGUIDBody
		volatility = "IMMUTABLE"
	case "convertguidlist":
		signature, result, body = "(p_newguid varchar, p_instr varchar)", "varchar", convertGUIDListBody
	case "getcategoryinfopubcount":
		signature = "(p_categorynum varchar, p_infostatuscode integer, p_startdate timestamp without time zone, p_enddate timestamp without time zone)"
		result, body = "integer", categoryInfoPubCountBody
		lookup = businessRelationLookup(name, "view_infomain", "infoid", "categorynum", "infostatuscode", "pubinwebdate")
	case "getscore":
		signature, result, body = "(p_infoid varchar)", "integer", getScoreBody
		lookup = businessRelationLookup(name, "view_infomain", "infoid", "score")
	case "getcategoryscore":
		signature, result, body = "(p_userguid varchar, p_fromdate date, p_todate date)", "integer", categoryScoreBody
		lookup = businessRelationLookup(name, "webdb_information", "infoid", "userguid", "pubinwebdate")
	case "getchildoulist":
		signature, result, body = "(p_rootid varchar)", "text", childOUListBody
		lookup = businessRelationLookup(name, "frame_ou", "ouguid", "parentouguid")
	default:
		return ""
	}
	if lookup != "" {
		body = strings.ReplaceAll(body, "{{RELATION_DECLARATIONS}}", businessRelationDeclarations)
		body = strings.ReplaceAll(body, "{{LOOKUP}}", lookup)
	}
	cat := source.QuotePGIdent(catalog)
	body = strings.NewReplacer(
		"{{CAT}}", cat,
		"{{CAT_LITERAL}}", sqlFunctionBody(cat),
		"{{SCHEMA}}", sqlFunctionBody(schema),
		"{{CONVERTGUID}}", qualifiedFunction(schema, "convertguid"),
		"{{GETSCORE_LITERAL}}", sqlFunctionBody(qualifiedFunction(schema, "getscore")),
	).Replace(body)
	return "CREATE FUNCTION " + qualifiedFunction(schema, name) + signature + " RETURNS " + result +
		" LANGUAGE plpgsql " + volatility + " SECURITY INVOKER AS " + sqlFunctionBody(body)
}

const businessRelationDeclarations = `
 relation_oid oid;
 relation_name text;
 relation_sql text;
 match_count integer;
 required_column text;
 actual_column text;
 column_sql text[] := ARRAY[]::text[];`

// Fixed relation/column names below are application constants. Actual names are
// looked up case-insensitively, then quoted; caller search_path never chooses a
// business object. Views are valid dependencies, including View_InfoMain.
func businessRelationLookup(function, relation string, columns ...string) string {
	columnList := "'" + strings.Join(columns, "','") + "'"
	return strings.NewReplacer("{{FUNCTION}}", function, "{{RELATION}}", relation, "{{COLUMNS}}", columnList).Replace(`
 SELECT {{CAT}}.count(*), {{CAT}}.min(c.oid::bigint)::oid, {{CAT}}.min(c.relname::text)
 INTO match_count, relation_oid, relation_name
 FROM {{CAT}}.pg_class c JOIN {{CAT}}.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname={{SCHEMA}} AND {{CAT}}.lower(c.relname::text)='{{RELATION}}'
 AND c.relkind IN ('r','p','v','m','f');
 IF match_count=0 THEN
  RAISE EXCEPTION '{{FUNCTION}}: {{RELATION}} is missing from the target schema';
 ELSIF match_count<>1 THEN
  RAISE EXCEPTION '{{FUNCTION}}: {{RELATION}} name is ambiguous in the target schema';
 END IF;
 relation_sql := {{CAT}}.quote_ident({{SCHEMA}})||'.'||{{CAT}}.quote_ident(relation_name);
 FOREACH required_column IN ARRAY ARRAY[{{COLUMNS}}] LOOP
  SELECT {{CAT}}.count(*), {{CAT}}.min(a.attname::text) INTO match_count, actual_column
  FROM {{CAT}}.pg_attribute a
  WHERE a.attrelid=relation_oid AND a.attnum>0 AND NOT a.attisdropped
  AND {{CAT}}.lower(a.attname::text)=required_column;
  IF match_count=0 THEN
   RAISE EXCEPTION '{{FUNCTION}}: column % is missing from {{RELATION}}', required_column;
  ELSIF match_count<>1 THEN
   RAISE EXCEPTION '{{FUNCTION}}: column % is ambiguous in {{RELATION}}', required_column;
  END IF;
  column_sql := {{CAT}}.array_append(column_sql, {{CAT}}.quote_ident(actual_column));
 END LOOP;`)
}

// MySQL LENGTH counts UTF8 bytes here, whereas CHAR_LENGTH counts characters.
// SUBSTR's nonpositive start rules differ from PostgreSQL: negative starts count
// from the untrimmed string's end, zero/out-of-range starts produce an empty value.
// RTRIM affects only length calculations; it must not trim the actual substring.
const convertGUIDBody = `DECLARE
 instr_length integer;
 start_index integer;
 take_length integer;
 prefix_value text := '';
 suffix_value text := '';
BEGIN
 IF p_instr IS NULL THEN
  RETURN '';
 END IF;
 IF p_newguid IS NULL THEN
  RETURN NULL;
 END IF;
 instr_length := {{CAT}}.char_length({{CAT}}.rtrim(p_instr));
 IF {{CAT}}.octet_length({{CAT}}.convert_to({{CAT}}.rtrim(p_instr),'UTF8'))<=12 THEN
  take_length := instr_length;
 ELSE
  take_length := 12;
  IF instr_length>12 THEN
   prefix_value := {{CAT}}.substr(p_instr,1,instr_length-12);
  END IF;
 END IF;
 start_index := {{CAT}}.char_length({{CAT}}.rtrim(p_newguid))-11;
 IF start_index<0 THEN
  start_index := {{CAT}}.char_length(p_newguid)+start_index+1;
 END IF;
 IF start_index>0 AND take_length>0 THEN
  suffix_value := {{CAT}}.substr(p_newguid,start_index,take_length);
 END IF;
 RETURN prefix_value||suffix_value;
END;`

const convertGUIDListBody = `DECLARE
 raw_value text := p_instr;
 work_value text := '';
 delimiter_index integer;
BEGIN
 delimiter_index := {{CAT}}.strpos(raw_value,';');
 WHILE delimiter_index>0 LOOP
  work_value := work_value||{{CONVERTGUID}}(p_newguid,{{CAT}}.substr(raw_value,1,delimiter_index-1)::varchar)||';';
  raw_value := {{CAT}}.substr(raw_value,delimiter_index+1);
  delimiter_index := {{CAT}}.strpos(raw_value,';');
 END LOOP;
 -- The original routine deliberately leaves the unterminated final item alone.
 RETURN work_value||raw_value;
END;`

const categoryInfoPubCountBody = `DECLARE
{{RELATION_DECLARATIONS}}
 result_value integer;
BEGIN
{{LOOKUP}}
 EXECUTE 'SELECT '||{{CAT_LITERAL}}||'.count(t.'||column_sql[1]||') FROM '||relation_sql||' AS t'
  ||' WHERE t.'||column_sql[2]||'=$1 AND t.'||column_sql[3]||'=$2'
  ||' AND t.'||column_sql[4]||' BETWEEN $3 AND $4'
 INTO result_value USING p_categorynum, p_infostatuscode, p_startdate, p_enddate;
 RETURN result_value;
END;`

const getScoreBody = `DECLARE
{{RELATION_DECLARATIONS}}
 result_value integer;
BEGIN
{{LOOKUP}}
 EXECUTE 'SELECT '||{{CAT_LITERAL}}||'.max(t.'||column_sql[2]||') FROM '||relation_sql||' AS t'
  ||' WHERE t.'||column_sql[1]||'=$1'
 INTO result_value USING p_infoid;
 RETURN coalesce(result_value,0);
END;`

const categoryScoreBody = `DECLARE
{{RELATION_DECLARATIONS}}
 result_value integer;
BEGIN
{{LOOKUP}}
 EXECUTE 'SELECT '||{{CAT_LITERAL}}||'.sum('||{{GETSCORE_LITERAL}}||'(t.'||column_sql[1]||'::varchar)) FROM '||relation_sql||' AS t'
  ||' WHERE t.'||column_sql[2]||'=$1 AND t.'||column_sql[3]||' BETWEEN $2 AND $3'
 INTO result_value USING p_userguid, p_fromdate, p_todate;
 RETURN coalesce(result_value,0);
END;`

// Track each path rather than a global visited set: overlapping roots and DAG
// branches are not cycles. Group at the minimum depth only for output, keeping
// cycle detection independent. Avoid PG 14 SEARCH/CYCLE syntax for older forks.
const childOUListBody = `DECLARE
{{RELATION_DECLARATIONS}}
 result_value text;
 found_cycle boolean;
BEGIN
 IF p_rootid IS NULL OR p_rootid='' THEN
  RETURN '';
 END IF;
{{LOOKUP}}
 EXECUTE 'WITH RECURSIVE walk(node,depth,visit_path,has_cycle) AS ('
  ||' SELECT r.node,0,ARRAY[r.node],false FROM '||{{CAT_LITERAL}}||'.unnest('
  ||{{CAT_LITERAL}}||'.string_to_array($1,'','')) AS r(node) WHERE r.node<>'''''
  ||' UNION ALL SELECT t.'||column_sql[1]||'::text,w.depth+1,'
  ||{{CAT_LITERAL}}||'.array_append(w.visit_path,t.'||column_sql[1]||'::text),'
  ||'t.'||column_sql[1]||'::text=ANY(w.visit_path) FROM '||relation_sql||' AS t'
  ||' JOIN walk w ON t.'||column_sql[2]||'::text=w.node'
  ||' WHERE NOT w.has_cycle AND t.'||column_sql[1]||' IS NOT NULL),'
  ||' nodes AS (SELECT node,'||{{CAT_LITERAL}}||'.min(depth) AS depth,'
  ||{{CAT_LITERAL}}||'.bool_or(has_cycle) AS has_cycle FROM walk GROUP BY node)'
  ||' SELECT '||{{CAT_LITERAL}}||'.bool_or(has_cycle),'
  ||{{CAT_LITERAL}}||'.string_agg(node,'','' ORDER BY depth,node) FROM nodes'
 INTO found_cycle, result_value USING p_rootid;
 IF found_cycle THEN
  RAISE EXCEPTION 'getchildoulist: cycle detected in frame_ou hierarchy';
 END IF;
 RETURN coalesce(result_value,'');
END;`
