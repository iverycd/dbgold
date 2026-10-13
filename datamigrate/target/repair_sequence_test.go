package target

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestRecoverySequenceNeverRewindsPostgres(t *testing.T) {
	for _, tc := range []struct {
		name, step, last, extreme, want string
		called                          bool
	}{{"ahead", "1", "100", "50", "", true}, {"advance", "1", "5", "50", "51", true}, {"unused ahead", "1", "100", "50", "", false}, {"negative increment", "-2", "-5", "-50", "-51", true}} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := repairPGMock(t)
			m.ExpectQuery(regexp.QuoteMeta(`SELECT pg_get_serial_sequence($1,$2)`)).WithArgs(`"app"."t"`, "id").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow("app.seq_t_id"))
			m.ExpectQuery("SELECT COALESCE.*attidentity").WithArgs(`"app"."t"`, "id", "app.seq_t_id").WillReturnRows(sqlmock.NewRows([]string{"bound"}).AddRow(true))
			m.ExpectQuery("SELECT n.nspname,c.relname,s.increment").WithArgs("app.seq_t_id").WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "increment"}).AddRow("app", "seq_t_id", tc.step))
			m.ExpectQuery(regexp.QuoteMeta(`SELECT last_value,is_called FROM "app"."seq_t_id"`)).WillReturnRows(sqlmock.NewRows([]string{"last", "called"}).AddRow(tc.last, tc.called))
			agg := "MAX"
			if strings.HasPrefix(tc.step, "-") {
				agg = "MIN"
			}
			m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + agg + `("id") FROM "app"."t"`)).WillReturnRows(sqlmock.NewRows([]string{"extreme"}).AddRow(tc.extreme))
			if tc.want != "" {
				m.ExpectExec(regexp.QuoteMeta(`SELECT setval('"app"."seq_t_id"'::regclass, ` + tc.want + `, false)`)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			require.NoError(t, CalibrateRecovery(context.Background(), w, "t", []source.SequenceInfo{{TableName: "t", ColumnName: "id"}}))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestRecoverySequenceRejectsChangedPostgresDefault(t *testing.T) {
	w, m := repairPGMock(t)
	m.ExpectQuery("SELECT pg_get_serial_sequence").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow("app.original"))
	m.ExpectQuery("SELECT COALESCE.*attidentity").WithArgs(`"app"."t"`, "id", "app.original").WillReturnRows(sqlmock.NewRows([]string{"bound"}).AddRow(false))
	require.ErrorContains(t, CalibrateRecovery(context.Background(), w, "t", []source.SequenceInfo{{TableName: "t", ColumnName: "id"}}), "DEFAULT")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRecoverySequenceMissingBindingWaitsForObjectRepair(t *testing.T) {
	w, m := repairPGMock(t)
	m.ExpectQuery("SELECT pg_get_serial_sequence").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(nil))
	require.NoError(t, CalibrateRecovery(context.Background(), w, "t", []source.SequenceInfo{{TableName: "t", ColumnName: "id"}}))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRecoveryMySQLSequencePreservesLargeExistingValue(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	w := &MySQLWriter{db: db, schema: "app", dia: dialect.NewMySQL()}
	m.ExpectQuery("SELECT EXTRA").WithArgs("app", "t", "id").WillReturnRows(sqlmock.NewRows([]string{"extra"}).AddRow("auto_increment"))
	m.ExpectQuery("SELECT AUTO_INCREMENT").WithArgs("app", "t").WillReturnRows(sqlmock.NewRows([]string{"auto"}).AddRow("18446744073709551610"))
	m.ExpectQuery(regexp.QuoteMeta("SELECT MAX(`id`) FROM `app`.`t`")).WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow("9223372036854775810"))
	require.NoError(t, CalibrateRecovery(context.Background(), w, "t", []source.SequenceInfo{{TableName: "t", ColumnName: "id", StartValue: 1}}))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRecoverySQLServerNegativeIdentityUsesMinimum(t *testing.T) {
	w, m := sqlServerMock(t)
	m.ExpectQuery("SELECT CONVERT.*increment_value").WithArgs("[dbo].[T]", "Id").WillReturnRows(sqlmock.NewRows([]string{"inc", "seed", "last"}).AddRow("-2", "0", "-10"))
	m.ExpectQuery(regexp.QuoteMeta("SELECT MIN([Id]) FROM [dbo].[T]")).WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow("-30"))
	m.ExpectExec("SET ANSI_NULLS").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectBegin()
	m.ExpectExec(regexp.QuoteMeta("DBCC CHECKIDENT (N'[dbo].[T]', RESEED, -30) WITH NO_INFOMSGS")).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectCommit()
	m.ExpectClose()
	require.NoError(t, CalibrateRecovery(context.Background(), w, "T", []source.SequenceInfo{{TableName: "T", ColumnName: "Id", StartValue: -31}}))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRecoveryOracleSequenceAdvancesWithoutDrop(t *testing.T) {
	for _, last := range []string{"2", "101"} {
		t.Run("cache boundary "+last, func(t *testing.T) {
			w, m := oracleMockWriter(t)
			s := source.SequenceInfo{TableName: "T", ColumnName: "ID", StartValue: 1}
			seq, trig := w.dia.SequenceName(s), w.dia.TriggerName(s)
			m.ExpectQuery("SELECT OBJECT_TYPE FROM ALL_OBJECTS").WithArgs("APP", seq).WillReturnRows(sqlmock.NewRows([]string{"type"}).AddRow("SEQUENCE"))
			m.ExpectQuery("SELECT TABLE_OWNER,TABLE_NAME,TRIGGERING_EVENT,TRIGGER_TYPE").WithArgs("APP", trig).WillReturnRows(sqlmock.NewRows([]string{"owner", "table", "event", "kind"}).AddRow("APP", "T", "INSERT", "BEFORE EACH ROW"))
			m.ExpectQuery("SELECT TEXT FROM ALL_SOURCE").WithArgs("APP", trig).WillReturnRows(sqlmock.NewRows([]string{"text"}).AddRow("/* " + w.dia.SequenceMarker("APP", s) + " */ " + w.dia.QualifyTable("APP", seq) + ".NEXTVAL"))
			m.ExpectQuery("SELECT TO_CHAR.*FROM ALL_SEQUENCES").WithArgs("APP", seq).WillReturnRows(sqlmock.NewRows([]string{"inc", "last"}).AddRow("1", last))
			m.ExpectQuery(regexp.QuoteMeta(`SELECT MAX("ID") FROM "APP"."T"`)).WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow("40"))
			m.ExpectExec("DECLARE n NUMBER; jump NUMBER;").WillReturnResult(sqlmock.NewResult(0, 0))
			var executed string
			ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { executed = e.SQL; return nil })
			require.NoError(t, CalibrateRecovery(ctx, w, "T", []source.SequenceInfo{s}))
			require.NotContains(t, executed, "DROP ")
			require.NotContains(t, executed, "CREATE ")
			require.Contains(t, executed, "EXCEPTION WHEN OTHERS")
			require.Contains(t, executed, "INCREMENT BY 1")
			if last == "101" {
				require.Contains(t, executed, "IF n < 101 THEN")
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestOracleInvalidViewLeavesAnEditableFailedUnit(t *testing.T) {
	w, m := oracleMockWriter(t)
	view := source.ViewInfo{ViewName: "V", Definition: "SELECT BAD FROM T"}
	m.ExpectExec("CREATE OR REPLACE VIEW").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery("SELECT STATUS FROM ALL_OBJECTS").WithArgs("APP", "V", "VIEW").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("INVALID"))
	m.ExpectQuery("SELECT LINE, POSITION, TEXT FROM ALL_ERRORS").WithArgs("APP", "V", "VIEW").WillReturnRows(sqlmock.NewRows([]string{"line", "position", "text"}).AddRow(1, 10, "invalid identifier"))
	var events []repair.Event
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { events = append(events, e); return nil })
	require.ErrorContains(t, w.CreateView(ctx, view), "invalid identifier")
	require.Equal(t, "failed", events[len(events)-1].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRecoveryOracleOnlyAllowsExactGeneratedTrigger(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "generated", true: "modified body"}[custom], func(t *testing.T) {
			w, m := oracleMockWriter(t)
			s := source.SequenceInfo{TableName: "T", ColumnName: "ID", StartValue: 1}
			name := w.dia.TriggerName(s)
			m.ExpectQuery("SELECT TRIGGER_NAME FROM ALL_TRIGGERS").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"trigger"}).AddRow(name))
			m.ExpectQuery("SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"column"}).AddRow("ID"))
			text := w.dia.SequenceStatements("APP", s, "")[1].SQL
			if custom {
				text = strings.Replace(text, "END;", "DELETE FROM OTHER_TABLE; END;", 1)
			}
			m.ExpectQuery("SELECT TEXT FROM ALL_SOURCE").WithArgs("APP", name).WillReturnRows(sqlmock.NewRows([]string{"text"}).AddRow(text))
			if !custom {
				m.ExpectQuery("SELECT STATUS FROM ALL_OBJECTS").WithArgs("APP", name, "TRIGGER").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("VALID"))
			}
			ok, e := w.onlyManagedRecoveryTriggers(context.Background(), "T")
			require.NoError(t, e)
			require.Equal(t, !custom, ok)
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
