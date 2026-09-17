package dialect

import (
	"dbgold/datamigrate/source"
	"strings"
	"testing"
)

func TestMySQLVersionGates(t *testing.T) {
	for _, tc := range []struct {
		v           string
		expr, check bool
	}{{"5.7.44", false, false}, {"8.0.12", false, false}, {"8.0.13", true, false}, {"8.0.15", true, false}, {"8.0.16", true, true}, {"8.0.36", true, true}} {
		v, e := ParseMySQLVersion(tc.v)
		if e != nil || v.Expressions() != tc.expr || v.Checks() != tc.check {
			t.Fatalf("%+v %v", v, e)
		}
	}
	for _, raw := range []string{"5.5.5-10.6-MariaDB", "8.4.1", "unknown"} {
		if _, e := ParseMySQLVersion(raw); e == nil {
			t.Fatal(raw)
		}
	}
}
func TestPGDefaultsPreserveLiteralCategory(t *testing.T) {
	v, _ := ParseMySQLVersion("8.0.36")
	for _, tc := range []struct{ raw, typ, want string }{{`''::varchar`, "VARCHAR(12)", " DEFAULT ''"}, {`'a::b'::text`, "LONGTEXT", ` DEFAULT ('a::b')`}, {`'uuid()'::varchar`, "VARCHAR(30)", ` DEFAULT 'uuid()'`}, {`E'a\\b'::varchar`, "VARCHAR(30)", ` DEFAULT 'a\b'`}, {`now()`, "DATETIME(6)", ` DEFAULT CURRENT_TIMESTAMP(6)`}, {`true`, "TINYINT(1)", ` DEFAULT 1`}} {
		got, e := PGDefault(tc.raw, tc.typ, false, v)
		if e != nil || got != tc.want {
			t.Fatalf("%s: got %q err=%v", tc.raw, got, e)
		}
	}
	v, _ = ParseMySQLVersion("5.7.44")
	for _, tc := range []struct{ raw, typ string }{{`'abc'::text`, "LONGTEXT"}, {`gen_random_uuid()`, "CHAR(36)"}, {`NULL`, "INT"}, {`CURRENT_DATE`, "DATE"}} {
		if _, e := PGDefault(tc.raw, tc.typ, false, v); e == nil {
			t.Fatal(tc)
		}
	}
}
func TestPGAutoIncrementIndexedAtCreation(t *testing.T) {
	v, _ := ParseMySQLVersion("5.7.44")
	table := source.PGTable{Info: source.TableDDLInfo{TableName: "Mixed", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int4", Extra: "auto_increment"}}}, Indexes: []source.IndexInfo{{TableName: "Mixed", IndexName: "Mixed_pkey", Columns: []string{"Id"}, IsPrimary: true, IsUnique: true}}}
	stmts, inline, e := NewMySQL().PGTableStatements("dest", table, v, func(s string) string { return s })
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(stmts[1].SQL, "AUTO_INCREMENT") || !strings.Contains(stmts[1].SQL, "PRIMARY KEY (`Id`)") || len(inline) != 1 {
		t.Fatalf("%+v %+v", stmts, inline)
	}
}

func TestPGTimezoneDefaultRemainsUTCInApplicationSessions(t *testing.T) {
	raw := "now()"
	c := source.ColumnInfo{DataType: "timestamptz", DateTimePrecision: 6, Default: &raw}
	for _, version := range []string{"5.7.44", "8.0.12", "8.0.13", "8.0.36"} {
		v, _ := ParseMySQLVersion(version)
		got, e := PGColumnDefault(c, "DATETIME(6)", v)
		if !v.Expressions() && e == nil {
			t.Fatal("session-dependent default allowed", version)
		}
		if v.Expressions() && (e != nil || got != " DEFAULT (UTC_TIMESTAMP(6))") {
			t.Fatal(version, got, e)
		}
	}
}

func TestPGTableInheritsDatabaseCharacterDefaults(t *testing.T) {
	v, _ := ParseMySQLVersion("8.0.36")
	def := "'Example'::varchar"
	table := source.PGTable{Info: source.TableDDLInfo{TableName: "Mixed", Comment: "table comment", Columns: []source.ColumnInfo{{Name: "Name", DataType: "varchar", Length: 50, Default: &def, Comment: "column comment"}}}}
	stmts, _, err := NewMySQL().PGTableStatements("dest", table, v, func(s string) string { return s })
	if err != nil {
		t.Fatal(err)
	}
	ddl := stmts[1].SQL
	for _, s := range []string{"CHARSET", "CHARACTER SET", "COLLATE"} {
		if strings.Contains(ddl, s) {
			t.Fatalf("unexpected %s in %s", s, ddl)
		}
	}
	for _, s := range []string{"`Name` VARCHAR(50) NOT NULL DEFAULT 'Example'", "column comment", "table comment", "ENGINE=InnoDB ROW_FORMAT=DYNAMIC"} {
		if !strings.Contains(ddl, s) {
			t.Fatalf("missing %s in %s", s, ddl)
		}
	}
}

func TestPGLocalTimestampDefault(t *testing.T) {
	for _, version := range []string{"5.7.44", "8.0.32"} {
		v, _ := ParseMySQLVersion(version)
		for _, tc := range []struct {
			raw, typ, want string
			precision      int64
		}{
			{"localtimestamp", "DATETIME(6)", " DEFAULT CURRENT_TIMESTAMP(6)", 6},
			{"LOCALTIMESTAMP()", "DATETIME(0)", " DEFAULT CURRENT_TIMESTAMP(0)", 0},
			{"localtimestamp(3)", "DATETIME(3)", " DEFAULT CURRENT_TIMESTAMP(3)", 3},
			{`"localtimestamp"()`, "DATETIME(6)", " DEFAULT CURRENT_TIMESTAMP(6)", 6},
			{`"localtimestamp"(6)`, "DATETIME(6)", " DEFAULT CURRENT_TIMESTAMP(6)", 6},
			{`"localtimestamp" ( 0 )`, "DATETIME(0)", " DEFAULT CURRENT_TIMESTAMP(0)", 0},
			{"CURRENT_TIMESTAMP ( 6 )", "DATETIME(6)", " DEFAULT CURRENT_TIMESTAMP(6)", 6},
			{"now( /* comment */ )", "DATETIME(6)", " DEFAULT CURRENT_TIMESTAMP(6)", 6},
		} {
			c := source.ColumnInfo{Name: "createtime", DataType: "timestamp", DateTimePrecision: tc.precision, Default: &tc.raw}
			got, err := PGColumnDefault(c, tc.typ, v)
			if err != nil || got != tc.want {
				t.Fatalf("%s %s: %s %v", version, tc.raw, got, err)
			}
			table := source.PGTable{Info: source.TableDDLInfo{TableName: "clog_exchangeconfig", Columns: []source.ColumnInfo{c}}}
			stmts, _, err := NewMySQL().PGTableStatements("dst", table, v, func(s string) string { return s })
			if err != nil || !strings.Contains(stmts[1].SQL, "`createtime` "+tc.typ+" NOT NULL"+tc.want) {
				t.Fatalf("DDL: %v %v", stmts, err)
			}
		}
		for _, raw := range []string{"localtimestamp(7)", "localtimestamp(1.5)", "localtimestamp + 1", "localtimestamp; DROP TABLE t", `"localtimestamp"`, `"LOCALTIMESTAMP"()`, `public."localtimestamp"()`, `'localtimestamp'()`, `"localtimestamp"(7)`, `"localtimestamp"('6')`, `"localtimestamp"(6, 0)`, `"localtimestamp"() + 1`, `"localtimestamp"(); SELECT 1`} {
			if _, err := PGDefault(raw, "DATETIME(6)", true, v); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		}
		if _, err := PGDefault("localtimestamp", "VARCHAR(40)", true, v); err == nil {
			t.Fatal("current time allowed on non-temporal column")
		}
	}
}

func TestPGCurrentTimeDefaultHasIndependentPrecision(t *testing.T) {
	for _, version := range []string{"5.7.44", "8.0.12", "8.0.13", "8.0.32"} {
		v, _ := ParseMySQLVersion(version)
		for _, tc := range []struct{ raw, dataType, want string }{
			{`"localtimestamp"(0)`, "timestamp", " DEFAULT (CAST(CURRENT_TIMESTAMP(0) AS DATETIME(6)))"},
			{"localtimestamp(3)", "timestamp", " DEFAULT (CAST(CURRENT_TIMESTAMP(3) AS DATETIME(6)))"},
			{"CURRENT_TIMESTAMP(0)", "timestamptz", " DEFAULT (CAST(UTC_TIMESTAMP(0) AS DATETIME(6)))"},
		} {
			c := source.ColumnInfo{Name: "createtime", DataType: tc.dataType, DateTimePrecision: 6, Default: &tc.raw}
			got, err := PGColumnDefault(c, "DATETIME(6)", v)
			if !v.Expressions() {
				if err == nil || !strings.Contains(err.Error(), "8.0.13+") {
					t.Fatalf("%s: %s %v", version, got, err)
				}
				continue
			}
			if err != nil || got != tc.want {
				t.Fatalf("%s %s: %s %v", version, tc.raw, got, err)
			}
			table := source.PGTable{Info: source.TableDDLInfo{TableName: "clog_exchangeconfig", Columns: []source.ColumnInfo{c}}}
			stmts, _, err := NewMySQL().PGTableStatements("dst", table, v, func(s string) string { return s })
			if err != nil || !strings.Contains(stmts[1].SQL, "`createtime` DATETIME(6) NOT NULL"+tc.want) {
				t.Fatalf("DDL: %v %v", stmts, err)
			}
		}
		if _, err := PGDefault("localtimestamp(6)", "DATETIME(3)", true, v); err == nil {
			t.Fatal("accepted implicit precision loss")
		}
	}
}
