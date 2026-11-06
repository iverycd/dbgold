package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type sqlServerPGViewReader struct {
	mockReader
	views []source.ViewInfo
	refs  map[string]*source.TableDDLInfo
}

func (r *sqlServerPGViewReader) DBType() string { return "sqlserver" }
func (r *sqlServerPGViewReader) GetViews(context.Context) ([]source.ViewInfo, error) {
	return r.views, nil
}
func (r *sqlServerPGViewReader) GetViewRelationColumns(_ context.Context, schema, name string) (*source.TableDDLInfo, error) {
	if info := r.refs[strings.ToLower(name)]; info != nil && (schema == "" || strings.EqualFold(schema, "dbo")) {
		return info, nil
	}
	return nil, fmt.Errorf("missing relation %s.%s", schema, name)
}

type sqlServerPGViewWriter struct {
	mockWriter
	views []source.ViewInfo
}

func (w *sqlServerPGViewWriter) DBType() string           { return "kingbase" }
func (w *sqlServerPGViewWriter) Dialect() dialect.Dialect { return dialect.NewPostgres("kingbase") }
func (w *sqlServerPGViewWriter) CreateView(_ context.Context, v source.ViewInfo) error {
	w.views = append(w.views, v)
	return nil
}

func pgViewTestMigrator() (*Migrator, *sqlServerPGViewReader, *sqlServerPGViewWriter) {
	r := &sqlServerPGViewReader{refs: map[string]*source.TableDDLInfo{
		"frame_user":          {TableName: "Frame_User", Columns: []source.ColumnInfo{{Name: "Row_id", DataType: "int"}, {Name: "UserGuid", DataType: "varchar"}}},
		"frame_user_secondou": {TableName: "FRAME_USER_SECONDOU", Columns: []source.ColumnInfo{{Name: "ROW_ID", DataType: "int"}, {Name: "UserGuid", DataType: "varchar"}}},
		"portal_item":         {TableName: "portal_item", Columns: []source.ColumnInfo{{Name: "disabled", DataType: "nvarchar"}, {Name: "RowGuid", DataType: "varchar"}}},
		"portal_myitem":       {TableName: "Portal_MyItem", Columns: []source.ColumnInfo{{Name: "disabled", DataType: "int"}, {Name: "PortaletGuid", DataType: "varchar"}}},
	}}
	w := &sqlServerPGViewWriter{}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetDBType, m.cfg.TargetSchema, m.cfg.LowerCaseNames = "kingbase", "TargetSchema", true
	return m, r, w
}

func TestSQLServerPGViewsBindEachUnionBranch(t *testing.T) {
	m, _, _ := pgViewTestMigrator()
	v, err := m.prepareView(context.Background(), source.ViewInfo{ViewName: "VIEW_FRAME_USER_SECONDOU", Definition: `CREATE VIEW [dbo].[VIEW_FRAME_USER_SECONDOU] AS
SELECT Row_id FROM [dbo].[Frame_User]
UNION SELECT 1000000000 + a.ROW_ID AS row_id FROM [FRAME_USER_SECONDOU] a LEFT JOIN Frame_User b ON a.UserGuid=b.UserGuid`})
	require.NoError(t, err)
	require.Contains(t, strings.Join(strings.Fields(v.Definition), " "), `(1000000000 + "a"."row_id") AS "row_id"`)
	require.Contains(t, v.Definition, `"TargetSchema"."frame_user"`)
	require.Contains(t, v.Definition, `"a"."userguid" = "b"."userguid"`)
	require.NotContains(t, v.Definition, "||")
}

func TestSQLServerPGViewsBindCorrectDisabledColumn(t *testing.T) {
	m, _, _ := pgViewTestMigrator()
	v, err := m.prepareView(context.Background(), source.ViewInfo{ViewName: "view_portal_myitem", Definition: `CREATE VIEW dbo.view_portal_myitem AS SELECT portal_myitem.disabled FROM portal_item INNER JOIN portal_myitem ON portal_item.RowGuid=portal_myitem.PortaletGuid WHERE (portal_item.disabled=0) AND portal_myitem.disabled=0`})
	require.NoError(t, err)
	require.Contains(t, v.Definition, sqlServerIntegerText(`"portal_item"."disabled"`, "integer"))
	require.Contains(t, v.Definition, `("portal_myitem"."disabled" = 0)`)
	require.NotContains(t, v.Definition, `btrim("portal_myitem"`)
}

