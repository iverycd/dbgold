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
	for _, unchanged := range []string{"/* preserve 'AbC' */", "q'[MiXeD 'quoted' SRC.PARENT]'", "-- preserve Src.Parent and SRC.PARENT@link", "N'汉字AbC'", "NVL(", "TO_CHAR(SYSDATE,'YYYY-MM-DD HH24:MI:SS')"} {
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
		`SELECT ID FROM (SELECT ID FROM PARENT)`,
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
	for _, literal := range []string{`'a''b'`, `q'[a'b @link]'`, `Q'{a "quote" b}'`, `q'(a'b)'`, `q'<a>b>'`, `q'!a'b!'`, `nq'[汉字'x]'`, `N'汉字''x'`} {
		t.Run(literal, func(t *testing.T) {
			got, err := ConvertView("SELECT "+literal+" AS value FROM DUAL", "SRC", "DST", viewTestCatalog())
			require.NoError(t, err)
			require.Contains(t, got.SQL, literal)
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
	require.True(t, strings.Contains(got.SQL, "CAST(") && strings.Contains(got.SQL, "EXTRACT(YEAR FROM DATE '2024-01-01')") && strings.Contains(got.SQL, "SYSDATE"))
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
