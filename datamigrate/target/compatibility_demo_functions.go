package target

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

const (
	demoScoreArguments     = "p_biaoduanguid varchar, p_danweiguid varchar, p_pingfentype varchar"
	demoDeviationArguments = "p_biaoduanguid varchar, p_pianlilv numeric"
	demoQingDanArguments   = "p_biaoduanguid varchar, p_qingdanno varchar, p_danweiguid varchar, p_danweigcno varchar"
	demoQingDanProcedure   = "getqingdanrcjgl_proc"
	demoVarchar            = "character varying"
)

var demoCompatibilityFunctionSpecs = []compatibilityFunctionSpec{
	{name: "fgettablecolsname", signature: "fgettablecolsname(p_tablename varchar)", inputs: []string{demoVarchar}},
	{name: "getavg_pwdf", signature: "getavg_pwdf(" + demoScoreArguments + ")", inputs: []string{demoVarchar, demoVarchar, demoVarchar}},
	{name: "getavg_pwdf2", signature: "getavg_pwdf2(" + demoScoreArguments + ")", inputs: []string{demoVarchar, demoVarchar, demoVarchar}},
	{name: "getavg_pwdf3", signature: "getavg_pwdf3(" + demoScoreArguments + ", p_techpwcount integer)", inputs: []string{demoVarchar, demoVarchar, demoVarchar, "integer"}},
	{name: "getavg_pwdf4", signature: "getavg_pwdf4(" + demoScoreArguments + ", p_pingweiguid varchar)", inputs: []string{demoVarchar, demoVarchar, demoVarchar, demoVarchar}},
	{name: "getavg_pwpm", signature: "getavg_pwpm(" + demoScoreArguments + ")", inputs: []string{demoVarchar, demoVarchar, demoVarchar}},
	{name: "getavg_pwpm2", signature: "getavg_pwpm2(" + demoScoreArguments + ")", inputs: []string{demoVarchar, demoVarchar, demoVarchar}},
	{name: "getchael", signature: "getchael(p_value1 numeric, p_value2 numeric)", inputs: []string{"numeric", "numeric"}},
	{name: "getcidxgcje", signature: "getcidxgcje(p_biaoduanguid varchar, p_danweigcno varchar, p_mc varchar)", inputs: []string{demoVarchar, demoVarchar, demoVarchar}},
	{name: "getpingweidfkhjg", signature: "getpingweidfkhjg(" + demoDeviationArguments + ")", inputs: []string{demoVarchar, "numeric"}},
	{name: "getpingweidfkhjieguo", signature: "getpingweidfkhjieguo(" + demoDeviationArguments + ")", inputs: []string{demoVarchar, "numeric"}},
	{name: "getpingweipmkhjg", signature: "getpingweipmkhjg(" + demoDeviationArguments + ")", inputs: []string{demoVarchar, "numeric"}},
	{name: "getpingweipmkhjieguo", signature: "getpingweipmkhjieguo(" + demoDeviationArguments + ")", inputs: []string{demoVarchar, "numeric"}},
	{name: "getqingdanrcjgl", signature: "getqingdanrcjgl(" + demoQingDanArguments + ")", inputs: []string{demoVarchar, demoVarchar, demoVarchar, demoVarchar}},
	{name: "getwbsseg", signature: "getwbsseg(p_wbs varchar, p_seg integer)", inputs: []string{demoVarchar, "integer"}},
	{name: "ifkong", signature: "ifkong(p_canshu varchar)", inputs: []string{demoVarchar}},
}

