package target

import (
	"context"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/sijms/go-ora/v2/converters"
	"github.com/stretchr/testify/require"
)

func TestPGOracleGeneratedStatementPreflightNeverExecutes(t *testing.T) {
	w, mock := oracleMockWriter(t)
	w.codec = converters.NewStringConverter(1)
	require.NoError(t, w.ValidatePGOracleStatements(context.Background(), []dialect.Statement{{SQL: `CREATE TABLE "T" ("V" NVARCHAR2(10) DEFAULT UNISTR('\4E2D'))`}}))
	require.ErrorContains(t, w.ValidatePGOracleStatements(context.Background(), []dialect.Statement{{SQL: `CREATE TABLE "T" ("V" VARCHAR2(10) DEFAULT '中')`}}), "目标字符集")
	require.Error(t, w.ValidatePGOracleStatements(context.Background(), []dialect.Statement{{SQL: "  "}}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, w.ValidatePGOracleStatements(ctx, []dialect.Statement{{SQL: "SELECT 1 FROM DUAL"}}), context.Canceled)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOracleNamesUseTargetEncodedBytes(t *testing.T) {
	w, mock := oracleMockWriter(t)
	w.codec = converters.NewStringConverter(852) // ZHS16GBK: two bytes per Han rune.
	name := strings.Repeat("中", 15)
	require.NoError(t, w.ValidatePGOracleName(name))
	require.Error(t, w.ValidatePGOracleName(name+"中"))
	require.Error(t, w.ValidatePGOracleName("😀"))
	require.NoError(t, w.ValidatePGOracleStatements(context.Background(), []dialect.Statement{{SQL: `CREATE TABLE "` + name + `" ("ID" NUMBER)`}}))
	w.codec = converters.NewStringConverter(873) // AL32UTF8: three bytes per Han rune.
	require.ErrorContains(t, w.ValidatePGOracleName(strings.Repeat("中", 11)), "30 字节")
	require.ErrorContains(t, w.ValidatePGOracleStatements(context.Background(), []dialect.Statement{{SQL: `CREATE TABLE "T" ("` + strings.Repeat("中", 11) + `" NUMBER)`}}), "30 字节")
	// SQL literals and comments are not object names. Generated sequence blocks
	// contain division and :=, which the expression grammar need not accept.
	sql := `DECLARE n NUMBER := 1; BEGIN n := FLOOR((n-1)/(2)); /* "` + strings.Repeat("X", 31) + `" */ EXECUTE IMMEDIATE 'COMMENT ON TABLE "T" IS ''` + strings.Repeat("X", 31) + `'''; END;`
	require.NoError(t, w.ValidatePGOracleStatements(context.Background(), []dialect.Statement{{SQL: sql}}))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOracleProfileAndRelationInspection(t *testing.T) {
	w, m := oracleMockWriter(t)
	m.ExpectQuery("SELECT VERSION FROM PRODUCT_COMPONENT_VERSION").WillReturnRows(sqlmock.NewRows([]string{"VERSION"}).AddRow("11.2.0.4.0"))
	m.ExpectQuery("SELECT PARAMETER,VALUE FROM NLS_DATABASE_PARAMETERS").WillReturnRows(sqlmock.NewRows([]string{"PARAMETER", "VALUE"}).AddRow("NLS_CHARACTERSET", "AL32UTF8").AddRow("NLS_NCHAR_CHARACTERSET", "AL16UTF16"))
	p, err := w.InspectPGOracle(context.Background())
	require.NoError(t, err)
	require.Equal(t, "11.2.0.4.0", p.Version)
	require.Equal(t, "AL32UTF8", p.Charset)
	require.Contains(t, p.Verification, "未完成实库验证")
	m.ExpectQuery("SELECT COUNT\\(\\*\\) FROM ALL_OBJECTS").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"COUNT"}).AddRow(1))
	ok, err := w.PGOracleRelationExists(context.Background(), "T")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = w.PGOracleRelationExists(context.Background(), strings.Repeat("X", 31))
	require.Error(t, err)
	require.NoError(t, m.ExpectationsWereMet())
	w, m = oracleMockWriter(t)
	m.ExpectQuery("SELECT VERSION FROM PRODUCT_COMPONENT_VERSION").WillReturnRows(sqlmock.NewRows([]string{"VERSION"}).AddRow("10.2.0.5.0"))
	_, err = w.InspectPGOracle(context.Background())
	require.ErrorContains(t, err, "11g")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPGOracleDataPreflightRefreshesTargetAndChecksExtraColumns(t *testing.T) {
	for _, extra := range []string{"optional", "required", "virtual"} {
		t.Run(extra, func(t *testing.T) {
			w, m := oracleMockWriter(t)
			w.columns["T"] = map[string]valueconv.OracleColumn{"OLD": {DataType: "VARCHAR2"}}
			m.ExpectQuery("SELECT COUNT\\(\\*\\) FROM ALL_TABLES").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"COUNT"}).AddRow(1))
			m.ExpectQuery("SELECT COLUMN_NAME, DATA_TYPE").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "NULLABLE", "DATA_PRECISION", "DATA_SCALE", "DATA_LENGTH", "CHAR_LENGTH", "CHAR_USED"}).AddRow("ID", "NUMBER", "N", 10, 0, 22, nil, nil))
			rows := sqlmock.NewRows([]string{"COLUMN_NAME", "NULLABLE", "DEFAULT_LENGTH", "VIRTUAL_COLUMN"})
			switch extra {
			case "optional":
				rows.AddRow("ID", "N", nil, "NO").AddRow("EXTRA", "Y", nil, "NO")
			case "required":
				rows.AddRow("ID", "N", nil, "NO").AddRow("EXTRA", "N", nil, "NO")
			case "virtual":
				rows.AddRow("ID", "N", nil, "YES")
			}
			m.ExpectQuery("SELECT COLUMN_NAME,NULLABLE,DEFAULT_LENGTH,VIRTUAL_COLUMN").WithArgs("APP", "T").WillReturnRows(rows)
			if extra == "optional" {
				m.ExpectQuery("SELECT OWNER,TRIGGER_NAME FROM ALL_TRIGGERS").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"OWNER", "TRIGGER_NAME"}))
			}
			err := w.ValidatePGOracleDataTable(context.Background(), "T", []source.ColumnInfo{{Name: "id", DataType: "int4"}}, nil)
			if extra == "optional" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestPGOracleExistingColumnNeverNarrowsSourceDomain(t *testing.T) {
	w, _ := oracleMockWriter(t)
	for _, tc := range []struct {
		src  source.ColumnInfo
		dst  valueconv.OracleColumn
		good bool
	}{
		{source.ColumnInfo{DataType: "int8"}, valueconv.OracleColumn{DataType: "NUMBER", Precision: 19}, true},
		{source.ColumnInfo{DataType: "int8"}, valueconv.OracleColumn{DataType: "NUMBER", Precision: 18}, false},
		{source.ColumnInfo{DataType: "numeric"}, valueconv.OracleColumn{DataType: "NUMBER"}, true},
		{source.ColumnInfo{DataType: "numeric"}, valueconv.OracleColumn{DataType: "NUMBER", ScaleKnown: true}, false},
		{source.ColumnInfo{DataType: "numeric", Precision: 10, Scale: 2}, valueconv.OracleColumn{DataType: "NUMBER", Precision: 11, Scale: 3}, true},
		{source.ColumnInfo{DataType: "numeric", Precision: 10, Scale: 2}, valueconv.OracleColumn{DataType: "NUMBER", Precision: 10, Scale: 3}, false},
		{source.ColumnInfo{DataType: "varchar", Length: 5}, valueconv.OracleColumn{DataType: "VARCHAR2", CharLength: 5, DataLength: 20}, true},
		{source.ColumnInfo{DataType: "varchar", Length: 5}, valueconv.OracleColumn{DataType: "VARCHAR2", CharLength: 5, DataLength: 5}, false},
		{source.ColumnInfo{DataType: "bpchar", Length: 5}, valueconv.OracleColumn{DataType: "CHAR", CharLength: 5, DataLength: 20}, true},
		{source.ColumnInfo{DataType: "bpchar", Length: 5}, valueconv.OracleColumn{DataType: "CHAR", CharLength: 6, DataLength: 24}, false},
		{source.ColumnInfo{DataType: "timestamptz", DateTimePrecision: 6}, valueconv.OracleColumn{DataType: "TIMESTAMP(6) WITH TIME ZONE", Scale: 6}, true},
		{source.ColumnInfo{DataType: "timestamptz", DateTimePrecision: 6}, valueconv.OracleColumn{DataType: "TIMESTAMP(6)", Scale: 6}, false},
		{source.ColumnInfo{DataType: "int4", IsNullable: true}, valueconv.OracleColumn{DataType: "NUMBER", Precision: 10}, false},
	} {
		err := w.pgOracleCompatibleColumn(tc.src, tc.dst)
		if tc.good {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
}

func TestPGOracleOnlyExactValidManagedTriggersMayRun(t *testing.T) {
	expected := `CREATE OR REPLACE TRIGGER "APP"."TRG"` + "\n" + `BEFORE INSERT ON "APP"."T" FOR EACH ROW BEGIN NULL; END;`
	for _, state := range []string{"exact", "drift", "foreign", "invalid"} {
		t.Run(state, func(t *testing.T) {
			w, m := oracleMockWriter(t)
			owner := "APP"
			if state == "foreign" {
				owner = "OTHER"
			}
			m.ExpectQuery("SELECT OWNER,TRIGGER_NAME FROM ALL_TRIGGERS").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"OWNER", "TRIGGER_NAME"}).AddRow(owner, "TRG"))
			if state != "foreign" {
				body := strings.Replace(strings.TrimPrefix(expected, "CREATE OR REPLACE "), `"APP"."TRG"`, `"TRG"`, 1)
				if state == "drift" {
					body = strings.Replace(body, "NULL;", "DELETE FROM AUDIT;", 1)
				}
				m.ExpectQuery("SELECT TEXT FROM ALL_SOURCE").WithArgs("APP", "TRG").WillReturnRows(sqlmock.NewRows([]string{"TEXT"}).AddRow(body))
				if state != "drift" {
					status := "VALID"
					if state == "invalid" {
						status = "INVALID"
					}
					m.ExpectQuery("SELECT STATUS FROM ALL_OBJECTS").WithArgs("APP", "TRG", "TRIGGER").WillReturnRows(sqlmock.NewRows([]string{"STATUS"}).AddRow(status))
					if state == "invalid" {
						m.ExpectQuery("SELECT LINE, POSITION, TEXT FROM ALL_ERRORS").WithArgs("APP", "TRG", "TRIGGER").WillReturnRows(sqlmock.NewRows([]string{"LINE", "POSITION", "TEXT"}).AddRow(1, 1, "compile error"))
					}
				}
			}
			err := w.ValidatePGOracleTriggers(context.Background(), "T", map[string]string{"TRG": expected})
			if state == "exact" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestPGOracleDDLRequiresCompilationSuccess(t *testing.T) {
	w, m := oracleMockWriter(t)
	m.ExpectExec("CREATE OR REPLACE VIEW").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery("SELECT STATUS FROM ALL_OBJECTS").WithArgs("APP", "V", "VIEW").WillReturnRows(sqlmock.NewRows([]string{"STATUS"}).AddRow("INVALID"))
	m.ExpectQuery("SELECT LINE, POSITION, TEXT FROM ALL_ERRORS").WithArgs("APP", "V", "VIEW").WillReturnRows(sqlmock.NewRows([]string{"LINE", "POSITION", "TEXT"}).AddRow(1, 4, "missing relation"))
	err := w.ExecutePGOracleStatements(context.Background(), "view", "V", []dialect.Statement{{SQL: `CREATE OR REPLACE VIEW "APP"."V" AS SELECT * FROM "APP"."T"`}})
	require.ErrorContains(t, err, "missing relation")
	require.NoError(t, m.ExpectationsWereMet())
	require.Error(t, w.ExecutePGOracleStatements(context.Background(), "view", "V", nil))
}
