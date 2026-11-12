package target

import (
	"strings"

	"dbgold/datamigrate/source"
)

const demoHistoryMain = "pingbiao_biaoduan"
const demoHistoryConfig = "pingbiao_historydbconfig"

func demoMoveDataToHistoryDBDDL(schema, catalog string, native bool) string {
	tables := []pgProcedureTable{
		{"lot", demoHistoryMain, strings.Fields("biaoduanguid pingbiaoisend historydb_name if_moved")},
		{"config", demoHistoryConfig, strings.Fields("historydb_name isnowuse dbtype")},
		{"log", "pingbiao_movedatalog", strings.Fields("logcontent logtime error_line biaoduanguid")},
		{"error_log", "pingbiao_movedataerror", strings.Fields("errorcontent errortime tablename biaoduanguid")},
	}
	declarations := ` eligible boolean;
 history_schema text;
 source_schema_oid oid;
 history_schema_oid oid;
 moving record;
 history_table_oid oid;
 history_table_name text;
 history_table_kind text;
 copied_tables oid[] := ARRAY[]::oid[];
 source_column record;
 history_column_name text;
 source_columns text[];
 history_columns text[];
 source_sql text;
 history_sql text;
 pending_deletes text[] := ARRAY[]::text[];
 pending_tables text[] := ARRAY[]::text[];
 delete_index integer;
 expected_rows bigint;
 copied_rows bigint;
 failed_table text;
 failure_message text;
`
	body := strings.NewReplacer(
		"{{SCHEMA_NAME}}", sqlFunctionBody(schema),
		"{{CATALOG_LITERAL}}", sqlFunctionBody(source.QuotePGIdent(catalog)),
		"{{SOURCE_INHERITANCE}}", demoCrossSchemaDescendants("moving.source_oid", "source_schema_oid", "{{catalog}}"),
		"{{HISTORY_INHERITANCE}}", demoCrossSchemaDescendants("history_table_oid", "history_schema_oid", "{{catalog}}"),
	).Replace(demoMoveDataToHistoryDBBody)
	return pgBusinessProcedureDDL(schema, catalog, native, "proc_pb_movedata2historydb", demoLotParameters, tables, "", declarations,
		body, map[string]string{
			"LOT_ELIGIBLE": `SELECT true FROM {{lot}} WHERE {{lot.biaoduanguid}}=$1 AND {{lot.pingbiaoisend}}::text='1'
 AND coalesce({{catalog}}.char_length({{lot.historydb_name}}::text),0)=0 AND coalesce({{lot.if_moved}}::text,'0')<>'1' FOR UPDATE`,
			"HISTORY_SCHEMA": demoHistorySchemaQuery,
			"MARK_MOVED":     `UPDATE {{lot}} SET {{lot.historydb_name}}=$1,{{lot.if_moved}}='1' WHERE {{lot.biaoduanguid}}=$2`,
			"LOG_SUCCESS": `INSERT INTO {{log}}({{log.logcontent}},{{log.logtime}},{{log.error_line}},{{log.biaoduanguid}})
 VALUES ('迁移成功',{{catalog}}.clock_timestamp(),'',$1)`,
			"LOG_FAILURE": `INSERT INTO {{log}}({{log.logcontent}},{{log.logtime}},{{log.error_line}},{{log.biaoduanguid}})
 VALUES ('迁移失败',{{catalog}}.clock_timestamp(),'',$1)`,
			"LOG_ERROR": `INSERT INTO {{error_log}}({{error_log.errorcontent}},{{error_log.errortime}},{{error_log.tablename}},{{error_log.biaoduanguid}})
 VALUES ($1,{{catalog}}.clock_timestamp(),$2,$3)`,
		})
}

const demoHistorySchemaQuery = `SELECT {{config.historydb_name}} FROM {{config}}
 WHERE {{config.isnowuse}}::text='1' AND {{config.dbtype}}::text='2'`

