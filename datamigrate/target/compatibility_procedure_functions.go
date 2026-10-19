package target

import (
	"strings"

	"dbgold/datamigrate/source"
)

type archiveTableSpec struct {
	source, history, key string
	columns              []string
}

// Preserve the explicit column lists and table order shared by
// ChangeDataToHistory and ChangeDataToMain.
// Source and destination identifiers are resolved independently at invocation,
// so retained mixed-case names need not be identical between the two tables.
var changeDataToHistoryTables = []archiveTableSpec{
	{"workflow_activity_instance", "workflow_activity_instance_his", "processversioninstanceguid", strings.Fields(`
row_id activityguid activityinstanceguid subprocessversioninstanceguid status startdate endtime terminatedate
is_overtime rollbacktag activityinstancename processversionguid processversioninstanceguid note`)},
	{"workflow_pvi_mistablerow", "workflow_pvi_mistablerow_his", "pviguid", strings.Fields(`
row_id materialinstanceguid tableid sql_tablename misrowguid pviguid mistablesetinstanceguid`)},
	{"workflow_pvi_material", "workflow_pvi_material_his", "processversioninstanceguid", strings.Fields(`
row_id materialinstanceguid materialinstancename adddate is_defined addinworkitemguid adduserguid
processversioninstanceguid materialguid materialname source type attachstorageinfogroupguid
pageurl_readandwrite pageurl_read pageurl_print status is_showingrid clienttag is_initmistable frommainpvimatinstanceguid`)},
	{"workflow_transition_instance", "workflow_transition_i_history", "processversioninstanceguid", strings.Fields(`
transitioninstanceguid transitionguid processversioninstanceguid sourceactiviryguid sourceactiviryname
srcactinstguid srcactinstname targetactivityguid targetactivityname tgtactinstguid tgtactinstname type transitdate direction`)},
	{"workflow_workitem", "workflow_workitem_history", "processversioninstanceguid", strings.Fields(`
row_id workitemguid activityguid activityname activityinstanceguid workitemname workitemtype workitemmodename
handleurl status priority senderguid sendername transactor transactorname jobguid operationguid readdate
operationdate operatorguid operatorname opinion waithandleguid createdate startdate enddate terminatedate
statusbeforeterminate limitminutes processguid processversionguid processversionname processversioninstanceguid
processversioninstancename groupguid is_locked lockeruserguid lockerdispname lockdate agentforuserguid
operationtype lockerwindowguid abauditopinion timelimit timelimitunit overtimepoint earlywarningpoint
is_needsendearlywarning is_needsendovertimenotify agentforworkitemguid operatorfordisplayguid
operatorfordisplayname note transitioninstanceguid`)},
	{"workflow_pvi_opinion", "workflow_pvi_opinion_his", "pviguid", strings.Fields(`
opinionguid workitemguid activityguid activityname activityinstanceguid pviguid opiniontext adddate adduserguid addusername`)},
	{"workflow_context_value", "workflow_context_value_history", "processversioninstanceguid", strings.Fields(`
fieldguid processversioninstanceguid activityinstanceguid fieldname fieldtype fieldvalue`)},
}

func changeDataToHistoryDDL(schema, catalog string, native bool) string {
	return workflowDataMoveDDL(schema, catalog, compatibilityProcedureName, "1", false, native)
}

func changeDataToMainDDL(schema, catalog string, native bool) string {
	return workflowDataMoveDDL(schema, catalog, compatibilityMainProcedureName, "", true, native)
}

func workflowDataMoveDDL(schema, catalog, name, childTag string, toMain, native bool) string {
	tables := []string{"workflow_pvi"}
	columns := []string{"processversioninstanceguid,mainpviguid,tag"}
	keys := []string{"processversioninstanceguid"}
	for _, pair := range changeDataToHistoryTables {
		if toMain {
			tables = append(tables, pair.history, pair.source)
		} else {
			tables = append(tables, pair.source, pair.history)
		}
		columns = append(columns, strings.Join(pair.columns, ","), strings.Join(pair.columns, ","))
		keys = append(keys, pair.key, pair.key)
	}
	// All list values are application constants, never source DDL or user input.
	array := func(values []string) string { return "ARRAY['" + strings.Join(values, "','") + "']::text[]" }
	cat := source.QuotePGIdent(catalog)
	operation := "archive"
	if toMain {
		operation = "restore"
	}
	body := strings.NewReplacer(
		"{{TABLES}}", array(tables), "{{COLUMNS}}", array(columns), "{{KEYS}}", array(keys),
		"{{SCHEMA}}", sqlFunctionBody(schema), "{{CAT}}", cat,
		"{{PROCEDURE}}", name, "{{TAG}}", childTag, "{{OPERATION}}", operation,
	).Replace(workflowDataMoveBody)
	header := "CREATE PROCEDURE " + qualifiedFunction(schema, name) + "(IN p_processversioninstanceguid varchar)"
	if native {
		// The '/' shown in native-client examples is a client delimiter, not SQL.
		return header + " SECURITY INVOKER AS\n" + body
	}
	return header + " LANGUAGE plpgsql SECURITY INVOKER AS " + sqlFunctionBody(body) + ";"
}

