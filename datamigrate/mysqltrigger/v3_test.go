package mysqltrigger

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"dbgold/datamigrate/source"
)

func convertV3(info source.TriggerInfo, tables map[string]*source.TableDDLInfo, opt Options) (Plan, error) {
	return convertVersion(info, tables, opt, 3)
}

func TestV3ReadableFunctionNamesAndByteBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"update_enableuser", "update_enableuser_fn"},
		{"update_isepoint", "update_isepoint_fn"},
		{"already_fn", "already_fn_fn"},
		{"更新用户", "更新用户_fn"},
		{`Update"User`, `Update"User_fn`},
		{strings.Repeat("a", 60), strings.Repeat("a", 60) + "_fn"},
		{strings.Repeat("a", 61), strings.Repeat("a", 47) + "_788151b7eddf_fn"},
		{strings.Repeat("a", 63), strings.Repeat("a", 47) + "_3a8b48f38b14_fn"},
		{strings.Repeat("触", 20), strings.Repeat("触", 20) + "_fn"},
		{strings.Repeat("触", 21), strings.Repeat("触", 15) + "_3a3bcb6ccd4f_fn"},
		{strings.Repeat("a", 46) + strings.Repeat("触", 5), strings.Repeat("a", 46) + "_7d33d7a844d7_fn"},
		{strings.Repeat("😀", 15) + "x", strings.Repeat("😀", 11) + "_c05d89548517_fn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := trigger("SET NEW.Amount=1")
			i.Name = tc.name
			p, err := convertV3(i, fixture(), Options{Schema: "Dest"})
			if err != nil {
				t.Fatal(err)
			}
			if p.Version != 3 || p.FunctionName != tc.want || !utf8.ValidString(p.FunctionName) || len(p.FunctionName) > 63 {
				t.Fatalf("unexpected helper name %q, version %d", p.FunctionName, p.Version)
			}
			if p.Name != tc.name || p.Source.Name != tc.name || p.Source.SourceDDL != i.SourceDDL {
				t.Fatal("readable helper name changed source identity or actual trigger name")
			}
			if !strings.Contains(p.FunctionSQL, "CREATE FUNCTION "+quote("Dest")+"."+quote(tc.want)+"()") || !strings.Contains(p.TriggerSQL, "CREATE TRIGGER "+quote(tc.name)+" ") || !strings.HasSuffix(p.TriggerSQL, "EXECUTE PROCEDURE "+quote("Dest")+"."+quote(tc.want)+"();") {
				t.Fatal("quoted names or function reference differ", p.Statements())
			}
			if err = ValidatePlan(p); err != nil {
				t.Fatal(err)
			}
			repeated, err := convertV3(i, fixture(), Options{Schema: "Dest"})
			if err != nil || !reflect.DeepEqual(p, repeated) {
				t.Fatal("repeated planning changed the name or frozen SQL", err)
			}
		})
	}
}

func TestV3ReadableFunctionNameUsesConfiguredCaseAndEscapesQuotes(t *testing.T) {
	i := trigger("SET NEW.Amount=1")
	i.Name = `Update"User`
	p, err := convertV3(i, fixture(), Options{Schema: `Dest"Schema`, Name: strings.ToLower})
	if err != nil {
		t.Fatal(err)
	}
	if p.FunctionName != `update"user_fn` || p.Name != `update"user` || p.Source.Name != `Update"User` {
		t.Fatal("case configuration changed the original name or was ignored", p.FunctionName, p.Name, p.Source.Name)
	}
	if !strings.HasPrefix(p.FunctionSQL, `CREATE FUNCTION "Dest""Schema"."update""user_fn"()`) || !strings.HasPrefix(p.TriggerSQL, `CREATE TRIGGER "update""user" BEFORE UPDATE ON "Dest""Schema"."orders"`) {
		t.Fatal("quoted identifiers were not escaped", p.Statements())
	}
	if err = ValidatePlan(p); err != nil {
		t.Fatal(err)
	}
}

