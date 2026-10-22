package mysqltrigger

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
)

func characterFixture(schema string, tables map[string]*source.TableDDLInfo) *source.TriggerCharacterMetadata {
	weights := map[rune]rune{'ß': 'S', 'é': 'E', 'É': 'E', 'İ': 'I', 'ı': 'I'}
	for ch := 'a'; ch <= 'z'; ch++ {
		weights[ch] = ch - 'a' + 'A'
	}
	var cp []int
	for ch := range weights {
		cp = append(cp, int(ch))
	}
	sort.Ints(cp)
	var from, to strings.Builder
	for _, ch := range cp {
		from.WriteRune(rune(ch))
		to.WriteRune(weights[rune(ch)])
	}
	p := source.TriggerCollationProfile{Charset: "utf8mb4", Collation: "utf8mb4_general_ci", PadAttribute: "PAD SPACE", From: from.String(), To: to.String(), SupplementaryWeight: 0xfffd}
	p.Digest = source.TriggerCollationDigest(p)
	m := &source.TriggerCharacterMetadata{ServerVersion: "8.4.0 MySQL Community Server - GPL", Profiles: []source.TriggerCollationProfile{p}}
	var names []string
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, col := range tables[name].Columns {
			if kindOf(col.DataType) == kindText {
				m.Columns = append(m.Columns, source.TriggerColumnCharacter{Schema: schema, Table: name, Name: col.Name, Charset: p.Charset, Collation: p.Collation})
			}
			if kindOf(col.DataType) == kindNumber {
				columnType := col.DataType
				if columnType == "decimal" || columnType == "numeric" {
					columnType = fmt.Sprintf("%s(%d,%d)", columnType, col.Precision, col.Scale)
				}
				m.NumericColumns = append(m.NumericColumns, source.TriggerNumericColumn{Schema: schema, Table: name, Name: col.Name, ColumnType: columnType})
			}
		}
	}
	return m
}

