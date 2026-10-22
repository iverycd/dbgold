package target

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func triggerPlanFixture(t *testing.T) mysqltrigger.Plan {
	t.Helper()
	info := source.TriggerInfo{Schema: "Source", TableName: "Orders", Name: "BeforeInsert", Timing: "BEFORE", Event: "INSERT", Body: "SET NEW.Value = NEW.Value + 1", SourceDDL: "CREATE TRIGGER BeforeInsert BEFORE INSERT ON Orders FOR EACH ROW SET NEW.Value = NEW.Value + 1", SQLMode: "STRICT_TRANS_TABLES"}
	table := source.TableDDLInfo{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Value", DataType: "int"}}}
	p, err := mysqltrigger.Convert(info, map[string]*source.TableDDLInfo{"Orders": &table}, mysqltrigger.Options{Schema: "Target"})
	require.NoError(t, err)
	p.Tables = []source.TableDDLInfo{{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Value", DataType: "INTEGER"}}}}
	return p
}

// This saved plan is independent of the current converter: old ledger rows
// must retain their hash-based function identity when the default advances.
func legacyTriggerPlanFixture(version int) mysqltrigger.Plan {
	markerVersion := version
	if markerVersion == 0 {
		markerVersion = 1
	}
	const functionName = "dbgold_trg_6f9a645b2bee48811d0e90743e9c06fa"
	body := fmt.Sprintf("\n-- dbgold:mysql-trigger:v%d\nBEGIN\nNEW.\"Value\" := (NEW.\"Value\" + 1);\nRETURN NEW;\nEND;\n", markerVersion)
	return mysqltrigger.Plan{
		Version: version,
		Source:  source.TriggerInfo{Schema: "Source", TableName: "Orders", Name: "BeforeInsert", Timing: "BEFORE", Event: "INSERT", Body: "SET NEW.Value = NEW.Value + 1", SourceDDL: "CREATE TRIGGER BeforeInsert BEFORE INSERT ON Orders FOR EACH ROW SET NEW.Value = NEW.Value + 1", SQLMode: "STRICT_TRANS_TABLES"},
		Schema:  "Target", Table: "Orders", Name: "BeforeInsert", FunctionName: functionName,
		FunctionBody: body,
		FunctionSQL:  `CREATE FUNCTION "Target"."` + functionName + `"() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER AS $dbgold$` + body + "$dbgold$;",
		TriggerSQL:   `CREATE TRIGGER "BeforeInsert" BEFORE INSERT ON "Target"."Orders" FOR EACH ROW EXECUTE PROCEDURE "Target"."` + functionName + `"();`,
		Dependencies: []string{"Orders"},
		SourceTables: []source.TableDDLInfo{{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Value", DataType: "int"}}}},
		Tables:       []source.TableDDLInfo{{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Value", DataType: "INTEGER"}}}},
	}
}

func triggerWriterFixture(t *testing.T) (*PostgresWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &PostgresWriter{db: db, schema: "Target", dbType: "postgres", dia: dialect.NewPostgres("postgres")}, m
}

func expectTriggerPreflight(m sqlmock.Sqlmock, p mysqltrigger.Plan, fnRows, trRows *sqlmock.Rows) {
	m.ExpectBegin()
	m.ExpectQuery("SELECT EXISTS.*pg_language").WillReturnRows(sqlmock.NewRows([]string{"installed"}).AddRow(true))
	m.ExpectExec(regexp.QuoteMeta(`LOCK TABLE "Target"."Orders" IN SHARE ROW EXCLUSIVE MODE`)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery("SELECT a.attname,format_type").WithArgs("Target", "Orders").WillReturnRows(sqlmock.NewRows([]string{"column", "type", "generated"}).AddRow("Value", "integer", ""))
	if fnRows == nil {
		fnRows = sqlmock.NewRows([]string{"oid", "body", "security", "trigger", "args", "language", "config", "owner", "plain"})
	}
	m.ExpectQuery("SELECT p.oid,p.prosrc").WithArgs("Target", p.FunctionName).WillReturnRows(fnRows)
	if trRows == nil {
		trRows = sqlmock.NewRows([]string{"name", "type", "enabled", "fn", "args", "noWhen", "constraint"})
	}
	m.ExpectQuery("SELECT g.tgname,g.tgtype").WithArgs("Target", "Orders").WillReturnRows(trRows)
}

func recordedTriggerFixture(p mysqltrigger.Plan) repair.Object {
	return repair.Object{Kind: "trigger", Schema: p.Schema, Table: p.Table, TargetName: p.Name, MySQLPGTrigger: &p, Status: "failed", Units: []repair.Unit{{SQL: p.FunctionSQL, OriginalSQL: p.FunctionSQL, Status: "rolled_back", Selected: true}, {SQL: p.TriggerSQL, OriginalSQL: p.TriggerSQL, Status: "failed", Selected: true}}}
}

func TestMySQLPGTriggerTransactionOutcomes(t *testing.T) {
	for _, stage := range []string{"success", "function", "trigger", "owner", "commit"} {
		t.Run(stage, func(t *testing.T) {
			p := triggerPlanFixture(t)
			if stage == "owner" {
				p.Owner = "appowner"
			}
			w, m := triggerWriterFixture(t)
			expectTriggerPreflight(m, p, nil, nil)
			failure := errors.New(stage + " failed")
			if stage == "function" {
				m.ExpectExec(regexp.QuoteMeta(p.FunctionSQL)).WillReturnError(failure)
				m.ExpectRollback()
			} else {
				m.ExpectExec(regexp.QuoteMeta(p.FunctionSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
				if stage == "trigger" {
					m.ExpectExec(regexp.QuoteMeta(p.TriggerSQL)).WillReturnError(failure)
					m.ExpectRollback()
				} else {
					m.ExpectExec(regexp.QuoteMeta(p.TriggerSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
					if stage == "owner" {
						m.ExpectExec(regexp.QuoteMeta(`ALTER FUNCTION "Target"."` + p.FunctionName + `"() OWNER TO "appowner"`)).WillReturnError(failure)
						m.ExpectRollback()
					} else if stage == "commit" {
						m.ExpectCommit().WillReturnError(failure)
					} else {
						m.ExpectCommit()
					}
				}
			}
			states := map[string]string{}
			ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { states[e.SQL] = e.Status; return nil })
			err := CreateMySQLPGTrigger(ctx, w, p, p.Owner)
			if stage == "success" {
				require.NoError(t, err)
				require.Equal(t, "succeeded", states[p.FunctionSQL])
				require.Equal(t, "succeeded", states[p.TriggerSQL])
			} else {
				require.Error(t, err)
				require.False(t, IsMySQLPGTriggerPreflight(err))
				if stage == "commit" {
					require.Equal(t, "unknown", states[p.FunctionSQL])
					require.Equal(t, "unknown", states[p.TriggerSQL])
				}
				if stage == "trigger" || stage == "owner" {
					require.Equal(t, "rolled_back", states[p.FunctionSQL])
				}
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestMySQLPGTriggerConflictsAndRecordedReuse(t *testing.T) {
	for _, mode := range []string{"normal_existing", "repair_reuse", "changed_function", "other_trigger"} {
		t.Run(mode, func(t *testing.T) {
			p := triggerPlanFixture(t)
			w, m := triggerWriterFixture(t)
			body := p.FunctionBody
			if mode == "changed_function" {
				body += "-- changed"
			}
			fn := sqlmock.NewRows([]string{"oid", "body", "security", "trigger", "args", "language", "config", "owner", "plain"}).AddRow(42, body, false, true, 0, "plpgsql", nil, "postgres", true)
			tr := sqlmock.NewRows([]string{"name", "type", "enabled", "fn", "args", "noWhen", "constraint"}).AddRow(p.Name, 7, "O", 42, 0, true, 0)
			if mode == "other_trigger" {
				fn = sqlmock.NewRows([]string{"oid", "body", "security", "trigger", "args", "language", "config", "owner", "plain"})
				tr = sqlmock.NewRows([]string{"name", "type", "enabled", "fn", "args", "noWhen", "constraint"}).AddRow("other", 7, "O", 12, 0, true, 0)
			}
			if mode == "changed_function" {
				m.ExpectBegin()
				m.ExpectQuery("SELECT EXISTS.*pg_language").WillReturnRows(sqlmock.NewRows([]string{"installed"}).AddRow(true))
				m.ExpectExec("LOCK TABLE").WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectQuery("SELECT a.attname,format_type").WillReturnRows(sqlmock.NewRows([]string{"c", "t", "g"}).AddRow("Value", "integer", ""))
				m.ExpectQuery("SELECT p.oid,p.prosrc").WillReturnRows(fn)
			} else {
				expectTriggerPreflight(m, p, fn, tr)
			}
			if mode == "repair_reuse" {
				m.ExpectCommit()
			} else {
				m.ExpectRollback()
			}
			var err error
			if mode == "repair_reuse" {
				o := recordedTriggerFixture(p)
				units, e := ExecuteRepair(context.Background(), w, o, o.Units, nil)
				err = e
				require.Equal(t, "succeeded", units[0].Status)
				require.Equal(t, "succeeded", units[1].Status)
			} else {
				err = CreateMySQLPGTrigger(context.Background(), w, p, "")
			}
			if mode == "repair_reuse" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.True(t, IsMySQLPGTriggerPreflight(err))
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestMySQLPGTriggerReadOnlyRepair(t *testing.T) {
	p := triggerPlanFixture(t)
	o := recordedTriggerFixture(p)
	require.False(t, o.CanEditDDL())
	require.True(t, o.CanExecuteDDL())
	require.NoError(t, ValidateMySQLPGTriggerRepair(o, o.Units))
	for _, change := range []func(*repair.Object){func(o *repair.Object) { o.Units[0].SQL += " -- changed" }, func(o *repair.Object) { o.Units[1].Selected = false }, func(o *repair.Object) { o.Units[0].Status = "unknown" }, func(o *repair.Object) { o.Status = "unknown" }, func(o *repair.Object) { o.Units = o.Units[:1] }} {
		candidate := o
		candidate.Units = append([]repair.Unit(nil), o.Units...)
		change(&candidate)
		require.Error(t, ValidateMySQLPGTriggerRepair(candidate, candidate.Units))
	}
}

func TestLegacyMySQLPGTriggerReadOnlyRetryPreservesNames(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		for _, mode := range []string{"install", "reuse"} {
			t.Run(fmt.Sprintf("v%d/%s", version, mode), func(t *testing.T) {
				saved, err := json.Marshal(legacyTriggerPlanFixture(version))
				require.NoError(t, err)
				var p mysqltrigger.Plan
				require.NoError(t, json.Unmarshal(saved, &p))
				require.NoError(t, mysqltrigger.ValidatePlan(p))
				w, m := triggerWriterFixture(t)
				if mode == "reuse" {
					fn := sqlmock.NewRows([]string{"oid", "body", "security", "trigger", "args", "language", "config", "owner", "plain"}).AddRow(42, p.FunctionBody, false, true, 0, "plpgsql", nil, "postgres", true)
					tr := sqlmock.NewRows([]string{"name", "type", "enabled", "fn", "args", "noWhen", "constraint"}).AddRow(p.Name, 7, "O", 42, 0, true, 0)
					expectTriggerPreflight(m, p, fn, tr)
				} else {
					expectTriggerPreflight(m, p, nil, nil)
					for _, statement := range p.Statements() {
						m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
					}
				}
				m.ExpectCommit()
				o := recordedTriggerFixture(p)
				units, err := ExecuteRepair(context.Background(), w, o, o.Units, nil)
				require.NoError(t, err)
				require.Len(t, units, 2)
				for i, unit := range units {
					require.Equal(t, "succeeded", unit.Status)
					require.Equal(t, p.Statements()[i], unit.SQL)
					require.Equal(t, p.Statements()[i], unit.OriginalSQL)
				}
				require.NoError(t, m.ExpectationsWereMet())
			})
		}
	}
}

func TestMySQLPGLoadTriggersRespectsSelectedNames(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unselected", true: "selected"}[blocked], func(t *testing.T) {
			w, m := triggerWriterFixture(t)
			rows := sqlmock.NewRows([]string{"table", "trigger"}).AddRow("Unrelated", "audit")
			if blocked {
				rows.AddRow("Orders", "audit_orders")
			}
			m.ExpectQuery("SELECT t.relname,g.tgname").WithArgs("Target").WillReturnRows(rows)
			err := CheckMySQLPGLoadTriggers(context.Background(), w, []string{"Orders"})
			if blocked {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestTriggerIdentifiersAndTypes(t *testing.T) {
	require.Equal(t, `"目""标"."表""名"`, qualifiedTriggerIdent(`目"标`, `表"名`))
	for _, pair := range [][2]string{{"int", "integer"}, {"int4", "int"}, {"char(12)", "character(12)"}, {"char", "character(1)"}, {"bit", "bit(1)"}, {"double", "double precision"}, {"float", "float8"}, {"decimal(10,2)", "numeric(10,2)"}, {"VARCHAR(12)", "character varying(12)"}, {"TIMESTAMP(3)", "timestamp(3) without time zone"}, {"TIMESTAMP", "timestamp(6) without time zone"}, {"NUMERIC(10)", "numeric(10,0)"}, {"TIMESTAMP(3) WITH TIME ZONE", "timestamptz(3)"}} {
		require.Equal(t, canonicalTriggerType(pair[0]), canonicalTriggerType(pair[1]))
	}
}

func TestMySQLPGCharacterCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, encoding string
		compatible     bool
		queryError     error
		valid          bool
	}{
		{name: "utf8", encoding: "UTF8", compatible: true, valid: true},
		{name: "wrong encoding", encoding: "LATIN1", compatible: true},
		{name: "wrong primitives", encoding: "UTF8", compatible: false},
		{name: "missing capability", queryError: errors.New("function unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := triggerWriterFixture(t)
			m.ExpectBegin()
			tx, err := w.db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			q := m.ExpectQuery(regexp.QuoteMeta(mysqlPGCharacterCapabilitySQL)).WithArgs("Aé😀 ", "[\U00010000-\U0010ffff]", "�", "Aé", "ae", "ae�")
			if tc.queryError != nil {
				q.WillReturnError(tc.queryError)
			} else {
				q.WillReturnRows(sqlmock.NewRows([]string{"encoding", "compatible"}).AddRow(tc.encoding, tc.compatible))
			}
			err = validateMySQLPGCharacterCapabilities(context.Background(), tx)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			m.ExpectRollback()
			require.NoError(t, tx.Rollback())
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestMySQLPGTextSemantics(t *testing.T) {
	for _, value := range []any{true, false, nil} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			w, m := triggerWriterFixture(t)
			m.ExpectBegin()
			tx, err := w.db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			m.ExpectQuery(regexp.QuoteMeta(mysqlPGTextSemanticsSQL)).WillReturnRows(sqlmock.NewRows([]string{"compatible"}).AddRow(value))
			err = validateMySQLPGTextSemantics(context.Background(), tx)
			if value == true {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "语义与 MySQL 不一致")
			}
			m.ExpectRollback()
			require.NoError(t, tx.Rollback())
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
