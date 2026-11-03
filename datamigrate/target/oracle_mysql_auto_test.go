package target

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func oracleAutoWriter(t *testing.T) (*MySQLWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); db.Close() })
	return &MySQLWriter{db: db, schema: "dst", srcType: "oracle", dia: dialect.NewMySQL()}, m
}
func expectAutoColumn(m sqlmock.Sqlmock, kind, extra string) {
	m.ExpectQuery("SELECT DATA_TYPE,COLUMN_TYPE,EXTRA,IS_NULLABLE").WithArgs("dst", "T", "Id").WillReturnRows(sqlmock.NewRows([]string{"kind", "type", "extra", "nullable"}).AddRow(kind, kind, extra, "NO"))
}
func expectAutoState(m sqlmock.Sqlmock, current string, maximum any) {
	expectAutoCounter(m, current)
	m.ExpectQuery(regexp.QuoteMeta("SELECT MAX(`Id`) FROM `dst`.`T`")).WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(maximum))
}

func expectAutoCounter(m sqlmock.Sqlmock, value string) {
	m.ExpectQuery("SELECT @@SESSION.information_schema_stats_expiry").WillReturnError(&mysql.MySQLError{Number: 1193, Message: "Unknown system variable"})
	m.ExpectQuery("SELECT AUTO_INCREMENT").WithArgs("dst", "T").WillReturnRows(sqlmock.NewRows([]string{"next"}).AddRow(value))
}

func TestOracleMySQLAutoCalibrationNeverRewinds(t *testing.T) {
	for _, tc := range []struct {
		name, start, current string
		maximum              any
		want                 string
	}{
		{"source ahead", "1000", "5", "4", "1000"},
		{"target data ahead", "10", "20", "9007199254740993", "9007199254740994"},
		{"target counter ahead", "10", "2000", "99", "2000"},
		{"empty", "1", "1", nil, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := oracleAutoWriter(t)
			s := source.SequenceInfo{TableName: "T", ColumnName: "Id", StartValueText: tc.start, OracleMySQL: &source.OracleMySQLAutoIncrement{}}
			expectAutoColumn(m, "bigint", "auto_increment")
			expectAutoState(m, tc.current, tc.maximum)
			q := "ALTER TABLE `dst`.`T` AUTO_INCREMENT = " + tc.want
			m.ExpectExec(regexp.QuoteMeta(q)).WillReturnResult(sqlmock.NewResult(0, 0))
			expectAutoCounter(m, tc.want)
			require.NoError(t, w.CreateSequence(context.Background(), s, ""))
		})
	}
}

func TestOracleMySQLAutoCalibrationRejectsInvalidTarget(t *testing.T) {
	for _, name := range []string{"missing", "not auto", "overflow", "verify"} {
		t.Run(name, func(t *testing.T) {
			w, m := oracleAutoWriter(t)
			s := source.SequenceInfo{TableName: "T", ColumnName: "Id", StartValueText: "100", OracleMySQL: &source.OracleMySQLAutoIncrement{}}
			switch name {
			case "missing":
				m.ExpectQuery("SELECT DATA_TYPE").WillReturnRows(sqlmock.NewRows([]string{"kind", "type", "extra", "nullable"}))
			case "not auto":
				expectAutoColumn(m, "bigint", "")
			case "overflow":
				expectAutoColumn(m, "bigint", "auto_increment")
				expectAutoState(m, "1", "9223372036854775807")
			case "verify":
				expectAutoColumn(m, "bigint", "auto_increment")
				expectAutoState(m, "1", "1")
				m.ExpectExec("ALTER TABLE").WillReturnResult(sqlmock.NewResult(0, 0))
				expectAutoCounter(m, "1")
			}
			require.Error(t, w.CreateSequence(context.Background(), s, ""))
		})
	}
}