const workflowDataMoveBody = `DECLARE
 required_tables text[] := {{TABLES}};
 required_columns text[] := {{COLUMNS}};
 required_keys text[] := {{KEYS}};
 relation_sql text[] := ARRAY[]::text[];
 column_lists text[] := ARRAY[]::text[];
 projection_lists text[] := ARRAY[]::text[];
 key_columns text[] := ARRAY[]::text[];
 resolved_columns text[];
 resolved_projection text[];
 relation_oid oid;
 relation_name text;
 match_count integer;
 required_column text;
 actual_column text;
 key_column text;
 pvi_guid_column text;
 pvi_main_column text;
 pvi_tag_column text;
 child_guids text[] := ARRAY[]::text[];
 target_guids text[];
 current_guid varchar;
 pvi_row record;
 table_index integer;
 source_index integer;
 pair_index integer;
 move_sql text;
 moved_count bigint;
 inserted_count bigint;
BEGIN
 IF p_processversioninstanceguid IS NULL OR p_processversioninstanceguid='' THEN
  RAISE EXCEPTION '{{PROCEDURE}}: process GUID must not be NULL or empty';
 END IF;

 -- Validate all fifteen tables and every required column before any writes.
 FOR table_index IN 1..{{CAT}}.array_length(required_tables,1) LOOP
  SELECT {{CAT}}.count(*),{{CAT}}.min(c.oid::bigint)::oid,{{CAT}}.min(c.relname::text)
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
  resolved_columns := ARRAY[]::text[];
  resolved_projection := ARRAY[]::text[];
  key_column := NULL;
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
   resolved_columns := {{CAT}}.array_append(resolved_columns,{{CAT}}.quote_ident(actual_column));
   resolved_projection := {{CAT}}.array_append(resolved_projection,'src.'||{{CAT}}.quote_ident(actual_column));
   IF required_column=required_keys[table_index] THEN
    key_column := {{CAT}}.quote_ident(actual_column);
   END IF;
  END LOOP;
  column_lists := {{CAT}}.array_append(column_lists,{{CAT}}.array_to_string(resolved_columns,','));
  projection_lists := {{CAT}}.array_append(projection_lists,{{CAT}}.array_to_string(resolved_projection,','));
  key_columns := {{CAT}}.array_append(key_columns,key_column);
  IF table_index=1 THEN
   pvi_guid_column := resolved_columns[1];
   pvi_main_column := resolved_columns[2];
   pvi_tag_column := resolved_columns[3];
  END IF;
 END LOOP;

 -- Row locks coordinate repeat calls without freezing other workflows' writes.
 -- Capture actual columns, never the same-named input parameter.
 FOR pvi_row IN EXECUTE 'SELECT p.'||pvi_guid_column||'::text AS guid,p.'||pvi_main_column||'::text AS main_guid FROM '
  ||relation_sql[1]||' AS p WHERE p.'||pvi_guid_column||'=$1 OR p.'||pvi_main_column||'=$1'
  ||' ORDER BY p.'||pvi_guid_column||',p.'||pvi_main_column||' FOR UPDATE'
  USING p_processversioninstanceguid LOOP
  IF pvi_row.guid IS NOT NULL AND pvi_row.guid<>p_processversioninstanceguid
   AND pvi_row.main_guid=p_processversioninstanceguid AND NOT (pvi_row.guid=ANY(child_guids)) THEN
   child_guids := {{CAT}}.array_append(child_guids,pvi_row.guid);
  END IF;
 END LOOP;
 target_guids := {{CAT}}.array_append(child_guids,p_processversioninstanceguid::text);
 FOREACH current_guid IN ARRAY target_guids LOOP
  FOR pair_index IN 1..7 LOOP
   source_index := pair_index*2;
   -- DELETE RETURNING and INSERT consume the exact same batch, even if an
   -- unrelated writer inserts more rows while the move is in progress.
   move_sql := 'WITH moved AS (DELETE FROM '||relation_sql[source_index]||' AS src WHERE src.'
    ||key_columns[source_index]||'=$1 RETURNING '||projection_lists[source_index]||'), archived AS (INSERT INTO '
    ||relation_sql[source_index+1]||' ('||column_lists[source_index+1]||') SELECT '
    ||projection_lists[source_index]||' FROM moved AS src RETURNING 1) SELECT '
    ||'(SELECT {{CAT}}.count(*) FROM moved),(SELECT {{CAT}}.count(*) FROM archived)';
   EXECUTE move_sql INTO moved_count,inserted_count USING current_guid;
   -- A destination BEFORE INSERT trigger can silently suppress an insertion.
   -- Never commit a deletion whose corresponding insertion was skipped.
   IF moved_count<>inserted_count THEN
    RAISE EXCEPTION '{{PROCEDURE}}: {{OPERATION}} row count mismatch for % (deleted %, inserted %)',
     required_tables[source_index],moved_count,inserted_count;
   END IF;
  END LOOP;
  IF current_guid<>p_processversioninstanceguid THEN
   EXECUTE 'UPDATE '||relation_sql[1]||' AS p SET '||pvi_tag_column||'=''{{TAG}}'' WHERE p.'
    ||pvi_guid_column||'=$1 AND p.'||pvi_main_column||'=$2'
    USING current_guid,p_processversioninstanceguid;
  END IF;
 END LOOP;
 -- Invoke with a standalone CALL. No earlier commit or exception swallowing:
 -- any move error aborts all moves and tag updates in this invocation.
 COMMIT;
END;`
