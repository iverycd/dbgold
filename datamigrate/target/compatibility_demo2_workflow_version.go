package target

import (
	"fmt"
	"strings"
)

const demo2CopyProcessVersionName = "copyprocessversion"
const demo2DeleteProcessVersionName = "deleteprocessversion"
const demo2CopyProcessVersionParameters = "IN p_from_version varchar, IN p_to_version varchar, IN p_new_guid varchar"
const demo2DeleteProcessVersionParameters = "IN p_version varchar"

var demo2WorkflowVersionProcedureSpecs = []compatibilityProcedureSpec{
	{name: demo2CopyProcessVersionName, signature: "copyprocessversion(p_from_version varchar, p_to_version varchar, p_new_guid varchar)",
		inputs: []string{"character varying", "character varying", "character varying"}, modes: "i,i,i",
		ddl: func(schema, catalog string, native bool) string {
			return demo2CopyProcessVersionDDL(schema, catalog, native, qualifiedFunction(schema, "get_uuid")+"()::varchar", true, true)
		}},
	{name: demo2DeleteProcessVersionName, signature: "deleteprocessversion(p_version varchar)",
		inputs: []string{"character varying"}, modes: "i", ddl: demo2DeleteProcessVersionDDL},
}

type demo2WorkflowVersionCopyTable struct {
	name       string
	projection string
}

// The suffix on a column declares how the original routine computes its
// destination value. An unmarked column is copied unchanged.
var demo2WorkflowVersionCopyTables = []demo2WorkflowVersionCopyTable{
	{"workflow_activity", `vmlid activityguid=guid activitytype activityname processversionguid=version copyfromactivityguid=guid
 handleurl handlebodypageurl splittype jointype iconx icony priority callsubprocessguid callsubpvmethodguid
 subprocesssynctype transactorinfo multitransactormode is_allowwithdraw is_allowaddattachfile
 addattachfilesource=empty is_passiftransctorispresenter=null is_passwhennotransactor=null timelimitenable
 timelimit timelimitunit earlywarning_enable earlywarning_time earlywarning_timeunit earlywarningremindtype
 earlywarningremindcontent overtimeremindtype overtimeremindcontent overtimetransaction note
 applicationconfig smscontent ordernum activitydispname`},
	{"workflow_activity_fieldaccess", `rowguid=uuid activitymaterialguid=guid materialguid=guid activityguid=guid tableid fieldname
 fieldchinesename accessright processversionguid=version`},
	{"workflow_activity_linkinfo", `linkguid=uuid activityguid=guid linkname openmode openfeatures linkurl linkstyle linkclass ordernumber
 processversionguid=version`},
	{"workflow_activity_material", `activitymaterialguid=guid materialguid=guid submittype is_mustsubmit activityguid=guid accessright
 is_allowprint ordernum processversionguid=version mainpvguid mainpvactivityguid`},
	{"workflow_activity_operation", `operationguid=guid activityguid=guid operationname operationtype operationpageurl ordernumber
 transitionguid=guid openerpageurlafteroperate operationcommonpageurl defaultopinion note is_requireopinion
 targetactivity=list is_checkmaterialsubmit processversionguid=version`},
	{"workflow_config", `rowguid=uuid belongto sourceguid=guid configname configvalue note processversionguid=version`},
	{"workflow_context", `fieldguid=uuid belongto activityguid=guid fieldname fieldtype fieldvalue servercode valuesource
 frommaterialguid=guid frommistableid fromfieldname note processversionguid=version`},
	{"workflow_event", `eventguid=uuid belongto sourceguid=guid eventname eventtype eventmethodguid=guid synctype note ordernumber
 processversionguid=version`},
	{"workflow_invokemethod", `invokeguid=uuid invokegroupguid=guid methodguid=guid ordernum whenthrowexception is_enabled note
 processversionguid=version`},
	{"workflow_method", `methodguid=guid belongto activityguid=guid dllpath typename methodname returnvaluetype note is_log
 processversionguid=version`},
	{"workflow_method_parameter", `mpguid=uuid methodguid=guid mpname mptype mpvalue encrypt processversionguid=version`},
	{"workflow_participator", `participatorguid=uuid sourceguid=guid participatorname belongto type dataid belongdept methodguid=guid
 modetype processversionguid=version`},
	{"workflow_pv_material", `materialguid=guid materialname status source type pageurl_readandwrite template_asinfogroupguid=empty
 is_showingrid is_initmistable clienttag note submittype processversionguid=version mainpvguid
 mainpvactivityguid frommainpvmatguid`},
	{"workflow_pv_mistableset", `mistablesetguid=uuid materialguid=guid tableid sql_tablename processversionguid=version`},
	{"workflow_pv_printadapter", `rowguid=uuid frommaterialguidlist=list toprinttempleteguid printadaptername ordernum note
 is_addsqltablename processversionguid=version`},
	{"workflow_transition", `transitionguid=guid transitionname type is_showasoperationbutton transitiondispname fromactivityguid=guid
 toactivityguid=guid point is_default priority preworkpageurl workpageurl afterworkpageurl workitemmodename
 workitemnotifytype workitemnotifycontent targetactivitytransactorsource is_targettransactor_editable
 selecttransctormethodguid=guid vmlid ordernum is_sendtomessagecenter processversionguid=version`},
	{"workflow_transition_condition", `conditionguid=uuid conditionname transitionguid=guid conditionexpressiontype leftvalue compareoperation
 rightvalue conditionexpression valuetype methodguid=guid processversionguid=version`},
}

