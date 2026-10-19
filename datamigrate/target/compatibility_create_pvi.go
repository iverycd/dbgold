package target

import (
	"fmt"
	"regexp"
	"strings"

	"dbgold/datamigrate/source"
)

type createPVITableSpec struct {
	alias, name string
	columns     []string
}

// These are the exact read and write dependencies of the supplied CreatePVI.
// Target columns omitted from its INSERTs retain their database defaults.
// The source material's attachment field is not read: the instance gets ”.
var createPVITables = []createPVITableSpec{
	{"version", "workflow_processversion", strings.Fields(`processversionguid processversionname status processguid`)},
	{"process", "workflow_process", strings.Fields(`processname processguid`)},
	{"activity", "workflow_activity", strings.Fields(`activityguid activityname processversionguid activitytype handleurl priority`)},
	{"transition", "workflow_transition", strings.Fields(`transitionguid toactivityguid workitemmodename fromactivityguid is_showasoperationbutton`)},
	{"context", "workflow_context", strings.Fields(`fieldname fieldtype fieldvalue processversionguid`)},
	{"material", "workflow_pv_material", strings.Fields(`materialguid materialname source type pageurl_readandwrite pageurl_read pageurl_print is_showingrid clienttag is_initmistable processversionguid status mainpvactivityguid`)},
	{"mistableset", "workflow_pv_mistableset", strings.Fields(`materialguid tableid sql_tablename`)},
	{"pvi", "workflow_pvi", strings.Fields(`processversioninstanceguid processversioninstancename processversionguid initiator startdate status is_subprocessversioninstance activityguid_callme activityinstanceguid_callme synctype note processguid is_needsendearlywarning is_needsendovertimenotify mainpviguid mainpvactivityguid lockstatus`)},
	{"contextvalue", "workflow_context_value", strings.Fields(`fieldguid processversioninstanceguid fieldname fieldtype fieldvalue`)},
	{"pvimaterial", "workflow_pvi_material", strings.Fields(`materialinstanceguid materialinstancename adddate is_defined processversioninstanceguid materialguid materialname source type attachstorageinfogroupguid pageurl_readandwrite pageurl_read pageurl_print status is_showingrid clienttag is_initmistable`)},
	{"mistablerow", "workflow_pvi_mistablerow", strings.Fields(`materialinstanceguid tableid sql_tablename pviguid mistablesetinstanceguid`)},
	{"activityinstance", "workflow_activity_instance", strings.Fields(`activityinstanceguid processversionguid processversioninstanceguid subprocessversioninstanceguid activityguid startdate status rollbacktag note`)},
	{"transitioninstance", "workflow_transition_instance", strings.Fields(`transitioninstanceguid transitionguid processversioninstanceguid sourceactiviryguid sourceactiviryname srcactinstguid srcactinstname targetactivityguid targetactivityname tgtactinstguid tgtactinstname type transitdate direction`)},
	{"workitem", "workflow_workitem", strings.Fields(`workitemguid activityguid activityname activityinstanceguid workitemname workitemtype workitemmodename handleurl status priority senderguid sendername transactor transactorname jobguid waithandleguid createdate limitminutes timelimit timelimitunit is_needsendearlywarning is_needsendovertimenotify processguid processversionguid processversionname processversioninstanceguid processversioninstancename groupguid agentforuserguid agentforworkitemguid operatorfordisplayguid operatorfordisplayname note`)},
	{"message", "messages_center", strings.Fields(`messageitemguid title messagetype content sendmode generatedate isschedule schedulesenddate messagetarget targetuser targetdispname fromuser fromdispname frommobile handleurl beizhu ouguid baseouguid isshow jobguid handletype clientidentifier clienttag isdel isnohandle pviguid overtimepoint earlywarningpoint`)},
}

var createPVIIdentifier = regexp.MustCompile(`\{\{([a-z]+(?:\.[a-z_]+)?)\}\}`)

