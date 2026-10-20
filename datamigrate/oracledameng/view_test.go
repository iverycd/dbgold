package oracledameng

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func viewTestCatalog() map[string][]MappedColumn {
	return map[string][]MappedColumn{
		"PARENT": {{Source: "ID", Target: "ID"}, {Source: "NAME", Target: "NAME"}, {Source: "ROWID", Target: "ROW_ID_2"}, {Source: "AMOUNT", Target: "AMOUNT"}},
		"CHILD":  {{Source: "ID", Target: "ID"}, {Source: "PARENT_ID", Target: "PARENT_ID"}},
		"Mixed":  {{Source: "camelCase", Target: "CAMELCASE"}, {Source: "a\"b", Target: "A\"B"}},
	}
}

func TestConvertViewOracleExpressionsAndComments(t *testing.T) {
	raw := `SELECT /* preserve 'AbC' */ p.ID, p.ROWID, NVL(p.NAME, q'[MiXeD 'quoted' SRC.PARENT]') AS display_name,
  CASE WHEN p.AMOUNT > 0 THEN 'YES' ELSE 'no' END decision,
  TO_CHAR(SYSDATE,'YYYY-MM-DD HH24:MI:SS') now_text
FROM SRC.PARENT p -- preserve Src.Parent and SRC.PARENT@link
WHERE p.ID IN (1,2) AND p.NAME <> N'汉字AbC' AND p.AMOUNT BETWEEN 0 AND 100
ORDER BY display_name;`
	got, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []string{"PARENT"}, got.Dependencies)
	require.Equal(t, []MappedColumn{{"ID", "ID"}, {"ROWID", "ROW_ID_2"}, {"DISPLAY_NAME", "DISPLAY_NAME"}, {"DECISION", "DECISION"}, {"NOW_TEXT", "NOW_TEXT"}}, got.Columns)
	for _, unchanged := range []string{"/* preserve 'AbC' */", "'MiXeD ''quoted'' SRC.PARENT'", "-- preserve Src.Parent and SRC.PARENT@link", "N'汉字AbC'", "NVL(", "TO_CHAR(SYSDATE,'YYYY-MM-DD HH24:MI:SS')"} {
		require.Contains(t, got.SQL, unchanged)
	}
	require.Contains(t, got.SQL, `"P"."ROW_ID_2"`)
	require.Contains(t, got.SQL, `FROM "DST"."PARENT" "P"`)
	require.Contains(t, got.SQL, `ORDER BY "DISPLAY_NAME"`)
	require.NotContains(t, got.SQL, "coalesce")
	require.NotContains(t, got.SQL, ";")
}

func TestConvertViewJoinsGroupAndUnion(t *testing.T) {
	raw := `SELECT p.NAME label, COUNT(c.ID) total FROM PARENT p LEFT JOIN CHILD c ON p.ID=c.PARENT_ID
WHERE p.AMOUNT>1 GROUP BY p.NAME HAVING COUNT(c.ID)>0
UNION ALL SELECT p.NAME label, SUM(p.AMOUNT) total FROM PARENT p GROUP BY p.NAME`
	got, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []string{"CHILD", "PARENT"}, got.Dependencies)
	require.Equal(t, []MappedColumn{{"LABEL", "LABEL"}, {"TOTAL", "TOTAL"}}, got.Columns)
	require.Contains(t, got.SQL, `LEFT JOIN "DST"."CHILD" "C" ON "P"."ID"="C"."PARENT_ID"`)
	require.Contains(t, got.SQL, "UNION ALL SELECT")
}

func TestConvertViewParenthesizedJoinWithoutAliases(t *testing.T) {
	raw := `select parent.rowid, parent.name, child.parent_id
from (parent inner join child on((child.parent_id = parent.id)))`
	got, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []string{"CHILD", "PARENT"}, got.Dependencies)
	require.Equal(t, []MappedColumn{{"ROWID", "ROW_ID_2"}, {"NAME", "NAME"}, {"PARENT_ID", "PARENT_ID"}}, got.Columns)
	require.Equal(t, `select "PARENT"."ROW_ID_2", "PARENT"."NAME", "CHILD"."PARENT_ID"
from ("DST"."PARENT" inner join "DST"."CHILD" on(("CHILD"."PARENT_ID" = "PARENT"."ID")))`, got.SQL)
}