func demoFunctionDDL(schema, catalog, name, product string) string {
	var signature string
	for _, spec := range demoCompatibilityFunctionSpecs {
		if spec.name == name {
			signature = spec.signature
			break
		}
	}
	if signature == "" {
		return ""
	}

	var result, body string
	volatility := "STABLE"
	quotedCatalog := source.QuotePGIdent(catalog)
	replaceIdentifiers := strings.NewReplacer(
		"{{CAT}}", quotedCatalog, "{{SCHEMA}}", sqlFunctionBody(schema),
		"{{STATEMENT_DATE}}", getDateExpression(quotedCatalog, product),
	)
	switch name {
	case "fgettablecolsname":
		result, body = "text", replaceIdentifiers.Replace(demoTableColsNameBody)
	case "getavg_pwdf", "getavg_pwdf2", "getavg_pwdf3", "getavg_pwdf4", "getavg_pwpm", "getavg_pwpm2":
		result, body = "numeric", demoAverageScoreBody(schema, catalog, name)
	case "getchael":
		result, body, volatility = "numeric", demoChaELBody, "IMMUTABLE"
	case "getcidxgcje":
		result, body = "numeric", demoSecondPriceBody(schema, catalog)
	case "getpingweidfkhjg", "getpingweidfkhjieguo", "getpingweipmkhjg", "getpingweipmkhjieguo":
		result, body = demoDeviationBody(schema, catalog, name)
	case "getqingdanrcjgl":
		result, body, volatility = "integer", demoQingDanBody(schema, catalog, name, " RETURN 0;"), "VOLATILE"
	case "getwbsseg":
		result, body, volatility = "integer", replaceIdentifiers.Replace(demoWBSSegBody), "IMMUTABLE"
	case "ifkong":
		result, body = "timestamp without time zone", replaceIdentifiers.Replace(demoIfKongBody)
	}
	if result == "" {
		return ""
	}
	return "CREATE FUNCTION " + qualifiedFunction(schema, name) + strings.TrimPrefix(signature, name) + " RETURNS " + result +
		" LANGUAGE plpgsql " + volatility + " SECURITY INVOKER AS " + sqlFunctionBody(body)
}

// Resolve fixed business identifiers at invocation. This uses the same catalog
// bindings as the existing procedures and also permits readable relations.
func demoRoutineBody(schema, catalog, name string, tables []pgProcedureTable, declarations, statements string, queries map[string]string) string {
	tableArray, columnArray, identifiers := pgProcedureIdentifierBindings(tables)
	quotedCatalog := source.QuotePGIdent(catalog)
	identifiers["catalog"] = sqlFunctionBody(quotedCatalog)
	resolver := strings.NewReplacer(
		"{{TABLES}}", tableArray, "{{COLUMNS}}", columnArray, "{{CAT}}", quotedCatalog,
		"{{SCHEMA}}", sqlFunctionBody(schema), "{{PROCEDURE}}", name,
		"('r','p')", "('r','p','v','m','f')",
	)
	replacements := make([]string, 0, 2*len(queries))
	for key, query := range queries {
		replacements = append(replacements, "{{"+key+"}}", pgProcedureSQL(query, identifiers))
	}
	return "DECLARE\n" + resolver.Replace(pgProcedureIdentifierDeclarations) + declarations + "\nBEGIN\n" +
		resolver.Replace(pgProcedureResolveIdentifiers) + strings.NewReplacer(replacements...).Replace(statements) + "\nEND;"
}

const demoTableColsNameBody = `DECLARE
 matched_table text;
 match_count integer;
 column_names text;
BEGIN
 SELECT {{CAT}}.min(c.table_name::text),{{CAT}}.count(DISTINCT c.table_name)
 INTO matched_table,match_count
 FROM information_schema.columns AS c
 WHERE c.table_schema={{SCHEMA}} AND {{CAT}}.lower(c.table_name::text)={{CAT}}.lower(p_tablename::text);
 IF match_count>1 THEN
  RAISE EXCEPTION 'fgettablecolsname: table % is ambiguous in the target schema',p_tablename;
 END IF;
 SELECT {{CAT}}.string_agg(c.column_name::text,',' ORDER BY c.ordinal_position) INTO column_names
 FROM information_schema.columns AS c
 WHERE c.table_schema={{SCHEMA}} AND c.table_name=matched_table;
 RETURN column_names;
END;`

const demoChaELBody = `DECLARE
 value1 numeric(18,4) := p_value1;
 value2 numeric(18,4) := p_value2;
BEGIN
 IF value2=0 THEN
  RETURN 0::numeric(18,4);
 END IF;
 RETURN (value1/value2)::numeric(18,4);
END;`

