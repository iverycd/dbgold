package target

import (
	"context"
	"regexp"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func oracleDMDefaultBoundSequence(descending bool) source.SequenceInfo {
	s := oracleDMNativeSequence()
	s.Native.MaxValue = "9999999999999999999999999999"
	if descending {
		s.StartValueText = "-1"
		s.Native.IncrementBy = "-5"
		s.Native.MinValue = "-999999999999999999999999999"
		s.Native.MaxValue = "-1"
	}
	return s
}

func oracleDMSequenceSnapshot(s source.SequenceInfo) source.SequenceInfo {
	n := *s.Native
	n.Bindings = append([]source.SequenceBinding(nil), n.Bindings...)
	s.Native = &n
	return s
}

func TestOracleDamengCreateUsesNormalizedDefaultBounds(t *testing.T) {
	for _, descending := range []bool{false, true} {
		name, start, step, lo, hi := "ascending", "1", "5", "1", "9223372036854775807"
		if descending {
			name, start, step, lo, hi = "descending", "-1", "-5", "-9223372036854775808", "-1"
		}
		t.Run(name, func(t *testing.T) {
			w, m := oracleDMRecoveryMock(t)
			s := oracleDMDefaultBoundSequence(descending)
			original := oracleDMSequenceSnapshot(s)
			expectOracleDMSequenceState(t, m, s, "", []bool{false})
			aggregate := "MAX"
			if descending {
				aggregate = "MIN"
			}
			expectOracleDMSequenceExtreme(m, "T", "ID", aggregate, nil)
			ddl := `CREATE SEQUENCE "APP"."S" START WITH ` + start + " INCREMENT BY " + step + " MINVALUE " + lo + " MAXVALUE " + hi + " NOCYCLE NOCACHE NOORDER"
			m.ExpectExec(regexp.QuoteMeta(ddl)).WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectExec(regexp.QuoteMeta(`CREATE TRIGGER "APP"."TRG" BEFORE INSERT ON "APP"."T"`)).WillReturnResult(sqlmock.NewResult(0, 0))
			require.NoError(t, w.CreateSequence(context.Background(), s, ""))
			require.Equal(t, original, s, "writer must preserve source metadata")
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestOracleDamengCalibrationUsesNormalizedBoundsWithoutRewinding(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		for _, tc := range []struct {
			name                           string
			descending                     bool
			current, extreme, next, lo, hi string
		}{
			{"ascending-advance", false, "11", "42", "46", "1", "9223372036854775807"},
			{"ascending-ahead", false, "51", "40", "", "1", "9223372036854775807"},
			{"descending-advance", true, "-11", "-42", "-46", "-9223372036854775808", "-1"},
			{"descending-ahead", true, "-51", "-40", "", "-9223372036854775808", "-1"},
		} {
			prefix := "direct/"
			if recovery {
				prefix = "recovery/"
			}
			t.Run(prefix+tc.name, func(t *testing.T) {
				w, m := oracleDMRecoveryMock(t)
				s := oracleDMDefaultBoundSequence(tc.descending)
				original := oracleDMSequenceSnapshot(s)
				expectOracleDMSequenceState(t, m, s, tc.current, []bool{true})
				agg := "MAX"
				if tc.descending {
					agg = "MIN"
				}
				m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + agg + `("ID") FROM "APP"."T"`)).WillReturnRows(sqlmock.NewRows([]string{"extreme"}).AddRow(tc.extreme))
				if tc.next != "" {
					m.ExpectExec(regexp.QuoteMeta(`ALTER SEQUENCE "APP"."S" CURRENT VALUE ` + tc.next)).WillReturnResult(sqlmock.NewResult(0, 0))
				}
				var err error
				if recovery {
					err = CalibrateRecovery(context.Background(), w, "T", []source.SequenceInfo{s})
				} else {
					err = w.CalibrateOracleDameng(context.Background(), s)
				}
				require.NoError(t, err)
				require.Equal(t, original, s, "calibration must preserve source metadata")
				require.NoError(t, m.ExpectationsWereMet())
			})
		}
	}
}

func TestOracleDamengSequenceInvalidActualValueFailsBeforeDB(t *testing.T) {
	for _, operation := range []string{"create", "calibrate"} {
		t.Run(operation, func(t *testing.T) {
			w, m := oracleDMRecoveryMock(t)
			s := oracleDMDefaultBoundSequence(false)
			s.StartValueText = "9999999999999999999999999998"
			original := oracleDMSequenceSnapshot(s)
			var err error
			if operation == "create" {
				err = w.CreateSequence(context.Background(), s, "")
			} else {
				err = w.CalibrateOracleDameng(context.Background(), s)
			}
			require.ErrorContains(t, err, "int64")
			require.Equal(t, original, s)
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestOracleDamengCalibrationRejectsFiniteBoundaryMismatch(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	expectOracleDMSequenceObject(m, "S", "SEQUENCE")
	m.ExpectQuery("SELECT INCREMENT_BY,MIN_VALUE,MAX_VALUE,LAST_NUMBER,CYCLE_FLAG,CACHE_SIZE,ORDER_FLAG").WithArgs("APP", "S").WillReturnRows(sqlmock.NewRows([]string{"step", "min", "max", "current", "cycle", "cache", "ordered"}).AddRow("5", "1", "9223372036854775807", "11", "N", "0", "N"))
	require.ErrorContains(t, w.CalibrateOracleDameng(context.Background(), s), "边界")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengCalibrationExhaustionFailsBeforeAlter(t *testing.T) {
	for _, descending := range []bool{false, true} {
		name, current, extreme, agg := "ascending", "1", "9223372036854775807", "MAX"
		if descending {
			name, current, extreme, agg = "descending", "-1", "-9223372036854775808", "MIN"
		}
		t.Run(name, func(t *testing.T) {
			w, m := oracleDMRecoveryMock(t)
			s := oracleDMDefaultBoundSequence(descending)
			expectOracleDMSequenceState(t, m, s, current, []bool{true})
			m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + agg + `("ID") FROM "APP"."T"`)).WillReturnRows(sqlmock.NewRows([]string{"extreme"}).AddRow(extreme))
			require.ErrorContains(t, w.CalibrateOracleDameng(context.Background(), s), "边界")
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
