package target

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDemo2CleanupSQLIntegration(t *testing.T) {
	testPGCompatibilityProducts(t, func(t *testing.T, ctx context.Context, db *sql.DB, product string) {
		checks := map[string]func(*testing.T, context.Context, *sql.DB, string){
			"clearallpvi":    checkDemo2ClearAllPVI,
			"cleardirtydata": checkDemo2ClearDirtyData,
			"delete_remail":  checkDemo2DeleteReMail,
		}
		for _, spec := range demo2CleanupProcedureSpecs {
			t.Run(spec.name, func(t *testing.T) {
				schema := newBusinessIntegrationSchema(t, ctx, db)
				writer := builtinIntegrationWriter(db, schema, product)
				names := CompatibilityRoutineNames{Procedures: []string{strings.ToUpper(spec.name)}}
				report, err := EnsureCompatibilityFunctions(ctx, writer, names)
				require.NoError(t, err, "initialization precedes business table creation")
				require.Equal(t, 1, report.Total)
				require.Equal(t, 1, report.Created)
				checks[spec.name](t, ctx, db, schema)
				report, err = EnsureCompatibilityFunctions(ctx, writer, names)
				require.NoError(t, err)
				require.Equal(t, 1, report.Reused)
			})
		}
		if product == "postgres" {
			t.Run("inheritance_scope", func(t *testing.T) {
				checkDemo2CleanupInheritanceScope(t, ctx, db, product)
			})
		}
	})
}

func checkDemo2ClearAllPVI(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	q := func(name string) string { return qualifiedFunction(schema, name) }
	globalTables := []string{
		"Workflow_Activity_Instance", "Workflow_PVI_Material", "Workflow_PVI_MisTableRow",
		"Workflow_Transition_Instance", "Workflow_WorkItem_Notify", "Workflow_PVI",
	}
	for _, table := range globalTables {
		businessExec(t, ctx, db, "CREATE TABLE "+q(table)+"(marker integer)")
		businessExec(t, ctx, db, "INSERT INTO "+q(table)+" VALUES (1)")
	}
	storage := q("WorkFlow_AttachStorage")
	info := q("WorkFlow_AttachStorageInfo")
	template := q("Workflow_PV_Material")
	businessExec(t, ctx, db, "CREATE TABLE "+storage+`("AttachGuid" varchar)`)
	businessExec(t, ctx, db, "INSERT INTO "+storage+" VALUES ('keep'),('orphan'),(NULL)")
	businessExec(t, ctx, db, "CREATE TABLE "+info+`("AttachStorageGuid" varchar,"AttachStorageInfoGroupGuid" varchar)`)
	businessExec(t, ctx, db, "INSERT INTO "+info+" VALUES ('keep','template'),('orphan','orphan'),('ungrouped',NULL),(NULL,'template')")
	businessExec(t, ctx, db, "CREATE TABLE "+template+`("Template_ASInfoGroupGuid" varchar)`)
	businessExec(t, ctx, db, "INSERT INTO "+template+" VALUES ('template')")
	work := q("Workflow_WorkItem")
	businessExec(t, ctx, db, "CREATE TABLE "+work+`("WorkItemGuid" varchar)`)
	businessExec(t, ctx, db, "INSERT INTO "+work+" VALUES ('work')")
	for _, name := range []string{"Messages_Center", "Messages_Center_Histroy"} {
		table := q(name)
		businessExec(t, ctx, db, "CREATE TABLE "+table+`("MessageItemGuid" varchar)`)
		businessExec(t, ctx, db, "INSERT INTO "+table+" VALUES ('work'),('unrelated'),(NULL)")
	}
	call := "CALL " + q("clearallpvi") + "()"
	withBusinessCatalogPath(t, ctx, db, func(tx *sql.Tx) {
		_, err := tx.ExecContext(ctx, call)
		require.NoError(t, err)
		var rows int
		require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM "+storage).Scan(&rows))
		require.Equal(t, 3, rows, "NULL in a NOT IN reference list suppresses deletion")
		require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM "+q("Workflow_PVI")).Scan(&rows))
		require.Zero(t, rows)
	})
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("Workflow_PVI")), "caller rollback restores cleared data")
	businessExec(t, ctx, db, "DELETE FROM "+info+` WHERE "AttachStorageGuid" IS NULL`)
	businessExec(t, ctx, db, call)
	for _, table := range append(globalTables, "Workflow_WorkItem") {
		require.Zero(t, demoCount(t, ctx, db, "SELECT count(*) FROM "+q(table)), table)
	}
	require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+storage))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+storage+` WHERE "AttachGuid"='keep'`))
	require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+info))
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+template))
	for _, table := range []string{"Messages_Center", "Messages_Center_Histroy"} {
		require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+q(table)), table)
		require.Zero(t, demoCount(t, ctx, db, "SELECT count(*) FROM "+q(table)+` WHERE "MessageItemGuid"='work'`))
	}
}