func TestConvertViewNestedJoinGroups(t *testing.T) {
	catalog := viewTestCatalog()
	catalog["GRANDCHILD"] = []MappedColumn{{"ID", "ID"}, {"CHILD_ID", "CHILD_ID"}}
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "nested outer joins retain grouping and direction",
			raw: `SELECT p.ID parent_id, c.ID child_id, g.ID grandchild_id
FROM ((PARENT p LEFT OUTER JOIN (CHILD c RIGHT JOIN GRANDCHILD g ON (c.ID=g.CHILD_ID)) ON ((p.ID=c.PARENT_ID))) FULL OUTER JOIN CHILD c2 ON c2.PARENT_ID=p.ID)`,
			want: []string{
				`FROM (("DST"."PARENT" "P" LEFT OUTER JOIN ("DST"."CHILD" "C" RIGHT JOIN "DST"."GRANDCHILD" "G" ON ("C"."ID"="G"."CHILD_ID")) ON (("P"."ID"="C"."PARENT_ID"))) FULL OUTER JOIN "DST"."CHILD" "C2" ON "C2"."PARENT_ID"="P"."ID")`,
			},
		},
		{
			name: "group mixed with outer join and comma",
			raw: `SELECT p.ID parent_id, c.ID child_id, g.ID grandchild_id
FROM (PARENT p INNER JOIN CHILD c ON p.ID=c.PARENT_ID) LEFT JOIN GRANDCHILD g ON (g.CHILD_ID=c.ID), CHILD c2 WHERE c2.ID=c.ID`,
			want: []string{
				`FROM ("DST"."PARENT" "P" INNER JOIN "DST"."CHILD" "C" ON "P"."ID"="C"."PARENT_ID") LEFT JOIN "DST"."GRANDCHILD" "G" ON ("G"."CHILD_ID"="C"."ID"), "DST"."CHILD" "C2" WHERE "C2"."ID"="C"."ID"`,
			},
		},
		{
			name: "comma relations inside nested groups",
			raw: `SELECT p.ID parent_id, c.ID child_id, g.ID grandchild_id
FROM ((PARENT p, CHILD c)) JOIN (GRANDCHILD g) ON (g.CHILD_ID=c.ID) WHERE p.ID=c.PARENT_ID`,
			want: []string{
				`FROM (("DST"."PARENT" "P", "DST"."CHILD" "C")) JOIN ("DST"."GRANDCHILD" "G") ON ("G"."CHILD_ID"="C"."ID") WHERE "P"."ID"="C"."PARENT_ID"`,
			},
		},
		{
			name: "ON function arguments and IN lists are expressions",
			raw: `SELECT p.ID parent_id, c.ID child_id, g.ID grandchild_id
FROM (PARENT p JOIN CHILD c ON ((NVL(c.PARENT_ID, 0)=p.ID) AND p.ID IN (1,2,3))) JOIN GRANDCHILD g ON ((g.CHILD_ID=c.ID) AND NVL(g.ID, 0)>0)`,
			want: []string{
				`ON ((NVL("C"."PARENT_ID", 0)="P"."ID") AND "P"."ID" IN (1,2,3))) JOIN "DST"."GRANDCHILD" "G"`,
				`ON (("G"."CHILD_ID"="C"."ID") AND NVL("G"."ID", 0)>0)`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConvertView(tc.raw, "SRC", "DST", catalog)
			require.NoError(t, err)
			require.Equal(t, []string{"CHILD", "GRANDCHILD", "PARENT"}, got.Dependencies)
			require.Equal(t, []MappedColumn{{"PARENT_ID", "PARENT_ID"}, {"CHILD_ID", "CHILD_ID"}, {"GRANDCHILD_ID", "GRANDCHILD_ID"}}, got.Columns)
			for _, want := range tc.want {
				require.Contains(t, got.SQL, want)
			}
		})
	}
}

