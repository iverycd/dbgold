package target

import (
	"strings"

	"dbgold/datamigrate/source"
)

const demo2DateDiffArgs = "p_date1 timestamp without time zone, p_date2 timestamp without time zone, INOUT p_years integer DEFAULT NULL, INOUT p_months integer DEFAULT NULL, INOUT p_days integer DEFAULT NULL"
const demo2CategoryAddArgs = "p_categoryname varchar, p_parentcategorynum varchar, p_homepageurl varchar"

var demo2UtilityProcedureSpecs = []compatibilityProcedureSpec{
	{name: "abs_date_diff", signature: "abs_date_diff(" + demo2DateDiffArgs + ")",
		inputs: []string{"timestamp without time zone", "timestamp without time zone", "integer", "integer", "integer"},
		modes:  "i,i,b,b,b", minDefaults: 3, maxDefaults: 3, ddl: demo2AbsDateDiffDDL},
	{name: "table_category_add", signature: "table_category_add(" + demo2CategoryAddArgs + ")",
		inputs: []string{"character varying", "character varying", "character varying"}, modes: "i,i,i", ddl: demo2TableCategoryAddDDL},
}

func demo2AbsDateDiffDDL(schema, catalog string, native bool) string {
	body := strings.ReplaceAll(demo2AbsDateDiffBody, "{{CAT}}", source.QuotePGIdent(catalog))
	return pgProcedureDDL(schema, "abs_date_diff", demo2DateDiffArgs, body, native)
}

// TIMESTAMPDIFF(MONTH) counts only complete months, using the day and time
// within each endpoint month. The source calculates days from an integer.
const demo2AbsDateDiffBody = `DECLARE
 earlier timestamp without time zone;
 later timestamp without time zone;
 earlier_day integer;
 later_day integer;
 month_count integer;
BEGIN
 IF p_date1 IS NULL OR p_date2 IS NULL THEN
  p_years := NULL;
  p_months := NULL;
  p_days := NULL;
  RETURN;
 END IF;
 earlier := least(p_date1,p_date2);
 later := greatest(p_date1,p_date2);
 earlier_day := extract(day FROM earlier)::integer;
 later_day := extract(day FROM later)::integer;
 month_count := 12*(extract(year FROM later)::integer-extract(year FROM earlier)::integer)
  + extract(month FROM later)::integer-extract(month FROM earlier)::integer;
 IF later_day<earlier_day OR (later_day=earlier_day AND later::time<earlier::time) THEN
  month_count := month_count-1;
 END IF;
 p_years := {{CAT}}.div(month_count::numeric,12)::integer;
 p_months := month_count-12*p_years;
 p_days := 0;
END;`

func demo2TableCategoryAddDDL(schema, catalog string, native bool) string {
	tables := []pgProcedureTable{
		{"category", "table_category", []string{"categorynum", "categoryname", "homepageurl"}},
	}
	return pgBusinessProcedureDDL(schema, catalog, native, "table_category_add", demo2CategoryAddArgs,
		tables, "", ` parent_code text := nullif(p_parentcategorynum,'');
 highest_suffix integer;
 new_code text;
`, `
 EXECUTE {{LOCK_CATEGORIES}};
 EXECUTE {{HIGHEST_SUFFIX}} INTO highest_suffix USING parent_code;
 IF highest_suffix>=99 THEN
  RAISE EXCEPTION 'table_category_add: two-digit category numbers exhausted for parent %',parent_code;
 END IF;
 new_code := {{catalog}}.lpad((highest_suffix+1)::text,2,'0');
 IF parent_code IS NOT NULL THEN
  new_code := parent_code||new_code;
 END IF;
 EXECUTE {{INSERT_CATEGORY}} USING new_code,p_categoryname,p_homepageurl;
`, map[string]string{
			"LOCK_CATEGORIES": `LOCK TABLE {{category}} IN SHARE ROW EXCLUSIVE MODE`,
			"HIGHEST_SUFFIX": `SELECT coalesce({{catalog}}.max(
 CASE WHEN {{catalog}}.right(c.{{category.categorynum}}::text,2) ~ '^[0-9]{2}$'
      THEN {{catalog}}.right(c.{{category.categorynum}}::text,2)::integer END),0)
 FROM {{category}} AS c
 WHERE {{catalog}}.char_length(c.{{category.categorynum}}::text)=coalesce({{catalog}}.char_length($1),0)+2
 AND ($1 IS NULL OR {{catalog}}.left(c.{{category.categorynum}}::text,{{catalog}}.char_length($1))=$1)`,
			"INSERT_CATEGORY": `INSERT INTO {{category}}({{category.categorynum}},{{category.categoryname}},{{category.homepageurl}}) VALUES($1,$2,$3)`,
		})
}