// Only fixed application statements pass through this renderer. Table and
// column expressions refer to the quoted catalog names resolved at invocation;
// every business value is supplied separately by EXECUTE ... USING.
func createPVISQL(query string, identifiers map[string]string) string {
	var fragments []string
	start := 0
	for _, match := range createPVIIdentifier.FindAllStringSubmatchIndex(query, -1) {
		if match[0] > start {
			fragments = append(fragments, sqlFunctionBody(query[start:match[0]]))
		}
		token := query[match[2]:match[3]]
		expression, ok := identifiers[token]
		if !ok {
			panic("unknown CreatePVI identifier: " + token)
		}
		fragments = append(fragments, expression)
		start = match[1]
	}
	if start < len(query) {
		fragments = append(fragments, sqlFunctionBody(query[start:]))
	}
	return strings.Join(fragments, "||")
}

func createPVIDDL(schema, catalog string, native bool) string {
	var tables, columns []string
	identifiers := map[string]string{"uuid": sqlFunctionBody(qualifiedFunction(schema, "get_uuid"))}
	columnIndex := 0
	for i, table := range createPVITables {
		tables = append(tables, table.name)
		columns = append(columns, strings.Join(table.columns, ","))
		identifiers[table.alias] = fmt.Sprintf("relation_sql[%d]", i+1)
		for _, column := range table.columns {
			columnIndex++
			identifiers[table.alias+"."+column] = fmt.Sprintf("column_sql[%d]", columnIndex)
		}
	}
	array := func(values []string) string { return "ARRAY['" + strings.Join(values, "','") + "']::text[]" }
	var replacements []string
	for name, query := range createPVIQueries {
		replacements = append(replacements, "{{"+name+"}}", createPVISQL(query, identifiers))
	}
	// INSERT column order comes from the explicit supplied field lists. Source
	// projections below remain independently resolved for mixed-case tables.
	for _, table := range createPVITables[7:] {
		var fields []string
		for _, column := range table.columns {
			fields = append(fields, "{{"+table.alias+"."+column+"}}")
		}
		prefix := "INSERT INTO {{" + table.alias + "}} (" + strings.Join(fields, ",") + ") "
		replacements = append(replacements, "{{INSERT_"+strings.ToUpper(table.alias)+"}}", createPVISQL(prefix, identifiers))
	}
	replacements = append(replacements,
		"{{TABLES}}", array(tables), "{{COLUMNS}}", array(columns),
		"{{SCHEMA}}", sqlFunctionBody(schema), "{{CAT}}", source.QuotePGIdent(catalog),
		"{{UUID_QUERY}}", sqlFunctionBody("SELECT "+qualifiedFunction(schema, "get_uuid")+"()::varchar"),
	)
	body := strings.NewReplacer(replacements...).Replace(createPVIBody)
	// The optional INOUT value replaces MySQL's SELECT T_HandleURL result set.
	// A six-input CALL still works and returns the resulting URL as one row.
	header := "CREATE PROCEDURE " + qualifiedFunction(schema, "createpvi") +
		"(IN p_processguid varchar, IN p_processversioninstanceguid varchar, IN p_userguid varchar, IN p_username varchar, IN p_workitemguid varchar, IN p_ouguid varchar, INOUT p_handleurl varchar DEFAULT NULL)"
	if native {
		return header + " SECURITY INVOKER AS\n" + body
	}
	return header + " LANGUAGE plpgsql SECURITY INVOKER AS " + sqlFunctionBody(body) + ";"
}