func checkDemo2ClearDirtyData(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	q := func(name string) string { return qualifiedFunction(schema, name) }
	for _, table := range []struct{ name, columns, values string }{
		{"Workflow_Process", `"ProcessGuid" varchar`, "('p')"},
		{"Workflow_ProcessVersion", "processguid varchar,processversionguid varchar", "('p','v'),('missing','orphan_v')"},
		{"Workflow_PV_Material", "processversionguid varchar,materialguid varchar", "('v','m'),('orphan_v','orphan_m')"},
		{"Workflow_PV_MisTableSet", "materialguid varchar", "('m'),('orphan_m'),(NULL)"},
		{"Workflow_PV_PrintAdapter", "processversionguid varchar", "('v'),('orphan_v')"},
		{"Workflow_Context", "processversionguid varchar", "('v'),('orphan_v')"},
		{"Workflow_Activity", "processversionguid varchar,activityguid varchar", "('v','a'),('orphan_v','orphan_a')"},
		{"Workflow_Activity_Material", "activityguid varchar,materialguid varchar,activitymaterialguid varchar", "('a','m','am'),('orphan_a','m','activity_orphan'),('a','orphan_m','material_orphan')"},
		{"Workflow_Activity_FieldAccess", "activitymaterialguid varchar", "('am'),('activity_orphan'),('material_orphan')"},
		{"Workflow_Activity_Operation", "activityguid varchar,operationguid varchar", "('a','o'),('orphan_a','orphan_o')"},
		{"Workflow_Event", "eventtype integer,sourceguid varchar", "(10,'a'),(15,'orphan_a'),(50,'o'),(55,'orphan_o'),(99,'missing'),(20,NULL)"},
		{"Workflow_Config", "belongto varchar,sourceguid varchar", "('10','v'),('10','orphan_v'),('20','a'),('20','orphan_a'),('22','o'),('22','orphan_o'),('30','t'),('30','missing'),('40','m'),('40','orphan_m'),('99','missing')"},
		{"Workflow_Transition", "transitionguid varchar,processversionguid varchar", "('t','v')"},
		{"Workflow_Participator", "sourceguid varchar,processversionguid varchar,belongto integer", "('a',NULL,10),('t',NULL,30),('orphan_a',NULL,15),('missing',NULL,35),('','v',10),(NULL,'v',10),('a','orphan_v',20),('other','v',99)"},
	} {
		businessExec(t, ctx, db, "CREATE TABLE "+q(table.name)+"("+table.columns+")")
		businessExec(t, ctx, db, "INSERT INTO "+q(table.name)+" VALUES "+table.values)
	}
	call := "CALL " + q("cleardirtydata") + "()"
	businessExec(t, ctx, db, call)
	for _, table := range []string{"Workflow_ProcessVersion", "Workflow_PV_Material", "Workflow_PV_PrintAdapter",
		"Workflow_Context", "Workflow_Activity", "Workflow_Activity_Material", "Workflow_Activity_FieldAccess", "Workflow_Activity_Operation"} {
		require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q(table)), table)
	}
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("Workflow_Activity_Material")+" WHERE activitymaterialguid='am'"))
	require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("Workflow_PV_MisTableSet")))
	require.Equal(t, 4, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("Workflow_Event")))
	require.Equal(t, 6, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("Workflow_Config")))
	participant := q("Workflow_Participator")
	require.Equal(t, 3, demoCount(t, ctx, db, "SELECT count(*) FROM "+participant))
	require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+participant+" WHERE processversionguid='v' AND sourceguid IN ('a','t')"))

	businessExec(t, ctx, db, "INSERT INTO "+q("Workflow_Process")+" VALUES (NULL)")
	businessExec(t, ctx, db, "INSERT INTO "+q("Workflow_ProcessVersion")+" VALUES ('missing','kept_by_null_reference')")
	businessExec(t, ctx, db, call)
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("Workflow_ProcessVersion")+" WHERE processversionguid='kept_by_null_reference'"))

	businessExec(t, ctx, db, "INSERT INTO "+q("Workflow_Activity")+" VALUES ('v','duplicate'),('v','duplicate')")
	businessExec(t, ctx, db, "INSERT INTO "+participant+" VALUES ('duplicate',NULL,15)")
	businessExec(t, ctx, db, "INSERT INTO "+q("Workflow_PV_MisTableSet")+" VALUES ('rollback_orphan')")
	_, err := db.ExecContext(ctx, call)
	require.Error(t, err, "duplicate activity must fail scalar participator backfill")
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM "+q("Workflow_PV_MisTableSet")+" WHERE materialguid='rollback_orphan'"))
}

