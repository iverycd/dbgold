package target

import (
	"fmt"
	"strings"
)

var demo2CleanupProcedureSpecs = []compatibilityProcedureSpec{
	{name: "clearallpvi", signature: "clearallpvi()", ddl: demo2ClearAllPVIDDL},
	{name: "cleardirtydata", signature: "cleardirtydata()", ddl: demo2ClearDirtyDataDDL},
	{name: "delete_remail", signature: "delete_remail()", ddl: demo2DeleteReMailDDL},
}

func demo2CleanupDDL(schema, catalog string, native bool, name string, tables []pgProcedureTable, orderedSQL []string) string {
	queries := make(map[string]string, len(orderedSQL))
	var statements strings.Builder
	statements.WriteString(demo2ScopedRelations())
	statements.WriteString(" relation_sql := scoped_relations;\n")
	for index, query := range orderedSQL {
		key := fmt.Sprintf("CLEANUP_%d", index)
		queries[key] = query
		fmt.Fprintf(&statements, " EXECUTE {{%s}};\n", key)
	}
	return pgBusinessProcedureDDL(schema, catalog, native, name, "", tables, "",
		demo2ScopedRelationDeclarations, statements.String(), queries)
}

const demo2ScopedRelationDeclarations = ` scoped_relations text[] := ARRAY[]::text[];
 relation_kind text;
 relation_schema oid;
`

// Ordinary inheritance can reach other tables implicitly. Restrict ordinary
// tables to their own rows and lock partition roots before checking their scope.
func demo2ScopedRelations() string {
	return strings.ReplaceAll(` FOR table_index IN 1..{{catalog}}.array_length(relation_sql,1) LOOP
  SELECT c.oid,c.relkind::text,c.relnamespace INTO STRICT relation_oid,relation_kind,relation_schema
   FROM {{catalog}}.pg_class c WHERE c.oid=relation_sql[table_index]::{{catalog}}.regclass;
  IF relation_kind='p' THEN
   EXECUTE 'LOCK TABLE ONLY '||relation_sql[table_index]||' IN SHARE ROW EXCLUSIVE MODE';
   IF EXISTS ({{DESCENDANTS}}) THEN
    RAISE EXCEPTION 'table % has partitions outside the target schema',relation_sql[table_index];
   END IF;
   scoped_relations := {{catalog}}.array_append(scoped_relations,relation_sql[table_index]);
  ELSE
   scoped_relations := {{catalog}}.array_append(scoped_relations,'ONLY '||relation_sql[table_index]);
  END IF;
 END LOOP;
`, "{{DESCENDANTS}}", demoCrossSchemaDescendants("relation_oid", "relation_schema", "{{catalog}}"))
}

func demo2ClearAllPVIDDL(schema, catalog string, native bool) string {
	tables := []pgProcedureTable{
		{"storage", "workflow_attachstorage", []string{"attachguid"}},
		{"storageinfo", "workflow_attachstorageinfo", strings.Fields("attachstorageguid attachstorageinfogroupguid")},
		{"template", "workflow_pv_material", []string{"template_asinfogroupguid"}},
		{"activity", "workflow_activity_instance", nil},
		{"material", "workflow_pvi_material", nil},
		{"mistable", "workflow_pvi_mistablerow", nil},
		{"transition", "workflow_transition_instance", nil},
		{"message", "messages_center", []string{"messageitemguid"}},
		{"messagehistory", "messages_center_histroy", []string{"messageitemguid"}},
		{"workitem", "workflow_workitem", []string{"workitemguid"}},
		{"notification", "workflow_workitem_notify", nil},
		{"pvi", "workflow_pvi", nil},
	}
	return demo2CleanupDDL(schema, catalog, native, "clearallpvi", tables, []string{
		`DELETE FROM {{storage}} AS s WHERE s.{{storage.attachguid}} NOT IN
 (SELECT i.{{storageinfo.attachstorageguid}} FROM {{storageinfo}} AS i WHERE i.{{storageinfo.attachstorageinfogroupguid}} IN
  (SELECT m.{{template.template_asinfogroupguid}} FROM {{template}} AS m))`,
		`DELETE FROM {{storageinfo}} AS i WHERE i.{{storageinfo.attachstorageinfogroupguid}} NOT IN
 (SELECT m.{{template.template_asinfogroupguid}} FROM {{template}} AS m)`,
		"DELETE FROM {{activity}}",
		"DELETE FROM {{material}}",
		"DELETE FROM {{mistable}}",
		"DELETE FROM {{transition}}",
		`DELETE FROM {{message}} AS m WHERE m.{{message.messageitemguid}} IN
 (SELECT w.{{workitem.workitemguid}} FROM {{workitem}} AS w)`,
		`DELETE FROM {{messagehistory}} AS m WHERE m.{{messagehistory.messageitemguid}} IN
 (SELECT w.{{workitem.workitemguid}} FROM {{workitem}} AS w)`,
		"DELETE FROM {{workitem}}",
		"DELETE FROM {{notification}}",
		"DELETE FROM {{pvi}}",
	})
}