// PostgreSQL databases are independent. The configured archive name resolves
// to an existing schema in this database. A subtransaction rolls back every
// partial move before the caller-visible error and status logs are written.
const demoMoveDataToHistoryDBBody = `
 BEGIN
  failed_table := 'pingbiao_biaoduan';
  EXECUTE {{LOT_ELIGIBLE}} INTO eligible USING p_biaoduanguid;
  IF eligible THEN
   failed_table := 'pingbiao_historydbconfig';
   BEGIN
    EXECUTE {{HISTORY_SCHEMA}} INTO STRICT history_schema;
   EXCEPTION WHEN no_data_found THEN
    history_schema := NULL;
   END;
   IF coalesce({{catalog}}.char_length(history_schema),0)>0 THEN
    SELECT n.oid INTO source_schema_oid FROM {{catalog}}.pg_namespace n WHERE n.nspname={{SCHEMA_NAME}};
    SELECT n.oid INTO history_schema_oid FROM {{catalog}}.pg_namespace n WHERE n.nspname::text=history_schema;
    IF history_schema_oid IS NULL THEN
     RAISE EXCEPTION 'history schema % does not exist in the current database',history_schema;
    END IF;
    IF history_schema_oid=source_schema_oid THEN
     RAISE EXCEPTION 'history schema must differ from the source schema';
    END IF;
    FOR moving IN
     SELECT c.oid AS source_oid,c.relname::text AS relation_name,c.relkind::text AS relation_kind,
      {{catalog}}.min(a.attname::text) AS lot_column,{{catalog}}.count(*) AS lot_count
     FROM {{catalog}}.pg_class c
     JOIN {{catalog}}.pg_namespace n ON n.oid=c.relnamespace
     JOIN {{catalog}}.pg_attribute a ON a.attrelid=c.oid
     WHERE n.nspname={{SCHEMA_NAME}} AND c.relkind IN ('r','p')
      AND NOT EXISTS (
       SELECT 1 FROM {{catalog}}.pg_inherits i JOIN {{catalog}}.pg_class parent ON parent.oid=i.inhparent
       WHERE i.inhrelid=c.oid AND parent.relkind='p'
      )
      AND a.attnum>0 AND NOT a.attisdropped AND {{catalog}}.lower(a.attname::text)='biaoduanguid'
      AND {{catalog}}.lower(c.relname::text) NOT IN ('pingbiao_biaoduan','pingbiao_pw_logininfo',
       'pingbiao_movedatalog','pingbiao_movedataerror','pingbiao_meeting','pingbiao_buzhoulist')
     GROUP BY c.oid,c.relname,c.relkind
     ORDER BY c.relname
    LOOP
     failed_table := moving.relation_name;
     IF moving.lot_count<>1 THEN
      RAISE EXCEPTION 'BiaoDuanGuid is ambiguous in %',failed_table;
     END IF;
     SELECT c.oid,c.relname::text,c.relkind::text INTO STRICT history_table_oid,history_table_name,history_table_kind
      FROM {{catalog}}.pg_class c
      WHERE c.relnamespace=history_schema_oid AND c.relkind IN ('r','p')
       AND {{catalog}}.lower(c.relname::text)={{catalog}}.lower(moving.relation_name);
     IF history_table_oid=ANY(copied_tables) THEN
      RAISE EXCEPTION 'multiple source tables resolve to history table %',history_table_name;
     END IF;
     copied_tables := {{catalog}}.array_append(copied_tables,history_table_oid);
     source_sql := {{catalog}}.quote_ident({{SCHEMA_NAME}})||'.'||{{catalog}}.quote_ident(moving.relation_name);
     history_sql := {{catalog}}.quote_ident(history_schema)||'.'||{{catalog}}.quote_ident(history_table_name);
     IF moving.relation_kind='r' THEN
      source_sql := 'ONLY '||source_sql;
     END IF;
     EXECUTE 'LOCK TABLE '||source_sql||' IN SHARE ROW EXCLUSIVE MODE';
     EXECUTE 'LOCK TABLE ONLY '||history_sql||' IN SHARE ROW EXCLUSIVE MODE';
     IF moving.relation_kind='p' AND EXISTS ({{SOURCE_INHERITANCE}}) THEN
      RAISE EXCEPTION 'source table % has partitions outside the source schema',moving.relation_name;
     END IF;
     IF history_table_kind='p' AND EXISTS ({{HISTORY_INHERITANCE}}) THEN
      RAISE EXCEPTION 'history table % has partitions outside the history schema',history_table_name;
     END IF;
     source_columns := ARRAY[]::text[];
     history_columns := ARRAY[]::text[];
     FOR source_column IN
      SELECT a.attname::text AS name
      FROM {{catalog}}.pg_attribute a
      WHERE a.attrelid=moving.source_oid AND a.attnum>0 AND NOT a.attisdropped
      ORDER BY a.attnum
     LOOP
      SELECT a.attname::text INTO STRICT history_column_name
       FROM {{catalog}}.pg_attribute a
       WHERE a.attrelid=history_table_oid AND a.attnum>0 AND NOT a.attisdropped
        AND {{catalog}}.lower(a.attname::text)={{catalog}}.lower(source_column.name);
      source_columns := {{catalog}}.array_append(source_columns,{{catalog}}.quote_ident(source_column.name));
      history_columns := {{catalog}}.array_append(history_columns,{{catalog}}.quote_ident(history_column_name));
     END LOOP;
     EXECUTE 'SELECT '||{{CATALOG_LITERAL}}||'.count(*) FROM '||source_sql
      ||' WHERE '||{{catalog}}.quote_ident(moving.lot_column)||'=$1' INTO expected_rows USING p_biaoduanguid;
     EXECUTE 'INSERT INTO '||history_sql||' ('||{{catalog}}.array_to_string(history_columns,',')||') SELECT '
      ||{{catalog}}.array_to_string(source_columns,',')||' FROM '||source_sql
      ||' WHERE '||{{catalog}}.quote_ident(moving.lot_column)||'=$1' USING p_biaoduanguid;
     GET DIAGNOSTICS copied_rows=ROW_COUNT;
     IF copied_rows<>expected_rows THEN
      RAISE EXCEPTION 'history insert into % copied % of % rows',history_table_name,copied_rows,expected_rows;
     END IF;
     IF {{catalog}}.lower(moving.relation_name)<>'pingbiao_kaibiaotoubiao' THEN
      pending_deletes := {{catalog}}.array_append(pending_deletes,'DELETE FROM '||source_sql
       ||' WHERE '||{{catalog}}.quote_ident(moving.lot_column)||'=$1');
      pending_tables := {{catalog}}.array_append(pending_tables,moving.relation_name);
     END IF;
    END LOOP;
    FOR delete_index IN 1..coalesce({{catalog}}.array_length(pending_deletes,1),0) LOOP
     failed_table := pending_tables[delete_index];
     EXECUTE pending_deletes[delete_index] USING p_biaoduanguid;
    END LOOP;
    failed_table := 'pingbiao_biaoduan';
    EXECUTE {{MARK_MOVED}} USING history_schema,p_biaoduanguid;
   END IF;
  END IF;
  failed_table := 'pingbiao_movedatalog';
  EXECUTE {{LOG_SUCCESS}} USING p_biaoduanguid;
 EXCEPTION WHEN OTHERS THEN
  failure_message := ':'||SQLSTATE||SQLERRM;
 END;
 IF failure_message IS NOT NULL THEN
  EXECUTE {{LOG_ERROR}} USING failure_message,failed_table,p_biaoduanguid;
  EXECUTE {{LOG_FAILURE}} USING p_biaoduanguid;
 END IF;
`

