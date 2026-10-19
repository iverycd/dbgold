package target

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func oracleDMRecoveryMock(t *testing.T) (*DaMengWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return &DaMengWriter{db: db, schema: "APP", srcType: "oracle", dia: dialect.NewDaMeng()}, m
}
func oracleDMNativeSequence() source.SequenceInfo {
	return source.SequenceInfo{TableName: "T", ColumnName: "ID", StartValueText: "1", Native: &source.NativeSequenceInfo{Name: "S", IncrementBy: "5", MinValue: "1", MaxValue: "1000", CacheSize: "0", Bindings: []source.SequenceBinding{{TableName: "T", ColumnName: "ID", TriggerName: "TRG", Kind: "trigger", OnlyWhenNull: true}}}}
}
func nativeTriggerRows(seq source.SequenceInfo) *sqlmock.Rows {
	ss := dialect.NewDaMeng().SequenceStatements("APP", seq, "")
	body := ss[1].SQL[strings.Index(ss[1].SQL, "\nBEGIN")+1:]
	return sqlmock.NewRows([]string{"owner", "name", "kind", "event", "when", "body"}).AddRow("APP", "TRG", "BEFORE EACH ROW", "INSERT", nil, body)
}

func TestOracleDamengNativeCalibrationNeverRewinds(t *testing.T) {
	for _, tc := range []struct{ current, extreme, want string }{{"51", "40", ""}, {"11", "42", "46"}, {"6", "10", "11"}} {
		t.Run(tc.current+"/"+tc.extreme, func(t *testing.T) {
			w, m := oracleDMRecoveryMock(t)
			seq := oracleDMNativeSequence()
			m.ExpectQuery("SELECT INCREMENT_BY,MIN_VALUE,MAX_VALUE,LAST_NUMBER,CYCLE_FLAG").WithArgs("APP", "S").WillReturnRows(sqlmock.NewRows([]string{"step", "min", "max", "next", "cycle"}).AddRow("5", "1", "1000", tc.current, "N"))
			m.ExpectQuery("SELECT OWNER,TRIGGER_NAME").WithArgs("APP", "T").WillReturnRows(nativeTriggerRows(seq))
			m.ExpectQuery(regexp.QuoteMeta(`SELECT MAX("ID") FROM "APP"."T"`)).WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(tc.extreme))
			if tc.want != "" {
				m.ExpectExec(regexp.QuoteMeta(`ALTER SEQUENCE "APP"."S" CURRENT VALUE ` + tc.want)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			require.NoError(t, w.CalibrateOracleDameng(context.Background(), seq))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestOracleDamengNativeCalibrationNegativeStepAndOverflow(t *testing.T) {
	for _, bound := range []string{"-42", "-1001"} {
		t.Run(bound, func(t *testing.T) {
			w, m := oracleDMRecoveryMock(t)
			seq := oracleDMNativeSequence()
			seq.Native.IncrementBy = "-5"
			seq.Native.MinValue = "-1000"
			seq.Native.MaxValue = "-1"
			seq.StartValueText = "-1"
			m.ExpectQuery("SELECT INCREMENT_BY,MIN_VALUE,MAX_VALUE,LAST_NUMBER,CYCLE_FLAG").WillReturnRows(sqlmock.NewRows([]string{"step", "min", "max", "next", "cycle"}).AddRow("-5", "-1000", "-1", "-11", "N"))
			m.ExpectQuery("SELECT OWNER,TRIGGER_NAME").WillReturnRows(nativeTriggerRows(seq))
			m.ExpectQuery(regexp.QuoteMeta(`SELECT MIN("ID") FROM "APP"."T"`)).WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(bound))
			if bound == "-42" {
				m.ExpectExec(regexp.QuoteMeta(`ALTER SEQUENCE "APP"."S" CURRENT VALUE -46`)).WillReturnResult(sqlmock.NewResult(0, 0))
				require.NoError(t, w.CalibrateOracleDameng(context.Background(), seq))
			} else {
				require.ErrorContains(t, w.CalibrateOracleDameng(context.Background(), seq), "边界")
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestOracleDamengOnlyDataRejectsTriggerSideEffects(t *testing.T) {
	for _, kind := range []string{"null-only", "unconditional", "nullable", "tampered", "custom"} {
		t.Run(kind, func(t *testing.T) {
			w, m := oracleDMRecoveryMock(t)
			seq := oracleDMNativeSequence()
			info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "ID", DataType: "NUMBER"}}}
			if kind == "unconditional" {
				seq.Native.Bindings[0].OnlyWhenNull = false
			}
			if kind == "nullable" {
				info.Columns[0].IsNullable = true
			}
			rows := nativeTriggerRows(seq)
			if kind == "tampered" {
				ss := w.dia.SequenceStatements("APP", seq, "")
				body := ss[1].SQL[strings.Index(ss[1].SQL, "\nBEGIN")+1:]
				body = strings.Replace(body, "END;", "INSERT INTO AUDIT_LOG VALUES(1); END;", 1)
				rows = sqlmock.NewRows([]string{"owner", "name", "kind", "event", "when", "body"}).AddRow("APP", "TRG", "BEFORE EACH ROW", "INSERT", nil, body)
			}
			if kind == "custom" {
				rows = sqlmock.NewRows([]string{"owner", "name", "kind", "event", "when", "body"}).AddRow("APP", "CUSTOM", "BEFORE EACH ROW", "INSERT", nil, "BEGIN NULL; END;")
			}
			m.ExpectQuery("SELECT OWNER,TRIGGER_NAME").WithArgs("APP", "T").WillReturnRows(rows)
			err := w.ValidateOracleDamengLoadTriggers(context.Background(), info)
			if kind == "null-only" {
				require.NoError(t, err)
			} else {
				require.Error(t, err, fmt.Sprint(kind))
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
