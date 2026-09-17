package datamigrate

import (
	"os"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

func pgViewTestColumns(names string) []source.ColumnInfo {
	var cols []source.ColumnInfo
	for _, name := range strings.Fields(names) {
		c := source.ColumnInfo{Name: name, DataType: "varchar", Length: 100}
		if strings.EqualFold(name, "row_id") {
			c.DataType = "int8"
		}
		if name == "disabled" {
			c.DataType, c.Length = "bit", 1
		}
		cols = append(cols, c)
	}
	return cols
}

// SQL is supplied from the failing Vastbase preflight. GUID columns in this
// fixture are VARCHAR; production uses catalog types and rejects unsafe casts.
func TestVastbaseViewDefinitions(t *testing.T) {
	relations := map[string][]source.ColumnInfo{}
	for table, names := range map[string]string{
		"frame_authority_expression": "appguid rowguid expressiontype rightlevel expressionname",
		"frame_auth_exp_resource":    "expressionguid operationguid datainsguid",
		"frame_auth_opt":             "rowguid operationname",
		"frame_auth_opt_resource":    "rowguid operationguid resourcetype rangetype resourcerange expression",
		"frame_ou":                   "ouguid oucode ouname oushortname ordernumber description address postalcode tel baseouguid issubwebflow parentouguid oucodelevel updatetime haschildou haschilduser",
		"frame_ou_extendinfo":        "ouguid isindependence",
		"frame_user":                 "userguid loginid password ouguid displayname isenabled title leaderguid ordernumber telephoneoffice mobile email description telephonehome fax allowuseemail sex oucodelevel updatetime row_id issyncthirdparty",
		"frame_user_extendinfo":      "userguid birthday shortmobile usbkey ntx_extnumber qqnumber msnnumber postaladdress postalcode carnum",
		"mem_user":                   "userguid loginid password ouguid displayname isenabled status usertype legalusertype baseuserguid ordernumber telephoneoffice mobile email description telephonehome fax enterpriseguid sex updatetime",
		"mem_ou":                     "ouguid ouname oushortname parentouguid oucodelevel oucodefull",
		"mem_user_extendinfo":        "userguid birthday shortmobile usbkey qqnumber msnnumber postaladdress postalcode cardtype cardnum carnum",
		"mem_user_relation":          "userguid relateuserguid relatetype",
		"portal_item":                "rowguid initrow initcol rowspan colspan belongportalguid title url disabled",
		"portal_myitem":              "rowguid userguid portaletguid itemcol itemrow rowspan colspan disabled",
		"frame_user_secondou":        "userguid ouguid title userordernumber tel row_id",
		"personal_portal_element":    "ptrowguid isdisable userguid elementlocation",
		"app_element":                "elementname elementurl rowguid isdisable",
		"app_portal_element":         "elementguid rowguid elementlocation belongportalguid",
	} {
		relations[table] = pgViewTestColumns(names)
	}
	for _, tc := range []struct {
		name, outputs string
		deps          int
		want          string
	}{
		{"frame_auth_metadata", "operationguid resourceguid appGuid expressionguid expressiontype datainsguid rightlevel expressionname operationname resourcetype rangetype resourcerange expression", 4, "`er`.`expressionguid` = `e`.`rowguid`"},
		{"view_frame_ou", "Ouguid Oucode Ouname Oushortname Ordernumber Description Address Postalcode Tel Baseouguid Issubwebflow Parentouguid Oucodelevel Updatetime Haschildou Haschilduser Isindependence", 2, " CROSS JOIN "},
		{"view_frame_user_secondou", "UserGuid LoginID password OUGuid DisplayName IsEnabled Title LeaderGuid OrderNumber TelephoneOffice Mobile Email Description TelephoneHome Fax AllowUseEmail Sex OUCodeLevel UpDateTime Row_id", 3, "CAST((`a`.`row_id` + 1000000000) AS SIGNED)"},
		{"view_personal_element", "elementname elementurl elementguid elementdisable ptrowguid isdisable userguid initlocation userlocation belongportalguid", 3, "`a`.`ptrowguid` = `c`.`rowguid`"},
		{"view_frame_user_java", "userguid loginid password ouguid displayname isenabled title leaderguid ordernumber telephoneoffice mobile email description telephonehome fax allowuseemail sex updatetime issyncthirdparty birthday shortmobile usbkey ntx_extnumber qqnumber msnnumber postaladdress postalcode carnum ouname oushortname parentouguid oucodelevel", 3, "SELECT DISTINCT "},
		{"view_mem_user_java", "UserGuid LoginID password OUGuid DisplayName IsEnabled status UserType legalUserType baseuserguid OrderNumber TelephoneOffice Mobile Email Description TelephoneHome Fax enterpriseGuid Sex UpDateTime birthday Shortmobile Usbkey Qqnumber Msnnumber Postaladdress Postalcode CardType cardNum carnum Ouname Oushortname Parentouguid Oucodelevel OucodeFull", 4, "COALESCE(`memuserrelation`.`relateuserguid`, `memuser`.`userguid`) AS `userguid`"},
		{"view_portal_myitem", "myrowguid userguid initrow initcol rowspan colspan belongportalguid title rowguid itemcol itemrow itemrowspan itemcolspan disabled url", 2, "WHERE (`portal_item`.`disabled` = b'0')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := os.ReadFile("testdata/pgmysql_views/" + tc.name + ".sql")
			if err != nil {
				t.Fatal(err)
			}
			v := source.ViewInfo{ViewName: tc.name, Definition: string(def), Columns: pgViewTestColumns(tc.outputs)}
			got, deps, err := translatePGView(v, "njjwgch_djg_basedata_4_0_kf", "target", relations, strings.ToLower)
			if err != nil {
				t.Fatal(err)
			}
			if len(deps) != tc.deps || !strings.Contains(got, tc.want) || strings.Contains(got, "::") || strings.Contains(got, "njjwgch_djg_basedata_4_0_kf") {
				t.Fatalf("sql=%s deps=%v", got, deps)
			}
			if tc.name == "view_frame_user_secondou" && (!strings.Contains(got, " UNION SELECT ") || strings.Contains(got, "UNION ALL")) {
				t.Fatal("UNION deduplication changed")
			}
			if tc.name == "view_personal_element" && !strings.Contains(got, "AS `b`) INNER JOIN") {
				t.Fatal("CROSS JOIN grouping changed")
			}
			if _, err := parser.New().ParseOneStmt("CREATE VIEW `target`.`"+tc.name+"` AS "+got, "utf8mb4", "utf8mb4_bin"); err != nil {
				t.Fatalf("invalid MySQL syntax: %v\n%s", err, got)
			}
		})
	}
}