const demoIfKongBody = `BEGIN
 IF p_canshu IS NULL OR {{CAT}}.char_length(p_canshu)=0 THEN
  RETURN {{STATEMENT_DATE}};
 END IF;
 RETURN {{CAT}}.date_trunc('second',p_canshu::timestamp+INTERVAL '0.5 second');
END;`

const demoWBSSegBody = `DECLARE
 remaining text := p_wbs;
 segment text;
 pos integer;
 loc integer := p_seg;
 result_value integer;
BEGIN
 IF remaining IS NULL OR {{CAT}}.upper(remaining)='NULL' THEN
  RETURN 0;
 END IF;
 WHILE loc>=1 LOOP
  pos := {{CAT}}.strpos(remaining,'.');
  IF pos=0 THEN
   IF loc>1 THEN RETURN 0; END IF;
   IF remaining ~ '^[0-9]+$' THEN RETURN remaining::numeric::integer; END IF;
   IF remaining='' THEN RETURN 0; END IF;
   RETURN {{CAT}}.get_byte({{CAT}}.convert_to(remaining,'UTF8'),0);
  END IF;
  segment := {{CAT}}.left(remaining,pos-1);
  result_value := segment::integer;
  remaining := {{CAT}}.right(remaining,{{CAT}}.octet_length({{CAT}}.convert_to(remaining,'UTF8'))-pos);
  IF loc=1 THEN RETURN result_value; END IF;
  loc := loc-1;
 END LOOP;
 RETURN result_value;
END;`

func demoAverageScoreBody(schema, catalog, name string) string {
	const techMarks = ` FROM {{marks}} WHERE {{marks.biaoduanguid}}=$1 AND {{marks.danweiguid}}=$2 AND {{marks.pingfentype}}=$3`
	const judgeMarks = ` FROM {{marks}} WHERE {{marks.biaoduanguid}}=$1 AND {{marks.danweiguid}}=$2 AND {{marks.pfdtype}}=$3`
	table := pgProcedureTable{alias: "marks"}
	var query string
	arguments := "p_biaoduanguid,p_danweiguid,p_pingfentype"
	switch name {
	case "getavg_pwdf", "getavg_pwpm":
		table.name = "pingbiao_techlastmark"
		table.columns = strings.Fields("biaoduanguid danweiguid pingfentype pingweimarkresult")
		query = "SELECT {{catalog}}.avg({{marks.pingweimarkresult}}::numeric)" + techMarks
		if name == "getavg_pwpm" {
			query = `SELECT {{catalog}}.avg(position) FROM (
 SELECT CASE WHEN {{marks.pingweimarkresult}} IS NULL THEN 1 ELSE
 {{catalog}}.row_number() OVER (PARTITION BY {{marks.pingweimarkresult}}) END AS position` + techMarks + `
) AS positions`
		}
	case "getavg_pwdf2":
		table.name = "pingbiao_eval_lastmark"
		table.columns = strings.Fields("biaoduanguid danweiguid techmark")
		query = `SELECT {{marks.techmark}}::numeric FROM {{marks}} WHERE {{marks.biaoduanguid}}=$1 AND {{marks.danweiguid}}=$2`
		arguments = "p_biaoduanguid,p_danweiguid"
	case "getavg_pwdf3", "getavg_pwdf4", "getavg_pwpm2":
		table.name = "pingbiao_pw_mark"
		table.columns = strings.Fields("biaoduanguid danweiguid pfdtype pingweimark")
		const sum = "{{catalog}}.sum({{marks.pingweimark}}::numeric)"
		switch name {
		case "getavg_pwdf3":
			query = "SELECT " + sum + "/$4" + judgeMarks
			arguments += ",p_techpwcount"
		case "getavg_pwdf4":
			table.columns = append(table.columns, "pingweiguid")
			query = "SELECT " + sum + judgeMarks + " AND {{marks.pingweiguid}}=$4"
			arguments += ",p_pingweiguid"
		case "getavg_pwpm2":
			table.columns = append(table.columns, "pingweiguid")
			// A NULL sum never compares equal to the previous row in MySQL.
			query = `SELECT {{catalog}}.avg(position) FROM (
 SELECT CASE WHEN total IS NULL THEN {{catalog}}.row_number() OVER (ORDER BY total DESC NULLS LAST)
 ELSE {{catalog}}.rank() OVER (ORDER BY total DESC NULLS LAST) END AS position
 FROM (SELECT ` + sum + ` AS total` + judgeMarks + ` GROUP BY {{marks.pingweiguid}},{{marks.pingweimark}}) AS totals
) AS positions`
		}
	}
	statement := " EXECUTE {{QUERY}} INTO result_value USING " + arguments + ";\n RETURN result_value;"
	if name == "getavg_pwdf2" {
		statement = " BEGIN\n  EXECUTE {{QUERY}} INTO STRICT result_value USING " + arguments + ";\n EXCEPTION WHEN no_data_found THEN\n  RETURN NULL;\n END;\n RETURN result_value;"
	}
	return demoRoutineBody(schema, catalog, name, []pgProcedureTable{table}, " result_value numeric(18,2);", statement, map[string]string{"QUERY": query})
}

