package target

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type workflowDeleteFixtureTable struct {
	name, key string
	rootOnly  bool
}

// Keep the fixture independent of the DDL's dependency list. Each table
// receives rows for the root, direct children, grandchild and unrelated GUID.
var workflowDeleteFixtureTables = []workflowDeleteFixtureTable{
	{"Frame_AttachInfo", "CliengGuid", false},
	{"frame_attachstorage", "cliengguid", false},
	{"Workflow_Activity_Instance", "ProcessVersionInstanceGuid", false},
	{"workflow_activity_instance_his", "processversioninstanceguid", false},
	{"Workflow_PVI_Material", "ProcessVersionInstanceGuid", false},
	{"workflow_pvi_material_his", "processversioninstanceguid", false},
	{"Workflow_PVI_MISTableRow", "PVIGuid", false},
	{"workflow_pvi_mistablerow_his", "pviguid", false},
	{"Workflow_Transition_Instance", "ProcessVersionInstanceGuid", false},
	{"workflow_transition_i_history", "processversioninstanceguid", false},
	{"Workflow_WorkItem", "ProcessVersionInstanceGuid", false},
	{"workflow_workitem_history", "processversioninstanceguid", false},
	{"Workflow_PVI_Opinion", "PVIGuid", false},
	{"workflow_pvi_opinion_his", "pviguid", false},
	{"Workflow_Def", "PVIGuid", false},
	{"Workflow_PVI", "ProcessVersionInstanceGuid", false},
	{"Workflow_Context_Value", "ProcessVersionInstanceGuid", false},
	{"workflow_context_value_history", "processversioninstanceguid", false},
	{"Messages_Center", "PVIGuid", true},
	{"messages_center_histroy", "pviguid", true},
}

func TestCompatibilityWorkflowDeleteSQLIntegration(t *testing.T) {
	testPGCompatibilityProducts(t, func(t *testing.T, ctx context.Context, db *sql.DB, product string) {
		t.Run("scope_and_caller_rollback", func(t *testing.T) {
			schema := newBusinessIntegrationSchema(t, ctx, db)
			report, err := ensurePGProcedureCompatibility(ctx, db, schema, product, []string{"DELETEProcessVersionInstance"})
			require.NoError(t, err)
			require.Equal(t, 1, report.Created)
			call := "CALL " + qualifiedFunction(schema, "deleteprocessversioninstance") + "($1::varchar)"
			businessExec(t, ctx, db, call, nil)
			businessExec(t, ctx, db, call, "")
			seedWorkflowDeleteFixture(t, ctx, db, schema)

			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer tx.Rollback()
			_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO pg_catalog")
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, call, "root")
			require.NoError(t, err)
			require.NoError(t, tx.Rollback())
			checkWorkflowDeleteFixture(t, ctx, db, schema, false)

			businessExec(t, ctx, db, call, "root")
			businessExec(t, ctx, db, call, "root")
			checkWorkflowDeleteFixture(t, ctx, db, schema, true)
		})
		t.Run("announcement_rollback_after_late_workflow_error", func(t *testing.T) {
			schema := newBusinessIntegrationSchema(t, ctx, db)
			names := []string{"PROC_CG_GONGGAOINFO_DELETE", "DELETEPROCESSVERSIONINSTANCE"}
			report, err := ensurePGProcedureCompatibility(ctx, db, schema, product, names)
			require.NoError(t, err)
			require.Equal(t, 2, report.Created)
			seedWorkflowDeleteFixture(t, ctx, db, schema)
			seedProcurementNoticeFixture(t, ctx, db, schema)

			// A foreign key prevents the last message delete after the child and
			// root detail deletes have run. All workflow and notice changes must roll back.
			blocker := qualifiedFunction(schema, "message_reference")
			messageHistory := qualifiedFunction(schema, "messages_center_histroy")
			businessExec(t, ctx, db, "CREATE TABLE "+blocker+" (id varchar REFERENCES "+messageHistory+"(pviguid))")
			businessExec(t, ctx, db, "INSERT INTO "+blocker+" VALUES ('root')")
			call := "CALL " + qualifiedFunction(schema, "proc_cg_gonggaoinfo_delete") + "('notice')"
			_, err = db.ExecContext(ctx, call)
			require.ErrorContains(t, err, "message_reference")
			checkWorkflowDeleteFixture(t, ctx, db, schema, false)
			notice := qualifiedFunction(schema, "CG_ZhaoBiaoGGInfo")
			var count int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+notice+` WHERE "GongGaoGuid"='notice'`).Scan(&count))
			require.Equal(t, 1, count)

			businessExec(t, ctx, db, "DROP TABLE "+blocker)
			businessExec(t, ctx, db, call)
			checkWorkflowDeleteFixture(t, ctx, db, schema, true)
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+notice+` WHERE "GongGaoGuid"='notice'`).Scan(&count))
			require.Zero(t, count)
		})
	})
}

func seedWorkflowDeleteFixture(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	t.Helper()
	for _, table := range workflowDeleteFixtureTables {
		relation := qualifiedFunction(schema, table.name)
		key := source.QuotePGIdent(table.key)
		if strings.EqualFold(table.name, "workflow_pvi") {
			businessExec(t, ctx, db, "CREATE TABLE "+relation+" ("+key+` varchar, "MainPVIGuid" varchar)`)
			businessExec(t, ctx, db, "INSERT INTO "+relation+` VALUES
 ('root','root'), ('child1','root'), ('child2','root'), ('child2','root'),
 ('grandchild','child1'), ('untouched',NULL)`)
			continue
		}
		businessExec(t, ctx, db, "CREATE TABLE "+relation+" ("+key+" varchar PRIMARY KEY)")
		businessExec(t, ctx, db, "INSERT INTO "+relation+" VALUES ('root'),('child1'),('child2'),('grandchild'),('untouched')")
	}
}

func checkWorkflowDeleteFixture(t *testing.T, ctx context.Context, db *sql.DB, schema string, deleted bool) {
	t.Helper()
	for _, table := range workflowDeleteFixtureTables {
		relation := qualifiedFunction(schema, table.name)
		key := source.QuotePGIdent(table.key)
		rows, err := db.QueryContext(ctx, "SELECT "+key+" FROM "+relation)
		require.NoError(t, err)
		var got []string
		for rows.Next() {
			var guid string
			require.NoError(t, rows.Scan(&guid))
			got = append(got, guid)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		want := []string{"grandchild", "untouched"}
		if !deleted || table.rootOnly {
			want = append(want, "child1", "child2")
		}
		if !deleted {
			want = append(want, "root")
			if strings.EqualFold(table.name, "workflow_pvi") {
				want = append(want, "child2")
			}
		}
		require.ElementsMatch(t, want, got, table.name)
	}
}
