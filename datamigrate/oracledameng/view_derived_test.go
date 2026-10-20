package oracledameng

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// This is the complete query body from VIEW_MODULEVISITLOG. Its anonymous
// derived tables expose only their projection, including the aggregate alias.
const moduleVisitLogViewSQL = `SELECT Rownum AS Row_ID,
       VisitCount, ModuleCode, BaseOuGuid, ModuleGuid, ModuleName
  from (select COUNT(*) AS VisitCount,
               ModuleCode, BaseOuGuid, ModuleGuid, ModuleName
          FROM (SELECT ModuleCode, BaseOuGuid, ModuleGuid, ModuleName
                  FROM Frame_VisitLog
                 WHERE BaseOuGuid IS NOT NULL
                UNION ALL
                SELECT ModuleCode, BaseOuGuid, ModuleGuid, ModuleName
                  FROM Frame_VisitLog_Histroy
                 WHERE BaseOuGuid IS NOT NULL)
         GROUP BY ModuleCode,ModuleGuid, BaseOuGuid, ModuleName)`

func TestConvertViewModuleVisitLogNestedUnionAndRownum(t *testing.T) {
	columns := []MappedColumn{{"MODULECODE", "MODULECODE"}, {"BASEOUGUID", "BASEOUGUID"}, {"MODULEGUID", "MODULEGUID"}, {"MODULENAME", "MODULENAME"}}
	catalog := map[string][]MappedColumn{"FRAME_VISITLOG": columns, "FRAME_VISITLOG_HISTROY": columns}
	for _, catalog := range []map[string][]MappedColumn{nil, catalog} {
		got, err := ConvertView(moduleVisitLogViewSQL, "SRC", "DST", catalog)
		require.NoError(t, err)
		require.Equal(t, []string{"FRAME_VISITLOG", "FRAME_VISITLOG_HISTROY"}, got.Dependencies)
		if catalog == nil {
			continue
		}
		require.Equal(t, []MappedColumn{{"ROW_ID", "ROW_ID"}, {"VISITCOUNT", "VISITCOUNT"}, {"MODULECODE", "MODULECODE"}, {"BASEOUGUID", "BASEOUGUID"}, {"MODULEGUID", "MODULEGUID"}, {"MODULENAME", "MODULENAME"}}, got.Columns)
		require.Contains(t, strings.ToUpper(got.SQL), `SELECT ROWNUM AS "ROW_ID"`)
		require.NotContains(t, got.SQL, `"ROWNUM"`)
		require.Contains(t, got.SQL, `"VISITCOUNT", "MODULECODE", "BASEOUGUID", "MODULEGUID", "MODULENAME"`)
		require.Contains(t, got.SQL, `FROM "DST"."FRAME_VISITLOG"`)
		require.Contains(t, got.SQL, `FROM "DST"."FRAME_VISITLOG_HISTROY"`)
		require.Contains(t, got.SQL, `COUNT(*) AS "VISITCOUNT"`)
		require.Equal(t, 2, strings.Count(got.SQL, `IS NOT NULL`))
		require.Contains(t, got.SQL, `GROUP BY "MODULECODE","MODULEGUID", "BASEOUGUID", "MODULENAME"`)
	}
}

