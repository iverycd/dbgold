package target

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func expectDMFunctionExists(m sqlmock.Sqlmock, name string, count int) {
	m.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=? AND OBJECT_TYPE='FUNCTION'`)).
		WithArgs("APP", name).WillReturnRows(sqlmock.NewRows([]string{"N"}).AddRow(count))
}

func expectDMFunctionDependencies(m sqlmock.Sqlmock, spec dmBusinessFunction) {
	for _, dep := range spec.dependencies {
		rows := sqlmock.NewRows([]string{"COLUMN_NAME"})
		for _, col := range dep.columns {
			rows.AddRow(col)
		}
		if len(dep.columns) == 0 {
			rows.AddRow("USERGUID")
		}
		m.ExpectQuery("SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS").WithArgs("APP", dep.table).WillReturnRows(rows)
	}
}

func TestMySQLDamengBusinessFunctionsCreateAndSkip(t *testing.T) {
	w, m := mockDM(t)
	specs := dmBusinessFunctions()
	for _, spec := range specs {
		if spec.name == "GETITEMTEXT" {
			expectDMFunctionExists(m, spec.name, 1) // No signature, body, or dependency reads.
			continue
		}
		expectDMFunctionExists(m, spec.name, 0)
		expectDMFunctionDependencies(m, spec)
		m.ExpectExec(regexp.QuoteMeta(w.dmBusinessFunctionDDL(spec))).WillReturnResult(sqlmock.NewResult(0, 0))
		m.ExpectQuery("SELECT STATUS FROM ALL_OBJECTS").WithArgs("APP", spec.name).
			WillReturnRows(sqlmock.NewRows([]string{"STATUS"}).AddRow("VALID"))
	}
	report, err := w.EnsureMySQLDamengBusinessFunctions(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 7, report.Total)
	require.Equal(t, 6, report.Created)
	require.Equal(t, 1, report.Reused)
	require.Equal(t, 7, report.Success)
	require.Zero(t, report.Failed)
	skipped := dmFunctionResult(t, report, "GETITEMTEXT")
	require.Empty(t, skipped.DDL)
	require.Equal(t, "已存在，跳过，未校验定义", skipped.DefinitionWarning)
}

func TestMySQLDamengBusinessFunctionsContinueAfterObjectFailures(t *testing.T) {
	w, m := mockDM(t)
	specs := dmBusinessFunctions()
	for _, spec := range specs {
		expectDMFunctionExists(m, spec.name, 0)
		switch spec.name {
		case "GETCHILDOULIST": // Missing table.
			m.ExpectQuery("SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS").WithArgs("APP", "FRAME_OU").
				WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME"}))
		case "GETITEMTEXT": // Missing column.
			m.ExpectQuery("SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS").WithArgs("APP", "CODE_MAIN").
				WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME"}).AddRow("CODEID"))
		case "GETOUNAMEBYUSERGUID": // Ordinary DDL error.
			expectDMFunctionDependencies(m, spec)
			m.ExpectExec("CREATE FUNCTION").WillReturnError(errors.New("create denied"))
		case "GETQUESTIONTEXT": // CREATE accepted but object invalid.
			expectDMFunctionDependencies(m, spec)
			m.ExpectExec("CREATE FUNCTION").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectQuery("SELECT STATUS FROM ALL_OBJECTS").WillReturnRows(sqlmock.NewRows([]string{"STATUS"}).AddRow("INVALID"))
			m.ExpectQuery("SELECT LINE,POSITION,TEXT FROM ALL_ERRORS").WithArgs("APP", spec.name).
				WillReturnRows(sqlmock.NewRows([]string{"LINE", "POSITION", "TEXT"}).AddRow(4, 2, "invalid column"))
		case "GETUSERNAMEBYUSERGUID": // CREATE accepted but function not found.
			expectDMFunctionDependencies(m, spec)
			m.ExpectExec("CREATE FUNCTION").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectQuery("SELECT STATUS FROM ALL_OBJECTS").WillReturnRows(sqlmock.NewRows([]string{"STATUS"}))
		case "SELECTALLUSERCOUNT", "FUN_GETPY":
			expectDMFunctionDependencies(m, spec)
			m.ExpectExec("CREATE FUNCTION").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectQuery("SELECT STATUS FROM ALL_OBJECTS").WillReturnRows(sqlmock.NewRows([]string{"STATUS"}).AddRow("VALID"))
		}
	}
	report, err := w.EnsureMySQLDamengBusinessFunctions(context.Background(), nil)
	require.Error(t, err)
	require.Equal(t, 5, report.Failed)
	require.Equal(t, 2, report.Created)
	require.Contains(t, dmFunctionResult(t, report, "GETCHILDOULIST").Error, "依赖表")
	require.Contains(t, dmFunctionResult(t, report, "GETITEMTEXT").Error, "CODENAME")
	require.Contains(t, dmFunctionResult(t, report, "GETOUNAMEBYUSERGUID").Error, "create denied")
	require.Contains(t, dmFunctionResult(t, report, "GETQUESTIONTEXT").Error, "4:2 invalid column")
	require.Contains(t, dmFunctionResult(t, report, "GETUSERNAMEBYUSERGUID").Error, sql.ErrNoRows.Error())
	for _, item := range report.Items {
		require.NotEmpty(t, item.DDL)
	}
}

func TestMySQLDamengBusinessFunctionsBlockedDependencyAndExistingSkip(t *testing.T) {
	w, m := mockDM(t)
	for _, spec := range dmBusinessFunctions() {
		if spec.name == "GETCHILDOULIST" {
			expectDMFunctionExists(m, spec.name, 0) // Stale target table must not be used.
		} else {
			expectDMFunctionExists(m, spec.name, 1) // User explicitly asked to skip even stale functions.
		}
	}
	report, err := w.EnsureMySQLDamengBusinessFunctions(context.Background(), map[string]string{"FRAME_OU": "CREATE TABLE failed"})
	require.ErrorContains(t, err, "CREATE TABLE failed")
	require.Equal(t, 1, report.Failed)
	require.Equal(t, 6, report.Reused)
}

func TestMySQLDamengBusinessFunctionsStopOnTaskFailure(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "connection lost", true: "cancelled"}[cancelFirst], func(t *testing.T) {
			w, m := mockDM(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelFirst {
				cancel()
			} else {
				first := dmBusinessFunctions()[0]
				expectDMFunctionExists(m, first.name, 0)
				expectDMFunctionDependencies(m, first)
				m.ExpectExec("CREATE FUNCTION").WillReturnError(io.EOF)
			}
			report, err := w.EnsureMySQLDamengBusinessFunctions(ctx, nil)
			require.Error(t, err)
			require.Equal(t, 7, report.Failed)
			require.Contains(t, dmFunctionResult(t, report, "FUN_GETPY").Error, "任务已终止")
		})
	}
}

func TestMySQLDamengBusinessFunctionDDL(t *testing.T) {
	w, _ := mockDM(t)
	w.schema = `APP"X`
	for _, spec := range dmBusinessFunctions() {
		ddl := w.dmBusinessFunctionDDL(spec)
		require.Contains(t, ddl, `CREATE FUNCTION "APP""X"."`+spec.name+`"`)
		require.True(t, strings.HasSuffix(strings.ToUpper(ddl), "END;"))
		for _, forbidden := range []string{"DEFINER", "DELIMITER", "CHARSET", "GROUP_CONCAT", "{{", "CREATE OR REPLACE"} {
			require.NotContains(t, ddl, forbidden)
		}
		for _, dep := range spec.dependencies {
			require.Contains(t, ddl, `"APP""X"."`+dep.table+`"`)
		}
	}
}