func TestSQLServerPGViewExpressions(t *testing.T) {
	for _, tt := range []struct{ sql, want string }{
		{`'1'+'2'+3`, `CAST(CASE WHEN btrim(('1' || '2')) = '' THEN '0' ELSE ('1' || '2') END AS integer) + 3`},
		{`'1'+2+3`, `CAST(CASE WHEN btrim('1') = '' THEN '0' ELSE '1' END AS integer) + 2`},
		{`CAST(1 AS nvarchar(10))+N'AbC'`, `(CAST(1 AS varchar(10)) || 'AbC')`},
		{`'AbC'+NULL`, `('AbC' || NULL)`},
		{`NULL+N'AbC'`, `(NULL || 'AbC')`},
		{`1+(2+3)`, `(1 + ((2 + 3)))`},
		{`LEN(N'ABc')`, `length(rtrim('ABc', ' '))`},
		{`LEN(123)`, `length(rtrim(CAST(123 AS text), ' '))`},
		{`ISNULL(CAST(NULL AS varchar(10)),N'AbC')`, `coalesce(CAST(NULL AS varchar(10)), 'AbC')`},
		{`CASE WHEN 1=1 THEN N'AbC' ELSE N'DeF' END`, `CASE WHEN (1 = 1) THEN 'AbC' ELSE 'DeF' END`},
	} {
		t.Run(tt.sql, func(t *testing.T) {
			m, _, _ := pgViewTestMigrator()
			v, err := m.prepareView(context.Background(), source.ViewInfo{ViewName: "V", Definition: "CREATE VIEW dbo.V AS SELECT " + tt.sql + " AS Result"})
			require.NoError(t, err)
			require.Contains(t, v.Definition, tt.want)
		})
	}
}

func TestSQLServerPGViewsPreserveExplicitHeadersAndGroupedJoins(t *testing.T) {
	m, _, _ := pgViewTestMigrator()
	v, err := m.prepareView(context.Background(), source.ViewInfo{ViewName: "V", Definition: `CREATE VIEW dbo.V ([MixedOutput]) AS SELECT a.ROW_ID FROM ((FRAME_USER_SECONDOU a LEFT JOIN Frame_User b ON a.UserGuid=b.UserGuid))`})
	require.NoError(t, err)
	require.Contains(t, v.Definition, `AS "__dbgold_view_columns" ("mixedoutput")`)
	require.Contains(t, v.Definition, `"a"."userguid" = "b"."userguid"`)
	require.NotContains(t, v.Definition, "a.UserGuid")
}

func TestSQLServerPGViewsDerivedStarAndWindowTypes(t *testing.T) {
	m, _, _ := pgViewTestMigrator()
	for _, raw := range []string{
		`SELECT d.disabled FROM (SELECT * FROM portal_item WHERE disabled=0) d WHERE d.disabled=0`,
		`SELECT d.rn FROM (SELECT ROW_NUMBER() OVER (PARTITION BY p.RowGuid ORDER BY p.disabled ASC) AS rn FROM portal_item p) d WHERE d.rn=1`,
	} {
		v, err := m.prepareView(context.Background(), source.ViewInfo{ViewName: "V", Definition: raw})
		require.NoError(t, err)
		require.Contains(t, v.Definition, `"d".`)
	}
}

func TestSQLServerPGViewCatalogCandidateOnlyRecognizesExactLookup(t *testing.T) {
	m, _, _ := pgViewTestMigrator()
	for _, raw := range []string{
		`SELECT t.name FROM syscolumns t WHERE id=OBJECT_ID('X')`,
		`SELECT t.name FROM (SELECT Extra FROM syscolumns WHERE id=OBJECT_ID('X')) t`,
		`SELECT t.name FROM (SELECT * FROM syscolumns WHERE id=OBJECT_ID('X') AND name='A') t`,
		`SELECT t.name FROM syscolumns t JOIN (SELECT RowGuid FROM portal_item WHERE disabled=OBJECT_ID('X')) x ON 1=1`,
	} {
		_, err := m.prepareView(context.Background(), source.ViewInfo{ViewName: "V", Definition: raw})
		var issue *sqlServerPGViewError
		require.ErrorAs(t, err, &issue)
		require.Empty(t, issue.suggested)
	}
}