func demo2CopyProcessVersionDDL(schema, catalog string, native bool, uuidExpression string, hasGuid, hasGuidList bool) string {
	var tables []pgProcedureTable
	var statements strings.Builder
	queries := make(map[string]string)
	for index, table := range demo2WorkflowVersionCopyTables {
		var columns, destinations, values []string
		for _, mapping := range strings.Fields(table.projection) {
			column, action, _ := strings.Cut(mapping, "=")
			columns = append(columns, column)
			identifier := "{{" + table.name + "." + column + "}}"
			destinations = append(destinations, identifier)
			values = append(values, demo2VersionCopyValue("src."+identifier, action, hasGuid, hasGuidList))
		}
		tables = append(tables, pgProcedureTable{table.name, table.name, columns})
		query := "INSERT INTO {{" + table.name + "}} (" + strings.Join(destinations, ", ") + ") SELECT " +
			strings.Join(values, ", ") + " FROM "
		before, after, needsUUID := strings.Cut(query, "{{UUID_EXPRESSION}}")
		prefixKey := fmt.Sprintf("COPY_%d", index)
		queries[prefixKey] = before
		fmt.Fprintf(&statements, " EXECUTE {{%s}}", prefixKey)
		if needsUUID {
			uuidKey := fmt.Sprintf("UUID_SUFFIX_%d", index)
			queries[uuidKey] = after
			fmt.Fprintf(&statements, "||uuid_expression_sql||{{%s}}", uuidKey)
		}
		filterKey := fmt.Sprintf("FILTER_%d", index)
		queries[filterKey] = " AS src WHERE src.{{" + table.name + ".processversionguid}}=$1"
		fmt.Fprintf(&statements, "||scoped_relations[%d]||{{%s}} USING p_from_version,p_to_version,p_new_guid;\n", index+1, filterKey)
	}
	declarations := demo2ScopedRelationDeclarations + " uuid_expression_sql text := " + sqlFunctionBody(uuidExpression) + ";\n"
	return pgBusinessProcedureDDL(schema, catalog, native, demo2CopyProcessVersionName, demo2CopyProcessVersionParameters,
		tables, "", declarations, demo2ScopedRelations()+statements.String(), queries)
}

func demo2DeleteProcessVersionDDL(schema, catalog string, native bool) string {
	var tables []pgProcedureTable
	var statements strings.Builder
	queries := make(map[string]string)
	for _, table := range demo2WorkflowVersionCopyTables {
		tables = append(tables, pgProcedureTable{table.name, table.name, []string{"processversionguid"}})
	}
	tables = append(tables, pgProcedureTable{"workflow_processversion", "workflow_processversion", []string{"processversionguid"}})
	for index, table := range tables {
		filterKey := fmt.Sprintf("FILTER_%d", index)
		queries[filterKey] = " WHERE {{" + table.alias + ".processversionguid}}=$1"
		fmt.Fprintf(&statements, " EXECUTE 'DELETE FROM '||scoped_relations[%d]||{{%s}} USING p_version;\n", index+1, filterKey)
	}
	return pgBusinessProcedureDDL(schema, catalog, native, demo2DeleteProcessVersionName, demo2DeleteProcessVersionParameters,
		tables, "", demo2ScopedRelationDeclarations, demo2ScopedRelations()+statements.String(), queries)
}