var createPVIQueries = map[string]string{
	"VERSION":        `SELECT v.{{version.processversionguid}},v.{{version.processversionname}} FROM {{version}} AS v WHERE v.{{version.status}}::text='10' AND v.{{version.processguid}}=$1`,
	"PROCESS":        `SELECT p.{{process.processname}} FROM {{process}} AS p WHERE p.{{process.processguid}}=$1`,
	"START_ACTIVITY": `SELECT a.{{activity.activityguid}},a.{{activity.activityname}} FROM {{activity}} AS a WHERE a.{{activity.processversionguid}}=$1 AND a.{{activity.activitytype}}::text='10'`,
	"TRANSITION":     `SELECT t.{{transition.transitionguid}},t.{{transition.toactivityguid}},t.{{transition.workitemmodename}} FROM {{transition}} AS t WHERE t.{{transition.fromactivityguid}}=$1 AND t.{{transition.is_showasoperationbutton}}::text='20'`,
	"FIRST_ACTIVITY": `SELECT a.{{activity.activityname}},a.{{activity.handleurl}},a.{{activity.priority}} FROM {{activity}} AS a WHERE a.{{activity.activityguid}}=$1`,
	"CONTEXT_VALUES": `SELECT {{uuid}}()::varchar,$1,c.{{context.fieldname}},c.{{context.fieldtype}},c.{{context.fieldvalue}} FROM {{context}} AS c WHERE c.{{context.processversionguid}}=$2`,
	// Keep the outer IN predicate: a MaterialGuid can occur in other versions.
	"MATERIAL_VALUES":    `SELECT $1,m.{{material.materialname}},$2,'10',$3,m.{{material.materialguid}},m.{{material.materialname}},m.{{material.source}},m.{{material.type}},'',m.{{material.pageurl_readandwrite}},m.{{material.pageurl_read}},m.{{material.pageurl_print}},'10',m.{{material.is_showingrid}},m.{{material.clienttag}},m.{{material.is_initmistable}} FROM {{material}} AS m WHERE m.{{material.materialguid}} IN (SELECT chosen.{{material.materialguid}} FROM {{material}} AS chosen WHERE chosen.{{material.processversionguid}}=$4 AND chosen.{{material.status}}::text='10' AND chosen.{{material.mainpvactivityguid}} IS NULL)`,
	"MISTABLEROW_VALUES": `SELECT $1,m.{{mistableset.tableid}},m.{{mistableset.sql_tablename}},$2,{{uuid}}()::varchar FROM {{mistableset}} AS m WHERE m.{{mistableset.materialguid}} IN (SELECT chosen.{{material.materialguid}} FROM {{material}} AS chosen WHERE chosen.{{material.processversionguid}}=$3 AND chosen.{{material.status}}::text='10' AND chosen.{{material.mainpvactivityguid}} IS NULL)`,
}

