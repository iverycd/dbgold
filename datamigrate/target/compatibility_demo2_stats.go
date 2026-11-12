package target

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

const demo2StatsDateArguments = "p_weekstartdate timestamp without time zone, p_weekenddate timestamp without time zone, p_monthstartdate timestamp without time zone, p_monthenddate timestamp without time zone"

var demo2ExtraScoreSpec = compatibilityFunctionSpec{
	name: "getextrascore", signature: "getextrascore(p_userguid varchar, p_fromdate date, p_todate date)",
	inputs: []string{"character varying", "date", "date"},
}

var demo2PAInfoCountSpec = compatibilityFunctionSpec{
	name: "getpainfocount", signature: "getpainfocount(p_userguid varchar, p_ouguid varchar, p_categorynum varchar, p_limittype integer, p_dtype integer, " + demo2StatsDateArguments + ")",
	inputs: []string{"character varying", "character varying", "character varying", "integer", "integer", "timestamp without time zone", "timestamp without time zone", "timestamp without time zone", "timestamp without time zone"},
}

var demo2CompatibilityFunctionSpecs = []compatibilityFunctionSpec{demo2ExtraScoreSpec, demo2PAInfoCountSpec}

const demo2PAStatArguments = "p_userguids text, p_ouguids text, " + demo2StatsDateArguments + ", INOUT p_result refcursor DEFAULT NULL"

var demo2PAStatProcedureSpec = compatibilityProcedureSpec{
	name: "pastat", signature: "pastat(" + demo2PAStatArguments + ")",
	inputs: []string{"text", "text", "timestamp without time zone", "timestamp without time zone", "timestamp without time zone", "timestamp without time zone", "refcursor"},
	modes:  "i,i,i,i,i,i,b", minDefaults: 1, maxDefaults: 1,
	ddl: func(schema, catalog string, native bool) string { return demo2PAStatDDL(schema, catalog, native, true) },
}

var demo2PAInfoTable = pgProcedureTable{"info", "view_infomain", []string{"infoid", "categorynum", "ouguid", "userguid", "pubinwebdate"}}

func demo2FunctionDDL(schema, catalog, name string) string {
	var spec compatibilityFunctionSpec
	var body string
	switch name {
	case demo2ExtraScoreSpec.name:
		spec = demo2ExtraScoreSpec
		tables := []pgProcedureTable{{"score", "webdb_pa_score", []string{"score", "userguid", "postdate"}}}
		// Compare the column to the argument. The unqualified MySQL source
		// compares its UserGuid parameter to itself and includes other users.
		const query = `SELECT COALESCE({{catalog}}.sum(s.{{score.score}}::numeric),0)::integer
 FROM {{score}} AS s WHERE s.{{score.userguid}}=$1 AND s.{{score.postdate}} BETWEEN $2 AND $3`
		body = demoRoutineBody(schema, catalog, name, tables, " result_value integer;", `
 EXECUTE {{QUERY}} INTO result_value USING p_userguid,p_fromdate,p_todate;
 RETURN result_value;`, map[string]string{"QUERY": query})
	case demo2PAInfoCountSpec.name:
		spec = demo2PAInfoCountSpec
		body = demo2PAInfoCountBody(schema, catalog)
	default:
		return ""
	}
	return "CREATE FUNCTION " + qualifiedFunction(schema, name) + strings.TrimPrefix(spec.signature, name) +
		" RETURNS integer LANGUAGE plpgsql STABLE SECURITY INVOKER AS " + sqlFunctionBody(body)
}

// Keep the WHERE clause identical when PAStat must inline the source routine.
// The arguments are fixed SQL expressions with values bound by EXECUTE USING.
func demo2PAInfoCountQuery(user, ou, category, limit, start, end string) string {
	return fmt.Sprintf(`SELECT {{catalog}}.count(i.{{info.infoid}})::integer FROM {{info}} AS i
 WHERE %s IN (0,2,3) AND {{catalog}}.strpos(i.{{info.categorynum}}::text,%s::text)=1
 AND i.{{info.userguid}}=%s AND i.{{info.ouguid}}=%s
 AND i.{{info.pubinwebdate}} BETWEEN %s AND %s`, limit, category, user, ou, start, end)
}

func demo2PAInfoCountBody(schema, catalog string) string {
	_, _, identifiers := pgProcedureIdentifierBindings([]pgProcedureTable{demo2PAInfoTable})
	identifiers["info"] = "relation_sql"
	quotedCatalog := source.QuotePGIdent(catalog)
	identifiers["catalog"] = sqlFunctionBody(quotedCatalog)
	lookup := businessRelationLookup(demo2PAInfoCountSpec.name, demo2PAInfoTable.name, demo2PAInfoTable.columns...)
	lookup = strings.NewReplacer("{{CAT}}", quotedCatalog, "{{SCHEMA}}", sqlFunctionBody(schema)).Replace(lookup)
	query := pgProcedureSQL(demo2PAInfoCountQuery("$1", "$2", "$3", "$4", "$5", "$6"), identifiers)
	return "DECLARE\n" + businessRelationDeclarations + `
 result_value integer;
BEGIN
 IF p_limittype IS NULL OR p_limittype NOT IN (0,2,3) THEN
  RETURN 0;
 END IF;
` + lookup + `
 EXECUTE ` + query + ` INTO result_value USING p_userguid,p_ouguid,p_categorynum,p_limittype,
  CASE WHEN p_dtype=1 THEN p_weekstartdate ELSE p_monthstartdate END,
  CASE WHEN p_dtype=1 THEN p_weekenddate ELSE p_monthenddate END;
 RETURN result_value;
END;`
}