func TestV3LongFunctionNameTracksIdentityAndNotProgramBody(t *testing.T) {
	i := trigger("SET NEW.Amount=1")
	i.Name = strings.Repeat("a", 61)
	first, err := convertV3(i, fixture(), Options{Schema: "Dest"})
	if err != nil {
		t.Fatal(err)
	}
	i.Body = "SET NEW.Amount=2"
	changedBody, err := convertV3(i, fixture(), Options{Schema: "Dest"})
	if err != nil {
		t.Fatal(err)
	}
	if first.FunctionName != changedBody.FunctionName || first.FunctionBody == changedBody.FunctionBody {
		t.Fatal("helper identity changed with the program body")
	}
	for _, tc := range []struct {
		schema, target string
		name           func(string) string
	}{
		{"OtherSource", "Dest", nil},
		{"SourceDB", "OtherTarget", nil},
		{"SourceDB", "Dest", strings.ToLower},
	} {
		i.Schema = tc.schema
		p, err := convertV3(i, fixture(), Options{Schema: tc.target, Name: tc.name})
		if err != nil {
			t.Fatal(err)
		}
		if p.FunctionName == first.FunctionName || !strings.HasPrefix(p.FunctionName, strings.Repeat("a", 47)+"_") {
			t.Fatal("long helper hash did not retain the original source/target identity seed", p.FunctionName)
		}
		if err = ValidatePlan(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestV3ReadableFunctionNamesRejectInvalidNamesWithoutExecutableSQL(t *testing.T) {
	for _, name := range []string{strings.Repeat("a", 64), strings.Repeat("触", 22), "bad\x00name", string([]byte{'b', 0xff})} {
		i := trigger("SET NEW.Amount=1")
		i.Name = name
		p, err := convertV3(i, fixture(), Options{Schema: "Dest"})
		if err == nil || p.FunctionSQL != "" || p.TriggerSQL != "" {
			t.Fatalf("accepted invalid source name or exposed partial SQL: %q", name)
		}
	}
}

func TestV3FunctionNameAndVersionAreFrozen(t *testing.T) {
	i := trigger("SET NEW.Amount=1")
	p, err := convertV3(i, fixture(), Options{Schema: "Dest"})
	if err != nil {
		t.Fatal(err)
	}
	if p.FunctionBody != "\n-- dbgold:mysql-trigger:v3\nBEGIN\nNEW.\"Amount\" := 1;\nRETURN NEW;\nEND;\n" {
		t.Fatal("v3 marker or program differs", p.FunctionBody)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Plan)
	}{
		{"function field", func(p *Plan) { p.FunctionName = "other_fn" }},
		{"coordinated SQL rename", func(p *Plan) {
			p.FunctionName = "other_fn"
			p.FunctionSQL = strings.ReplaceAll(p.FunctionSQL, `"OrdersAudit_fn"`, `"other_fn"`)
			p.TriggerSQL = strings.ReplaceAll(p.TriggerSQL, `"OrdersAudit_fn"`, `"other_fn"`)
		}},
		{"downgrade", func(p *Plan) { p.Version = 2 }},
		{"unknown version", func(p *Plan) { p.Version = 99 }},
		{"legacy marker", func(p *Plan) {
			p.FunctionBody = strings.ReplaceAll(p.FunctionBody, "dbgold:mysql-trigger:v3", "dbgold:mysql-trigger:v2")
			p.FunctionSQL = strings.ReplaceAll(p.FunctionSQL, "dbgold:mysql-trigger:v3", "dbgold:mysql-trigger:v2")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := p
			tc.mutate(&changed)
			if err := ValidatePlan(changed); err == nil {
				t.Fatal("modified name, SQL or version validated")
			}
		})
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored Plan
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if err = ValidatePlan(restored); err != nil {
		t.Fatal("v3 saved plan failed validation", err)
	}
}

func TestLegacyVersionsPreserveCompleteSQLGoldenAndJSONRetry(t *testing.T) {
	const oldName = "dbgold_trg_2a641a13cb5e8cf4459be7ccec9df0d0"
	const oldTriggerSQL = `CREATE TRIGGER "OrdersAudit" BEFORE UPDATE ON "Dest"."Orders" FOR EACH ROW EXECUTE PROCEDURE "Dest"."dbgold_trg_2a641a13cb5e8cf4459be7ccec9df0d0"();`
	for _, tc := range []struct {
		version int
		body    string
	}{
		{0, "\n-- dbgold:mysql-trigger:v1\nBEGIN\nNEW.\"Amount\" := 1;\nRETURN NEW;\nEND;\n"},
		{1, "\n-- dbgold:mysql-trigger:v1\nBEGIN\nNEW.\"Amount\" := 1;\nRETURN NEW;\nEND;\n"},
		{2, "\n-- dbgold:mysql-trigger:v2\nBEGIN\nNEW.\"Amount\" := 1;\nRETURN NEW;\nEND;\n"},
	} {
		t.Run(fmt.Sprint(tc.version), func(t *testing.T) {
			generatorVersion := tc.version
			if generatorVersion == 0 {
				generatorVersion = 1
			}
			p, err := convertVersion(trigger("SET NEW.Amount=1"), fixture(), Options{Schema: "Dest"}, generatorVersion)
			if err != nil {
				t.Fatal(err)
			}
			p.Version = tc.version
			wantFunctionSQL := `CREATE FUNCTION "Dest"."dbgold_trg_2a641a13cb5e8cf4459be7ccec9df0d0"() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER AS $dbgold$` + tc.body + "$dbgold$;"
			if p.FunctionName != oldName || p.FunctionBody != tc.body || p.FunctionSQL != wantFunctionSQL || p.TriggerSQL != oldTriggerSQL {
				t.Fatal("legacy name, marker or complete SQL changed", p.Statements())
			}
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			var restored Plan
			if err = json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if err = ValidatePlan(restored); err != nil {
				t.Fatal("legacy JSON plan cannot be retried", err)
			}
			if restored.Version != tc.version || !reflect.DeepEqual(restored.Statements(), []string{wantFunctionSQL, oldTriggerSQL}) {
				t.Fatal("legacy JSON round trip changed frozen statements")
			}
		})
	}
}

func TestV2ConcatCompleteSQLGoldenAndJSONRetry(t *testing.T) {
	i := trigger("SET NEW.Note=CONCAT('prefix:',IFNULL(NEW.Note,'a '),OLD.Id)")
	tables := fixture()
	p, err := convertVersion(i, tables, Options{Schema: "Dest", CharacterMetadata: characterFixture("SourceDB", tables)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	wantBody := "\n-- dbgold:mysql-trigger:v2\nBEGIN\nNEW.\"Note\" := (CAST(E'prefix:' AS text) || COALESCE(CAST(NEW.\"Note\" AS text), CAST(E'a ' AS text)) || CAST(OLD.\"Id\" AS text));\nRETURN NEW;\nEND;\n"
	wantFunctionSQL := `CREATE FUNCTION "Dest"."dbgold_trg_2a641a13cb5e8cf4459be7ccec9df0d0"() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER AS $dbgold$` + wantBody + "$dbgold$;"
	wantTriggerSQL := `CREATE TRIGGER "OrdersAudit" BEFORE UPDATE ON "Dest"."Orders" FOR EACH ROW EXECUTE PROCEDURE "Dest"."dbgold_trg_2a641a13cb5e8cf4459be7ccec9df0d0"();`
	if p.Version != 2 || p.FunctionBody != wantBody || !reflect.DeepEqual(p.Statements(), []string{wantFunctionSQL, wantTriggerSQL}) || !p.RequiresTextSemantics {
		t.Fatal("v2 CONCAT SQL or frozen format behavior changed", p.Statements())
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored Plan
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if err = ValidatePlan(restored); err != nil {
		t.Fatal(err)
	}
}

func TestV3InheritsV2BusinessTriggerSemantics(t *testing.T) {
	for _, tc := range []struct{ ddl, name, table string }{
		{userEnableTriggerDDL, "update_enableuser", "frame_user"},
		{userUSBKeyTriggerDDL, "update_isepoint", "frame_user_extendinfo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := userTables()
			meta := characterFixture("sxk", tables)
			i := source.TriggerInfo{Schema: "sxk", Name: tc.name, TableName: tc.table, Timing: "AFTER", Event: "UPDATE", Body: tc.ddl[strings.Index(tc.ddl, "begin"):], SourceDDL: tc.ddl, Definer: "root@%", ActionOrder: 1}
			opt := Options{Schema: "TargetSchema", Name: strings.ToLower, CharacterMetadata: meta}
			old, err := convertVersion(i, tables, opt, 2)
			if err != nil {
				t.Fatal(err)
			}
			current, err := convertV3(i, tables, opt)
			if err != nil {
				t.Fatal(err)
			}
			if current.FunctionBody != strings.Replace(old.FunctionBody, "dbgold:mysql-trigger:v2", "dbgold:mysql-trigger:v3", 1) || !reflect.DeepEqual(current.CharacterMetadata, old.CharacterMetadata) || current.RequiresTextSemantics != old.RequiresTextSemantics || !reflect.DeepEqual(current.Dependencies, old.Dependencies) || !reflect.DeepEqual(current.SourceTables, old.SourceTables) || current.Source != old.Source {
				t.Fatal("v3 changed v2 program semantics or frozen metadata")
			}
			if current.FunctionName != tc.name+"_fn" || !strings.HasPrefix(old.FunctionName, "dbgold_trg_") {
				t.Fatal("versioned function naming differs")
			}
			for _, p := range []Plan{old, current} {
				if err = ValidatePlan(p); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