func demo2ClearDirtyDataDDL(schema, catalog string, native bool) string {
	tables := []pgProcedureTable{
		{"process", "workflow_process", []string{"processguid"}},
		{"version", "workflow_processversion", strings.Fields("processguid processversionguid")},
		{"material", "workflow_pv_material", strings.Fields("processversionguid materialguid")},
		{"mistable", "workflow_pv_mistableset", []string{"materialguid"}},
		{"print", "workflow_pv_printadapter", []string{"processversionguid"}},
		{"context", "workflow_context", []string{"processversionguid"}},
		{"activity", "workflow_activity", strings.Fields("processversionguid activityguid")},
		{"activitymaterial", "workflow_activity_material", strings.Fields("activityguid materialguid activitymaterialguid")},
		{"access", "workflow_activity_fieldaccess", []string{"activitymaterialguid"}},
		{"operation", "workflow_activity_operation", strings.Fields("activityguid operationguid")},
		{"event", "workflow_event", strings.Fields("eventtype sourceguid")},
		{"config", "workflow_config", strings.Fields("belongto sourceguid")},
		{"transition", "workflow_transition", strings.Fields("transitionguid processversionguid")},
		{"participant", "workflow_participator", strings.Fields("sourceguid processversionguid belongto")},
	}
	// Keep NOT IN predicates: NULL reference keys suppress deletion in the source.
	return demo2CleanupDDL(schema, catalog, native, "cleardirtydata", tables, []string{
		`DELETE FROM {{version}} AS v WHERE v.{{version.processguid}} NOT IN
 (SELECT p.{{process.processguid}} FROM {{process}} AS p)`,
		`DELETE FROM {{material}} AS m WHERE m.{{material.processversionguid}} NOT IN
 (SELECT v.{{version.processversionguid}} FROM {{version}} AS v)`,
		`DELETE FROM {{mistable}} AS t WHERE t.{{mistable.materialguid}} NOT IN
 (SELECT m.{{material.materialguid}} FROM {{material}} AS m)`,
		`DELETE FROM {{print}} AS p WHERE p.{{print.processversionguid}} NOT IN
 (SELECT v.{{version.processversionguid}} FROM {{version}} AS v)`,
		`DELETE FROM {{context}} AS c WHERE c.{{context.processversionguid}} NOT IN
 (SELECT v.{{version.processversionguid}} FROM {{version}} AS v)`,
		`DELETE FROM {{activity}} AS a WHERE a.{{activity.processversionguid}} NOT IN
 (SELECT v.{{version.processversionguid}} FROM {{version}} AS v)`,
		`DELETE FROM {{activitymaterial}} AS m WHERE m.{{activitymaterial.activityguid}} NOT IN
 (SELECT a.{{activity.activityguid}} FROM {{activity}} AS a) OR m.{{activitymaterial.materialguid}} NOT IN
 (SELECT v.{{material.materialguid}} FROM {{material}} AS v)`,
		`DELETE FROM {{access}} AS f WHERE f.{{access.activitymaterialguid}} NOT IN
 (SELECT m.{{activitymaterial.activitymaterialguid}} FROM {{activitymaterial}} AS m)`,
		`DELETE FROM {{operation}} AS o WHERE o.{{operation.activityguid}} NOT IN
 (SELECT a.{{activity.activityguid}} FROM {{activity}} AS a)`,
		`DELETE FROM {{event}} AS e WHERE e.{{event.eventtype}} IN ('10','15','20','25') AND e.{{event.sourceguid}} NOT IN
 (SELECT a.{{activity.activityguid}} FROM {{activity}} AS a)`,
		`DELETE FROM {{event}} AS e WHERE e.{{event.eventtype}} IN ('50','55') AND e.{{event.sourceguid}} NOT IN
 (SELECT o.{{operation.operationguid}} FROM {{operation}} AS o)`,
		`DELETE FROM {{config}} AS c WHERE c.{{config.belongto}}='22' AND c.{{config.sourceguid}} NOT IN
 (SELECT o.{{operation.operationguid}} FROM {{operation}} AS o)`,
		`DELETE FROM {{config}} AS c WHERE c.{{config.belongto}}='10' AND c.{{config.sourceguid}} NOT IN
 (SELECT v.{{version.processversionguid}} FROM {{version}} AS v)`,
		`DELETE FROM {{config}} AS c WHERE c.{{config.belongto}}='20' AND c.{{config.sourceguid}} NOT IN
 (SELECT a.{{activity.activityguid}} FROM {{activity}} AS a)`,
		`DELETE FROM {{config}} AS c WHERE c.{{config.belongto}}='30' AND c.{{config.sourceguid}} NOT IN
 (SELECT t.{{transition.transitionguid}} FROM {{transition}} AS t)`,
		`DELETE FROM {{config}} AS c WHERE c.{{config.belongto}}='40' AND c.{{config.sourceguid}} NOT IN
 (SELECT m.{{material.materialguid}} FROM {{material}} AS m)`,
		`DELETE FROM {{participant}} AS p WHERE p.{{participant.sourceguid}}::text='' OR p.{{participant.sourceguid}} IS NULL`,
		`DELETE FROM {{participant}} AS p WHERE p.{{participant.processversionguid}} NOT IN
 (SELECT v.{{version.processversionguid}} FROM {{version}} AS v)`,
		`DELETE FROM {{participant}} AS p WHERE p.{{participant.belongto}} IN ('30','35') AND p.{{participant.sourceguid}} NOT IN
 (SELECT t.{{transition.transitionguid}} FROM {{transition}} AS t)`,
		`DELETE FROM {{participant}} AS p WHERE p.{{participant.belongto}} IN ('10','15','20') AND p.{{participant.sourceguid}} NOT IN
 (SELECT a.{{activity.activityguid}} FROM {{activity}} AS a)`,
		`UPDATE {{participant}} AS p SET {{participant.processversionguid}}=
 (SELECT a.{{activity.processversionguid}} FROM {{activity}} AS a WHERE a.{{activity.activityguid}}=p.{{participant.sourceguid}})
 WHERE p.{{participant.processversionguid}} IS NULL AND p.{{participant.belongto}} IN ('10','15','20')`,
		`UPDATE {{participant}} AS p SET {{participant.processversionguid}}=
 (SELECT t.{{transition.processversionguid}} FROM {{transition}} AS t WHERE t.{{transition.transitionguid}}=p.{{participant.sourceguid}})
 WHERE p.{{participant.processversionguid}} IS NULL AND p.{{participant.belongto}} IN ('30','35')`,
	})
}

func demo2DeleteReMailDDL(schema, catalog string, native bool) string {
	tables := []pgProcedureTable{{"mail", "dbmail_allmails", strings.Fields("mailguid row_id")}}
	// A strict greater-than comparison preserves minimum ties and null row IDs.
	return demo2CleanupDDL(schema, catalog, native, "delete_remail", tables, []string{
		`DELETE FROM {{mail}} AS duplicate USING (
 SELECT m.{{mail.mailguid}} AS guid,{{catalog}}.min(m.{{mail.row_id}}::bigint) AS first_id
 FROM {{mail}} AS m
 WHERE m.{{mail.mailguid}} IS NOT NULL AND {{catalog}}.char_length(m.{{mail.mailguid}}::text)>0
 GROUP BY m.{{mail.mailguid}}
 ) AS keeper
 WHERE duplicate.{{mail.mailguid}}=keeper.guid AND duplicate.{{mail.row_id}}::bigint>keeper.first_id`,
	})
}
