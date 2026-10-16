package target

import (
	_ "embed"
	"strings"

	"dbgold/datamigrate/source"
)

// Generated explicitly by compatibility_data/generate_pinyin.py. Builds and
// migrations consume this pinned asset without downloading Unicode data.
//
//go:embed compatibility_data/pinyin_initials_17_0.txt
var builtinPinyinInitials string

//go:embed compatibility_data/LICENSE-UNICODE.txt
var builtinUnicodeLicense string

// builtinFunctionDDL creates only the requested lowercase canonical function.
// The initializer checks for existing functions before executing this DDL.
func builtinFunctionDDL(schema, catalog, name string) string {
	cat := source.QuotePGIdent(catalog)
	switch name {
	case "auto_increase_itemid":
		body := strings.NewReplacer("{{CAT}}", cat, "{{CAT_LITERAL}}", sqlFunctionBody(cat), "{{SCHEMA}}", sqlFunctionBody(schema)).Replace(autoIncreaseItemIDBody)
		return "CREATE FUNCTION " + qualifiedFunction(schema, name) + "() RETURNS varchar LANGUAGE plpgsql VOLATILE SECURITY INVOKER AS " + sqlFunctionBody(body)
	case "count_rows":
		body := strings.NewReplacer("{{CAT}}", cat, "{{CAT_LITERAL}}", sqlFunctionBody(cat)).Replace(countRowsBody)
		return "CREATE FUNCTION " + qualifiedFunction(schema, name) + "(table_name varchar, owner varchar DEFAULT NULL) RETURNS numeric LANGUAGE plpgsql VOLATILE SECURITY INVOKER AS " + sqlFunctionBody(body)
	case "fun_getpy":
		body := strings.NewReplacer("{{CAT}}", cat, "{{MAPPING}}", builtinPinyinInitials, "{{LICENSE}}", builtinUnicodeLicense).Replace(pinyinInitialsBody)
		return "CREATE FUNCTION " + qualifiedFunction(schema, name) + "(p_name varchar) RETURNS varchar LANGUAGE plpgsql IMMUTABLE SECURITY INVOKER AS " + sqlFunctionBody(body)
	default:
		return businessFunctionDDL(schema, catalog, name)
	}
}

// Resolve identifiers at invocation time so preparing a schema does not require
// code_items to exist yet. Restrict lookup to the schema captured at creation;
// the caller's search_path cannot redirect access to another code_items table.
const autoIncreaseItemIDBody = `DECLARE
 table_matches integer;
 column_matches integer;
 table_oid oid;
 table_name text;
 column_name text;
 relation_sql text;
 column_sql text;
 itemidtemp numeric;
 itemidfinal numeric := 1;
 isexist numeric;
BEGIN
 SELECT {{CAT}}.count(*) INTO table_matches
 FROM {{CAT}}.pg_class c JOIN {{CAT}}.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname={{SCHEMA}} AND {{CAT}}.lower(c.relname::text)='code_items' AND c.relkind IN ('r','p');
 IF table_matches=0 THEN
  RAISE EXCEPTION 'auto_increase_itemid: code_items table is missing from the target schema';
 ELSIF table_matches<>1 THEN
  RAISE EXCEPTION 'auto_increase_itemid: code_items table name is ambiguous in the target schema';
 END IF;
 SELECT c.oid,c.relname INTO table_oid,table_name
 FROM {{CAT}}.pg_class c JOIN {{CAT}}.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname={{SCHEMA}} AND {{CAT}}.lower(c.relname::text)='code_items' AND c.relkind IN ('r','p');
 SELECT {{CAT}}.count(*),{{CAT}}.min(a.attname::text) INTO column_matches,column_name
 FROM {{CAT}}.pg_attribute a
 WHERE a.attrelid=table_oid AND a.attnum>0 AND NOT a.attisdropped AND {{CAT}}.lower(a.attname::text)='itemid';
 IF column_matches=0 THEN
  RAISE EXCEPTION 'auto_increase_itemid: itemid column is missing from code_items';
 ELSIF column_matches<>1 THEN
  RAISE EXCEPTION 'auto_increase_itemid: itemid column name is ambiguous in code_items';
 END IF;
 relation_sql := {{CAT}}.quote_ident({{SCHEMA}})||'.'||{{CAT}}.quote_ident(table_name);
 column_sql := {{CAT}}.quote_ident(column_name);
 EXECUTE 'SELECT '||{{CAT_LITERAL}}||'.count(*) FROM '||relation_sql INTO isexist;
 IF isexist>0 THEN
  EXECUTE 'SELECT '||{{CAT_LITERAL}}||'.min('||column_sql||') FROM '||relation_sql INTO itemidtemp;
  EXECUTE 'WITH tabs AS (SELECT '||{{CAT_LITERAL}}||'.row_number() OVER (ORDER BY '||column_sql||') num,'||column_sql||' AS itemid FROM '||relation_sql||') SELECT '||{{CAT_LITERAL}}||'.max(itemid)+1 FROM tabs WHERE num+($1-1)=itemid'
   INTO itemidfinal USING itemidtemp;
 END IF;
 RETURN CAST(itemidfinal AS varchar);
END;`