func TestV2ConcatPreservesNullAndSupportedFormats(t *testing.T) {
	tables := fixture()
	tables["Orders"].Columns[1].Precision = 10
	tables["Orders"].Columns[1].Scale = 2
	for _, tc := range []struct {
		body string
		want []string
	}{
		{"SET NEW.Note=CONCAT('prefix',NEW.Note,OLD.Id,NULL,'',-1,1.2300)", []string{`CAST(E'prefix' AS text) || CAST(NEW."Note" AS text) || CAST(OLD."Id" AS text) || CAST(NULL AS text)`, `CAST((-1) AS text)`, `CAST(1.2300 AS text)`}},
		{"SET NEW.Note=CONCAT(CONCAT(NEW.Note,':'),NEW.Amount)", []string{`CAST((CAST(NEW."Note" AS text) || CAST(E':' AS text)) AS text)`, `CAST(CAST(NEW."Amount" AS numeric(10,2)) AS text)`}},
		{"SET NEW.Note=CONCAT(NULL)", []string{`NEW."Note" := CAST(NULL AS text);`}},
		{"SET NEW.Note=CONCAT(IFNULL(NEW.Note,'a '),'')", []string{`COALESCE(CAST(NEW."Note" AS text), CAST(E'a ' AS text))`}},
	} {
		t.Run(tc.body, func(t *testing.T) {
			p, err := Convert(trigger(tc.body), tables, Options{Schema: "Dest", CharacterMetadata: characterFixture("SourceDB", tables)})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(p.FunctionBody, want) {
					t.Fatalf("missing %q in %s", want, p.FunctionBody)
				}
			}
			if p.Version != CurrentVersion || p.CharacterMetadata != nil && len(p.CharacterMetadata.Profiles) != 0 {
				t.Fatal("CONCAT must not need a collation profile")
			}
			if !p.RequiresTextSemantics {
				t.Fatal("CONCAT omitted the target text-semantics requirement")
			}
			if strings.Contains(p.FunctionBody, "pg_catalog.concat(") {
				t.Fatal("PG concat would discard NULL")
			}
			if err = ValidatePlan(p); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestV2ConcatRejectsUnprovenFormats(t *testing.T) {
	for _, body := range []string{"SET NEW.Note=CONCAT()", "SET NEW.Note=CONCAT(NOW())", "SET NEW.Note=CONCAT(NEW.Amount+1)", "SET NEW.Note=CONCAT(IFNULL(NEW.Amount,0))", "SET NEW.Note=CONCAT(TRUE)", "SET NEW.Note=CONCAT(1e3)", "SET NEW.Note=CONCAT(x'41')", "SET NEW.Note=CONCAT(NEW.Amount)"} {
		if _, err := Convert(trigger(body), fixture(), Options{Schema: "Dest"}); err == nil {
			t.Fatalf("accepted unsupported CONCAT: %s", body)
		}
	}
}

func TestV2TextCoalesceCastsCharAndLiteralBeforeResolvingType(t *testing.T) {
	tables := fixture()
	for _, table := range tables {
		table.Columns = append(table.Columns, source.ColumnInfo{Name: "CharNote", DataType: "char", Length: 8, IsNullable: true})
	}
	for _, tc := range []struct{ body, want string }{
		{"SET NEW.Note=IFNULL(NEW.CharNote,'a ')", `COALESCE(CAST(NEW."CharNote" AS text), CAST(E'a ' AS text))`},
		{"SET NEW.Note=COALESCE(NEW.CharNote,NULL,'a ')", `COALESCE(CAST(NEW."CharNote" AS text), CAST(NULL AS text), CAST(E'a ' AS text))`},
		{"INSERT INTO Audit(Note) SELECT IFNULL(CharNote,'a ') FROM Audit WHERE Id=NEW.Id", `COALESCE(CAST("Audit"."CharNote" AS text), CAST(E'a ' AS text))`},
	} {
		t.Run(tc.body, func(t *testing.T) {
			p, err := Convert(trigger(tc.body), tables, Options{Schema: "Dest"})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(p.FunctionBody, tc.want) {
				t.Fatal("CHAR fallback can lose its trailing space", p.FunctionBody)
			}
			if !p.RequiresTextSemantics {
				t.Fatal("text COALESCE omitted the target text-semantics requirement")
			}
			if err = ValidatePlan(p); err != nil {
				t.Fatal(err)
			}
		})
	}
	p, err := convertVersion(trigger("SET NEW.Note=IFNULL(NEW.CharNote,'a ')"), tables, Options{Schema: "Dest"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.FunctionBody, `NEW."Note" := COALESCE(NEW."CharNote", E'a ');`) {
		t.Fatal("legacy COALESCE SQL changed", p.FunctionBody)
	}
	if p.RequiresTextSemantics {
		t.Fatal("legacy text requirement changed")
	}
	if err = ValidatePlan(p); err != nil {
		t.Fatal(err)
	}
}

func TestV2TextSemanticsRequirementIsFrozenWithoutCollationProfile(t *testing.T) {
	for _, body := range []string{"SET NEW.Note=CONCAT('a','')", "SET NEW.Note=COALESCE(NEW.Note,'a ')", "SET NEW.Note=IFNULL(NEW.Note,'')"} {
		p, err := Convert(trigger(body), fixture(), Options{Schema: "Dest"})
		if err != nil {
			t.Fatal(err)
		}
		if !p.RequiresTextSemantics || p.CharacterMetadata != nil {
			t.Fatal("text operators must require a probe without requiring source collation metadata")
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
		restored.RequiresTextSemantics = false
		if err = ValidatePlan(restored); err == nil {
			t.Fatal("removing target semantics requirement was accepted")
		}
	}
	for _, version := range []int{1, 2} {
		p, err := convertVersion(trigger("SET NEW.Amount=IFNULL(NEW.Amount,0)"), fixture(), Options{Schema: "Dest"}, version)
		if err != nil {
			t.Fatal(err)
		}
		if p.RequiresTextSemantics {
			t.Fatal("numeric COALESCE requires no text probe")
		}
		p.RequiresTextSemantics = true
		if err = ValidatePlan(p); err == nil {
			t.Fatal("altered text requirement was accepted")
		}
	}
}

func TestV2CharacterComparisonFreezesOnlyUsedSourceProfile(t *testing.T) {
	tables := fixture()
	meta := characterFixture("SourceDB", tables)
	i := trigger("IF OLD.Note != NEW.Note THEN SET NEW.Amount=1; END IF")
	p, err := Convert(i, tables, Options{Schema: "Dest", CharacterMetadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	if p.CharacterMetadata == nil || len(p.CharacterMetadata.Profiles) != 1 || len(p.CharacterMetadata.Columns) != 1 || p.CharacterMetadata.Columns[0].Table != "Orders" {
		t.Fatalf("not pruned: %+v", p.CharacterMetadata)
	}
	for _, want := range []string{"pg_catalog.rtrim(CAST(OLD.\"Note\" AS text), ' ')", "pg_catalog.regexp_replace(", "COLLATE pg_catalog.\"C\"", "pg_catalog.translate(", "pg_catalog.convert_to(", hex.EncodeToString([]byte(meta.Profiles[0].From)), " <> "} {
		if !strings.Contains(p.FunctionBody, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(p.FunctionBody, "lower(") || strings.Contains(p.FunctionBody, "unaccent(") || strings.Contains(p.FunctionBody, "IS DISTINCT FROM") {
		t.Fatal("comparison semantics changed")
	}
	if err = ValidatePlan(p); err != nil {
		t.Fatal(err)
	}
	// The frozen plan must not keep a mutable reference to caller metadata.
	meta.Columns[0].Name = "mutated"
	meta.Profiles[0].From = "bad"
	if err = ValidatePlan(p); err != nil {
		t.Fatal("plan mutated with caller metadata", err)
	}
}

func TestV2CharacterComparisonLiteralUsesColumnCollation(t *testing.T) {
	tables := fixture()
	meta := characterFixture("SourceDB", tables)
	for _, body := range []string{"IF NEW.Note='é' THEN SET NEW.Amount=1; END IF", "IF 'é' != OLD.Note THEN SET NEW.Amount=1; END IF", "IF IFNULL(NEW.Note,'') = OLD.Note THEN SET NEW.Amount=1; END IF"} {
		p, err := Convert(trigger(body), tables, Options{Schema: "Dest", CharacterMetadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		if err = ValidatePlan(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestV2CharacterComparisonRejectsUnknownPolicyAndTampering(t *testing.T) {
	for _, scenario := range []string{"missing metadata", "missing column", "unknown collation", "bad digest", "bad map", "unsupported operator", "literal only", "unknown version", "mixed column collations"} {
		t.Run(scenario, func(t *testing.T) {
			tables := fixture()
			meta := characterFixture("SourceDB", tables)
			body := "IF OLD.Note!=NEW.Note THEN SET NEW.Amount=1; END IF"
			switch scenario {
			case "missing metadata":
				meta = nil
			case "missing column":
				meta.Columns = nil
			case "unknown collation":
				for i := range meta.Columns {
					meta.Columns[i].Collation = "utf8mb4_unicode_ci"
				}
			case "bad digest":
				meta.Profiles[0].Digest = "wrong"
			case "bad map":
				meta.Profiles[0].From += " "
				meta.Profiles[0].To += "A"
				meta.Profiles[0].Digest = source.TriggerCollationDigest(meta.Profiles[0])
			case "unsupported operator":
				body = "IF OLD.Note>NEW.Note THEN SET NEW.Amount=1; END IF"
			case "literal only":
				body = "IF 'a'='A' THEN SET NEW.Amount=1; END IF"
			case "unknown version":
				meta.ServerVersion = "10.11.0 MariaDB"
			case "mixed column collations":
				tables["Orders"].Columns = append(tables["Orders"].Columns, source.ColumnInfo{Name: "OtherNote", DataType: "varchar"})
				meta.Columns = append(meta.Columns, source.TriggerColumnCharacter{Schema: "SourceDB", Table: "Orders", Name: "OtherNote", Charset: "utf8mb4", Collation: "utf8mb4_bin"})
				body = "IF NEW.Note=NEW.OtherNote THEN SET NEW.Amount=1; END IF"
			}
			if _, err := Convert(trigger(body), tables, Options{Schema: "Dest", CharacterMetadata: meta}); err == nil {
				t.Fatal("accepted unknown character semantics")
			}
		})
	}
	tables := fixture()
	p, err := Convert(trigger("IF OLD.Note!=NEW.Note THEN SET NEW.Amount=1; END IF"), tables, Options{Schema: "Dest", CharacterMetadata: characterFixture("SourceDB", tables)})
	if err != nil {
		t.Fatal(err)
	}
	q := p
	q.CharacterMetadata = &source.TriggerCharacterMetadata{ServerVersion: p.CharacterMetadata.ServerVersion, Columns: append([]source.TriggerColumnCharacter(nil), p.CharacterMetadata.Columns...), Profiles: append([]source.TriggerCollationProfile(nil), p.CharacterMetadata.Profiles...)}
	q.CharacterMetadata.Profiles[0].To = strings.Repeat("Z", len([]rune(q.CharacterMetadata.Profiles[0].From)))
	q.CharacterMetadata.Profiles[0].Digest = source.TriggerCollationDigest(q.CharacterMetadata.Profiles[0])
	if err = ValidatePlan(q); err == nil {
		t.Fatal("updated profile must not validate old SQL")
	}
}

func TestV2UnusedCharacterMetadataDoesNotBlockNumericOrConcat(t *testing.T) {
	tables := fixture()
	meta := characterFixture("SourceDB", tables)
	meta.ServerVersion = "unavailable"
	meta.Profiles[0].Digest = "broken"
	for _, body := range []string{"SET NEW.Amount=1", "SET NEW.Note=CONCAT('note:',NEW.Note)"} {
		p, err := Convert(trigger(body), tables, Options{Schema: "Dest", CharacterMetadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		if p.CharacterMetadata != nil {
			t.Fatal("unused source profile was frozen")
		}
		if err = ValidatePlan(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestV2InsertSelectMapsReadTableAndPreservesWriteRestriction(t *testing.T) {
	tables := fixture()
	i := trigger("INSERT INTO Audit(Id, Amount, Note, ChangedAt) SELECT Id, Amount, CONCAT(Note, ':', NEW.Id), NOW() FROM SourceDB.Orders WHERE Id=NEW.Id")
	p, err := Convert(i, tables, Options{Schema: "Dest", Name: strings.ToLower, CharacterMetadata: characterFixture("SourceDB", tables)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`INSERT INTO "Dest"."audit" ("id", "amount", "note", "changedat") SELECT "orders"."id", "orders"."amount"`, `FROM "Dest"."orders" WHERE ("orders"."id" = NEW."id")`} {
		if !strings.Contains(p.FunctionBody, want) {
			t.Fatalf("missing %q in %s", want, p.FunctionBody)
		}
	}
	if !reflect.DeepEqual(p.Dependencies, []string{"audit", "orders"}) {
		t.Fatal(p.Dependencies)
	}
	if err = ValidatePlan(p); err != nil {
		t.Fatal(err)
	}
	i.Body = "INSERT INTO Orders(Note) SELECT Note FROM Audit WHERE Id=NEW.Id"
	if _, err = Convert(i, tables, Options{Schema: "Dest"}); err == nil {
		t.Fatal("allowed write to trigger base table")
	}
}

func TestV2InsertSelectRejectsUnconsumedFeatures(t *testing.T) {
	for _, body := range []string{
		"INSERT INTO Audit(Id) SELECT Id FROM Orders", "INSERT INTO Audit(Id) SELECT * FROM Orders WHERE Id=NEW.Id",
		"INSERT INTO Audit(Id) SELECT Id AS other FROM Orders WHERE Id=NEW.Id", "INSERT INTO Audit(Id) SELECT a.Id FROM Orders a WHERE a.Id=NEW.Id",
		"INSERT INTO Audit(Id) SELECT Id FROM OtherDB.Orders WHERE Id=NEW.Id", "INSERT INTO Audit(Id) SELECT Id FROM Orders WHERE Id=NEW.Id ORDER BY Id",
		"INSERT INTO Audit(Id) SELECT Id FROM Orders WHERE Id=NEW.Id LIMIT 1", "INSERT INTO Audit(Id) SELECT Id FROM Orders WHERE Id=NEW.Id GROUP BY Id",
		"INSERT INTO Audit(Id) SELECT Id FROM Orders WHERE Id=NEW.Id HAVING Id=1", "INSERT INTO Audit(Id) SELECT DISTINCT Id FROM Orders WHERE Id=NEW.Id",
		"INSERT INTO Audit(Id) SELECT SQL_CACHE Id FROM Orders WHERE Id=NEW.Id", "INSERT INTO Audit(Id) SELECT HIGH_PRIORITY Id FROM Orders WHERE Id=NEW.Id",
		"INSERT INTO Audit(Id) SELECT Id FROM Orders WHERE Id=NEW.Id FOR UPDATE", "INSERT INTO Audit(Id) SELECT Id FROM Orders USE INDEX(x) WHERE Id=NEW.Id",
		"INSERT INTO Audit(Id) SELECT Id FROM Orders WHERE Id=NEW.Id UNION SELECT 1", "INSERT INTO Audit(Id) SELECT (SELECT Id FROM Orders) FROM Orders WHERE Id=NEW.Id",
		"INSERT INTO Audit(Id) SELECT a.Id FROM Orders a JOIN Audit b ON a.Id=b.Id WHERE a.Id=NEW.Id", "INSERT IGNORE INTO Audit(Id) SELECT Id FROM Orders WHERE Id=NEW.Id",
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := Convert(trigger(body), fixture(), Options{Schema: "Dest"}); err == nil {
				t.Fatalf("accepted unsupported SELECT %s", body)
			}
		})
	}
}

func TestVersionedPlanPreservesLegacySQLAndRetry(t *testing.T) {
	i := trigger("SET NEW.Amount=1")
	p, err := convertVersion(i, fixture(), Options{Schema: "Dest"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	wantBody := "\n-- dbgold:mysql-trigger:v1\nBEGIN\nNEW.\"Amount\" := 1;\nRETURN NEW;\nEND;\n"
	if p.FunctionBody != wantBody {
		t.Fatalf("legacy SQL changed %q", p.FunctionBody)
	}
	if p.FunctionName != "dbgold_trg_2a641a13cb5e8cf4459be7ccec9df0d0" {
		t.Fatal("legacy helper identity changed", p.FunctionName)
	}
	wantSQL := "CREATE FUNCTION \"Dest\".\"dbgold_trg_2a641a13cb5e8cf4459be7ccec9df0d0\"() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER AS $dbgold$" + wantBody + "$dbgold$;"
	if p.FunctionSQL != wantSQL {
		t.Fatalf("legacy statement changed %s", p.FunctionSQL)
	}
	if p.TriggerSQL != `CREATE TRIGGER "OrdersAudit" BEFORE UPDATE ON "Dest"."Orders" FOR EACH ROW EXECUTE PROCEDURE "Dest"."dbgold_trg_2a641a13cb5e8cf4459be7ccec9df0d0"();` {
		t.Fatal("legacy trigger SQL changed", p.TriggerSQL)
	}
	for _, version := range []int{0, 1} {
		q := p
		q.Version = version
		raw, err := json.Marshal(q)
		if err != nil {
			t.Fatal(err)
		}
		var restored Plan
		if err = json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		if err = ValidatePlan(restored); err != nil {
			t.Fatalf("legacy %d: %v", version, err)
		}
	}
	newPlan, err := Convert(i, fixture(), Options{Schema: "Dest"})
	if err != nil {
		t.Fatal(err)
	}
	if newPlan.Version != CurrentVersion || newPlan.FunctionBody == p.FunctionBody {
		t.Fatal("new generator version not frozen")
	}
	if err = ValidatePlan(newPlan); err != nil {
		t.Fatal(err)
	}
	newPlan.Version = 99
	if err = ValidatePlan(newPlan); err == nil {
		t.Fatal("accepted unknown version")
	}
	for _, body := range []string{"SET NEW.Note=CONCAT(NEW.Note,'x')", "IF OLD.Note!=NEW.Note THEN SET NEW.Amount=1; END IF", "INSERT INTO Audit(Id) SELECT Id FROM Orders WHERE Id=NEW.Id"} {
		if _, err = convertVersion(trigger(body), fixture(), Options{Schema: "Dest", CharacterMetadata: characterFixture("SourceDB", fixture())}, 1); err == nil {
			t.Fatal("new feature leaked into v1")
		}
	}
}