func demo2PAStatDDL(schema, catalog string, native, hasCount bool) string {
	tables := []pgProcedureTable{
		{"cfg", "webdb_pa_count", []string{"categorynum", "dtype", "mincount", "limittype", "belongto"}},
		{"usr", "frame_user", []string{"userguid", "displayname"}},
		{"cat", "webdb_category", []string{"categorynum", "categoryname"}},
	}
	pubCount := `{{schema}}."getpainfocount"($1::varchar,$2::varchar,c.{{cfg.categorynum}}::varchar,
  c.{{cfg.limittype}}::integer,c.{{cfg.dtype}}::integer,$4,$5,$6,$7)`
	if !hasCount {
		tables = append(tables, demo2PAInfoTable)
		start := "CASE WHEN c.{{cfg.dtype}}::integer=1 THEN $4 ELSE $6 END"
		end := "CASE WHEN c.{{cfg.dtype}}::integer=1 THEN $5 ELSE $7 END"
		pubCount = "(" + demo2PAInfoCountQuery("$1", "$2", "c.{{cfg.categorynum}}", "c.{{cfg.limittype}}::integer", start, end) + ")"
	}
	queries := map[string]string{
		"INSERT_ROWS": `INSERT INTO pg_temp.temp_tb (sysid,userguid,ouguid,categorynum,dtype,mincount,pubcount)
 SELECT $3,$1,$2,c.{{cfg.categorynum}},c.{{cfg.dtype}}::integer,c.{{cfg.mincount}}::integer,` + pubCount + `
 FROM {{cfg}} AS c WHERE c.{{cfg.belongto}}='All' OR c.{{cfg.belongto}}=$1`,
		"UPDATE_NAMES": `UPDATE pg_temp.temp_tb AS t SET
 displayname=(SELECT u.{{usr.displayname}} FROM {{usr}} AS u WHERE u.{{usr.userguid}}=t.userguid),
 categoryname=(SELECT c.{{cat.categoryname}} FROM {{cat}} AS c WHERE c.{{cat.categorynum}}=t.categorynum)`,
	}
	const declarations = ` remaining_users text := p_userguids;
 remaining_ous text := p_ouguids;
 current_userguid text;
 current_ouguid text;
 user_split integer;
 ou_split integer;
 row_index integer := 1;
`
	// The source processes only semicolon-terminated user GUIDs. A non-NULL
	// OU list with no next delimiter supplies an empty OU for that user.
	const statementsTemplate = ` DROP TABLE IF EXISTS pg_temp.temp_tb;
 CREATE TEMP TABLE pg_temp.temp_tb (
  sysid integer, displayname varchar(50), userguid varchar(50), ouguid varchar(50),
  categorynum varchar(50), categoryname varchar(100), dtype integer, mincount integer, pubcount integer
 );
 user_split := {{CAT}}.strpos(remaining_users,';');
 WHILE user_split>0 LOOP
  current_userguid := {{CAT}}.left(remaining_users,user_split-1);
  remaining_users := {{CAT}}.substr(remaining_users,user_split+1);
  ou_split := {{CAT}}.strpos(remaining_ous,';');
  current_ouguid := {{CAT}}.left(remaining_ous,greatest(ou_split-1,0));
  remaining_ous := {{CAT}}.substr(remaining_ous,ou_split+1);
  EXECUTE {{INSERT_ROWS}} USING current_userguid,current_ouguid,row_index,
   p_weekstartdate,p_weekenddate,p_monthstartdate,p_monthenddate;
  row_index := row_index+1;
  user_split := {{CAT}}.strpos(remaining_users,';');
 END LOOP;
 EXECUTE {{UPDATE_NAMES}};
 OPEN p_result FOR SELECT sysid AS "SYSID", displayname AS "DISPLAYNAME", userguid AS "USERGUID",
  ouguid AS "OUGUID", categorynum AS "CATEGORYNUM", categoryname AS "CATEGORYNAME",
  dtype AS "DTYPE", mincount AS "MINCOUNT", pubcount AS "PUBCOUNT" FROM pg_temp.temp_tb;
`
	statements := strings.ReplaceAll(statementsTemplate, "{{CAT}}", source.QuotePGIdent(catalog))
	if hasCount {
		return pgBusinessProcedureDDL(schema, catalog, native, "pastat", demo2PAStatArguments, tables, "", declarations, statements, queries)
	}
	// View_InfoMain can be an actual view. Use the readable relation resolver.
	body := demoRoutineBody(schema, catalog, "pastat", tables, declarations, statements, queries)
	return pgProcedureDDL(schema, "pastat", demo2PAStatArguments, body, native)
}