const countRowsBody = `DECLARE
 num_rows numeric;
 stmt text;
BEGIN
 IF owner IS NULL THEN
  stmt := 'SELECT '||{{CAT_LITERAL}}||'.count(*) FROM '||{{CAT}}.quote_ident(table_name);
 ELSE
  stmt := 'SELECT '||{{CAT_LITERAL}}||'.count(*) FROM '||{{CAT}}.quote_ident(owner)||'.'||{{CAT}}.quote_ident(table_name);
 END IF;
 EXECUTE stmt INTO num_rows;
 RETURN num_rows;
END;`

// UTF8 byte decoding avoids depending on vendor-specific ASCII(non-ASCII)
// behavior or the target's database encoding. All mapped Han characters require
// three or four UTF8 bytes. The mapping constant itself contains only ASCII.
const pinyinInitialsBody = `/*
Derived from Unicode 17.0.0 Unihan kMandarin, first reading (zh-Hans).
Source: https://www.unicode.org/Public/17.0.0/ucd/Unihan.zip
Source SHA256: f7a48b2b545acfaa77b2d607ae28747404ce02baefee16396c5d2d7a8ef34b5e
{{LICENSE}}*/
DECLARE
 initials CONSTANT text := '{{MAPPING}}';
 result text := '';
 word_bytes bytea;
 byte_count integer;
 codepoint integer;
 initial text;
 i integer;
BEGIN
 IF p_name IS NULL OR p_name='' THEN
  RETURN NULL;
 END IF;
 FOR i IN 1..{{CAT}}.length(p_name) LOOP
  word_bytes := {{CAT}}.convert_to({{CAT}}.substr(p_name,i,1),'UTF8');
  byte_count := {{CAT}}.octet_length(word_bytes);
  IF byte_count=3 THEN
   codepoint := ({{CAT}}.get_byte(word_bytes,0)-224)*4096+({{CAT}}.get_byte(word_bytes,1)-128)*64+{{CAT}}.get_byte(word_bytes,2)-128;
  ELSIF byte_count=4 THEN
   codepoint := ({{CAT}}.get_byte(word_bytes,0)-240)*262144+({{CAT}}.get_byte(word_bytes,1)-128)*4096+({{CAT}}.get_byte(word_bytes,2)-128)*64+{{CAT}}.get_byte(word_bytes,3)-128;
  ELSE
   CONTINUE;
  END IF;
  IF codepoint BETWEEN 13312 AND 205705 THEN
   initial := {{CAT}}.substr(initials,codepoint-13312+1,1);
   IF initial<>'?' THEN
    result := coalesce(result,'')||initial;
   END IF;
  END IF;
 END LOOP;
 RETURN nullif(result,'');
END;`
