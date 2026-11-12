package target

import (
	"fmt"
	"regexp"
	"strings"

	"dbgold/datamigrate/source"
)

type pgProcedureTable struct {
	alias, name string
	columns     []string
}

func pgProcedureIdentifierBindings(tables []pgProcedureTable) (tableArray, columnArray string, identifiers map[string]string) {
	var names, columns []string
	identifiers = make(map[string]string)
	columnIndex := 0
	for i, table := range tables {
		names = append(names, table.name)
		columns = append(columns, strings.Join(table.columns, ","))
		identifiers[table.alias] = fmt.Sprintf("relation_sql[%d]", i+1)
		for _, column := range table.columns {
			columnIndex++
			identifiers[table.alias+"."+column] = fmt.Sprintf("column_sql[%d]", columnIndex)
		}
	}
	array := func(values []string) string { return "ARRAY['" + strings.Join(values, "','") + "']::text[]" }
	return array(names), array(columns), identifiers
}

var pgProcedureIdentifier = regexp.MustCompile(`\{\{([a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)?)\}\}`)

// Only fixed queries use this renderer. Table and column expressions refer to
// quoted catalog names, and business values are supplied by EXECUTE ... USING.
func pgProcedureSQL(query string, identifiers map[string]string) string {
	var fragments []string
	start := 0
	for _, match := range pgProcedureIdentifier.FindAllStringSubmatchIndex(query, -1) {
		if match[0] > start {
			fragments = append(fragments, sqlFunctionBody(query[start:match[0]]))
		}
		token := query[match[2]:match[3]]
		expression, ok := identifiers[token]
		if !ok {
			panic("unknown compatibility procedure identifier: " + token)
		}
		fragments = append(fragments, expression)
		start = match[1]
	}
	if start < len(query) {
		fragments = append(fragments, sqlFunctionBody(query[start:]))
	}
	return strings.Join(fragments, "||")
}

func pgBusinessProcedureDDL(schema, catalog string, native bool, name, parameters string, tables []pgProcedureTable, guard, declarations, statements string, queries map[string]string) string {
	return pgBusinessProcedureDDLWithDeferredTables(schema, catalog, native, name, parameters, tables, guard, declarations, statements, queries, 0)
}

func pgBusinessProcedureDDLWithDeferredTables(schema, catalog string, native bool, name, parameters string, tables []pgProcedureTable, guard, declarations, statements string, queries map[string]string, deferredTables int) string {
	tableArray, columnArray, identifiers := pgProcedureIdentifierBindings(tables)
	quotedCatalog := source.QuotePGIdent(catalog)
	quotedSchema := source.QuotePGIdent(schema)
	identifiers["catalog"] = sqlFunctionBody(quotedCatalog)
	identifiers["schema"] = sqlFunctionBody(quotedSchema)
	// The dynamic query needs a string value for catalog lookups and an
	// identifier for qualified SQL references.
	identifiers["schema_name"] = sqlFunctionBody(sqlFunctionBody(schema))
	resolver := strings.NewReplacer(
		"{{TABLES}}", tableArray, "{{COLUMNS}}", columnArray,
		"{{CAT}}", quotedCatalog, "{{SCHEMA}}", sqlFunctionBody(schema), "{{PROCEDURE}}", name,
	)
	replacements := []string{"{{catalog}}", quotedCatalog, "{{schema}}", quotedSchema}
	for key, query := range queries {
		replacements = append(replacements, "{{"+key+"}}", pgProcedureSQL(query, identifiers))
	}
	resolveIdentifiers := pgProcedureResolveIdentifiers
	if deferredTables > 0 {
		immediateTables := len(tables) - deferredTables
		resolveIdentifiers = fmt.Sprintf(" FOR table_index IN 1..%d LOOP\n", immediateTables) + pgProcedureResolveIdentifierLoop
		deferredLoop := fmt.Sprintf(" FOR table_index IN %d..%d LOOP\n", immediateTables+1, len(tables)) + pgProcedureResolveIdentifierLoop
		replacements = append(replacements, "{{RESOLVE_DEFERRED_TABLES}}", resolver.Replace(deferredLoop))
	}
	body := "DECLARE\n" + resolver.Replace(pgProcedureIdentifierDeclarations) + declarations + "\nBEGIN\n" + guard + "\n" +
		resolver.Replace(resolveIdentifiers) + strings.NewReplacer(replacements...).Replace(statements) + "\nEND;"
	return pgProcedureDDL(schema, name, parameters, body, native)
}

func pgProcedureDDL(schema, name, parameters, body string, native bool) string {
	header := "CREATE PROCEDURE " + qualifiedFunction(schema, name) + "(" + parameters + ")"
	if native {
		return header + " SECURITY INVOKER AS\n" + body
	}
	return header + " LANGUAGE plpgsql SECURITY INVOKER AS " + sqlFunctionBody(body) + ";"
}

const pgProcedureIdentifierDeclarations = ` required_tables text[] := {{TABLES}};
 required_columns text[] := {{COLUMNS}};
 relation_sql text[] := ARRAY[]::text[];
 column_sql text[] := ARRAY[]::text[];
 relation_oid oid;
 relation_name text;
 match_count integer;
 table_index integer;
 required_column text;
 actual_column text;
`

const pgProcedureResolveIdentifiers = " FOR table_index IN 1..{{CAT}}.array_length(required_tables,1) LOOP\n" + pgProcedureResolveIdentifierLoop

const pgProcedureResolveIdentifierLoop = `  SELECT {{CAT}}.count(*),{{CAT}}.min(c.oid::bigint)::oid,{{CAT}}.min(c.relname::text)
   INTO match_count,relation_oid,relation_name
   FROM {{CAT}}.pg_class c JOIN {{CAT}}.pg_namespace n ON n.oid=c.relnamespace
   WHERE n.nspname={{SCHEMA}} AND {{CAT}}.lower(c.relname::text)=required_tables[table_index]
   AND c.relkind IN ('r','p');
  IF match_count=0 THEN
   RAISE EXCEPTION '{{PROCEDURE}}: table % is missing from the target schema',required_tables[table_index];
  ELSIF match_count<>1 THEN
   RAISE EXCEPTION '{{PROCEDURE}}: table % is ambiguous in the target schema',required_tables[table_index];
  END IF;
  relation_sql := {{CAT}}.array_append(relation_sql,{{CAT}}.quote_ident({{SCHEMA}})||'.'||{{CAT}}.quote_ident(relation_name));
  FOREACH required_column IN ARRAY {{CAT}}.string_to_array(required_columns[table_index],',') LOOP
   SELECT {{CAT}}.count(*),{{CAT}}.min(a.attname::text) INTO match_count,actual_column
    FROM {{CAT}}.pg_attribute a
    WHERE a.attrelid=relation_oid AND a.attnum>0 AND NOT a.attisdropped
    AND {{CAT}}.lower(a.attname::text)=required_column;
   IF match_count=0 THEN
    RAISE EXCEPTION '{{PROCEDURE}}: column % is missing from %',required_column,required_tables[table_index];
   ELSIF match_count<>1 THEN
    RAISE EXCEPTION '{{PROCEDURE}}: column % is ambiguous in %',required_column,required_tables[table_index];
   END IF;
   column_sql := {{CAT}}.array_append(column_sql,{{CAT}}.quote_ident(actual_column));
  END LOOP;
 END LOOP;
`