func checkDemo2DeleteReMail(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	table := qualifiedFunction(schema, "DBMail_AllMails")
	businessExec(t, ctx, db, "CREATE TABLE "+table+`("Row_ID" integer,"MailGuid" varchar,marker varchar)`)
	businessExec(t, ctx, db, "INSERT INTO "+table+` VALUES
 (10,'duplicate','minimumA'),(10,'duplicate','minimumB'),(20,'duplicate','duplicate'),(NULL,'duplicate','nullID'),
 (30,'unique','unique'),(NULL,'allNull','allNullA'),(NULL,'allNull','allNullB'),
 (1,'','emptyA'),(2,'','emptyB'),(1,NULL,'nullGuidA'),(2,NULL,'nullGuidB')`)
	call := "CALL " + qualifiedFunction(schema, "delete_remail") + "()"
	for range 2 {
		businessExec(t, ctx, db, call)
		require.Equal(t, 10, demoCount(t, ctx, db, "SELECT count(*) FROM "+table))
		require.Zero(t, demoCount(t, ctx, db, "SELECT count(*) FROM "+table+" WHERE marker='duplicate'"))
	}
}

func checkDemo2CleanupInheritanceScope(t *testing.T, ctx context.Context, db *sql.DB, product string) {
	schema := newBusinessIntegrationSchema(t, ctx, db)
	other := newBusinessIntegrationSchema(t, ctx, db)
	report, err := ensurePGProcedureSpecCompatibility(ctx, db, schema, product, demo2CleanupProcedureSpecs[2])
	require.NoError(t, err)
	require.Equal(t, 1, report.Created)
	table := qualifiedFunction(schema, "dbmail_allmails")
	outside := qualifiedFunction(other, "dbmail_allmails")
	businessExec(t, ctx, db, "CREATE TABLE "+table+"(row_id integer,mailguid varchar)")
	businessExec(t, ctx, db, "CREATE TABLE "+outside+"() INHERITS ("+table+")")
	businessExec(t, ctx, db, "INSERT INTO "+table+" VALUES (10,'mail'),(20,'mail')")
	businessExec(t, ctx, db, "INSERT INTO "+outside+" VALUES (1,'mail'),(2,'mail')")
	call := "CALL " + qualifiedFunction(schema, "delete_remail") + "()"
	businessExec(t, ctx, db, call)
	require.Equal(t, 1, demoCount(t, ctx, db, "SELECT count(*) FROM ONLY "+table+" WHERE row_id=10"))
	require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+outside))
	businessExec(t, ctx, db, "DROP TABLE "+outside)
	businessExec(t, ctx, db, "DROP TABLE "+table)
	businessExec(t, ctx, db, "CREATE TABLE "+table+"(row_id integer,mailguid varchar) PARTITION BY LIST(mailguid)")
	businessExec(t, ctx, db, "CREATE TABLE "+outside+" PARTITION OF "+table+" FOR VALUES IN ('mail')")
	businessExec(t, ctx, db, "INSERT INTO "+table+" VALUES (10,'mail'),(20,'mail')")
	_, err = db.ExecContext(ctx, call)
	require.ErrorContains(t, err, "partitions outside the target schema")
	require.Equal(t, 2, demoCount(t, ctx, db, "SELECT count(*) FROM "+outside))
}