func TestPGViewCastsAndUnionBoundaries(t *testing.T) {
	relations := map[string][]source.ColumnInfo{"t": {
		{Name: "id", DataType: "int8"}, {Name: "s", DataType: "varchar", Length: 20}, {Name: "padded", DataType: "bpchar", Length: 20}, {Name: "at", DataType: "timestamp"},
	}}
	for _, tc := range []struct{ def, typ, contains string }{
		{`SELECT s::text AS value FROM t`, "varchar", "`t`.`s` AS `value`"},
		{`SELECT (padded)::text AS value FROM t`, "varchar", "RTRIM(`t`.`padded`)"},
		{`SELECT id FROM t UNION ALL SELECT id + 1000000000 FROM t;`, "int8", " UNION ALL SELECT "},
		{`SELECT id FROM t UNION DISTINCT SELECT (1000000000 + id) FROM t`, "int8", " UNION DISTINCT SELECT "},
		{`SELECT 'UNION FROM ::text' FROM t`, "varchar", "'UNION FROM ::text'"},
		{`SELECT DISTINCT s FROM t`, "varchar", "SELECT DISTINCT `t`.`s`"},
		{`SELECT ALL s FROM t`, "varchar", "SELECT ALL `t`.`s`"},
		{`SELECT DISTINCT id FROM t UNION ALL SELECT DISTINCT id FROM t`, "int8", " UNION ALL SELECT DISTINCT "},
	} {
		v := source.ViewInfo{Definition: tc.def, Columns: []source.ColumnInfo{{Name: "value", DataType: tc.typ, Length: 20}}}
		got, _, err := translatePGView(v, "public", "target", relations, strings.ToLower)
		if err != nil || !strings.Contains(got, tc.contains) {
			t.Fatalf("%s: %s %v", tc.def, got, err)
		}
	}
	for _, def := range []string{
		`SELECT id::text FROM t`, `SELECT at::text FROM t`, `SELECT s::varchar(2) FROM t`,
		`SELECT (s = 'x')::text FROM t`, `SELECT id + 0.5 FROM t`, `SELECT id + s FROM t`, `SELECT 1 + s FROM t`, `SELECT id + 9223372036854775808 FROM t`,
		`SELECT id FROM t UNION SELECT id, s FROM t`, `SELECT id FROM t UNION`,
		`SELECT id FROM t UNION SELECT id FROM missing`, `SELECT id FROM t UNION SELECT id FROM other.t`,
		`SELECT id FROM t UNION SELECT id FROM t ORDER BY id`, `SELECT id FROM t; DROP TABLE t`,
		`SELECT a.id FROM t a CROSS JOIN t b ON a.id=b.id`,
		`SELECT DISTINCT ON (s) id FROM t`,
	} {
		v := source.ViewInfo{Definition: def, Columns: []source.ColumnInfo{{Name: "value", DataType: "int8"}}}
		if got, _, err := translatePGView(v, "public", "target", relations, strings.ToLower); err == nil {
			t.Fatalf("unsafe SQL accepted: %s -> %s", def, got)
		}
	}
}