func TestConvertViewJoinGroupsPreserveQuotedIdentifiersCommentsAndWildcard(t *testing.T) {
	raw := `SELECT "m".* FROM ("SRC" /* owner */ . "Mixed" "m" /* relation */
LEFT JOIN ("SRC"."CHILD" "c") -- joined group
ON ((NVL("c"."PARENT_ID",0)=1)))`
	got, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []string{"CHILD", "Mixed"}, got.Dependencies)
	require.Equal(t, viewTestCatalog()["Mixed"], got.Columns)
	require.Contains(t, got.SQL, `SELECT "M"."CAMELCASE", "M"."A""B" FROM (`)
	require.Contains(t, got.SQL, `"DST"."MIXED"`)
	require.Contains(t, got.SQL, `LEFT JOIN ("DST"."CHILD" "C")`)
	require.Contains(t, got.SQL, `ON ((NVL("C"."PARENT_ID",0)=1)))`)
	for _, comment := range []string{"/* owner */", "/* relation */", "-- joined group\n"} {
		require.Equal(t, 1, strings.Count(got.SQL, comment))
	}
}

func TestConvertViewJoinGroupsDiscoverAllDependenciesBeforeBinding(t *testing.T) {
	raw := `SELECT p.ID parent_id, a.ID a_id, b.ID b_id, c.ID c_id
FROM ((PARENT p LEFT JOIN MISSING_A a ON p.ID=a.ID) JOIN (MISSING_B b, CHILD c) ON b.ID=c.ID), MISSING_C d
UNION ALL SELECT x.ID parent_id, x.ID a_id, x.ID b_id, x.ID c_id FROM (LAST_DEP x)`
	want := []string{"CHILD", "LAST_DEP", "MISSING_A", "MISSING_B", "MISSING_C", "PARENT"}
	got, err := ConvertView(raw, "SRC", "DST", nil)
	require.NoError(t, err)
	require.Equal(t, want, got.Dependencies)
	got, err = ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.ErrorContains(t, err, "列映射")
	require.Equal(t, want, got.Dependencies)
}

func TestConvertViewJoinGroupsDiscoverDependenciesBeforeAliasConflict(t *testing.T) {
	catalog := viewTestCatalog()
	catalog["GRANDCHILD"] = []MappedColumn{{"ID", "ID"}, {"CHILD_ID", "CHILD_ID"}}
	raw := `SELECT x.ID FROM (PARENT x JOIN CHILD x ON x.ID=x.PARENT_ID), (GRANDCHILD g)`
	for _, catalog := range []map[string][]MappedColumn{nil, catalog} {
		got, err := ConvertView(raw, "SRC", "DST", catalog)
		require.ErrorContains(t, err, "别名")
		require.Equal(t, []string{"CHILD", "GRANDCHILD", "PARENT"}, got.Dependencies)
	}
}

func TestConvertViewJoinGroupsFailClosed(t *testing.T) {
	cases := []string{
		`SELECT p.ID FROM (PARENT p JOIN CHILD c ON p.ID=c.PARENT_ID) grouped`,
		`SELECT p.ID FROM (PARENT p JOIN CHILD c ON p.ID=c.PARENT_ID) AS grouped`,
		`SELECT p.ID FROM ((PARENT p) grouped JOIN CHILD c ON p.ID=c.PARENT_ID)`,
		`SELECT p.ID FROM (PARENT p NATURAL JOIN CHILD c)`,
		`SELECT p.ID FROM (PARENT p JOIN (CHILD c NATURAL JOIN PARENT p2) ON p.ID=c.PARENT_ID)`,
		`SELECT p.ID FROM (PARENT p JOIN CHILD c USING (ID))`,
		`SELECT p.ID FROM (PARENT p JOIN (CHILD c JOIN PARENT p2 USING (ID)) ON p.ID=c.PARENT_ID)`,
		`SELECT p.ID FROM (PARENT p JOIN OTHER.CHILD c ON p.ID=c.PARENT_ID)`,
		`SELECT p.ID FROM (PARENT p JOIN CHILD p ON p.ID=p.PARENT_ID)`,
		`SELECT p.ID FROM (PARENT p), ((CHILD p))`,
		`SELECT p.ID FROM ()`,
		`SELECT p.ID FROM ((()))`,
		`SELECT p.ID FROM (PARENT p JOIN ())`,
		`SELECT p.ID FROM ((PARENT p), ())`,
		`SELECT p.ID FROM (PARENT p JOIN)`,
		`SELECT p.ID FROM (PARENT p,)`,
		`SELECT p.ID FROM (PARENT p JOIN CHILD c ON p.ID=c.PARENT_ID),`,
		`SELECT p.ID FROM ((PARENT p JOIN CHILD c ON p.ID=c.PARENT_ID) JOIN)`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			for _, catalog := range []map[string][]MappedColumn{nil, viewTestCatalog()} {
				_, err := ConvertView(raw, "SRC", "DST", catalog)
				require.Error(t, err)
			}
		})
	}
}