func TestConvertViewDerivedColumnsPreservePhysicalAndLogicalMapping(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		cols []MappedColumn
		want []string
	}{
		{
			name: "physical ROWID rename survives two query boundaries",
			raw:  `SELECT q.ROWID FROM (SELECT d.ROWID FROM (SELECT p.ROWID FROM PARENT p) d) q`,
			cols: []MappedColumn{{"ROWID", "ROW_ID_2"}},
			want: []string{`SELECT "Q"."ROW_ID_2"`, `SELECT "D"."ROW_ID_2"`, `SELECT "P"."ROW_ID_2" FROM "DST"."PARENT" "P"`},
		},
		{
			name: "explicit output alias replaces inherited source name",
			raw:  `SELECT q.logical_rowid FROM (SELECT d.ROWID AS logical_rowid FROM (SELECT p.ROWID FROM PARENT p) d) q`,
			cols: []MappedColumn{{"LOGICAL_ROWID", "LOGICAL_ROWID"}},
			want: []string{`SELECT "Q"."LOGICAL_ROWID"`, `SELECT "D"."ROW_ID_2" AS "LOGICAL_ROWID"`, `SELECT "P"."ROW_ID_2"`},
		},
		{
			name: "quoted mixed case output resolves from parent",
			raw:  `SELECT "q"."readableName" FROM (SELECT "m"."camelCase" AS "readableName" FROM "SRC"."Mixed" "m") "q"`,
			cols: []MappedColumn{{"readableName", "READABLENAME"}},
			want: []string{`SELECT "Q"."READABLENAME"`, `SELECT "M"."CAMELCASE" AS "READABLENAME"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConvertView(tc.raw, "SRC", "DST", viewTestCatalog())
			require.NoError(t, err)
			require.Equal(t, tc.cols, got.Columns)
			for _, want := range tc.want {
				require.Contains(t, got.SQL, want)
			}
		})
	}
}

func TestConvertViewDerivedRelationAliasScopesAndJoinGroups(t *testing.T) {
	raw := `SELECT p.ID parent_id, c.ID child_id
FROM ((SELECT p.ID FROM PARENT p) p LEFT JOIN (CHILD c) ON ((p.ID=c.PARENT_ID)))`
	got, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []string{"CHILD", "PARENT"}, got.Dependencies)
	require.Equal(t, []MappedColumn{{"PARENT_ID", "PARENT_ID"}, {"CHILD_ID", "CHILD_ID"}}, got.Columns)
	require.Contains(t, got.SQL, `FROM ((SELECT "P"."ID" FROM "DST"."PARENT" "P") "P" LEFT JOIN ("DST"."CHILD" "C") ON (("P"."ID"="C"."PARENT_ID")))`)
}

func TestConvertViewDerivedQueryPreservesCommentsAndStrings(t *testing.T) {
	raw := `SELECT d /* parent ref */ . display_name
FROM ( /* begin child */ SELECT NVL(p.NAME, 'FROM (SELECT ROWNUM)') /* alias */ AS display_name
FROM SRC /* owner */ . PARENT p -- inner relation
WHERE p.ID>0 /* end child */) /* outer relation */ d`
	got, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []MappedColumn{{"DISPLAY_NAME", "DISPLAY_NAME"}}, got.Columns)
	require.Contains(t, got.SQL, `'FROM (SELECT ROWNUM)'`)
	for _, comment := range []string{"/* parent ref */", "/* begin child */", "/* alias */", "/* owner */", "-- inner relation\n", "/* end child */", "/* outer relation */"} {
		require.Equal(t, 1, strings.Count(got.SQL, comment), comment)
	}
}

func TestConvertViewDerivedWildcard(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"anonymous", `SELECT * FROM (SELECT p.ROWID, p.NAME FROM PARENT p)`, `SELECT "ROW_ID_2", "NAME" FROM (`},
		{"Oracle alias", `SELECT d.* FROM (SELECT p.ROWID, p.NAME FROM PARENT p) d`, `SELECT "D"."ROW_ID_2", "D"."NAME" FROM (`},
		{"star across two anonymous query levels", `SELECT * FROM (SELECT * FROM (SELECT p.ROWID, p.NAME FROM PARENT p))`, `SELECT "ROW_ID_2", "NAME" FROM (SELECT "ROW_ID_2", "NAME" FROM (`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConvertView(tc.raw, "SRC", "DST", viewTestCatalog())
			require.NoError(t, err)
			require.Equal(t, []MappedColumn{{"ROWID", "ROW_ID_2"}, {"NAME", "NAME"}}, got.Columns)
			require.Equal(t, []string{"PARENT"}, got.Dependencies)
			require.Contains(t, got.SQL, tc.want)
		})
	}
	got, err := ConvertView(`SELECT d.*, c.ID child_id FROM ((SELECT p.ID parent_id, p.ROWID FROM PARENT p) d JOIN CHILD c ON d.parent_id=c.PARENT_ID)`, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []MappedColumn{{"PARENT_ID", "PARENT_ID"}, {"ROWID", "ROW_ID_2"}, {"CHILD_ID", "CHILD_ID"}}, got.Columns)
	require.Contains(t, got.SQL, `SELECT "D"."PARENT_ID", "D"."ROW_ID_2", "C"."ID" "CHILD_ID"`)
}

func TestConvertViewUnionOrderUsesFirstBranchOutput(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		raw := `SELECT p.ROWID AS first_name FROM PARENT p UNION ALL SELECT c.ID AS last_name FROM CHILD c ORDER BY first_name DESC, 1`
		if wrapped {
			raw = "SELECT q.first_name FROM (" + raw + ") q"
		}
		got, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
		require.NoError(t, err)
		require.Equal(t, []MappedColumn{{"FIRST_NAME", "FIRST_NAME"}}, got.Columns)
		require.Equal(t, []string{"CHILD", "PARENT"}, got.Dependencies)
		require.Contains(t, got.SQL, `ORDER BY "FIRST_NAME" DESC, 1`)
	}
	got, err := ConvertView(`SELECT p.ROWID FROM PARENT p UNION ALL SELECT c.ID FROM CHILD c ORDER BY ROWID`, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []MappedColumn{{"ROWID", "ROW_ID_2"}}, got.Columns)
	require.Contains(t, got.SQL, `ORDER BY "ROW_ID_2"`)
	for _, order := range []string{"last_name", "c.ID", "p.ROWID"} {
		_, err := ConvertView(`SELECT p.ROWID AS first_name FROM PARENT p UNION ALL SELECT c.ID AS last_name FROM CHILD c ORDER BY `+order, "SRC", "DST", viewTestCatalog())
		require.Error(t, err, order)
	}
}

func TestConvertViewDerivedUnionColumnsAndBranchScopes(t *testing.T) {
	got, err := ConvertView(`SELECT q.ROWID FROM (SELECT p.ROWID FROM PARENT p UNION ALL SELECT p.ID other_name FROM CHILD p) q`, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []MappedColumn{{"ROWID", "ROW_ID_2"}}, got.Columns)
	require.Contains(t, got.SQL, `SELECT "Q"."ROW_ID_2" FROM (`)
	for _, raw := range []string{
		`SELECT q.ID FROM (SELECT p.ID FROM PARENT p UNION ALL SELECT c.ID,c.PARENT_ID FROM CHILD c) q`,
		`SELECT q.ID FROM (SELECT p.ID FROM PARENT p UNION ALL SELECT p.ID FROM CHILD c) q`,
		`SELECT q.other_name FROM (SELECT p.ID FROM PARENT p UNION ALL SELECT c.ID other_name FROM CHILD c) q`,
	} {
		_, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
		require.Error(t, err, raw)
	}
}

func TestConvertViewDerivedAggregateUsesItsOwnOutputScope(t *testing.T) {
	raw := `SELECT d.label, SUM(d.total) AS grand_total
FROM (SELECT p.NAME AS label, p.AMOUNT AS total FROM PARENT p WHERE p.ID>0) d
GROUP BY d.label HAVING SUM(d.total)>1 ORDER BY grand_total DESC`
	got, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []MappedColumn{{"LABEL", "LABEL"}, {"GRAND_TOTAL", "GRAND_TOTAL"}}, got.Columns)
	require.Contains(t, got.SQL, `SUM("D"."TOTAL") AS "GRAND_TOTAL"`)
	require.Contains(t, got.SQL, `GROUP BY "D"."LABEL" HAVING SUM("D"."TOTAL")>1 ORDER BY "GRAND_TOTAL" DESC`)
	for _, tail := range []string{" WHERE p.ID>0", " ORDER BY p.ID"} {
		_, err := ConvertView(`SELECT q.ID FROM (SELECT p.ID FROM PARENT p) q`+tail, "SRC", "DST", viewTestCatalog())
		require.Error(t, err, tail)
	}
}

func TestConvertViewDerivedUnionQuotedOrderOutput(t *testing.T) {
	raw := `SELECT q."readableName" FROM (SELECT p.NAME AS "readableName" FROM PARENT p
UNION SELECT p.NAME AS another_name FROM PARENT p ORDER BY "readableName" NULLS LAST) q`
	got, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []MappedColumn{{"readableName", "READABLENAME"}}, got.Columns)
	require.Contains(t, got.SQL, `ORDER BY "READABLENAME" NULLS LAST`)
	_, err = ConvertView(strings.Replace(raw, `ORDER BY "readableName"`, `ORDER BY readableName`, 1), "SRC", "DST", viewTestCatalog())
	require.Error(t, err)
}

func TestConvertViewDerivedDiscoversWholeTreeBeforeBinding(t *testing.T) {
	raw := `SELECT q.ID first_id, c.ID child_id, tail.ID tail_id
FROM (SELECT p.ID FROM PARENT p JOIN MISSING_A a ON p.ID=a.ID
      UNION ALL SELECT b.ID FROM (SELECT x.ID FROM MISSING_B x) b) q
JOIN CHILD c ON q.ID=c.PARENT_ID, MISSING_C tail
UNION ALL SELECT final_rel.ID first_id, final_rel.ID child_id, final_rel.ID tail_id FROM LAST_DEP final_rel`
	want := []string{"CHILD", "LAST_DEP", "MISSING_A", "MISSING_B", "MISSING_C", "PARENT"}
	got, err := ConvertView(raw, "SRC", "DST", nil)
	require.NoError(t, err)
	require.Equal(t, want, got.Dependencies)
	got, err = ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.ErrorContains(t, err, "列映射")
	require.Equal(t, want, got.Dependencies)

	raw = `SELECT q.ID FROM (SELECT p.missing FROM PARENT p) q JOIN CHILD c ON q.ID=c.PARENT_ID
UNION ALL SELECT x.ID FROM MISSING_AFTER_BIND x`
	got, err = ConvertView(raw, "SRC", "DST", viewTestCatalog())
	require.Error(t, err)
	require.Equal(t, []string{"CHILD", "MISSING_AFTER_BIND", "PARENT"}, got.Dependencies)

	raw = `SELECT q.ID FROM (SELECT x.ID FROM PARENT x JOIN CHILD x ON x.ID=x.PARENT_ID) q, MISSING_AFTER_ALIAS tail`
	for _, catalog := range []map[string][]MappedColumn{nil, viewTestCatalog()} {
		got, err = ConvertView(raw, "SRC", "DST", catalog)
		require.ErrorContains(t, err, "别名")
		require.Equal(t, []string{"CHILD", "MISSING_AFTER_ALIAS", "PARENT"}, got.Dependencies)
	}
}

func TestConvertViewDerivedFailsClosedForUnsupportedOrOutOfScopeQueries(t *testing.T) {
	for _, raw := range []string{
		`SELECT p.ID FROM PARENT p WHERE EXISTS (SELECT c.ID FROM CHILD c WHERE c.PARENT_ID=p.ID)`,
		`SELECT p.ID FROM PARENT p WHERE p.ID IN (SELECT c.ID FROM CHILD c)`,
		`SELECT (SELECT c.ID FROM CHILD c) AS scalar_value FROM PARENT p`,
		`SELECT p.ID FROM PARENT p JOIN CHILD c ON p.ID=(SELECT x.ID FROM PARENT x)`,
		`WITH q AS (SELECT ID FROM PARENT) SELECT ID FROM q`,
		`SELECT q.ID FROM (WITH p AS (SELECT ID FROM PARENT) SELECT ID FROM p) q`,
		`SELECT p.ID FROM PARENT p, (SELECT c.ID FROM CHILD c WHERE c.PARENT_ID=p.ID) q`,
		`SELECT p.ID FROM (SELECT p.ID FROM PARENT p) q`,
		`SELECT q.NAME FROM (SELECT p.ID FROM PARENT p) q`,
		`SELECT q.ROWID FROM (SELECT p.ROWID AS renamed FROM PARENT p) q`,
		`SELECT "q".readableName FROM (SELECT "m"."camelCase" AS "readableName" FROM "Mixed" "m") "q"`,
		`SELECT ID FROM (SELECT ID FROM PARENT) q JOIN CHILD c ON q.ID=c.PARENT_ID`,
		`SELECT q.ID FROM (SELECT p.ID FROM PARENT p) AS q`,
		`SELECT q.ID FROM (SELECT ID FROM OTHER.PARENT) q`,
		`SELECT q.ID FROM (SELECT ID FROM PARENT NATURAL JOIN CHILD) q`,
		`SELECT q.ID FROM (SELECT ID FROM PARENT JOIN CHILD USING (ID)) q`,
		`SELECT q.ID FROM (SELECT ID FROM PARENT) q, (SELECT ID FROM CHILD) q`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
			require.Error(t, err)
		})
	}
}

func TestConvertViewDerivedDepthLimit(t *testing.T) {
	query := `SELECT p.ID FROM PARENT p`
	for i := 0; i < 60; i++ {
		query = "SELECT ID FROM (" + query + ")"
	}
	got, err := ConvertView(query, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Equal(t, []string{"PARENT"}, got.Dependencies)
	require.Equal(t, []MappedColumn{{"ID", "ID"}}, got.Columns)
	_, err = ConvertView("SELECT ID FROM ("+query+")", "SRC", "DST", viewTestCatalog())
	require.Error(t, err)
}

func TestConvertViewRownumOnlyAsDirectOutermostAliasedProjection(t *testing.T) {
	for _, raw := range []string{
		`SELECT ROWNUM AS row_id, p.ID FROM PARENT p`,
		`SELECT rownum row_id, q.ID FROM (SELECT p.ID FROM PARENT p) q`,
	} {
		got, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
		require.NoError(t, err)
		require.Equal(t, []MappedColumn{{"ROW_ID", "ROW_ID"}, {"ID", "ID"}}, got.Columns)
		require.NotContains(t, got.SQL, `"ROWNUM"`)
	}
	for _, raw := range []string{
		`SELECT ROWNUM FROM PARENT`,
		`SELECT ROWNUM + 1 AS row_id FROM PARENT`,
		`SELECT NVL(ROWNUM,0) AS row_id FROM PARENT`,
		`SELECT (ROWNUM) AS row_id FROM PARENT`,
		`SELECT p.ROWNUM AS row_id FROM PARENT p`,
		`SELECT ID FROM PARENT WHERE ROWNUM<=10`,
		`SELECT ID FROM PARENT ORDER BY ROWNUM`,
		`SELECT ROWNUM AS row_id, ID FROM PARENT ORDER BY ROWNUM`,
		`SELECT ID FROM PARENT GROUP BY ID, ROWNUM`,
		`SELECT COUNT(*) AS total FROM PARENT HAVING ROWNUM>0`,
		`SELECT p.ID FROM PARENT p JOIN CHILD c ON p.ID=ROWNUM`,
		`SELECT q.row_id FROM (SELECT ROWNUM AS row_id FROM PARENT) q`,
		`SELECT q.ID FROM (SELECT ID FROM PARENT WHERE ROWNUM<=10) q`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := ConvertView(raw, "SRC", "DST", viewTestCatalog())
			require.ErrorContains(t, err, "ROWNUM")
		})
	}
	// A quoted ordinary column named ROWNUM is a real mapped column, not the
	// pseudo-column, even when it appears in a derived-table projection.
	catalog := map[string][]MappedColumn{"T": {{"ROWNUM", "ROW_NUMBER_DATA"}}}
	got, err := ConvertView(`SELECT q."ROWNUM" FROM (SELECT "ROWNUM" FROM T) q`, "SRC", "DST", catalog)
	require.NoError(t, err)
	require.Equal(t, []MappedColumn{{"ROWNUM", "ROW_NUMBER_DATA"}}, got.Columns)
	require.Contains(t, got.SQL, `SELECT "Q"."ROW_NUMBER_DATA" FROM (SELECT "ROW_NUMBER_DATA" FROM "DST"."T") "Q"`)
	got, err = ConvertView(`SELECT 'ROWNUM' AS marker, ID FROM PARENT /* ROWNUM */`, "SRC", "DST", viewTestCatalog())
	require.NoError(t, err)
	require.Contains(t, got.SQL, `'ROWNUM'`)
	require.Contains(t, got.SQL, `/* ROWNUM */`)
}