func demoMoveAllDataToHistoryDBDDL(schema, catalog string, native bool) string {
	tables := []pgProcedureTable{
		{"lot", demoHistoryMain, strings.Fields("biaoduanguid pingbiaoisend historydb_name if_moved kaibiaodate")},
		{"config", demoHistoryConfig, strings.Fields("historydb_name isnowuse dbtype")},
	}
	return pgBusinessProcedureDDL(schema, catalog, native, "proc_pingbiao_movealldata2historydb", "", tables, "", ` history_schema text;
 lot_guid varchar;
 has_archive_procedure boolean;
`, `
 BEGIN
  EXECUTE {{HISTORY_SCHEMA}} INTO STRICT history_schema;
 EXCEPTION WHEN no_data_found THEN
  RETURN;
 END;
 IF coalesce({{catalog}}.char_length(history_schema),0)>0 THEN
  EXECUTE {{CHECK_ARCHIVE}} INTO has_archive_procedure;
  FOR lot_guid IN EXECUTE {{ELIGIBLE_LOTS}} LOOP
   IF NOT has_archive_procedure THEN
    RAISE EXCEPTION 'expected proc_pb_movedata2historydb(varchar) in the target schema';
   END IF;
   CALL {{schema}}."proc_pb_movedata2historydb"(lot_guid);
  END LOOP;
 END IF;
`, map[string]string{
		"HISTORY_SCHEMA": demoHistorySchemaQuery,
		"CHECK_ARCHIVE": `SELECT EXISTS (
 SELECT 1 FROM {{catalog}}.pg_proc p JOIN {{catalog}}.pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname={{schema_name}} AND p.proname='proc_pb_movedata2historydb' AND p.prokind='p'
  AND p.pronargs=1 AND p.proargtypes[0]='character varying'::regtype
  AND p.provariadic=0 AND p.pronargdefaults=0 AND (p.proargmodes IS NULL OR {{catalog}}.array_to_string(p.proargmodes,',')='i')
 )`,
		"ELIGIBLE_LOTS": `SELECT {{lot.biaoduanguid}} FROM {{lot}}
 WHERE coalesce({{catalog}}.char_length({{lot.historydb_name}}::text),0)=0 AND coalesce({{lot.if_moved}}::text,'0')<>'1'
  AND ({{catalog}}.statement_timestamp()::date-{{lot.kaibiaodate}}::date)>
   CASE WHEN {{lot.pingbiaoisend}}::text='1' THEN 28 ELSE 90 END
 ORDER BY {{lot.biaoduanguid}}`,
	})
}