func TestConvertViewWildcardAndQuotedIdentifiers(t *testing.T) {
	got, err := ConvertView(`SELECT p.* FROM PARENT p`, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, viewTestCatalog()["PARENT"], got.Columns)
	require.Contains(t, got.SQL, `SELECT "P"."ID", "P"."NAME", "P"."ROW_ID_2", "P"."AMOUNT"`)
	got, err = ConvertView(`SELECT "m"."camelCase", "m"."a""b" FROM "SRC"."Mixed" "m"`, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []string{"Mixed"}, got.Dependencies)
	require.Contains(t, got.SQL, `SELECT "M"."CAMELCASE", "M"."A""B" FROM "DST"."MIXED" "M"`)
	_, err = ConvertView(`SELECT camelCase FROM "Mixed"`, "SRC", "DST", viewTestCatalog())
	require.ErrorContains(t, err, "未知")
}

func TestConvertViewPreservesCommentsInsideMappedReferences(t *testing.T) {
	raw := "SELECT p /* column */ . ROWID FROM SRC -- owner\n . PARENT p"
	got, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Contains(t, got.SQL, "/* column */")
	require.Contains(t, got.SQL, "-- owner\n")
	require.Contains(t, got.SQL, `"P"."ROW_ID_2"`)
	require.Contains(t, got.SQL, `"DST"."PARENT"`)
}

func TestConvertViewDiscoveryAndMissingDependencies(t *testing.T) {
	raw := `SELECT a.*, b.ID other FROM PARENT a JOIN MISSING b ON a.ID=b.ID UNION ALL SELECT c.*,d.ID other FROM CHILD c JOIN LAST_DEP d ON c.ID=d.ID`
	got, err := ConvertView(raw, "SRC", "DST", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"CHILD", "LAST_DEP", "MISSING", "PARENT"}, got.Dependencies)
	got, err = ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.ErrorContains(t, err, "列映射")
	require.Equal(t, []string{"CHILD", "LAST_DEP", "MISSING", "PARENT"}, got.Dependencies)
}

func TestConvertViewFailsClosed(t *testing.T) {
	cases := []string{
		`SELECT ID FROM OTHER.PARENT`,
		`SELECT ID FROM PARENT@other`,
		`SELECT ID FROM PARENT; DROP TABLE PARENT`,
		`WITH a AS (SELECT ID FROM PARENT) SELECT ID FROM a`,
		`SELECT (SELECT ID FROM PARENT) value FROM DUAL`,
		`SELECT ID FROM PARENT CONNECT BY PRIOR ID=ID`,
		`SELECT p.ID FROM PARENT p, CHILD c WHERE p.ID=c.ID(+)`,
		`SELECT ID FROM PARENT p JOIN CHILD c ON p.ID=c.ID`,
		`SELECT * FROM PARENT p JOIN CHILD c ON p.ID=c.ID`,
		`SELECT ID FROM PARENT NATURAL JOIN CHILD`,
		`SELECT ID FROM PARENT JOIN CHILD USING (ID)`,
		`SELECT custom_function(ID) value FROM PARENT`,
		`SELECT pkg.func(ID) value FROM PARENT`,
		`SELECT ROWID FROM PARENT`,
		`SELECT NAME FROM PARENT UNION ALL SELECT ID,NAME FROM PARENT`,
		`SELECT NAME FROM PARENT UNION ALL DELETE FROM PARENT`,
		`SELECT NVL(NAME,'x') FROM PARENT`,
		`SELECT ID AS x, NAME AS X FROM PARENT`,
		`SELECT missing FROM PARENT`,
		`SELECT "camelCase" FROM Mixed`,
		`SELECT ID FROM PARENT FOR UPDATE`,
		`SELECT XMLTYPE(NAME) value FROM PARENT`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			_, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
			require.Error(t, err)
		})
	}
}