func TestOracleMySQLAutoImportPinsAndRestoresZeroMode(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "rollback"}[fail], func(t *testing.T) {
			w, m := oracleAutoWriter(t)
			m.ExpectQuery("SELECT @@SESSION.sql_mode").WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("ANSI_QUOTES"))
			m.ExpectExec("SET SESSION sql_mode=\\?").WithArgs("ANSI_QUOTES,STRICT_ALL_TABLES,NO_AUTO_VALUE_ON_ZERO").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectBegin()
			stmt := m.ExpectPrepare(regexp.QuoteMeta("INSERT INTO `dst`.`T` (`Id`) VALUES (?)"))
			stmt.ExpectExec().WithArgs("0").WillReturnResult(sqlmock.NewResult(0, 1))
			if fail {
				stmt.ExpectExec().WithArgs("9007199254740993").WillReturnError(errors.New("write failed"))
				m.ExpectRollback()
			} else {
				stmt.ExpectExec().WithArgs("9007199254740993").WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectCommit()
			}
			m.ExpectExec("SET SESSION sql_mode=\\?").WithArgs("ANSI_QUOTES").WillReturnResult(sqlmock.NewResult(0, 0))
			err := w.CopyData(context.Background(), "T", []string{"Id"}, []string{"NUMBER"}, [][]any{{"0"}, {"9007199254740993"}})
			if fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestOracleMySQLAutoRepairRefreshesSavedStart(t *testing.T) {
	w, m := oracleAutoWriter(t)
	s := source.SequenceInfo{TableName: "T", ColumnName: "Id", StartValueText: "10", OracleMySQL: &source.OracleMySQLAutoIncrement{}}
	expectAutoColumn(m, "bigint", "auto_increment")
	expectAutoState(m, "400", "999")
	m.ExpectExec(regexp.QuoteMeta("ALTER TABLE `dst`.`T` AUTO_INCREMENT = 1000")).WillReturnResult(sqlmock.NewResult(0, 0))
	expectAutoCounter(m, "1000")
	units, err := ExecuteRepair(context.Background(), w, repair.Object{Kind: "sequence", Schema: "dst", Sequence: &s}, []repair.Unit{{SQL: "ALTER TABLE `dst`.`T` AUTO_INCREMENT = 10", Selected: true}}, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", units[0].Status)
	require.Contains(t, units[0].SQL, "1000")
}

func TestOracleMySQLAutoCounterBypassesMySQL8CacheAndRestoresSession(t *testing.T) {
	w, m := oracleAutoWriter(t)
	m.ExpectQuery("SELECT @@SESSION.information_schema_stats_expiry").WillReturnRows(sqlmock.NewRows([]string{"ttl"}).AddRow("86400"))
	m.ExpectExec("SET SESSION information_schema_stats_expiry=0").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery("SELECT AUTO_INCREMENT").WithArgs("dst", "T").WillReturnRows(sqlmock.NewRows([]string{"next"}).AddRow("9007199254742000"))
	m.ExpectExec("SET SESSION information_schema_stats_expiry=\\?").WithArgs("86400").WillReturnResult(sqlmock.NewResult(0, 0))
	n, err := w.oracleMySQLAutoCounter(context.Background(), "dst", "T")
	require.NoError(t, err)
	require.Equal(t, "9007199254742000", n.String())
}

func TestOracleMySQLAutoCalibrationCancellationDoesNotWrite(t *testing.T) {
	w, _ := oracleAutoWriter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := w.PrepareOracleMySQLAutoIncrement(ctx, source.SequenceInfo{TableName: "T", ColumnName: "Id", StartValueText: "1"})
	require.ErrorIs(t, err, context.Canceled)
}

func TestOracleMySQLAutoRepairVerificationChecksCurrentCounter(t *testing.T) {
	for _, current := range []string{"100", "1000"} {
		t.Run(current, func(t *testing.T) {
			w, m := oracleAutoWriter(t)
			s := source.SequenceInfo{TableName: "T", ColumnName: "Id", StartValueText: "1000", OracleMySQL: &source.OracleMySQLAutoIncrement{}}
			expectAutoColumn(m, "bigint", "auto_increment")
			expectAutoState(m, current, "99")
			expectAutoCounter(m, current)
			v, err := InspectRepair(context.Background(), w, repair.Object{Kind: "sequence", Schema: "dst", Sequence: &s})
			require.NoError(t, err)
			if current == "1000" {
				require.Equal(t, "succeeded", v.Status)
			} else {
				require.Equal(t, "failed", v.Status)
			}
		})
	}
}
