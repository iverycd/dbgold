package mysqltrigger

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
)

func nativeVariableFixture() (map[string]*source.TableDDLInfo, Options) {
	tables := fixture()
	tables["Orders"].Columns = append(tables["Orders"].Columns, source.ColumnInfo{Name: "CurrentValue", DataType: "int"})
	meta := &source.TriggerCharacterMetadata{
		ServerVersion: "8.0.42 Source distribution",
		NumericColumns: []source.TriggerNumericColumn{
			{Schema: "SourceDB", Table: "Orders", Name: "Id", ColumnType: "int"},
			{Schema: "SourceDB", Table: "Orders", Name: "CurrentValue", ColumnType: "int"},
		},
	}
	return tables, Options{Schema: "Dest", CharacterMetadata: meta, NativeUserVariables: true}
}

func TestNativeUserVariablesPreserveSessionWritesAndSource(t *testing.T) {
	for _, tc := range []struct{ timing, event, body, want string }{
		{"BEFORE", "UPDATE", "SET @CurrentValue = OLD.CurrentValue + 1", `PERFORM @CurrentValue := ((CAST(OLD."currentvalue" AS bigint) + CAST(1 AS bigint)));`},
		{"AFTER", "INSERT", "SET @CurrentValue := NEW.CurrentValue", `PERFORM @CurrentValue := (CAST(NEW."currentvalue" AS bigint));`},
	} {
		t.Run(tc.timing+tc.event, func(t *testing.T) {
			tables, opt := nativeVariableFixture()
			opt.Name = strings.ToLower
			i := trigger(tc.body)
			i.Timing, i.Event = tc.timing, tc.event
			p, err := Convert(i, tables, opt)
			if err != nil {
				t.Fatal(err)
			}
			if p.Version != 4 || !p.NativeUserVariables || !strings.Contains(p.FunctionBody, tc.want) || !strings.Contains(p.FunctionBody, "dbgold:mysql-trigger:v4") {
				t.Fatal("native session write not preserved", p.FunctionBody)
			}
			if p.Source != i || strings.Contains(p.FunctionBody, `NEW."currentvalue" :=`) {
				t.Fatal("source changed or session write became a row write")
			}
			if p.CharacterMetadata == nil || len(p.CharacterMetadata.NumericColumns) != 1 || p.CharacterMetadata.NumericColumns[0].Name != "CurrentValue" || len(p.CharacterMetadata.Profiles) != 0 {
				t.Fatal("did not freeze exactly the referenced integer type", p.CharacterMetadata)
			}
			opt.CharacterMetadata.NumericColumns[1].ColumnType = "int unsigned"
			if err = ValidatePlan(p); err != nil {
				t.Fatal("frozen metadata retained caller storage", err)
			}
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			var restored Plan
			if err = json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if err = ValidatePlan(restored); err != nil || !reflect.DeepEqual(restored.Statements(), p.Statements()) {
				t.Fatal("native plan cannot be safely restored", err)
			}
		})
	}
}

