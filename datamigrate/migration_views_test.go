package datamigrate

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestMigrationViewDependenciesRespectQueryScopes(t *testing.T) {
	for _, tt := range []struct {
		name, sql string
		want      []string
	}{
		{"quoted relations", `SELECT * FROM "Schema"."MiXeD" a JOIN [dbo].[BracketName] b ON a.id=b.id, ` + "`OtherTable`" + ` c`, []string{"MiXeD", "BracketName", "OtherTable"}},
		{"cte body and main", `WITH Broken AS (SELECT * FROM Healthy) SELECT * FROM Broken JOIN Actual ON 1=1`, []string{"Healthy", "Actual"}},
		{"nonrecursive same name base", `WITH SameName AS (SELECT * FROM SameName) SELECT * FROM SameName`, []string{"SameName"}},
		{"qualified base not hidden by cte", `WITH Hidden AS (SELECT * FROM Healthy) SELECT * FROM Schema.Hidden`, []string{"Healthy", "Hidden"}},
		{"sequential ctes", `WITH a(x) AS NOT MATERIALIZED (SELECT id FROM First), b AS (SELECT * FROM a JOIN Second ON 1=1) SELECT * FROM b JOIN Third ON 1=1`, []string{"First", "Second", "Third"}},
		{"recursive cte", `WITH RECURSIVE tree AS (SELECT * FROM Roots UNION ALL SELECT n.* FROM Nodes n JOIN tree t ON n.parent=t.id) SELECT * FROM tree`, []string{"Roots", "Nodes"}},
		{"nested scope leaves outer base", `SELECT * FROM OuterTable WHERE EXISTS (WITH OuterTable AS (SELECT * FROM NestedBase) SELECT * FROM OuterTable)`, []string{"OuterTable", "NestedBase"}},
		{"nested scope does not escape", `SELECT * FROM (WITH Shadow AS (SELECT * FROM InnerBase) SELECT * FROM Shadow) x JOIN Shadow ON 1=1`, []string{"InnerBase", "Shadow"}},
		{"expression from is not relation", `SELECT EXTRACT(YEAR FROM Broken),SUBSTRING(txt FROM 2),TRIM(' ' FROM txt) FROM Healthy`, []string{"Healthy"}},
		{"literals and comments", `SELECT 'FROM Broken', $$JOIN Bad$$, q'[FROM Wrong]' FROM Healthy /* JOIN Nope */ -- FROM Other
`, []string{"Healthy"}},
		{"nested joins and commas", `SELECT * FROM (First f JOIN Second s ON f.id=s.id), Third WHERE EXISTS (SELECT 1 FROM Fourth)`, []string{"First", "Second", "Third", "Fourth"}},
		{"table function nested subquery", `SELECT * FROM dbo.RowsFrom((SELECT id FROM Input)) f JOIN OnlyTable ON 1=1`, []string{"Input", "OnlyTable"}},
		{"only grouping", `SELECT * FROM ONLY (Schema.Base) b JOIN LATERAL (SELECT * FROM Later) l ON TRUE`, []string{"Base", "Later"}},
		{"quoted keyword name", `SELECT * FROM "FROM" JOIN "JOIN" ON TRUE`, []string{"FROM", "JOIN"}},
		{"set operations", `SELECT * FROM First UNION SELECT * FROM Second EXCEPT SELECT * FROM Third`, []string{"First", "Second", "Third"}},
		{"function comma not relation", `SELECT * FROM Healthy h JOIN Other o ON COALESCE(h.id,o.id)=1`, []string{"Healthy", "Other"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := tt.sql
			names, err := migrationViewDependencies(tt.sql)
			require.NoError(t, err)
			require.Equal(t, tt.want, names)
			require.Equal(t, original, tt.sql)
		})
	}
	for _, sql := range []string{`SELECT * FROM (Broken`, `SELECT 'unterminated`, `SELECT * FROM Broken)`, `SELECT * FROM "bad`} {
		_, err := migrationViewDependencies(sql)
		require.Error(t, err, sql)
	}
}

type independentViewWriter struct {
	recordingOscarWriter
	names    []string
	failures map[string]error
}

func (w *independentViewWriter) CreateView(ctx context.Context, view source.ViewInfo) error {
	w.names = append(w.names, view.ViewName)
	if err := w.failures[view.ViewName]; err != nil {
		return err
	}
	return w.recordingOscarWriter.CreateView(ctx, view)
}