func demo2VersionCopyValue(column, action string, hasGuid, hasGuidList bool) string {
	switch action {
	case "guid":
		return demo2VersionConvertGUID(column, hasGuid)
	case "list":
		if hasGuidList {
			return `{{schema}}."convertguidlist"($3,` + column + `::varchar)`
		}
		return demo2VersionConvertGUIDList(column, hasGuid)
	case "version":
		return "$2"
	case "uuid":
		return "{{UUID_EXPRESSION}}"
	case "empty":
		return "''"
	case "null":
		return "NULL"
	case "":
		return column
	default:
		panic("unknown workflow copy projection action: " + action)
	}
}

// Match ConvertGuid's character-versus-byte cutoff, trailing-space handling,
// and MySQL's negative SUBSTR start positions when the routine is unavailable.
func demo2VersionConvertGUID(column string, hasGuid bool) string {
	if hasGuid {
		return `{{schema}}."convertguid"($3,` + column + `::varchar)`
	}
	return strings.ReplaceAll(`(SELECT CASE
 WHEN input_value IS NULL THEN ''
 WHEN $3 IS NULL THEN NULL
 ELSE (CASE WHEN input_bytes>12 AND input_chars>12 THEN {{catalog}}.substr(input_value,1,input_chars-12) ELSE '' END) ||
  (CASE WHEN start_position>0 AND take_length>0 THEN {{catalog}}.substr($3,start_position,take_length) ELSE '' END)
 END
 FROM (SELECT {{INPUT}}::text AS input_value) AS guid_input
 CROSS JOIN LATERAL (SELECT {{catalog}}.char_length({{catalog}}.rtrim(input_value)) AS input_chars,
  {{catalog}}.octet_length({{catalog}}.convert_to({{catalog}}.rtrim(input_value),'UTF8')) AS input_bytes,
  {{catalog}}.char_length({{catalog}}.rtrim($3))-11 AS mysql_start) AS guid_lengths
 CROSS JOIN LATERAL (SELECT CASE WHEN mysql_start<0 THEN {{catalog}}.char_length($3)+mysql_start+1 ELSE mysql_start END AS start_position,
  CASE WHEN input_bytes<=12 THEN input_chars ELSE 12 END AS take_length) AS guid_bounds)`, "{{INPUT}}", column)
}

// ConvertGuidList remaps only semicolon-terminated items and preserves the
// unprocessed suffix. One NULL conversion makes the concatenated result NULL.
func demo2VersionConvertGUIDList(column string, hasGuid bool) string {
	return strings.NewReplacer("{{INPUT}}", column, "{{CONVERT_ITEM}}", demo2VersionConvertGUID("item.value", hasGuid)).Replace(`(SELECT CASE
 WHEN raw_value IS NULL OR {{catalog}}.strpos(raw_value,';')=0 THEN raw_value
 ELSE (SELECT CASE WHEN {{catalog}}.count(converted_value)={{catalog}}.count(*) THEN
  {{catalog}}.string_agg(converted_value::text,';' ORDER BY ordinal)||';'||parts[{{catalog}}.array_length(parts,1)] END
  FROM (SELECT item.ordinal,{{CONVERT_ITEM}} AS converted_value
   FROM {{catalog}}.unnest(parts) WITH ORDINALITY AS item(value,ordinal)
   WHERE item.ordinal<{{catalog}}.array_length(parts,1)) AS converted_items)
 END
 FROM (SELECT {{INPUT}}::text AS raw_value) AS list_input
 CROSS JOIN LATERAL (SELECT {{catalog}}.string_to_array(raw_value,';') AS parts) AS guid_list)`)
}
