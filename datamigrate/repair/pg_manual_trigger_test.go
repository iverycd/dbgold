package repair

import (
	"strings"
	"testing"

	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

const pgManualFunction = `CREATE FUNCTION app.audit_fn() RETURNS trigger
LANGUAGE plpgsql SECURITY INVOKER AS $body$
DECLARE v integer := 1;
BEGIN
    NEW.value := NEW.value + v;
    PERFORM 'literal; with "quotes"';
    RETURN NEW;
END;
$body$;`

const pgManualCreate = `CREATE TRIGGER audit BEFORE INSERT ON app.entries
FOR EACH ROW EXECUTE PROCEDURE app.audit_fn();`

func TestParsePGManualTrigger(t *testing.T) {
	d, err := ParsePGManualTrigger(pgManualFunction, pgManualCreate)
	require.NoError(t, err)
	require.Equal(t, PGManualTriggerDefinition{Schema: "app", FunctionName: "audit_fn", TriggerName: "audit", Table: "entries"}, d)

	function := `/* Outer /* nested */ comment */
	CREATE FUNCTION "部门"."Fun""Name"() RETURNS pg_catalog.trigger
	AS $$BEGIN RETURN NEW; END;$$ LANGUAGE PLPGSQL VOLATILE;`
	trigger := `CREATE TRIGGER "审计历史" BEFORE UPDATE OF "State", amount OR INSERT ON "部门"."Event.Log"
	FOR ROW WHEN ((NEW."State" <> 'literal;') AND (NEW.amount > COALESCE(OLD.amount, 0)))
	EXECUTE FUNCTION "部门"."Fun""Name"('source;import'); -- trailing comment`
	d, err = ParsePGManualTrigger(function, trigger)
	require.NoError(t, err)
	require.Equal(t, PGManualTriggerDefinition{Schema: "部门", FunctionName: `Fun"Name`, TriggerName: "审计历史", Table: "Event.Log"}, d)

	d, err = ParsePGManualTrigger(strings.Replace(pgManualFunction, "app.audit_fn", "App.Audit_FN", 1), pgManualCreate)
	require.NoError(t, err)
	require.Equal(t, "app", d.Schema)
	require.Equal(t, "audit_fn", d.FunctionName)
}

func TestParsePGManualTriggerRejectsInvalidEnvelopes(t *testing.T) {
	for _, c := range []struct {
		name, function, trigger string
	}{
		{"unqualified function", strings.Replace(pgManualFunction, "app.audit_fn", "audit_fn", 1), pgManualCreate},
		{"unqualified table", pgManualFunction, strings.Replace(pgManualCreate, "app.entries", "entries", 1)},
		{"different table schema", pgManualFunction, strings.Replace(pgManualCreate, "app.entries", "other.entries", 1)},
		{"different function schema", pgManualFunction, strings.Replace(pgManualCreate, "app.audit_fn", "other.audit_fn", 1)},
		{"different helper", pgManualFunction, strings.Replace(pgManualCreate, "audit_fn", "other_fn", 1)},
		{"different helper case", pgManualFunction, strings.Replace(pgManualCreate, "audit_fn", `"Audit_fn"`, 1)},
		{"function replacement", strings.Replace(pgManualFunction, "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1), pgManualCreate},
		{"trigger replacement", pgManualFunction, strings.Replace(pgManualCreate, "CREATE TRIGGER", "CREATE OR REPLACE TRIGGER", 1)},
		{"function parameters", strings.Replace(pgManualFunction, "audit_fn()", "audit_fn(integer)", 1), pgManualCreate},
		{"foreign language", strings.Replace(pgManualFunction, "plpgsql", "sql", 1), pgManualCreate},
		{"foreign return type", strings.Replace(pgManualFunction, "RETURNS trigger", "RETURNS other.trigger", 1), pgManualCreate},
		{"definer rights", strings.Replace(pgManualFunction, "INVOKER", "DEFINER", 1), pgManualCreate},
		{"second function statement", pgManualFunction + " SELECT 1", pgManualCreate},
		{"second trigger statement", pgManualFunction, pgManualCreate + " SELECT 1"},
		{"statement trigger", pgManualFunction, strings.Replace(pgManualCreate, "ROW", "STATEMENT", 1)},
		{"another expression statement", pgManualFunction, strings.Replace(pgManualCreate, "EXECUTE", "WHEN (TRUE; SELECT 1) EXECUTE", 1)},
		{"unterminated dollar body", strings.TrimSuffix(pgManualFunction, "$body$;"), pgManualCreate},
		{"unterminated comment", pgManualFunction, pgManualCreate + " /*"},
		{"zero byte", pgManualFunction + "\x00", pgManualCreate},
		{"missing function", "", pgManualCreate},
		{"missing trigger", pgManualFunction, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParsePGManualTrigger(c.function, c.trigger)
			require.Error(t, err)
		})
	}
}

func TestPGManualTriggerTrustRequiresProvenanceAndBothSavedUnits(t *testing.T) {
	original := Object{
		ID: "audit", Kind: "trigger", Schema: "app", Table: "entries", TargetName: "audit",
		SourceName: "SourceAudit", SourceTable: "Entries", SourceDDL: "CREATE TRIGGER SourceAudit ...",
		DiagnosticOnly: true, TriggerFailureStage: "unsupported",
	}
	o := original
	o.DiagnosticOnly = false
	o.ManualTrigger = &ManualTriggerPlan{SQL: pgManualCreate, FunctionSQL: pgManualFunction, Revision: 1, Original: &original}
	o.Units = []Unit{
		{ID: "func", SQL: pgManualFunction, OriginalSQL: pgManualFunction},
		{ID: "trigger", SQL: pgManualCreate, OriginalSQL: pgManualCreate},
	}
	require.True(t, o.TrustedManualTrigger())
	require.True(t, o.CanExecuteDDL())

	o.Units[0], o.Units[1] = o.Units[1], o.Units[0]
	require.False(t, o.CanExecuteDDL())
	o.Units[0], o.Units[1] = o.Units[1], o.Units[0]
	o.Units[0].OriginalSQL += " "
	require.False(t, o.CanExecuteDDL())
	o.Units[0].OriginalSQL = pgManualFunction
	o.Table = "other"
	require.False(t, o.CanExecuteDDL())
	o.Table = original.Table
	o.ManualTrigger.Owner = "other"
	require.False(t, o.CanExecuteDDL())
	o.ManualTrigger.Owner = o.Schema
	require.True(t, o.CanExecuteDDL())
	original.TriggerFailureStage = "metadata"
	require.False(t, o.CanExecuteDDL())
	original.TriggerFailureStage = "identity_conflict"
	require.False(t, o.CanExecuteDDL())
	original.TriggerFailureStage = "execute"
	require.False(t, o.CanExecuteDDL())
	original.MySQLPGTrigger = &mysqltrigger.Plan{
		Schema: "app", Table: "entries", Name: "audit",
		Source: source.TriggerInfo{Name: "SourceAudit", TableName: "Entries"},
	}
	require.True(t, o.CanExecuteDDL())
	original.MySQLPGTrigger.Schema = "other"
	require.False(t, o.CanExecuteDDL())
}