func dmFunctionResult(t *testing.T, report *CompatibilityFunctionsReport, name string) CompatibilityFunctionResult {
	t.Helper()
	for _, item := range report.Items {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("missing function %s", name)
	return CompatibilityFunctionResult{}
}

func TestMySQLDamengFunGetPyPreservesOriginalBody(t *testing.T) {
	original, err := os.ReadFile("testdata/dameng_fun_getpy_original.sql")
	require.NoError(t, err)
	_, body, ok := strings.Cut(string(original), "        as\n")
	require.True(t, ok)
	require.Equal(t, strings.TrimSuffix(body, "\n"), dmFunGetPyBody)
	w, _ := mockDM(t)
	var found bool
	for _, spec := range dmBusinessFunctions() {
		if spec.name != "FUN_GETPY" {
			continue
		}
		found = true
		require.Empty(t, spec.dependencies)
		require.Equal(t, "CREATE FUNCTION \"APP\".\"FUN_GETPY\"(p_name IN VARCHAR2)\nRETURN VARCHAR2\nAS\n"+dmFunGetPyBody, w.dmBusinessFunctionDDL(spec))
	}
	require.True(t, found)
	boundaries := regexp.MustCompile(`(?:if|elsif) v_compare >= f_nlssort\('([^']*)'\) and v_compare <= f_nlssort\('([^']*)'\) then\s+v_return := v_return \|\| '([a-z])';`).FindAllStringSubmatch(dmFunGetPyBody, -1)
	want := []struct{ first, last, initial string }{
		{" 吖 ", "驁 ", "a"}, {"八 ", "簿 ", "b"}, {"嚓 ", "錯 ", "c"}, {"咑 ", "鵽 ", "d"},
		{"妸 ", "樲 ", "e"}, {"发 ", "猤 ", "f"}, {"旮 ", "腂 ", "g"}, {"妎 ", "夻 ", "h"},
		{"丌 ", "攈 ", "j"}, {"咔 ", "穒 ", "k"}, {"垃 ", "擽 ", "l"}, {"嘸 ", "椧 ", "m"},
		{"拏 ", "瘧 ", "n"}, {"筽 ", "漚 ", "o"}, {"妑 ", "曝 ", "p"}, {"七 ", "裠", "q"},
		{"亽 ", "鶸 ", "r"}, {"仨 ", "蜶 ", "s"}, {"侤 ", "籜 ", "t"}, {"屲 ", "鶩 ", "w"},
		{"夕 ", "鑂 ", "x"}, {"丫 ", "韻 ", "y"}, {"帀", "咗 ", "z"},
	}
	require.Len(t, boundaries, 23)
	for i, expected := range want {
		require.Equal(t, []string{expected.first, expected.last, expected.initial}, boundaries[i][1:])
	}
}

func TestMySQLDamengFunGetPyWithoutBusinessTables(t *testing.T) {
	for _, status := range []string{"created", "reused", "invalid"} {
		t.Run(status, func(t *testing.T) {
			w, m := mockDM(t)
			for _, spec := range dmBusinessFunctions() {
				if spec.name != "FUN_GETPY" {
					expectDMFunctionExists(m, spec.name, 0)
					m.ExpectQuery("SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS").WithArgs("APP", spec.dependencies[0].table).
						WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME"}))
					continue
				}
				if status == "reused" {
					expectDMFunctionExists(m, spec.name, 1)
					continue
				}
				expectDMFunctionExists(m, spec.name, 0)
				m.ExpectExec(regexp.QuoteMeta(w.dmBusinessFunctionDDL(spec))).WillReturnResult(sqlmock.NewResult(0, 0))
				compile := "VALID"
				if status == "invalid" {
					compile = "INVALID"
				}
				m.ExpectQuery("SELECT STATUS FROM ALL_OBJECTS").WithArgs("APP", spec.name).
					WillReturnRows(sqlmock.NewRows([]string{"STATUS"}).AddRow(compile))
				if status == "invalid" {
					m.ExpectQuery("SELECT LINE,POSITION,TEXT FROM ALL_ERRORS").WithArgs("APP", spec.name).
						WillReturnRows(sqlmock.NewRows([]string{"LINE", "POSITION", "TEXT"}).AddRow(8, 20, "NLSSORT unavailable"))
				}
			}
			report, err := w.EnsureMySQLDamengBusinessFunctions(context.Background(), nil)
			require.Error(t, err) // Six unrelated dependency failures remain visible.
			require.Equal(t, 7, report.Total)
			item := dmFunctionResult(t, report, "FUN_GETPY")
			if status == "invalid" {
				require.Equal(t, 7, report.Failed)
				require.Equal(t, "failed", item.Status)
				require.Contains(t, item.Error, "8:20 NLSSORT unavailable")
				require.Contains(t, item.DDL, dmFunGetPyBody)
			} else {
				require.Equal(t, 6, report.Failed)
				require.Equal(t, status, item.Status)
				if status == "reused" {
					require.Empty(t, item.DDL)
					require.Equal(t, "已存在，跳过，未校验定义", item.DefinitionWarning)
				}
			}
		})
	}
}
