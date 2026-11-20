package datamigrate

import (
	"os"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

func TestMySQLViewSchemaAndAliasReferences(t *testing.T) {
	_, _, m := repairFixture("mysql")
	m.writer = &target.MySQLWriter{}
	m.dmSourceSchema = "Source.1"
	m.cfg.TargetDBType = "mysql"
	m.cfg.TargetSchema = "Dst.2"
	m.cfg.StripViewSchemas = []string{"Old.3"}
	m.cfg.LowerCaseNames = true
	raw := "SELECT `Source.1`.*, `Source.1`.`Column`, '%Y Source.1. convert( uuid()', `Old.3`.`T2`.`Col` FROM `Source.1`.`TaBle` AS `Source.1` JOIN `Old.3`.`T2` ON `Source.1`.`Id`=`Old.3`.`T2`.`Id` JOIN `External`.`MiXeD` ON `External`.`MiXeD`.`Id`=1"
	out, err := m.prepareView(t.Context(), source.ViewInfo{ViewName: "V", Definition: raw})
	require.NoError(t, err)
	for _, piece := range []string{"`Source.1`.*", "`Source.1`.`Column`", "'%Y Source.1. convert( uuid()'", "`Dst.2`.`table` AS `Source.1`", "`Dst.2`.`t2`.`Col`", "`External`.`MiXeD`"} {
		require.Contains(t, out.Definition, piece)
	}
}

func TestMySQLDamengOverdueView(t *testing.T) {
	r, _, m := repairFixture("mysql")
	raw, err := os.ReadFile("testdata/mysql_views/overdue.sql")
	require.NoError(t, err)
	_, body, ok := strings.Cut(string(raw), " AS\n")
	require.True(t, ok)
	r.ddl["cg_hetongtiaokuan"] = &source.TableDDLInfo{Columns: []source.ColumnInfo{{Name: "FuKuanTime", DataType: "datetime"}}}
	for _, name := range []string{"cg_hetonginfo", "cg_hetongbiangeng"} {
		r.ddl[name] = &source.TableDDLInfo{Columns: []source.ColumnInfo{{Name: "isbiangeng", DataType: "varchar"}, {Name: "isterminate", DataType: "varchar"}}}
	}
	out, err := m.prepareView(t.Context(), source.ViewInfo{ViewName: "view_hetong_yuqitongji_ysjx", Definition: body})
	require.NoError(t, err)
	require.Equal(t, 4, strings.Count(out.Definition, "TO_DAYS("))
	require.Equal(t, 2, strings.Count(out.Definition, "IS NOT NULL"))
	require.Contains(t, out.Definition, "DATEADD(DAY, 7, NOW())")
	require.NotContains(t, out.Definition, "::date")
	require.Contains(t, out.Definition, `"B"."SHR_DATE" AS "REALFUKUANTIME"`)
}

func TestMySQLDamengViewUnsupportedExpressionIsLocal(t *testing.T) {
	r, _, m := repairFixture("mysql")
	r.views = []source.ViewInfo{{ViewName: "Bad", Definition: "SELECT CONVERT('abc' USING latin1) AS val FROM T"}, {ViewName: "Child", Definition: "SELECT val FROM Bad"}, {ViewName: "Good", Definition: "SELECT IFNULL(Txt,'%Y') AS val FROM T"}}
	m.cfg.ViewNames = []string{"Bad", "Child", "Good"}
	m.cfg.CaptureRepair = true
	report, _ := m.RunSelectedViews(t.Context())
	require.Equal(t, 1, report.Views.Success)
	require.Equal(t, 2, report.Views.Failed)
	for _, item := range report.Views.Items {
		switch item.SourceName {
		case "Bad":
			require.Equal(t, "unsupported", item.Stage)
			require.Contains(t, item.Error, "latin1")
			require.Equal(t, r.views[0].Definition, item.SourceDDL)
		case "Child":
			require.Equal(t, "dependency", item.Stage)
			require.Contains(t, item.Error, "Bad")
		default:
			t.Fatalf("unexpected failure: %+v", item)
		}
	}
}