func demoSecondPriceBody(schema, catalog string) string {
	tables := []pgProcedureTable{{"item", "pingbiao_eval_dxgcfx", strings.Fields("biaoduanguid danweigcno mc je")}}
	const query = `WITH prices AS (
 SELECT DISTINCT {{item.je}}::numeric AS price FROM {{item}}
 WHERE {{item.biaoduanguid}}=$1 AND {{item.danweigcno}}=$2 AND {{item.mc}}=$3 AND {{item.je}} IS NOT NULL
)
SELECT COALESCE((SELECT price FROM prices ORDER BY price LIMIT 1 OFFSET 1),
                (SELECT price FROM prices ORDER BY price LIMIT 1))`
	return demoRoutineBody(schema, catalog, "getcidxgcje", tables, " result_value numeric(18,2);", `
 EXECUTE {{QUERY}} INTO result_value USING p_biaoduanguid,p_danweigcno,p_mc;
 RETURN result_value;`, map[string]string{"QUERY": query})
}

func demoDeviationBody(schema, catalog, name string) (string, string) {
	prefix, divisor, inputType := "pianli_pm_", "", "numeric(18,2)"
	if strings.HasPrefix(name, "getpingweidf") {
		prefix, divisor, inputType = "pianli_df_", "/100.0", "numeric(18,4)"
	}
	result, outcomes := "integer", []string{"y", "l", "z", "c"}
	labels := strings.HasSuffix(name, "jieguo")
	if labels {
		result = "varchar"
	}
	table := pgProcedureTable{"config", "pingbiao_pwdfsz", []string{"biaoduanguid"}}
	for _, suffix := range []string{"y", "s_l", "x_l", "s_z", "x_z", "c"} {
		table.columns = append(table.columns, prefix+suffix)
	}
	resultSQL := []string{"'优'", "'良'", "'中'", "'差'"}
	if !labels {
		for i, suffix := range outcomes {
			column := prefix + "df_" + suffix
			table.columns = append(table.columns, column)
			resultSQL[i] = "{{config." + column + "}}::integer"
		}
	}
	threshold := func(suffix string) string {
		return "({{config." + prefix + suffix + "}}::numeric" + divisor + ")::numeric(18,2)"
	}
	query := fmt.Sprintf(`SELECT CASE
 WHEN $2<%s THEN %s
 WHEN $2>=%s AND $2<%s THEN %s
 WHEN $2>=%s AND $2<%s THEN %s
 WHEN $2>=%s THEN %s
 END FROM {{config}} WHERE {{config.biaoduanguid}}=$1`,
		threshold("y"), resultSQL[0], threshold("s_l"), threshold("x_l"), resultSQL[1],
		threshold("s_z"), threshold("x_z"), resultSQL[2], threshold("c"), resultSQL[3])
	return result, demoRoutineBody(schema, catalog, name, []pgProcedureTable{table},
		" result_value "+result+";\n deviation "+inputType+" := p_pianlilv;", `
 BEGIN
  EXECUTE {{QUERY}} INTO STRICT result_value USING p_biaoduanguid,deviation;
 EXCEPTION WHEN no_data_found THEN
  RETURN NULL;
 END;
 RETURN result_value;`, map[string]string{"QUERY": query})
}

