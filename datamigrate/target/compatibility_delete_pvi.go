package target

import "strings"

const deleteProcessVersionInstanceName = "deleteprocessversioninstance"

func deleteProcessVersionInstanceDDL(schema, catalog string, native bool) string {
	// The first column is the delete key. Keep workflow_pvi last so
	// dependent rows are removed before their owning instance.
	tables := []pgProcedureTable{
		{"attachinfo", "frame_attachinfo", []string{"cliengguid"}},
		{"attachstorage", "frame_attachstorage", []string{"cliengguid"}},
		{"activity", "workflow_activity_instance", []string{"processversioninstanceguid"}},
		{"activityhistory", "workflow_activity_instance_his", []string{"processversioninstanceguid"}},
		{"mistablerow", "workflow_pvi_mistablerow", []string{"pviguid"}},
		{"mistablerowhistory", "workflow_pvi_mistablerow_his", []string{"pviguid"}},
		{"material", "workflow_pvi_material", []string{"processversioninstanceguid"}},
		{"materialhistory", "workflow_pvi_material_his", []string{"processversioninstanceguid"}},
		{"transition", "workflow_transition_instance", []string{"processversioninstanceguid"}},
		{"transitionhistory", "workflow_transition_i_history", []string{"processversioninstanceguid"}},
		{"workitem", "workflow_workitem", []string{"processversioninstanceguid"}},
		{"workitemhistory", "workflow_workitem_history", []string{"processversioninstanceguid"}},
		{"opinion", "workflow_pvi_opinion", []string{"pviguid"}},
		{"opinionhistory", "workflow_pvi_opinion_his", []string{"pviguid"}},
		{"definition", "workflow_def", []string{"pviguid"}},
		{"context", "workflow_context_value", []string{"processversioninstanceguid"}},
		{"contexthistory", "workflow_context_value_history", []string{"processversioninstanceguid"}},
		{"message", "messages_center", []string{"pviguid"}},
		{"messagehistory", "messages_center_histroy", []string{"pviguid"}},
		{"pvi", "workflow_pvi", []string{"processversioninstanceguid", "mainpviguid"}},
	}
	queries := map[string]string{
		"LOCK_INSTANCES": `SELECT p.{{pvi.processversioninstanceguid}}::varchar AS guid,
 p.{{pvi.processversioninstanceguid}}=$1 AS is_root
 FROM {{pvi}} AS p
 WHERE p.{{pvi.processversioninstanceguid}}=$1 OR p.{{pvi.mainpviguid}}=$1
 ORDER BY p.{{pvi.processversioninstanceguid}}, p.{{pvi.mainpviguid}} FOR UPDATE`,
	}
	var statements strings.Builder
	statements.WriteString(` FOR candidate IN EXECUTE {{LOCK_INSTANCES}} USING p_dpviguid LOOP
  IF candidate.guid IS NOT NULL AND NOT candidate.is_root AND NOT (candidate.guid=ANY(target_guids)) THEN
   target_guids := {{catalog}}.array_append(target_guids, candidate.guid);
  END IF;
 END LOOP;
 target_guids := {{catalog}}.array_append(target_guids, p_dpviguid);
 FOREACH current_guid IN ARRAY target_guids LOOP
`)
	for _, table := range tables {
		deleteQuery := "DELETE_" + strings.ToUpper(table.alias)
		queries[deleteQuery] = "DELETE FROM {{" + table.alias + "}} AS item WHERE item.{{" + table.alias + "." + table.columns[0] + "}}=$1"
		statement := "  EXECUTE {{" + deleteQuery + "}} USING current_guid;\n"
		if table.alias == "message" || table.alias == "messagehistory" {
			statement = "  IF current_guid=p_dpviguid THEN\n " + statement + "  END IF;\n"
		}
		statements.WriteString(statement)
	}
	statements.WriteString(" END LOOP;\n")
	const guard = ` IF p_dpviguid IS NULL OR p_dpviguid='' THEN
  RETURN;
 END IF;`
	const declarations = ` target_guids varchar[] := ARRAY[]::varchar[];
 current_guid varchar;
 candidate record;`
	return pgBusinessProcedureDDL(schema, catalog, native, deleteProcessVersionInstanceName,
		"IN p_dpviguid varchar", tables, guard, declarations, statements.String(), queries)
}
