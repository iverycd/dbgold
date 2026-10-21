package target

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// These opt-in DSNs identify disposable databases. The same six row-trigger
// behaviors are executable against every supported PG-family product.
func TestMySQLPGTriggerSQLIntegration(t *testing.T) {
	for _, product := range []struct{ name, variable, driver string }{{"postgres", "DBGOLD_TEST_POSTGRES_DSN", "postgres"}, {"kingbase", "DBGOLD_TEST_KINGBASE_DSN", "postgres"}, {"highgo", "DBGOLD_TEST_HIGHGO_DSN", "highgo"}, {"gaussdb", "DBGOLD_TEST_GAUSSDB_DSN", "opengauss"}, {"vastbase", "DBGOLD_TEST_VASTBASE_DSN", "postgres"}, {"gbase", "DBGOLD_TEST_GBASE_DSN", "postgres"}, {"seabox", "DBGOLD_TEST_SEABOX_DSN", "postgres"}} {
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
			schema := fmt.Sprintf("dbgold 触发器_\"_%d", time.Now().UnixNano())
			q := func(table string) string { return qualifiedTriggerIdent(schema, table) }
			_, err = db.ExecContext(ctx, "CREATE SCHEMA "+quoteTriggerIdent(schema))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := db.ExecContext(context.Background(), "DROP SCHEMA "+quoteTriggerIdent(schema)+" CASCADE")
				require.NoError(t, err)
			})
			_, err = db.ExecContext(ctx, "CREATE TABLE "+q("Orders")+` ("ID" integer PRIMARY KEY,"Value" integer)`)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, "CREATE TABLE "+q("Audit")+` ("Event" integer,"Value" integer)`)
			require.NoError(t, err)
			w := builtinIntegrationWriter(db, schema, product.name)
			_, err = db.ExecContext(ctx, "INSERT INTO "+q("Orders")+` ("ID","Value") VALUES (99,999)`)
			require.NoError(t, err)
			orders := source.TableDDLInfo{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}, {Name: "Value", DataType: "int"}}}
			audit := source.TableDDLInfo{TableName: "Audit", Columns: []source.ColumnInfo{{Name: "Event", DataType: "int"}, {Name: "Value", DataType: "int"}}}
			tables := map[string]*source.TableDDLInfo{"Orders": &orders, "Audit": &audit}
			for _, tr := range []struct{ name, timing, event, body string }{{"bi", "BEFORE", "INSERT", "SET NEW.Value = NEW.Value + 1"}, {"ai", "AFTER", "INSERT", "INSERT INTO Audit (Event,Value) VALUES (1,NEW.Value)"}, {"bu", "BEFORE", "UPDATE", "SET NEW.Value = NEW.Value + 1"}, {"au", "AFTER", "UPDATE", "BEGIN INSERT INTO Audit (Event,Value) VALUES (2,NEW.Value); INSERT INTO Audit (Event,Value) VALUES (3,OLD.Value); END"}, {"bd", "BEFORE", "DELETE", "INSERT INTO Audit (Event,Value) VALUES (4,OLD.Value)"}, {"ad", "AFTER", "DELETE", "INSERT INTO Audit (Event,Value) VALUES (5,OLD.Value)"}} {
				p, err := mysqltrigger.Convert(source.TriggerInfo{Schema: "Source", TableName: "Orders", Name: tr.name, Timing: tr.timing, Event: tr.event, Body: tr.body}, tables, mysqltrigger.Options{Schema: schema})
				require.NoError(t, err)
				for _, src := range p.SourceTables {
					target := src
					target.Columns = append([]source.ColumnInfo(nil), src.Columns...)
					for i := range target.Columns {
						target.Columns[i].DataType = "INTEGER"
					}
					p.Tables = append(p.Tables, target)
				}
				require.NoError(t, CreateMySQLPGTrigger(ctx, w, p, ""))
				if tr.name == "bi" {
					require.Error(t, CreateMySQLPGTrigger(ctx, w, p, ""))
					o := recordedTriggerFixture(p)
					_, err = ExecuteRepair(ctx, w, o, o.Units, nil)
					require.NoError(t, err)
				}
			}
			var historical, audits int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+q("Audit")).Scan(&audits))
			require.Zero(t, audits)
			require.NoError(t, db.QueryRowContext(ctx, "SELECT \"Value\" FROM "+q("Orders")+" WHERE \"ID\"=99").Scan(&historical))
			require.Equal(t, 999, historical)
			_, err = db.ExecContext(ctx, "INSERT INTO "+q("Orders")+` ("ID","Value") VALUES (1,1)`)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, "UPDATE "+q("Orders")+` SET "Value"=10 WHERE "ID"=1`)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, "DELETE FROM "+q("Orders")+` WHERE "ID"=1`)
			require.NoError(t, err)
			rows, err := db.QueryContext(ctx, "SELECT \"Event\",\"Value\" FROM "+q("Audit")+" ORDER BY \"Event\"")
			require.NoError(t, err)
			var got [][2]int
			for rows.Next() {
				var a, b int
				require.NoError(t, rows.Scan(&a, &b))
				got = append(got, [2]int{a, b})
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			require.Equal(t, [][2]int{{1, 2}, {2, 11}, {3, 2}, {4, 11}, {5, 11}}, got)
			require.Error(t, CheckMySQLPGLoadTriggers(ctx, w, []string{"Orders"}))
			_, err = InspectRecovery(ctx, w, "Orders")
			require.Error(t, err)
		})
	}
}