func demoQingDanProcedureDDL(schema, catalog string, native bool) string {
	body := demoQingDanBody(schema, catalog, demoQingDanProcedure, "")
	return pgProcedureDDL(schema, demoQingDanProcedure, demoQingDanArguments, body, native)
}

func demoQingDanBody(schema, catalog, name, tail string) string {
	tables := []pgProcedureTable{
		{"item", "pingbiao_tb_qingdanitem", strings.Fields("biaoduanguid qingdanno danweiguid danweigcno")},
		{"bid", "pingbiao_kaibiaotoubiao", strings.Fields("biaoduanguid danweiguid iscalculated")},
	}
	var averages, results []string
	for i, price := range []struct{ column, label string }{
		{"laborunitprice", "人工费"}, {"materialunitprice", "材料费"}, {"machineunitprice", "机械费"},
		{"overheadunitprice", "管理费"}, {"profit", "利润"},
	} {
		tables[0].columns = append(tables[0].columns, price.column, price.column+"_ok")
		averages = append(averages, "{{catalog}}.avg(a.{{item."+price.column+"_ok}}::numeric) AS avg_"+price.column)
		results = append(results, fmt.Sprintf(`SELECT a.{{item.%s}}::numeric AS unitprice, a.avg_%s AS avg_value,
 '%s'::text AS xiangmuname, %d AS orderno, a.{{item.biaoduanguid}} AS biaoduanguid,
 a.{{item.qingdanno}} AS qingdanno, a.{{item.danweiguid}} AS danweiguid, a.{{item.danweigcno}} AS danweigcno
 FROM pg_temp.tmp_table%d AS a WHERE a.{{item.danweiguid}}=$1`, price.column, price.column, price.label, i+1, i+1))
	}
	// The exported routines build five identical intermediate tables and combine
	// their columns with UNION. Compute the averages once and retain all six
	// session-local tables for callers that read either intermediate or final data.
	stage := `CREATE TEMP TABLE pg_temp.tmp_table1 AS WITH items AS (
 SELECT a.* FROM {{item}} AS a
 WHERE a.{{item.biaoduanguid}}=$1 AND a.{{item.qingdanno}}=$2 AND a.{{item.danweigcno}}=$3
), prices AS (
 SELECT ` + strings.Join(averages, ", ") + ` FROM items AS a
 JOIN {{bid}} AS b ON a.{{item.biaoduanguid}}=b.{{bid.biaoduanguid}} AND a.{{item.danweiguid}}=b.{{bid.danweiguid}}
 WHERE b.{{bid.iscalculated}}='1'
)
SELECT a.*,p.* FROM items AS a CROSS JOIN prices AS p`
	result := "CREATE TEMP TABLE pg_temp.tmp_table AS " + strings.Join(results, "\nUNION\n")

	const statements = ` DROP TABLE IF EXISTS pg_temp.tmp_table,pg_temp.tmp_table1,pg_temp.tmp_table2,
  pg_temp.tmp_table3,pg_temp.tmp_table4,pg_temp.tmp_table5;
 EXECUTE {{STAGE}} USING p_biaoduanguid,p_qingdanno,p_danweigcno;
 CREATE TEMP TABLE pg_temp.tmp_table2 AS SELECT * FROM pg_temp.tmp_table1;
 CREATE TEMP TABLE pg_temp.tmp_table3 AS SELECT * FROM pg_temp.tmp_table1;
 CREATE TEMP TABLE pg_temp.tmp_table4 AS SELECT * FROM pg_temp.tmp_table1;
 CREATE TEMP TABLE pg_temp.tmp_table5 AS SELECT * FROM pg_temp.tmp_table1;
 EXECUTE {{RESULT}} USING p_danweiguid;
`

	return demoRoutineBody(schema, catalog, name, tables, "", statements+tail, map[string]string{"STAGE": stage, "RESULT": result})
}