func TestIndependentViewsContinueAndPropagateOnlyActualDependencies(t *testing.T) {
	r, w := newOscarFixture(), &independentViewWriter{failures: map[string]error{"VFAIL": errors.New("invalid view syntax")}}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetDBType = "oscar"
	m.blockMigrationTable("Broken", "execute", errors.New("table create failed"))
	views := []source.ViewInfo{
		{ViewName: "vChain", Definition: `SELECT * FROM vBlocked`},
		{ViewName: "vHealthy", Definition: `WITH Broken AS (SELECT * FROM Healthy) SELECT * FROM Broken`},
		{ViewName: "vBlocked", Definition: `SELECT * FROM Broken`},
		{ViewName: "vFail", Definition: `SELECT * FROM Healthy`},
		{ViewName: "vFailChild", Definition: `SELECT * FROM vFail`},
		{ViewName: "vLiteral", Definition: `SELECT EXTRACT(YEAR FROM Broken), 'FROM Broken' FROM Healthy`},
	}
	category := newCategoryReport()
	category.Total = len(views)
	m.migrateIndependentViews(context.Background(), views, &category)
	require.Equal(t, 2, category.Success)
	require.Equal(t, 4, category.Failed)
	require.ElementsMatch(t, []string{"VHEALTHY", "VFAIL", "VLITERAL"}, w.names)
	for _, item := range category.Items {
		if item.Name == "VFAIL" {
			require.Equal(t, "execute", item.Stage)
		} else {
			require.Equal(t, "dependency", item.Stage)
		}
	}
}

func TestIndependentViewsDuplicateTargetsAreBothBlocked(t *testing.T) {
	for _, names := range [][]string{{"CaseView", "CASEVIEW"}, {"Same", "Same"}} {
		r, w := newOscarFixture(), &independentViewWriter{}
		m, _ := newTestMigrator(r, w)
		m.cfg.TargetDBType = "oscar"
		views := []source.ViewInfo{{ViewName: names[0], Definition: `SELECT 1`}, {ViewName: names[1], Definition: `SELECT 2`}, {ViewName: "Healthy", Definition: `SELECT 3`}}
		category := newCategoryReport()
		category.Total = len(views)
		m.migrateIndependentViews(context.Background(), views, &category)
		require.Equal(t, 1, category.Success)
		require.Equal(t, 2, category.Failed)
		require.Equal(t, []string{"HEALTHY"}, w.names)
		for _, item := range category.Items {
			require.Equal(t, "preflight", item.Stage)
			require.Contains(t, item.Error, "视图名冲突")
			require.NotEmpty(t, item.SourceName)
		}
	}
}

func TestIndependentViewsConnectionFailureStopsNewObjects(t *testing.T) {
	r, w := newOscarFixture(), &independentViewWriter{failures: map[string]error{"BROKEN": driver.ErrBadConn}}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetDBType = "oscar"
	views := []source.ViewInfo{{ViewName: "Broken", Definition: `SELECT 1`}, {ViewName: "Healthy", Definition: `SELECT 2`}}
	category := newCategoryReport()
	category.Total = len(views)
	m.migrateIndependentViews(context.Background(), views, &category)
	require.Equal(t, 1, category.Failed)
	require.Equal(t, []string{"BROKEN"}, w.names)
}

func TestMigrationViewCTEsUseSourceIdentifierAndRecursionRules(t *testing.T) {
	for _, tt := range []struct {
		source, sql string
		want        []string
	}{
		{"sqlserver", `WITH [Tree] AS (SELECT * FROM Roots UNION ALL SELECT n.* FROM Nodes n JOIN tree t ON n.parent=t.id) SELECT * FROM TREE`, []string{"Roots", "Nodes"}},
		{"oracle", `WITH "NODES"(id) AS (SELECT id FROM "Actual") SELECT * FROM nodes`, []string{"Actual"}},
		{"postgres", `WITH "MiXeD" AS (SELECT * FROM "Actual") SELECT * FROM "MiXeD" JOIN mixed ON TRUE`, []string{"Actual", "mixed"}},
	} {
		names, err := migrationViewDependencies(tt.sql, tt.source)
		require.NoError(t, err)
		require.Equal(t, tt.want, names)
	}
}