const createPVIBody = `DECLARE
 required_tables text[] := {{TABLES}};
 required_columns text[] := {{COLUMNS}};
 relation_sql text[] := ARRAY[]::text[];
 column_sql text[] := ARRAY[]::text[];
 relation_oid oid;
 relation_name text;
 match_count integer;
 table_index integer;
 required_column text;
 actual_column text;
 process_version_guid varchar;
 process_version_name varchar;
 instance_name varchar;
 add_date timestamp;
 material_instance_guid varchar;
 start_activity_instance_guid varchar;
 start_activity_guid varchar;
 start_activity_name varchar;
 first_activity_instance_guid varchar;
 first_activity_guid varchar;
 transition_guid varchar;
 transition_instance_guid varchar;
 first_activity_name varchar;
 handle_url varchar;
 work_item_name varchar;
 suffix_str varchar;
 priority_value integer;
 work_item_mode_name varchar;
 wait_handle_guid varchar;
BEGIN
 -- Resolve all read and write dependencies before creating any workflow data.
 FOR table_index IN 1..{{CAT}}.array_length(required_tables,1) LOOP
  SELECT {{CAT}}.count(*),{{CAT}}.min(c.oid::bigint)::oid,{{CAT}}.min(c.relname::text)
   INTO match_count,relation_oid,relation_name
   FROM {{CAT}}.pg_class c JOIN {{CAT}}.pg_namespace n ON n.oid=c.relnamespace
   WHERE n.nspname={{SCHEMA}} AND {{CAT}}.lower(c.relname::text)=required_tables[table_index]
   AND c.relkind IN ('r','p');
  IF match_count=0 THEN
   RAISE EXCEPTION 'createpvi: table % is missing from the target schema',required_tables[table_index];
  ELSIF match_count<>1 THEN
   RAISE EXCEPTION 'createpvi: table % is ambiguous in the target schema',required_tables[table_index];
  END IF;
  relation_sql := {{CAT}}.array_append(relation_sql,{{CAT}}.quote_ident({{SCHEMA}})||'.'||{{CAT}}.quote_ident(relation_name));
  FOREACH required_column IN ARRAY {{CAT}}.string_to_array(required_columns[table_index],',') LOOP
   SELECT {{CAT}}.count(*),{{CAT}}.min(a.attname::text) INTO match_count,actual_column
    FROM {{CAT}}.pg_attribute a
    WHERE a.attrelid=relation_oid AND a.attnum>0 AND NOT a.attisdropped
    AND {{CAT}}.lower(a.attname::text)=required_column;
   IF match_count=0 THEN
    RAISE EXCEPTION 'createpvi: column % is missing from %',required_column,required_tables[table_index];
   ELSIF match_count<>1 THEN
    RAISE EXCEPTION 'createpvi: column % is ambiguous in %',required_column,required_tables[table_index];
   END IF;
   column_sql := {{CAT}}.array_append(column_sql,{{CAT}}.quote_ident(actual_column));
  END LOOP;
 END LOOP;

 BEGIN
  EXECUTE {{VERSION}} INTO STRICT process_version_guid,process_version_name USING p_processguid;
 EXCEPTION WHEN NO_DATA_FOUND OR TOO_MANY_ROWS THEN
  RAISE EXCEPTION 'createpvi: expected exactly one active process version';
 END;
 IF process_version_guid IS NULL OR process_version_guid='' THEN
  RAISE EXCEPTION 'createpvi: active process version GUID is missing';
 END IF;
 BEGIN
  EXECUTE {{PROCESS}} INTO STRICT instance_name USING p_processguid;
 EXCEPTION WHEN NO_DATA_FOUND OR TOO_MANY_ROWS THEN
  RAISE EXCEPTION 'createpvi: expected exactly one process';
 END;
 BEGIN
  EXECUTE {{START_ACTIVITY}} INTO STRICT start_activity_guid,start_activity_name USING process_version_guid;
 EXCEPTION WHEN NO_DATA_FOUND OR TOO_MANY_ROWS THEN
  RAISE EXCEPTION 'createpvi: expected exactly one start activity';
 END;
 IF start_activity_guid IS NULL OR start_activity_guid='' THEN
  RAISE EXCEPTION 'createpvi: start activity GUID is missing';
 END IF;
 BEGIN
  EXECUTE {{TRANSITION}} INTO STRICT transition_guid,first_activity_guid,work_item_mode_name USING start_activity_guid;
 EXCEPTION WHEN NO_DATA_FOUND OR TOO_MANY_ROWS THEN
  RAISE EXCEPTION 'createpvi: expected exactly one transition after the start activity';
 END;
 IF transition_guid IS NULL OR transition_guid='' OR first_activity_guid IS NULL OR first_activity_guid='' THEN
  RAISE EXCEPTION 'createpvi: transition or first activity GUID is missing';
 END IF;
 BEGIN
  EXECUTE {{FIRST_ACTIVITY}} INTO STRICT first_activity_name,handle_url,priority_value USING first_activity_guid;
 EXCEPTION WHEN NO_DATA_FOUND OR TOO_MANY_ROWS THEN
  RAISE EXCEPTION 'createpvi: expected exactly one first activity';
 END;

 add_date := {{CAT}}.statement_timestamp()::timestamp;
 EXECUTE {{UUID_QUERY}} INTO material_instance_guid;
 EXECUTE {{UUID_QUERY}} INTO start_activity_instance_guid;
 EXECUTE {{UUID_QUERY}} INTO first_activity_instance_guid;
 EXECUTE {{UUID_QUERY}} INTO wait_handle_guid;
 EXECUTE {{UUID_QUERY}} INTO transition_instance_guid;
 work_item_name := '【'||first_activity_name||'】'||instance_name;
 suffix_str := 'ProcessVersionInstanceGuid='||p_processversioninstanceguid||'&WorkItemGuid='||p_workitemguid;
 -- MySQL's source INSTR arguments were reversed. Preserve an existing query.
 IF {{CAT}}.strpos(handle_url,'?')>0 THEN
  handle_url := handle_url||'&'||suffix_str;
 ELSE
  handle_url := handle_url||'?'||suffix_str;
 END IF;

 EXECUTE {{INSERT_PVI}}||'VALUES ($1,$2,$3,$4,$5,''10'',''20'','''','''',''10'','''',$6,''20'',''20'','''','''',''20'')'
  USING p_processversioninstanceguid,instance_name,process_version_guid,p_userguid,add_date,p_processguid;
 EXECUTE {{INSERT_CONTEXTVALUE}}||{{CONTEXT_VALUES}}
  USING p_processversioninstanceguid,process_version_guid;
 -- The source deliberately shares one material instance GUID across materials.
 EXECUTE {{INSERT_PVIMATERIAL}}||{{MATERIAL_VALUES}}
  USING material_instance_guid,add_date,p_processversioninstanceguid,process_version_guid;
 EXECUTE {{INSERT_MISTABLEROW}}||{{MISTABLEROW_VALUES}}
  USING material_instance_guid,p_processversioninstanceguid,process_version_guid;
 EXECUTE {{INSERT_ACTIVITYINSTANCE}}||'VALUES ($1,$2,$3,'''',$4,$5,''80'',''20'','''')'
  USING start_activity_instance_guid,process_version_guid,p_processversioninstanceguid,start_activity_guid,add_date;
 EXECUTE {{INSERT_ACTIVITYINSTANCE}}||'VALUES ($1,$2,$3,'''',$4,$5,''20'',''20'','''')'
  USING first_activity_instance_guid,process_version_guid,p_processversioninstanceguid,first_activity_guid,add_date;
 EXECUTE {{INSERT_TRANSITIONINSTANCE}}||'VALUES ($1,$2,$3,$4,$5,$6,$5,$7,$8,$9,$8,''20'',$10,''10'')'
  USING transition_instance_guid,transition_guid,p_processversioninstanceguid,start_activity_guid,start_activity_name,
   start_activity_instance_guid,first_activity_guid,first_activity_name,first_activity_instance_guid,add_date;
 EXECUTE {{INSERT_WORKITEM}}||'VALUES ($1,$2,$3,$4,$5,''FirstWorkItem'',$6,$7,''10'',$8,$9,$10,$9,$10,'''',$11,$12,''-1'',''-1'',''50'',''20'',''20'',$13,$14,$15,$16,$17,'''','''','''',$9,$10,'''')'
  USING p_workitemguid,first_activity_guid,first_activity_name,first_activity_instance_guid,work_item_name,
   work_item_mode_name,handle_url,priority_value,p_userguid,p_username,wait_handle_guid,add_date,
   p_processguid,process_version_guid,process_version_name,p_processversioninstanceguid,instance_name;
 EXECUTE {{INSERT_MESSAGE}}||'VALUES ($1,$2,$3,'''',''4'',$4,''0'',NULL,'''',$5,$6,$5,$6,'''',$7,$8,$9,'''',''0'','''',$3,$10,'''',''0'',''0'',$11,NULL,NULL)'
  USING wait_handle_guid,work_item_name,work_item_mode_name,add_date,p_userguid,p_username,handle_url,
   first_activity_name,p_ouguid,p_processguid,p_processversioninstanceguid;
 p_handleurl := handle_url;
 -- No COMMIT: the complete creation participates in the caller's transaction.
END;`