func TestSQLServerPGViewScopesAndLiteralCase(t *testing.T) {
	m, _, _ := pgViewTestMigrator()
	m.cfg.LowerCaseNames = false
	v, err := m.prepareView(context.Background(), source.ViewInfo{ViewName: "V", Definition: `CREATE VIEW dbo.V AS SELECT d.Mixed FROM (SELECT p.disabled AS Mixed FROM Portal_Item p WHERE p.disabled=0) d WHERE d.Mixed=0`})
	require.NoError(t, err)
	require.Contains(t, v.Definition, `"d"."Mixed"`)
	require.Contains(t, v.Definition, `AS "Mixed"`)
	v, err = m.prepareView(context.Background(), source.ViewInfo{ViewName: "V", Definition: `CREATE VIEW dbo.V AS SELECT N'AbC + dbo.[Keep] GETDATE()' AS Label, 'O''Brien' AS Author FROM Portal_Item p WHERE NOT p.disabled=0`})
	require.NoError(t, err)
	require.Contains(t, v.Definition, `'AbC + dbo.[Keep] GETDATE()'`)
	require.Contains(t, v.Definition, `'O''Brien'`)
	require.Contains(t, v.Definition, `NOT (`+sqlServerIntegerText(`"p"."disabled"`, "integer"))
}

func TestSQLServerPGViewUnknownTypesDoNotGuess(t *testing.T) {
	for _, sql := range []string{
		`SELECT p.disabled+unknown_function() FROM portal_item p`,
		`SELECT missing+1 FROM portal_item`,
		`SELECT disabled+1 FROM portal_item p JOIN portal_myitem m ON p.RowGuid=m.PortaletGuid`,
	} {
		m, _, w := pgViewTestMigrator()
		_, err := m.createOneView(context.Background(), source.ViewInfo{ViewName: "V", Definition: sql})
		require.ErrorContains(t, err, "无法确定")
		require.Empty(t, w.views)
	}
}

func TestSQLServerPGSystemCatalogIsManualAndIndependentViewsContinue(t *testing.T) {
	m, r, w := pgViewTestMigrator()
	const raw = `CREATE VIEW dbo.Catalog AS SELECT t.name FROM (SELECT * FROM syscolumns WHERE id=OBJECT_ID(N'MixedView')) t`
	r.views = []source.ViewInfo{{ViewName: "Catalog", Definition: raw}, {ViewName: "Healthy", Definition: `CREATE VIEW dbo.Healthy AS SELECT N'AbC' AS Label`}}
	m.cfg.CaptureRepair = true
	m.beginRepair(context.Background())
	category := newCategoryReport()
	m.migrateIndependentViews(context.Background(), r.views, &category)
	require.Equal(t, 1, category.Success)
	require.Equal(t, 1, category.Failed)
	require.Len(t, w.views, 1)
	require.Equal(t, "healthy", w.views[0].ViewName)
	result := category.Items[0]
	require.Contains(t, result.Error, "需人工适配")
	require.Equal(t, raw, result.SourceDDL)
	require.Empty(t, result.DDL)
	require.Contains(t, result.SuggestedDDL, "information_schema.columns")
	require.Contains(t, result.SuggestedDDL, "'mixedview'")
	require.Contains(t, result.SuggestedDDL, `CREATE OR REPLACE VIEW "TargetSchema"."catalog"`)
	report := newMigrationReport()
	report.Views = category
	m.finishRepair(context.Background(), &report)
	for _, obj := range report.RepairManifest.Objects {
		if obj.SourceName == "Catalog" {
			require.Empty(t, obj.Units)
			require.Equal(t, raw, obj.SourceDDL)
			require.Equal(t, result.SuggestedDDL, obj.SuggestedDDL)
			require.True(t, obj.CanEditDDL())
			require.Equal(t, "failed", obj.Status)
		}
	}
}

func TestSQLServerPGRepairInventoryUsesExecutableViewSQL(t *testing.T) {
	m, r, w := pgViewTestMigrator()
	r.views = []source.ViewInfo{{ViewName: "V", Definition: `CREATE VIEW dbo.V AS SELECT 1000000000+ROW_ID AS Total FROM FRAME_USER_SECONDOU`}}
	m.cfg.CaptureRepair = true
	m.beginRepair(context.Background())
	var planned string
	for _, object := range m.repairs.objects {
		if object.Kind == "view" {
			planned = object.DDL
			require.Equal(t, r.views[0].Definition, object.SourceDDL)
		}
	}
	ddl, err := m.createOneView(context.Background(), r.views[0])
	require.NoError(t, err)
	require.Len(t, w.views, 1)
	require.Equal(t, strings.TrimSuffix(planned, ";"), strings.TrimSuffix(ddl, ";"))
	require.Contains(t, planned, `(1000000000 + "row_id")`)
}