func TestPGViewTextCoalesce(t *testing.T) {
	relations := map[string][]source.ColumnInfo{"t": {
		{Name: "s", DataType: "varchar", Length: 40}, {Name: "fallback", DataType: "text"}, {Name: "n", DataType: "int4"},
	}}
	for _, tc := range []struct{ expr, want string }{
		{`COALESCE(s::text, fallback::text)`, "COALESCE(`t`.`s`, `t`.`fallback`)"},
		{`COALESCE(s, NULL, ''::text, fallback)`, "COALESCE(`t`.`s`, NULL, '', `t`.`fallback`)"},
		{`COALESCE(s, COALESCE(fallback, 'none'))`, "COALESCE(`t`.`s`, COALESCE(`t`.`fallback`, 'none'))"},
		{`COALESCE(NULL, NULL)`, "COALESCE(NULL, NULL)"},
		{`COALESCE(s)`, "`t`.`s`"},
	} {
		v := source.ViewInfo{Definition: "SELECT " + tc.expr + " AS value FROM t", Columns: []source.ColumnInfo{{Name: "value", DataType: "text"}}}
		got, _, err := translatePGView(v, "public", "dst", relations, strings.ToLower)
		if err != nil || !strings.HasPrefix(got, "SELECT "+tc.want+" AS `value`") {
			t.Fatalf("%s: %s %v", tc.expr, got, err)
		}
		if _, err := parser.New().ParseOneStmt(got, "utf8mb4", "utf8mb4_bin"); err != nil {
			t.Fatal(err)
		}
	}
	for _, expr := range []string{`COALESCE()`, `COALESCE(s,)`, `COALESCE(s, 1)`, `COALESCE(s, n)`, `COALESCE(s, now())`, `COALESCE(s = 'x', fallback)`, `COALESCE(s, n::text)`, `COALESCE(s, fallback)::integer`} {
		v := source.ViewInfo{Definition: "SELECT " + expr + " FROM t", Columns: []source.ColumnInfo{{Name: "value", DataType: "text"}}}
		if got, _, err := translatePGView(v, "public", "dst", relations, strings.ToLower); err == nil {
			t.Fatalf("accepted unsafe expression %s: %s", expr, got)
		}
	}
}

func TestPGViewBitPredicates(t *testing.T) {
	relations := map[string][]source.ColumnInfo{"t": {
		{Name: "id", DataType: "int8"}, {Name: "disabled", DataType: "bit", Length: 1},
		{Name: "flags", DataType: "bit", Length: 8}, {Name: "wide", DataType: "bit", Length: 64},
	}}
	for _, predicate := range []string{`disabled = B'0'::"bit"`, `B'1'::bit = disabled`, `disabled <> B'1' AND id = 1`, `disabled IS NULL OR disabled = B'0'`, `flags = B'00110001'::bit(8)`, `wide = B'` + strings.Repeat("1", 64) + `'::bit(64)`} {
		v := source.ViewInfo{Definition: "SELECT id FROM t WHERE " + predicate, Columns: []source.ColumnInfo{{Name: "id", DataType: "int8"}}}
		got, _, err := translatePGView(v, "public", "dst", relations, strings.ToLower)
		if err != nil {
			t.Fatalf("%s: %v", predicate, err)
		}
		if _, err := parser.New().ParseOneStmt(got, "utf8mb4", "utf8mb4_bin"); err != nil {
			t.Fatal(err)
		}
	}
	for _, predicate := range []string{`flags = B'1'`, `disabled = B'01'`, `disabled = flags`, `id = B'0'`, `disabled = 0`, `disabled = B'2'`, `disabled = B'01'::bit`, `disabled = B'1'::bit(8)`, `disabled = B'0'::integer`, `disabled = B''`, `disabled = B'` + strings.Repeat("0", 65) + `'`} {
		v := source.ViewInfo{Definition: "SELECT id FROM t WHERE " + predicate, Columns: []source.ColumnInfo{{Name: "id", DataType: "int8"}}}
		if got, _, err := translatePGView(v, "public", "dst", relations, strings.ToLower); err == nil {
			t.Fatalf("accepted unsafe predicate %s: %s", predicate, got)
		}
	}
}