func TestConvertViewOracleLiterals(t *testing.T) {
	for literal, rendered := range map[string]string{`'a''b'`: `'a''b'`, `q'[a'b @link]'`: `'a''b @link'`, `Q'{a "quote" b}'`: `'a "quote" b'`, `q'(a'b)'`: `'a''b'`, `q'<a>b>'`: `'a>b'`, `q'!a'b!'`: `'a''b'`, `nq'[汉字'x]'`: `N'汉字''x'`, `N'汉字''x'`: `N'汉字''x'`} {
		t.Run(literal, func(t *testing.T) {
			got, err := ConvertView("SELECT "+literal+" AS value FROM DUAL", "SRC", "DST", viewTestCatalog())
			require.NoError(t, err)
			require.Contains(t, got.SQL, rendered)
			require.Empty(t, got.Dependencies)
		})
	}
	for _, raw := range []string{"SELECT 'x AS value FROM DUAL", "SELECT q'[x' AS value FROM DUAL", "SELECT ID FROM PARENT /* x", `SELECT "ID FROM PARENT`} {
		_, err := ConvertView(raw, "SRC", "DST", nil)
		require.Error(t, err)
	}
}

func TestConvertViewDoesNotMistakeWhereColumnForOutputAlias(t *testing.T) {
	catalog := map[string][]MappedColumn{"T": {{"A", "A"}, {"B", "B_2"}}}
	got, err := ConvertView(`SELECT A AS B FROM T WHERE B=1 ORDER BY B`, "SRC", "DST", catalog)
	require.NoError(t, err)
	require.Contains(t, got.SQL, `WHERE "B_2"=1 ORDER BY "B"`)
}

func TestConvertViewNoUnexpectedChangesToOracleFunctions(t *testing.T) {
	raw := `SELECT CAST(AMOUNT AS NUMBER(20,2)) val, EXTRACT(YEAR FROM DATE '2024-01-01') year_value, SYSDATE now_value, TRIM(BOTH 'x' FROM NAME) trimmed FROM PARENT WHERE NAME LIKE 'AbC%'`
	got, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.True(t, strings.Contains(got.SQL, "CAST(") && strings.Contains(got.SQL, "EXTRACT(YEAR FROM CAST(DATE '2024-01-01' AS TIMESTAMP(0)))") && strings.Contains(got.SQL, "SYSDATE"))
}

func TestConvertViewMapsCastsToApprovedTargetTypes(t *testing.T) {
	for _, tc := range []struct{ source, target string }{
		{"VARCHAR2(10 CHAR)", "VARCHAR2(10 CHAR)"}, {"CHAR(10 BYTE)", "CHAR(10)"},
		{"NCHAR(10)", "NCHAR(10 CHAR)"}, {"NVARCHAR2(10)", "NVARCHAR(10 CHAR)"},
		{"NATIONAL CHARACTER VARYING(10)", "NVARCHAR(10 CHAR)"},
		{"NUMBER(20,2)", "NUMBER(20,2)"}, {"INT", "NUMBER(38,0)"},
		{"FLOAT(126)", "FLOAT(126)"}, {"REAL", "FLOAT(63)"}, {"DOUBLE PRECISION", "FLOAT(126)"},
		{"BINARY_FLOAT", "FLOAT"}, {"BINARY_DOUBLE", "DOUBLE"},
		{"DATE", "TIMESTAMP(0)"}, {"TIMESTAMP(9)", "TIMESTAMP(9)"},
		{"TIMESTAMP(9) WITH LOCAL TIME ZONE", "TIMESTAMP(9) WITH TIME ZONE"},
		{"RAW(32)", "VARBINARY(32)"}, {"INTERVAL DAY(2) TO SECOND(6)", "INTERVAL DAY(2) TO SECOND(6)"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			got, err := ConvertView("SELECT CAST(AMOUNT AS "+tc.source+") AS value FROM PARENT", "SRC", "DST", viewTestCatalog())
			require.NoError(t, err)
			require.Contains(t, got.SQL, `CAST("AMOUNT" AS `+tc.target)
		})
	}
	for _, typ := range []string{"XMLTYPE", "BFILE", "SDO_GEOMETRY", "SDE.ST_GEOMETRY", "UNSUPPORTED_TYPE", "NUMBER(4,5)", "NUMBER(0)", "FLOAT(127)", "INTERVAL DAY(2) TO SECOND(9)"} {
		_, err := ConvertView("SELECT CAST(AMOUNT AS "+typ+") AS value FROM PARENT", "SRC", "DST", viewTestCatalog())
		require.Error(t, err, typ)
	}
}

