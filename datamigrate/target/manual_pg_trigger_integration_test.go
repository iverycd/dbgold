package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/repair"

	"github.com/stretchr/testify/require"
)

func TestPGManualTriggerSQLIntegration(t *testing.T) {
	products := []struct{ name, variable, driver string }{
		{"postgres", "DBGOLD_TEST_POSTGRES_DSN", "postgres"},
		{"kingbase", "DBGOLD_TEST_KINGBASE_DSN", "postgres"},
		{"highgo", "DBGOLD_TEST_HIGHGO_DSN", "highgo"},
		{"gaussdb", "DBGOLD_TEST_GAUSSDB_DSN", "opengauss"},
		{"vastbase", "DBGOLD_TEST_VASTBASE_DSN", "postgres"},
		{"gbase", "DBGOLD_TEST_GBASE_DSN", "postgres"},
		{"seabox", "DBGOLD_TEST_SEABOX_DSN", "postgres"},
	}
	for _, product := range products {
		t.Run(product.name, func(t *testing.T) {
			dsn := os.Getenv(product.variable)
			if dsn == "" {
				t.Skip(product.variable + " is not set")
			}
			db, err := sql.Open(product.driver, dsn)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			schema := fmt.Sprintf("dbgold manual_trigger_%d", time.Now().UnixNano())
			_, err = db.ExecContext(ctx, "CREATE SCHEMA "+quoteTriggerIdent(schema))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := db.ExecContext(context.Background(), "DROP SCHEMA "+quoteTriggerIdent(schema)+" CASCADE")
				require.NoError(t, err)
			})
			table, name, function := "Orders", "AfterDelete", "Order fn"
			qTable, qFunction := qualifiedTriggerIdent(schema, table), qualifiedTriggerIdent(schema, function)
			qAudit := qualifiedTriggerIdent(schema, "Order audit")
			_, err = db.ExecContext(ctx, "CREATE TABLE "+qTable+` ("Value" integer)`)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, "CREATE TABLE "+qAudit+` ("Value" integer)`)
			require.NoError(t, err)
			w := builtinIntegrationWriter(db, schema, product.name)
			createFunction := "CREATE FUNCTION " + qFunction + `() RETURNS trigger AS $body$
				DECLARE
					inc integer := 1;
				BEGIN
					INSERT INTO ` + qAudit + ` ("Value") VALUES (OLD."Value" + inc);
					RETURN OLD;
				END;
			$body$ LANGUAGE plpgsql SECURITY INVOKER;`
			triggerHeader := "CREATE TRIGGER " + quoteTriggerIdent(name) + " AFTER DELETE ON " + qTable + " FOR EACH ROW "
			triggerTail := "EXECUTE PROCEDURE " + qFunction + "();"
			createTrigger := triggerHeader + triggerTail
			object := pgManualTriggerFixture(schema, table, name, createFunction, createTrigger)

			functionCount := func() int {
				var count int
				err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
					WHERE n.nspname=$1 AND p.proname=$2`, schema, function).Scan(&count)
				require.NoError(t, err)
				return count
			}
			triggerCount := func() int {
				var count int
				err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_trigger g JOIN pg_catalog.pg_class c ON c.oid=g.tgrelid
					JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 AND g.tgname=$3`,
					schema, table, name).Scan(&count)
				require.NoError(t, err)
				return count
			}

			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			require.Error(t, execPreparedPGTriggerSQL(ctx, tx, "SELECT 1; SELECT 2;"))
			require.NoError(t, tx.Rollback())

			conflictBody := `BEGIN RETURN OLD; END;`
			_, err = db.ExecContext(ctx, "CREATE FUNCTION "+qFunction+"() RETURNS trigger AS $$"+conflictBody+"$$ LANGUAGE plpgsql")
			require.NoError(t, err)
			out, err := ExecuteRepair(ctx, w, object, object.Units, nil)
			require.ErrorContains(t, err, "同名零参数函数")
			require.Equal(t, "failed", out[0].Status)
			require.Equal(t, 1, functionCount())
			require.Zero(t, triggerCount())
			var existingBody string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT p.prosrc FROM pg_catalog.pg_proc p
				JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname=$2`, schema, function).Scan(&existingBody))
			require.Equal(t, conflictBody, existingBody)
			_, err = db.ExecContext(ctx, "DROP FUNCTION "+qFunction+"()")
			require.NoError(t, err)

			otherFunction := qualifiedTriggerIdent(schema, "other_fn")
			_, err = db.ExecContext(ctx, "CREATE FUNCTION "+otherFunction+"() RETURNS trigger AS $$ BEGIN RETURN OLD; END; $$ LANGUAGE plpgsql")
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, triggerHeader+"EXECUTE PROCEDURE "+otherFunction+"()")
			require.NoError(t, err)
			out, err = ExecuteRepair(ctx, w, object, object.Units, nil)
			require.ErrorContains(t, err, "同名触发器")
			require.Equal(t, "failed", out[1].Status)
			require.Zero(t, functionCount())
			require.Equal(t, 1, triggerCount())
			_, err = db.ExecContext(ctx, "DROP TRIGGER "+quoteTriggerIdent(name)+" ON "+qTable)
			require.NoError(t, err)

			out, err = ExecuteRepair(ctx, w, object, object.Units, func(units []repair.Unit) error {
				if units[1].Status == "running" {
					return errors.New("模拟第二步执行前状态保存失败")
				}
				return nil
			})
			require.ErrorContains(t, err, "保存执行前状态")
			require.Equal(t, "rolled_back", out[0].Status)
			require.Equal(t, "pending", out[1].Status)
			require.Zero(t, functionCount())

			badTrigger := triggerHeader + `WHEN (OLD."Missing" > 0) ` + triggerTail
			failed := pgManualTriggerFixture(schema, table, name, createFunction, badTrigger)
			out, err = ExecuteRepair(ctx, w, failed, failed.Units, nil)
			require.ErrorContains(t, err, "Missing")
			require.Equal(t, "rolled_back", out[0].Status)
			require.Equal(t, "failed", out[1].Status)
			require.Contains(t, out[1].Error, "Missing")
			require.Zero(t, functionCount())
			require.Zero(t, triggerCount())

			object.Status = "unknown"
			_, err = ExecuteRepair(ctx, w, object, object.Units, nil)
			require.ErrorContains(t, err, "状态不可执行")
			require.Zero(t, functionCount())
			object.Status = "draft"
			object.ManualTrigger.Revision = 2
			out, err = ExecuteRepair(ctx, w, object, object.Units, nil)
			require.NoError(t, err)
			for _, unit := range out {
				require.Equal(t, "succeeded", unit.Status)
				require.False(t, unit.Selected)
			}
			require.Equal(t, 1, functionCount())
			require.Equal(t, 1, triggerCount())
			_, err = db.ExecContext(ctx, "INSERT INTO "+qTable+" VALUES (41)")
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, "DELETE FROM "+qTable+` WHERE "Value"=41`)
			require.NoError(t, err)
			var value int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT "Value" FROM `+qAudit).Scan(&value))
			require.Equal(t, 42, value)
		})
	}
}

func pgManualTriggerFixture(schema, table, trigger, functionSQL, triggerSQL string) repair.Object {
	o := repair.Object{
		ID: "trigger", Kind: "trigger", Name: trigger, Schema: schema, Table: table, TargetName: trigger,
		SourceName: trigger, SourceTable: table, SourceDDL: "CREATE TRIGGER ... DECLARE ...",
		TriggerFailureStage: "unsupported", DiagnosticOnly: true, Status: "failed",
	}
	original := o
	o.DiagnosticOnly, o.Status = false, "draft"
	o.ManualTrigger = &repair.ManualTriggerPlan{
		FunctionSQL: functionSQL, SQL: triggerSQL, Revision: 1, Original: &original,
	}
	o.Units = []repair.Unit{
		{ID: "function", SQL: functionSQL, OriginalSQL: functionSQL, Status: "pending", Selected: true},
		{ID: "trigger", SQL: triggerSQL, OriginalSQL: triggerSQL, Status: "pending", Selected: true},
	}
	return o
}
