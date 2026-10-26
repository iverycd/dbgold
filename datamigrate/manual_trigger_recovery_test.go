package datamigrate

import (
	"testing"

	"dbgold/datamigrate/repair"
	"github.com/stretchr/testify/require"
)

func TestManualTriggerRecoveryPreservesAcceptedSQLAndRequiresColumnRenameReview(t *testing.T) {
	sql := `CREATE OR REPLACE TRIGGER "Dst"."AuditTrigger" AFTER DELETE ON "Dst"."Orders" FOR EACH ROW BEGIN INSERT INTO "Dst"."Audit" VALUES (:OLD."Id"); END;`
	trigger := repair.Object{ID: "trigger", Kind: "trigger", Table: "Orders", ColumnMapping: map[string]string{"Id": "Id"}, ManualTrigger: &repair.ManualTriggerPlan{SQL: sql, Revision: 2}, Units: []repair.Unit{{ID: "manual-2", SQL: sql, OriginalSQL: sql, Status: "failed", Selected: true}}}
	table := repair.Object{Kind: "table", Table: "Orders", ColumnMapping: map[string]string{"Id": "Id"}}
	unchanged := regenerateRepairCandidate(&mockWriter{}, Config{}, table, trigger, map[string]string{"Id": "Id"})
	require.Equal(t, trigger, unchanged)
	changed := regenerateRepairCandidate(&mockWriter{}, Config{}, table, trigger, map[string]string{"Id": "Renamed"})
	require.True(t, changed.RequiresReview)
	require.Equal(t, sql, changed.Units[0].SQL)
	require.Equal(t, trigger.ManualTrigger, changed.ManualTrigger)
	require.Equal(t, trigger.ColumnMapping, changed.ColumnMapping, "a mapping change cannot make stale authored SQL appear reviewed")
}