func TestConvertViewOptionsAndDateFunctionsPreserveSeconds(t *testing.T) {
	raw := `SELECT CAST(NAME AS CHAR(10)) c, CAST(TO_DATE(NAME,'YYYY-MM-DD HH24:MI:SS') AS DATE) d FROM PARENT`
	got, err := ConvertViewWithOptions(raw, "SRC", "DST", viewTestCatalog(), Options{UseNvarchar2: true, UseDate: true})
	require.NoError(t, err)
	require.Contains(t, got.SQL, `CAST("NAME" AS NVARCHAR(10 CHAR)`)
	require.Contains(t, got.SQL, `CAST(CAST(TO_TIMESTAMP("NAME",'YYYY-MM-DD HH24:MI:SS') AS TIMESTAMP(0)) AS DATE)`)
	got, err = ConvertViewWithOptions(`SELECT CAST(NAME AS VARCHAR2(10)) c FROM PARENT`, "SRC", "DST", viewTestCatalog(), Options{CharInLength: true})
	require.NoError(t, err)
	require.Contains(t, got.SQL, `AS VARCHAR2(10 CHAR)`)
	for _, raw := range []string{`SELECT TO_DATE(NAME) d FROM PARENT`, `SELECT TO_DATE(NAME,'YYYY', 'NLS=ENGLISH') d FROM PARENT`, `SELECT TO_DATE(NAME,'YYYY',) d FROM PARENT`} {
		_, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
		require.ErrorContains(t, err, "两个参数")
	}
}

func TestConvertViewEmptyStringsAndBackslashFailClosed(t *testing.T) {
	got, err := ConvertView(`SELECT '' a, N'' b, q'[]' c, Nq'[]' d FROM PARENT WHERE NAME=''`, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Contains(t, got.SQL, `SELECT NULL "A", NULL "B", NULL "C", NULL "D"`)
	require.Contains(t, got.SQL, `WHERE "NAME"=NULL`)
	for _, literal := range []string{`'C:\folder'`, `n'C:\folder'`, `q'[C:\folder]'`} {
		got, err := ConvertView("SELECT "+literal+" AS value FROM PARENT", "SRC", "DST", viewTestCatalog())
		require.ErrorContains(t, err, "反斜杠")
		require.Equal(t, []string{"PARENT"}, got.Dependencies)
	}
}

func TestConvertViewDualShadowing(t *testing.T) {
	discovery, err := ConvertView(`SELECT ID FROM DUAL`, "SRC", "DST", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"DUAL"}, discovery.Dependencies)
	catalog := map[string][]MappedColumn{"DUAL": {{"ID", "ID"}}}
	got, err := ConvertView(`SELECT ID FROM DUAL`, "SRC", "DST", catalog)
	require.NoError(t, err)
	require.Equal(t, []string{"DUAL"}, got.Dependencies)
	require.Contains(t, got.SQL, `FROM "DST"."DUAL"`)
	got, err = ConvertView(`SELECT SYSDATE AS NOW_VALUE FROM DUAL`, "SRC", "DST", map[string][]MappedColumn{})
	require.NoError(t, err)
	require.Empty(t, got.Dependencies)
	require.Contains(t, got.SQL, `FROM "DUAL"`)
	got, err = ConvertView(`SELECT DUMMY FROM DUAL`, "SRC", "DST", map[string][]MappedColumn{})
	require.NoError(t, err)
	require.Equal(t, []MappedColumn{{"DUMMY", "DUMMY"}}, got.Columns)
	_, err = ConvertView(`SELECT SYSDATE AS NOW_VALUE FROM SRC.DUAL`, "SRC", "DST", map[string][]MappedColumn{})
	require.ErrorContains(t, err, "列映射")
}