func TestNativeUserVariableIntegerLiteralsAndArithmetic(t *testing.T) {
	for _, rhs := range []string{"NULL", "-9223372036854775808", "9223372036854775807", "2147483647 + 1", "-(OLD.CurrentValue - 1) * (+2 + 3)", "NULL + 1"} {
		t.Run(rhs, func(t *testing.T) {
			tables, opt := nativeVariableFixture()
			p, err := Convert(trigger("SET @v="+rhs), tables, opt)
			if err != nil {
				t.Fatal(err)
			}
			if !p.NativeUserVariables || !strings.Contains(p.FunctionBody, "AS bigint") {
				t.Fatal("integer operand was not widened", p.FunctionBody)
			}
			if err = ValidatePlan(p); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Constant writes do not depend on source character or numeric metadata.
	p, err := Convert(trigger("SET @v=1"), fixture(), Options{Schema: "Dest", NativeUserVariables: true})
	if err != nil || p.CharacterMetadata != nil {
		t.Fatal("constant assignment unexpectedly requires metadata", err)
	}
}

func TestNativeUserVariableNegationRequiresProvenSignedRange(t *testing.T) {
	for _, tc := range []struct {
		typ, rhs string
		accept   bool
	}{
		{"tinyint", "-OLD.CurrentValue", true},
		{"smallint", "-OLD.CurrentValue", true},
		{"mediumint", "-OLD.CurrentValue", true},
		{"int", "-OLD.CurrentValue", true},
		{"int", "-(OLD.CurrentValue - 1)", true},
		{"int", "-(OLD.CurrentValue * OLD.CurrentValue)", true},
		{"int", "-(OLD.CurrentValue * OLD.CurrentValue * 3)", false},
		{"bigint", "-OLD.CurrentValue", false},
		{"bigint", "-(OLD.CurrentValue + 0)", false},
		{"bigint", "-(OLD.CurrentValue - 1)", false},
		{"bigint", "-(OLD.CurrentValue * 1)", false},
		{"bigint", "-(OLD.CurrentValue + 1)", true},
		{"bigint", "-(OLD.CurrentValue * 0)", true},
		{"bigint", "-(-9223372036854775808)", false},
		{"bigint", "-(+(-9223372036854775808))", false},
		{"bigint", "-(0 - 9223372036854775807 - 1)", false},
		{"bigint", "-(-9223372036854775807 - 1)", false},
		{"bigint", "-(-9223372036854775807)", true},
		{"bigint", "-9223372036854775808", true},
		{"bigint", "-NULL", true},
		{"bigint", "-(NULL + OLD.CurrentValue)", true},
	} {
		t.Run(tc.typ+" "+tc.rhs, func(t *testing.T) {
			tables, opt := nativeVariableFixture()
			tables["Orders"].Columns[len(tables["Orders"].Columns)-1].DataType = tc.typ
			opt.CharacterMetadata.NumericColumns[1].ColumnType = tc.typ
			p, err := Convert(trigger("SET @v="+tc.rhs), tables, opt)
			if !tc.accept {
				if err == nil || !strings.Contains(err.Error(), "MIN_INT64") || p.FunctionSQL != "" || p.TriggerSQL != "" {
					t.Fatal("potential unsigned negation was not rejected safely", p, err)
				}
				return
			}
			if err != nil {
				t.Fatal("proven signed negation was rejected", err)
			}
			if err = ValidatePlan(p); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeUserVariablesAcceptOnlyProvenSignedIntegerColumns(t *testing.T) {
	for _, typ := range []string{"tinyint", "smallint(6)", "mediumint", "int(11)", "integer", "bigint"} {
		t.Run(typ, func(t *testing.T) {
			tables, opt := nativeVariableFixture()
			col := &tables["Orders"].Columns[len(tables["Orders"].Columns)-1]
			col.DataType = strings.Split(typ, "(")[0]
			opt.CharacterMetadata.NumericColumns[1].ColumnType = typ
			p, err := Convert(trigger("SET @v=OLD.CurrentValue"), tables, opt)
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidatePlan(p); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, typ := range []string{"int unsigned", "int zerofill", "int(11) unsigned zerofill", "bigint unsigned", "decimal(10,2)", "float", "bigint", "int(10,2)"} {
		t.Run("reject "+typ, func(t *testing.T) {
			tables, opt := nativeVariableFixture()
			opt.CharacterMetadata.NumericColumns[1].ColumnType = typ
			p, err := Convert(trigger("SET @v=OLD.CurrentValue"), tables, opt)
			if err == nil || p.FunctionSQL != "" || p.TriggerSQL != "" {
				t.Fatal("unproven integer type produced executable SQL", typ)
			}
		})
	}
}

func TestNativeUserVariableMetadataFailuresAreClassified(t *testing.T) {
	for _, scenario := range []string{"absent", "missing column"} {
		t.Run(scenario, func(t *testing.T) {
			tables, opt := nativeVariableFixture()
			if scenario == "absent" {
				opt.CharacterMetadata = nil
			} else {
				opt.CharacterMetadata.NumericColumns = nil
			}
			_, err := Convert(trigger("SET @v=OLD.CurrentValue"), tables, opt)
			if !errors.Is(err, ErrSourceMetadataRequired) {
				t.Fatal("missing numeric snapshot did not preserve dependency error", err)
			}
		})
	}
	for _, scenario := range []string{"duplicate", "character conflict", "unknown version"} {
		t.Run(scenario, func(t *testing.T) {
			tables, opt := nativeVariableFixture()
			switch scenario {
			case "duplicate":
				opt.CharacterMetadata.NumericColumns = append(opt.CharacterMetadata.NumericColumns, opt.CharacterMetadata.NumericColumns[1])
			case "character conflict":
				opt.CharacterMetadata.Columns = []source.TriggerColumnCharacter{{Schema: "SourceDB", Table: "Orders", Name: "CurrentValue"}}
			case "unknown version":
				opt.CharacterMetadata.ServerVersion = "8.0.42 Percona"
			}
			_, err := Convert(trigger("SET @v=OLD.CurrentValue"), tables, opt)
			if err == nil || errors.Is(err, ErrSourceMetadataRequired) {
				t.Fatal("invalid snapshot should fail without claiming it is missing", err)
			}
		})
	}
}

func TestNativeUserVariablesRejectUnsupportedFormsWithoutPartialPlan(t *testing.T) {
	for _, body := range []string{
		"SET @@v=1", "SET @v=1,@other=2", "SET @v=@other", "SET @v=@v+1", "SET @v=(SELECT 1)",
		"SET @v=1.5", "SET @v=1e2", "SET @v='1'", "SET @v=TRUE", "SET @v=FALSE", "SET @v=NEW.Amount", "SET @v=NEW.Note", "SET @v=NOW()",
		"SET @v=9223372036854775808", "SET @v=-9223372036854775809", "SET @v=1/2", "SET @v=IFNULL(OLD.CurrentValue,0)",
		"SET @v=OLD.CurrentValue; DROP TABLE Orders", "SET @v=1 UNION SELECT 2", "SET @v=NEW.CurrentValue AS value",
		"SET @`v`=1", "SET @'v'=1", "SET @foo.bar=1", "SET @变量=1", "SET @1v=1", "SET @ v=1", "SET @" + strings.Repeat("a", 65) + "=1",
	} {
		t.Run(body, func(t *testing.T) {
			tables, opt := nativeVariableFixture()
			p, err := Convert(trigger(body), tables, opt)
			if err == nil || p.FunctionSQL != "" || p.TriggerSQL != "" || p.NativeUserVariables {
				t.Fatal("unsupported user-variable syntax produced a partial plan", body, p)
			}
		})
	}
	tables, opt := nativeVariableFixture()
	if _, err := Convert(trigger("SET @"+strings.Repeat("a", 64)+"=1"), tables, opt); err != nil {
		t.Fatal("64-byte user variable was rejected", err)
	}
}

func TestNativeUserVariableCapabilityAndVersionAreFrozen(t *testing.T) {
	tables, opt := nativeVariableFixture()
	i := trigger("SET @CurrentValue=OLD.CurrentValue+1")
	opt.NativeUserVariables = false
	if _, err := Convert(i, tables, opt); !errors.Is(err, ErrNativeUserVariablesRequired) {
		t.Fatal("missing native target capability was not identified", err)
	}
	opt.NativeUserVariables = true
	p, err := Convert(i, tables, opt)
	if err != nil {
		t.Fatal(err)
	}
	p.NativeUserVariables = false
	if err = ValidatePlan(p); err == nil {
		t.Fatal("native capability flag can be stripped from a saved plan")
	}
	plain, err := Convert(trigger("SET NEW.Amount=1"), tables, opt)
	if err != nil || plain.NativeUserVariables {
		t.Fatal("ordinary plan acquired an unused native capability", err)
	}
	plain.NativeUserVariables = true
	if err = ValidatePlan(plain); err == nil {
		t.Fatal("unused native capability can be forged into a saved plan")
	}
	for _, version := range []int{0, 1, 2, 3} {
		generator := version
		if generator == 0 {
			generator = 1
		}
		if _, err = convertVersion(i, tables, opt, generator); err == nil {
			t.Fatal("legacy generator accepted new user-variable semantics", version)
		}
		legacy, err := convertVersion(trigger("SET NEW.Amount=1"), tables, opt, generator)
		if err != nil {
			t.Fatal(err)
		}
		legacy.Version, legacy.NativeUserVariables = version, true
		if err = ValidatePlan(legacy); err == nil {
			t.Fatal("legacy plan accepted new native capability flag", version)
		}
	}
}
