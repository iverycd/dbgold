package mysqltrigger

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
)

func fixture() map[string]*source.TableDDLInfo {
	return map[string]*source.TableDDLInfo{
		"Orders": {TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int", Extra: "auto_increment"}, {Name: "Amount", DataType: "decimal"}, {Name: "Note", DataType: "varchar"}, {Name: "ChangedAt", DataType: "datetime", Extra: "DEFAULT_GENERATED"}}},
		"Audit":  {TableName: "Audit", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int"}, {Name: "Amount", DataType: "decimal"}, {Name: "Note", DataType: "varchar"}, {Name: "ChangedAt", DataType: "datetime"}}},
	}
}
func trigger(body string) source.TriggerInfo {
	return source.TriggerInfo{Name: "OrdersAudit", TableName: "Orders", Schema: "SourceDB", Timing: "BEFORE", Event: "UPDATE", Body: body, SQLMode: "STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION", SourceDDL: "original DDL retained", ActionOrder: 1, Definer: "root@localhost"}
}
func TestConvertAssignmentsBranchesAndTime(t *testing.T) {
	i := trigger("BEGIN IF NEW.Amount > 0 THEN SET NEW.Note = IFNULL(NEW.Note, 'default'); ELSEIF NEW.Amount IS NULL THEN SET NEW.Amount = 0; ELSE SET NEW.Amount = -NEW.Amount; END IF; SET NEW.ChangedAt = CURRENT_TIMESTAMP; END")
	p, err := Convert(i, fixture(), Options{Schema: "TargetDB", Name: strings.ToLower})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`NEW."note" := COALESCE(NEW."note", E'default');`, `ELSIF (NEW."amount" IS NULL) THEN`, `pg_catalog.date_trunc('second', pg_catalog.statement_timestamp())::timestamp without time zone`, `RETURN NEW;`, `-- ` + ToolMarker} {
		if !strings.Contains(p.FunctionBody, want) {
			t.Fatalf("missing %q in %s", want, p.FunctionBody)
		}
	}
	if p.Source.Body != i.Body || p.Source.SourceDDL != i.SourceDDL || p.Source.Name != i.Name {
		t.Fatal("source definition changed")
	}
	if p.Name != "ordersaudit" || p.Table != "orders" || p.Dependencies[0] != "orders" {
		t.Fatalf("mapped plan: %+v", p)
	}
	if err = ValidatePlan(p); err != nil {
		t.Fatal(err)
	}
	if len(p.Statements()) != 2 || !strings.Contains(p.FunctionSQL, "SECURITY INVOKER") || !strings.Contains(p.TriggerSQL, `EXECUTE PROCEDURE "TargetDB".`) {
		t.Fatal("incorrect two-part DDL")
	}
}
func TestConvertSixTriggerKindsAndDML(t *testing.T) {
	for _, timing := range []string{"BEFORE", "AFTER"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			t.Run(timing+event, func(t *testing.T) {
				i := trigger("")
				i.Timing = timing
				i.Event = event
				row := "NEW"
				if event == "DELETE" {
					row = "OLD"
				}
				i.Body = "BEGIN INSERT INTO SourceDB.Audit (Amount, Note, ChangedAt) VALUES (" + row + ".Amount, COALESCE(" + row + ".Note, ''), NOW(6)); UPDATE Audit SET Amount=Amount+1 WHERE Id=" + row + ".Amount; DELETE FROM Audit WHERE Id=" + row + ".Amount; END"
				p, err := Convert(i, fixture(), Options{Schema: "Dest"})
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{`INSERT INTO "Dest"."Audit"`, `UPDATE "Dest"."Audit" SET "Amount" = ("Audit"."Amount" + 1)`, `DELETE FROM "Dest"."Audit"`, `RETURN ` + row + `;`} {
					if !strings.Contains(p.FunctionBody, want) {
						t.Fatalf("missing %s in %s", want, p.FunctionBody)
					}
				}
				if strings.Join(p.Dependencies, ",") != "Audit,Orders" {
					t.Fatalf("dependencies %v", p.Dependencies)
				}
				if err = ValidatePlan(p); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
func TestConvertRejectsUnsupportedAndUnsafeSemantics(t *testing.T) {
	cases := []string{
		"SET @sum = NEW.Amount", "SET NEW.Amount = 1, NEW.Note = 'x'", "DECLARE v int", "CALL p()",
		"SET NEW.Amount = IFNULL(NEW.Note,0)", "SET NEW.Amount = (SELECT 1)", "SET NEW.Amount = unknown_fn(1)",
		"SET NEW.ChangedAt=NOW(3)", "IF NEW.Note='x' THEN SET NEW.Amount=1; END IF", "IF NEW.Amount THEN SET NEW.Amount=1; END IF",
		"INSERT IGNORE INTO Audit(Amount) VALUES(NEW.Amount)", "REPLACE INTO Audit(Amount) VALUES(1)",
		"INSERT INTO Audit(Amount) SELECT Amount FROM Orders", "INSERT INTO Audit(Amount) VALUES(1) ON DUPLICATE KEY UPDATE Amount=1",
		"UPDATE Audit SET Amount=1, Id=Amount", "UPDATE Audit SET Amount=1 LIMIT 1", "DELETE FROM Audit LIMIT 1",
		"DELETE a FROM Audit a JOIN Orders b ON a.Id=b.Id", "INSERT INTO OtherDB.Audit(Amount) VALUES(1)",
		"UPDATE Orders SET Amount=1", "SET NEW.Amount=NEW.Id+OLD.Id; DROP TABLE Audit", "SET NEW.Amount=1/0",
		"SET NEW.Note=_binary'abc'", "SET NEW.Note='\\0'", "SET NEW.Amount=/*!50000 1 */",
		"SET NEW.Amount=1 UNION SELECT 2", "SET NEW.Amount=1 FROM Audit", "SET NEW.Amount=1 AS value",
		"SET NEW.Amount = NEW.Amount + '1'", "SET NEW.Note = CONCAT(NEW.Note,'x')", "SELECT Amount INTO @a FROM Audit",
		"INSERT HIGH_PRIORITY INTO Audit(Amount) VALUES(1)", "INSERT INTO Audit PARTITION(p0) (Amount) VALUES(1)", "INSERT INTO Audit SET Amount=1",
		"UPDATE LOW_PRIORITY Audit SET Amount=1", "UPDATE IGNORE Audit SET Amount=1", "UPDATE Audit SET Amount=1 ORDER BY Id",
		"DELETE QUICK FROM Audit", "DELETE LOW_PRIORITY FROM Audit", "DELETE IGNORE FROM Audit", "DELETE FROM Audit ORDER BY Id",
		"DELETE FROM Audit PARTITION(p0)", "UPDATE Audit USE INDEX(x) SET Amount=1", "UPDATE Audit AS a SET Amount=1",
		"SET NEW.Amount=ALL 1", "SET NEW.Amount=SQL_CACHE 1", "SET NEW.Amount=SQL_NO_CACHE 1", "SET NEW.Amount=SQL_CALC_FOUND_ROWS 1",
		"SET NEW.Amount={fn IFNULL(NEW.Amount,0)}",
		"IF NEW.ChangedAt > OLD.ChangedAt THEN SET NEW.Amount=1; END IF",
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			p, err := Convert(trigger(body), fixture(), Options{Schema: "Dest"})
			if err == nil {
				t.Fatalf("accepted unsafe body %q: %s", body, p.FunctionBody)
			}
			if p.FunctionSQL != "" || p.TriggerSQL != "" {
				t.Fatal("partial executable plan escaped")
			}
		})
	}
}

func TestConvertRejectsExplicitDependencyIdentityWrites(t *testing.T) {
	tables := fixture()
	tables["Audit"].Columns[0].Extra = "auto_increment"
	for _, body := range []string{"INSERT INTO Audit(Id) VALUES(NEW.Id)", "INSERT INTO Audit(Id) VALUES(NULL)", "UPDATE Audit SET Id=1"} {
		if _, err := Convert(trigger(body), tables, Options{Schema: "Dest"}); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	if _, err := Convert(trigger("INSERT INTO Audit(Amount) VALUES(NEW.Amount)"), tables, Options{Schema: "Dest"}); err != nil {
		t.Fatal(err)
	}
}

func TestConvertRejectsOmittedDynamicDefaultsAndAutomaticUpdates(t *testing.T) {
	text := func(s string) *string { return &s }
	for _, tc := range []struct {
		name string
		col  source.ColumnInfo
		body string
	}{
		{"MySQL8 current timestamp", source.ColumnInfo{Name: "ChangedAt", DataType: "datetime", Default: text("CURRENT_TIMESTAMP(6)"), Extra: "DEFAULT_GENERATED"}, "INSERT INTO Audit(Amount) VALUES(1)"},
		{"MySQL57 current timestamp", source.ColumnInfo{Name: "ChangedAt", DataType: "timestamp", Default: text("CURRENT_TIMESTAMP")}, "INSERT INTO Audit(Amount) VALUES(1)"},
		{"UUID expression", source.ColumnInfo{Name: "Note", DataType: "varchar", Default: text("uuid()"), Extra: "DEFAULT_GENERATED"}, "INSERT INTO Audit(Amount) VALUES(1)"},
		{"generated dependency column", source.ColumnInfo{Name: "Derived", DataType: "int", Extra: "VIRTUAL GENERATED"}, "INSERT INTO Audit(Amount) VALUES(1)"},
		{"implicit update timestamp", source.ColumnInfo{Name: "ChangedAt", DataType: "timestamp", Extra: "on update CURRENT_TIMESTAMP"}, "UPDATE Audit SET Amount=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := fixture()
			tables["Audit"].Columns = append(tables["Audit"].Columns, tc.col)
			// Replace a fixture column of the same name so metadata remains valid.
			for i := 0; i < len(tables["Audit"].Columns)-1; i++ {
				if tables["Audit"].Columns[i].Name == tc.col.Name {
					tables["Audit"].Columns = append(tables["Audit"].Columns[:i], tables["Audit"].Columns[i+1:]...)
					break
				}
			}
			if _, err := Convert(trigger(tc.body), tables, Options{Schema: "Dest"}); err == nil {
				t.Fatalf("accepted implicit behavior: %s", tc.body)
			}
		})
	}
	for _, tc := range []struct {
		name string
		col  source.ColumnInfo
		body string
	}{
		{"literal looks like function", source.ColumnInfo{Name: "Note", DataType: "varchar", Default: text("CURRENT_TIMESTAMP()")}, "INSERT INTO Audit(Amount) VALUES(1)"},
		{"constant numeric default", source.ColumnInfo{Name: "Amount", DataType: "decimal", Default: text("1.25")}, "INSERT INTO Audit(Note) VALUES('x')"},
		{"null default", source.ColumnInfo{Name: "Note", DataType: "varchar", Default: nil, IsNullable: true}, "INSERT INTO Audit(Amount) VALUES(1)"},
		{"omitted identity", source.ColumnInfo{Name: "Id", DataType: "int", Extra: "auto_increment"}, "INSERT INTO Audit(Amount) VALUES(1)"},
		{"explicit dynamic column", source.ColumnInfo{Name: "ChangedAt", DataType: "datetime", Default: text("CURRENT_TIMESTAMP"), Extra: "DEFAULT_GENERATED"}, "INSERT INTO Audit(ChangedAt) VALUES(NOW())"},
		{"explicit automatic update column", source.ColumnInfo{Name: "ChangedAt", DataType: "datetime", Extra: "on update CURRENT_TIMESTAMP"}, "UPDATE Audit SET ChangedAt=NOW()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := fixture()
			for i := range tables["Audit"].Columns {
				if tables["Audit"].Columns[i].Name == tc.col.Name {
					tables["Audit"].Columns[i] = tc.col
				}
			}
			if _, err := Convert(trigger(tc.body), tables, Options{Schema: "Dest"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConvertTemporalAssignmentPreservesColumnPrecision(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		precision  int64
		want       string
	}{
		{"NOW6 into datetime0", "SET NEW.ChangedAt=NOW(6)", 0, `NEW."ChangedAt" := CAST(pg_catalog.statement_timestamp()::timestamp without time zone AS timestamp(0) without time zone);`},
		{"NOW0 into datetime6", "SET NEW.ChangedAt=NOW()", 6, `NEW."ChangedAt" := CAST(pg_catalog.date_trunc('second', pg_catalog.statement_timestamp())::timestamp without time zone AS timestamp(6) without time zone);`},
		{"insert NOW6 into datetime0", "INSERT INTO Audit(ChangedAt) VALUES(NOW(6))", 0, `VALUES (CAST(pg_catalog.statement_timestamp()::timestamp without time zone AS timestamp(0) without time zone));`},
		{"update NOW0 into datetime6", "UPDATE Audit SET ChangedAt=NOW(0)", 6, `SET "ChangedAt" = CAST(pg_catalog.date_trunc('second', pg_catalog.statement_timestamp())::timestamp without time zone AS timestamp(6) without time zone);`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := fixture()
			tables["Orders"].Columns[3].DateTimePrecision = tc.precision
			tables["Audit"].Columns[3].DateTimePrecision = tc.precision
			p, err := Convert(trigger(tc.body), tables, Options{Schema: "Dest"})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(p.FunctionBody, tc.want) {
				t.Fatalf("missing precision coercion %q in %s", tc.want, p.FunctionBody)
			}
			if err = ValidatePlan(p); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConvertTemporalReadRetainsSourcePrecisionBeforeWiderWrite(t *testing.T) {
	tables := fixture()
	tables["Orders"].Columns[3].DateTimePrecision = 0
	tables["Audit"].Columns[3].DateTimePrecision = 6
	p, err := Convert(trigger("INSERT INTO Audit(ChangedAt) VALUES(NEW.ChangedAt)"), tables, Options{Schema: "Dest"})
	if err != nil {
		t.Fatal(err)
	}
	want := `VALUES (CAST(CAST(NEW."ChangedAt" AS timestamp(0) without time zone) AS timestamp(6) without time zone));`
	if !strings.Contains(p.FunctionBody, want) {
		t.Fatalf("source precision was lost in %s", p.FunctionBody)
	}
	tables["Orders"].Columns[3].Extra = "DEFAULT_GENERATED on update CURRENT_TIMESTAMP"
	if _, err = Convert(trigger("INSERT INTO Audit(ChangedAt) VALUES(NEW.ChangedAt)"), tables, Options{Schema: "Dest"}); err == nil {
		t.Fatal("accepted ON UPDATE dependency")
	}
}
func TestConvertRejectsInvalidNewOldAndGeneratedColumns(t *testing.T) {
	for _, tc := range []struct{ timing, event, body string }{{"BEFORE", "INSERT", "SET NEW.Amount=NEW.Id"}, {"BEFORE", "INSERT", "SET NEW.Amount=OLD.Amount"}, {"BEFORE", "DELETE", "SET NEW.Amount=1"}, {"AFTER", "INSERT", "SET NEW.Amount=1"}, {"BEFORE", "UPDATE", "SET NEW.Id=1"}} {
		i := trigger(tc.body)
		i.Timing = tc.timing
		i.Event = tc.event
		if _, err := Convert(i, fixture(), Options{Schema: "Dest"}); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
	tables := fixture()
	tables["Orders"].Columns[1].Extra = "STORED GENERATED"
	if _, err := Convert(trigger("SET NEW.Amount=1"), tables, Options{Schema: "Dest"}); err == nil {
		t.Fatal("accepted generated column")
	}
}
func TestConvertStringsModesAndSafeFunctionDelimiter(t *testing.T) {
	i := trigger("SET NEW.Note='END IF; $dbgold$ $dbgold_1$ -- \\\\ ; ''quoted''' ")
	p, err := Convert(i, fixture(), Options{Schema: "Dest"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.FunctionSQL, "AS $dbgold_2$") || !strings.HasSuffix(p.FunctionSQL, "$dbgold_2$;") {
		t.Fatal(p.FunctionSQL)
	}
	if !strings.Contains(p.FunctionBody, "END IF; $dbgold$ $dbgold_1$") {
		t.Fatal("string rewritten")
	}
	if err = ValidatePlan(p); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ mode, body string }{{"ANSI_QUOTES", `SET NEW."Note"='value'`}, {"NO_BACKSLASH_ESCAPES", `SET NEW.Note='path\name'`}, {"", `BEGIN /* END IF; */ SET NEW.Note='/* ; */'; END`}} {
		i := trigger(tc.body)
		i.SQLMode = tc.mode
		if _, err = Convert(i, fixture(), Options{Schema: "Dest"}); err != nil {
			t.Fatal(err)
		}
	}
	i = trigger(`SET NEW."Note"='value'`)
	if _, err = Convert(i, fixture(), Options{Schema: "Dest"}); err == nil {
		t.Fatal("accepted string as identifier without ANSI_QUOTES")
	}
	i = trigger("SET NEW.Amount=1")
	i.SQLMode = "PIPES_AS_CONCAT"
	if _, err = Convert(i, fixture(), Options{Schema: "Dest"}); err == nil {
		t.Fatal("accepted unknown mode")
	}
}

func TestConvertMySQL57DefaultMode(t *testing.T) {
	i := trigger("SET NEW.Amount=IFNULL(NEW.Amount,0)")
	i.SQLMode = "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_AUTO_CREATE_USER,NO_ENGINE_SUBSTITUTION"
	p, err := Convert(i, fixture(), Options{Schema: "Dest"})
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidatePlan(p); err != nil {
		t.Fatal(err)
	}
	i.SQLMode = "ANSI"
	if _, err = Convert(i, fixture(), Options{Schema: "Dest"}); err == nil {
		t.Fatal("accepted composite ANSI mode")
	}
}
func TestConvertRejectsNameCollisionsAndMissingDependencies(t *testing.T) {
	for _, which := range []string{"table", "column", "missing", "long"} {
		t.Run(which, func(t *testing.T) {
			tables := fixture()
			body := "SET NEW.Amount=1"
			switch which {
			case "table":
				tables["orders"] = &source.TableDDLInfo{TableName: "orders"}
			case "column":
				tables["Orders"].Columns = append(tables["Orders"].Columns, source.ColumnInfo{Name: "amount", DataType: "int"})
			case "missing":
				delete(tables, "Audit")
				body = "INSERT INTO Audit(Amount) VALUES(1)"
			case "long":
				tables["Orders"].Columns[1].Name = strings.Repeat("a", 64)
				body = "SET NEW." + strings.Repeat("a", 64) + "=1"
			}
			if _, err := Convert(trigger(body), tables, Options{Schema: "Dest", Name: strings.ToLower}); err == nil {
				t.Fatal("accepted invalid metadata")
			}
		})
	}
}
func TestValidatePlanRejectsTampering(t *testing.T) {
	p, err := Convert(trigger("SET NEW.Amount=1"), fixture(), Options{Schema: "Dest"})
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Plan){func(p *Plan) { p.FunctionSQL += "DROP TABLE x" }, func(p *Plan) { p.FunctionBody += "danger" }, func(p *Plan) { p.TriggerSQL += "x" }, func(p *Plan) { p.FunctionName = "other" }, func(p *Plan) { p.Dependencies = []string{"other"} }, func(p *Plan) { p.SourceTables = nil }} {
		q := p
		mutate(&q)
		if err = ValidatePlan(q); err == nil {
			t.Fatal("accepted modified plan")
		}
	}
}
