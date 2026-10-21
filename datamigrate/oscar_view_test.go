package datamigrate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeOscarView(t *testing.T) {
	tests := []struct {
		name, sql, want string
		strip           []string
	}{
		{"references", `select t.configname AS "FriendlyName" from "MixedSchema"."api_gateway_config" t`, `select "T"."CONFIGNAME" AS "FRIENDLYNAME" from "MixedSchema"."API_GATEWAY_CONFIG" "T"`, nil},
		{"functions", `select coalesce(t.name, 'Keep.MiXeD') AS result, "CustomFn"(t.id) AS f from tbl t`, `select coalesce("T"."NAME", 'Keep.MiXeD') AS "RESULT", "CustomFn"("T"."ID") AS "F" from "TBL" "T"`, nil},
		{"cast", `SELECT CAST(t.col AS varchar(36)) AS val, t.id::"Types"."MyType", t.x::double precision FROM tbl t`, `SELECT CAST("T"."COL" AS varchar(36)) AS "VAL", "T"."ID"::"Types"."MyType", "T"."X"::double precision FROM "TBL" "T"`, nil},
		{"strip schema", `SELECT dbo.tbl.col, 'dbo.tbl', t.col FROM dbo.tbl t -- dbo.tbl`, `SELECT "TBL"."COL", 'dbo.tbl', "T"."COL" FROM "TBL" "T" -- dbo.tbl`, []string{"dbo"}},
		{"quoted schema", `SELECT "Db"."Tbl"."Name" FROM "Db"."Tbl"`, `SELECT "TBL"."NAME" FROM "TBL"`, []string{"Db"}},
		{"cte", `WITH cte (id) AS (SELECT id FROM tbl) SELECT c.id FROM cte c`, `WITH "CTE" ("ID") AS (SELECT "ID" FROM "TBL") SELECT "C"."ID" FROM "CTE" "C"`, nil},
		{"escaped identifiers", "SELECT [a]]b], `x``y`, \"z\"\"w\" FROM [表t]", `SELECT "A]B", "X` + "`" + `Y", "Z""W" FROM "表T"`, nil},
		{"numeric", `SELECT 1.23e-10 AS v, -2E+3 AS n FROM t`, `SELECT 1.23e-10 AS "V", -2E+3 AS "N" FROM "T"`, nil},
		{"date literal", `SELECT DATE '2026-01-02', INTERVAL '1 day', CURRENT_TIMESTAMP FROM t`, `SELECT DATE '2026-01-02', INTERVAL '1 day', CURRENT_TIMESTAMP FROM "T"`, nil},
		{"comma relation", `SELECT a.id FROM "Ext".a a, "Other".b b`, `SELECT "A"."ID" FROM "Ext"."A" "A", "Other"."B" "B"`, nil},
		{"expressions", `SELECT EXTRACT(year FROM t.created), t.v COLLATE "CaseSensitive", t.d::timestamp(6) without time zone FROM t`, `SELECT EXTRACT(year FROM "T"."CREATED"), "T"."V" COLLATE "CaseSensitive", "T"."D"::timestamp(6) without time zone FROM "T"`, nil},
		{"array and hex", `SELECT ARRAY[t.id, 0xAbCd], t.val[1], t.val::text[] FROM t`, `SELECT ARRAY["T"."ID", 0xAbCd], "T"."VAL"[1], "T"."VAL"::text[] FROM "T"`, nil},
		{"qualified wildcard", `SELECT "External".t.* FROM "External".t`, `SELECT "External"."T".* FROM "External"."T"`, nil},
		{"interval units", `SELECT INTERVAL '1' DAY TO SECOND FROM t`, `SELECT INTERVAL '1' DAY TO SECOND FROM "T"`, nil},
		{"table function arguments", `SELECT a.id FROM tbl a, fn(a.id, a.value) AS b(x, y)`, `SELECT "A"."ID" FROM "TBL" "A", fn("A"."ID", "A"."VALUE") AS "B"("X", "Y")`, nil},
		{"join followed by comma", `SELECT t.id FROM "First".t t JOIN u ON t.id=u.id, "Other".v v`, `SELECT "T"."ID" FROM "First"."T" "T" JOIN "U" ON "T"."ID"="U"."ID", "Other"."V" "V"`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeOscarView(tt.sql, tt.strip)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestOscarViewPreservesLiteralAndCommentBytes(t *testing.T) {
	fragments := []string{`'MiXeD''Value'`, `E'MiXeD\'Value'`, `N'中文AbC'`, `q'[a'bC]'`, `$$MiXeD$$`, `$tag$MiXeD$tag$`, `/* Outer /* MiXeD */ More */`, "-- MiXeD\n"}
	for _, fragment := range fragments {
		query := "SELECT " + fragment + " FROM tbl"
		if strings.HasPrefix(fragment, "/*") || strings.HasPrefix(fragment, "--") {
			query = "SELECT id " + fragment + " FROM tbl"
		}
		got, err := normalizeOscarView(query, []string{"MiXeD"})
		require.NoError(t, err)
		require.Contains(t, got, fragment)
		require.Contains(t, got, `"TBL"`)
	}
}

func TestOscarViewRejectsMalformedSQL(t *testing.T) {
	for _, sql := range []string{`SELECT 'bad`, `SELECT "bad`, `SELECT /* bad`, `SELECT $$bad`, `SELECT q'[bad`, `SELECT f(id`, `SELECT id)`} {
		_, err := normalizeOscarView(sql, nil)
		require.Error(t, err, sql)
	}
}

func TestOscarViewDottedMySQLSchemas(t *testing.T) {
	quote := func(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }
	schema := quote("FinancePlatform_3.0")
	query := "SELECT " + schema + ".`AppManage_ElementRight`.`PortalEltGuid`, " + schema + ".`AppManage_ElementRight`.*, t.`PortalEltGuid`, 1.0, .0, 'FinancePlatform_3.0.tbl `Keep`' FROM " + schema + ".`AppManage_ElementRight` t JOIN " + schema + ".`OtherTable` b ON t.`Id` = b.`Id` /* FinancePlatform_3.0.tbl */ -- `Keep`\n"
	for _, tt := range []struct {
		name   string
		strip  []string
		prefix string
	}{
		{name: "preserve external schema", prefix: `"FinancePlatform_3.0".`},
		{name: "strip whole schema", strip: []string{"financeplatform_3.0"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeOscarView(query, tt.strip)
			require.NoError(t, err)
			want := `SELECT ` + tt.prefix + `"APPMANAGE_ELEMENTRIGHT"."PORTALELTGUID", ` + tt.prefix + `"APPMANAGE_ELEMENTRIGHT".*, "T"."PORTALELTGUID", 1.0, .0, 'FinancePlatform_3.0.tbl ` + "`Keep`" + `' FROM ` + tt.prefix + `"APPMANAGE_ELEMENTRIGHT" "T" JOIN ` + tt.prefix + `"OTHERTABLE" "B" ON "T"."ID" = "B"."ID" /* FinancePlatform_3.0.tbl */ -- ` + "`Keep`\n"
			require.Equal(t, want, got)
		})
	}

	t.Run("escaped schema spelling", func(t *testing.T) {
		name := "External`\"Db.3.0"
		query := "SELECT " + quote(name) + ".`t``x`.`c\"y` FROM " + quote(name) + ".`t``x`"
		got, err := normalizeOscarView(query, nil)
		require.NoError(t, err)
		require.Equal(t, "SELECT \"External`\"\"Db.3.0\".\"T`X\".\"C\"\"Y\" FROM \"External`\"\"Db.3.0\".\"T`X\"", got)
		got, err = normalizeOscarView(query, []string{name})
		require.NoError(t, err)
		require.Equal(t, "SELECT \"T`X\".\"C\"\"Y\" FROM \"T`X\"", got)
	})
}

func TestOscarViewDottedQualifiedFunctions(t *testing.T) {
	query := "SELECT `FinancePlatform_3.0`.`MixedFn`(t.id), `External.Db`.`OtherFn`(t.id), `LocalFn`(t.id) FROM tbl t"
	for _, tt := range []struct {
		name, source, localPrefix, externalPrefix string
		strip                                     []string
	}{
		{name: "preserve", localPrefix: `"FinancePlatform_3.0".`, externalPrefix: `"External.Db".`},
		{name: "source database", source: "FinancePlatform_3.0", externalPrefix: `"External.Db".`},
		{name: "configured", strip: []string{"financeplatform_3.0", "external.db"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeOscarViewForSource(query, tt.strip, tt.source)
			require.NoError(t, err)
			require.Equal(t, `SELECT `+tt.localPrefix+`"MixedFn"("T"."ID"), `+tt.externalPrefix+`"OtherFn"("T"."ID"), "LocalFn"("T"."ID") FROM "TBL" "T"`, got)
		})
	}
}
