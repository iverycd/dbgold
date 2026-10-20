package target

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func expectOracleDMSequenceObject(m sqlmock.Sqlmock, name, kind string) {
	rows := sqlmock.NewRows([]string{"kind"})
	if kind != "" {
		rows.AddRow(kind)
	}
	m.ExpectQuery("SELECT OBJECT_TYPE FROM ALL_OBJECTS").WithArgs("APP", strings.ToUpper(name)).WillReturnRows(rows)
}

func oracleDMSequenceTriggerRows(seq source.SequenceInfo, i int) *sqlmock.Rows {
	ss := dialect.NewDaMeng().SequenceStatements("APP", seq, "")
	b := seq.Native.Bindings[i]
	name := dialect.OracleDamengTriggerName(seq, b)
	description := name + " BEFORE INSERT ON " + strings.ToUpper(b.TableName) + " FOR EACH ROW"
	fullSQL := strings.Replace(ss[i+1].SQL, "CREATE TRIGGER ", "CREATE OR REPLACE  TRIGGER ", 1)
	return sqlmock.NewRows([]string{"owner", "name", "table_owner", "table", "timing", "event", "status", "description", "body"}).AddRow("APP", name, "APP", strings.ToUpper(b.TableName), "BEFORE ROW", "INSERT", "Y", description, fullSQL)
}

func expectOracleDMSequenceState(t *testing.T, m sqlmock.Sqlmock, seq source.SequenceInfo, current string, existingTriggers []bool) {
	t.Helper()
	prepared, err := dialect.NormalizeOracleDamengSequence(seq)
	require.NoError(t, err)
	n := prepared.Native
	kind := ""
	if current != "" {
		kind = "SEQUENCE"
	}
	expectOracleDMSequenceObject(m, n.Name, kind)
	if current != "" {
		ordered := "N"
		if n.Ordered {
			ordered = "Y"
		}
		m.ExpectQuery("SELECT INCREMENT_BY,MIN_VALUE,MAX_VALUE,LAST_NUMBER,CYCLE_FLAG,CACHE_SIZE,ORDER_FLAG").WithArgs("APP", strings.ToUpper(n.Name)).WillReturnRows(sqlmock.NewRows([]string{"step", "min", "max", "current", "cycle", "cache", "ordered"}).AddRow(n.IncrementBy, n.MinValue, n.MaxValue, current, "N", n.CacheSize, ordered))
	}
	for i, b := range n.Bindings {
		kind = ""
		if existingTriggers[i] {
			kind = "TRIGGER"
		}
		expectOracleDMSequenceObject(m, dialect.OracleDamengTriggerName(prepared, b), kind)
		if existingTriggers[i] {
			m.ExpectQuery("SELECT OWNER,TRIGGER_NAME,TABLE_OWNER,TABLE_NAME,TRIGGERING_TYPE,TRIGGERING_EVENT,STATUS,DESCRIPTION,TRIGGER_BODY").WithArgs("APP", dialect.OracleDamengTriggerName(prepared, b)).WillReturnRows(oracleDMSequenceTriggerRows(prepared, i))
		}
	}
}

func expectOracleDMSequenceExtreme(m sqlmock.Sqlmock, table, column, aggregate string, extreme any) {
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + aggregate + `("` + column + `") FROM "APP"."` + table + `"`)).WillReturnRows(sqlmock.NewRows([]string{"extreme"}).AddRow(extreme))
}

func TestOracleDamengPlanReusesHandmadeSequenceAndRestoresBinding(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	before := oracleDMSequenceSnapshot(s)
	expectOracleDMSequenceState(t, m, s, "2", []bool{false})
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "42")
	m.ExpectExec(regexp.QuoteMeta(`ALTER SEQUENCE "APP"."S" CURRENT VALUE 47`)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(regexp.QuoteMeta(`CREATE TRIGGER "APP"."TRG" BEFORE INSERT ON "APP"."T"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	var reused []string
	ctx := repair.WithObserver(context.Background(), func(event repair.Event) error {
		if event.Status == "reused" {
			reused = append(reused, event.SQL)
		}
		return nil
	})
	require.NoError(t, w.CreateSequence(ctx, s, ""))
	require.Equal(t, []string{w.dia.SequenceStatements("APP", s, "")[0].SQL}, reused)
	require.Equal(t, before, s)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengPlanIsReadOnlyAndIdempotent(t *testing.T) {
	for _, apply := range []bool{false, true} {
		w, m := oracleDMRecoveryMock(t)
		s := oracleDMNativeSequence()
		expectOracleDMSequenceState(t, m, s, "51", []bool{true})
		expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "40")
		var events []repair.Event
		ctx := repair.WithObserver(context.Background(), func(event repair.Event) error {
			events = append(events, event)
			return nil
		})
		if apply {
			require.NoError(t, w.CreateSequence(ctx, s, ""))
			require.Len(t, events, 2)
			for _, event := range events {
				require.Equal(t, "reused", event.Status)
			}
		} else {
			statements, err := w.PlanOracleDamengSequence(ctx, s)
			require.NoError(t, err)
			require.Empty(t, statements)
			require.Empty(t, events, "planning must not emit execution events")
		}
		require.NoError(t, m.ExpectationsWereMet())
	}
}

func TestOracleDamengPlanReadsEverySharedBindingBeforeWrites(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	s.Native.Bindings = append(s.Native.Bindings, source.SequenceBinding{TableName: "T2", ColumnName: "ID2", TriggerName: "TRG2", Kind: "trigger", OnlyWhenNull: true})
	expectOracleDMSequenceState(t, m, s, "2", []bool{true, false})
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "40")
	expectOracleDMSequenceExtreme(m, "T2", "ID2", "MAX", "112")
	statements, err := w.PlanOracleDamengSequence(context.Background(), s)
	require.NoError(t, err)
	require.Len(t, statements, 2)
	require.Equal(t, `ALTER SEQUENCE "APP"."S" CURRENT VALUE 117`, statements[0].SQL)
	require.Contains(t, statements[1].SQL, `CREATE TRIGGER "APP"."TRG2"`)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengNewSequenceStartsBeyondTargetData(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	expectOracleDMSequenceState(t, m, s, "", []bool{false})
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "42")
	statements, err := w.PlanOracleDamengSequence(context.Background(), s)
	require.NoError(t, err)
	require.Len(t, statements, 2)
	require.Equal(t, `CREATE SEQUENCE "APP"."S" START WITH 46 INCREMENT BY 5 MINVALUE 1 MAXVALUE 1000 NOCYCLE NOCACHE NOORDER`, statements[0].SQL)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengPlanRejectsMismatchedSequencePropertiesBeforeWrites(t *testing.T) {
	for _, tc := range []struct {
		name, step, lo, hi, current, cycle, cache, ordered string
	}{
		{"increment", "6", "1", "1000", "1", "N", "0", "N"},
		{"minimum", "5", "0", "1000", "1", "N", "0", "N"},
		{"maximum", "5", "1", "1001", "1", "N", "0", "N"},
		{"cache", "5", "1", "1000", "1", "N", "20", "N"},
		{"order", "5", "1", "1000", "1", "N", "0", "Y"},
		{"cycle", "5", "1", "1000", "1", "Y", "0", "N"},
		{"current", "5", "1", "1000", "1001", "N", "0", "N"},
		{"invalid-current", "5", "1", "1000", "bad", "N", "0", "N"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := oracleDMRecoveryMock(t)
			expectOracleDMSequenceObject(m, "S", "SEQUENCE")
			m.ExpectQuery("SELECT INCREMENT_BY,MIN_VALUE,MAX_VALUE,LAST_NUMBER,CYCLE_FLAG,CACHE_SIZE,ORDER_FLAG").WithArgs("APP", "S").WillReturnRows(sqlmock.NewRows([]string{"step", "min", "max", "current", "cycle", "cache", "ordered"}).AddRow(tc.step, tc.lo, tc.hi, tc.current, tc.cycle, tc.cache, tc.ordered))
			err := w.CreateSequence(context.Background(), oracleDMNativeSequence(), "")
			require.Error(t, err)
			require.True(t, IsOracleDamengSequencePreflight(err))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestOracleDamengPlanComparesSequencePropertiesNumerically(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	expectOracleDMSequenceObject(m, "S", "SEQUENCE")
	m.ExpectQuery("SELECT INCREMENT_BY,MIN_VALUE,MAX_VALUE,LAST_NUMBER,CYCLE_FLAG,CACHE_SIZE,ORDER_FLAG").WithArgs("APP", "S").WillReturnRows(sqlmock.NewRows([]string{"step", "min", "max", "current", "cycle", "cache", "ordered"}).AddRow("+0005", "0001", "01000", "0051", "N", "00", "N"))
	expectOracleDMSequenceObject(m, "TRG", "")
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "40")
	statements, err := w.PlanOracleDamengSequence(context.Background(), s)
	require.NoError(t, err)
	require.Len(t, statements, 1)
	require.Contains(t, statements[0].SQL, `CREATE TRIGGER "APP"."TRG"`)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengPlanRejectsChangedOrDisabledTriggerBeforeWrites(t *testing.T) {
	for _, field := range []string{"owner", "name", "table_owner", "table", "timing", "event", "status", "condition", "full-header-condition", "full-header-table", "missing-header", "body"} {
		t.Run(field, func(t *testing.T) {
			w, m := oracleDMRecoveryMock(t)
			s := oracleDMNativeSequence()
			expectOracleDMSequenceObject(m, "S", "SEQUENCE")
			m.ExpectQuery("SELECT INCREMENT_BY,MIN_VALUE,MAX_VALUE,LAST_NUMBER,CYCLE_FLAG,CACHE_SIZE,ORDER_FLAG").WithArgs("APP", "S").WillReturnRows(sqlmock.NewRows([]string{"step", "min", "max", "current", "cycle", "cache", "ordered"}).AddRow("5", "1", "1000", "1", "N", "0", "N"))
			expectOracleDMSequenceObject(m, "TRG", "TRIGGER")
			ss := w.dia.SequenceStatements("APP", s, "")
			pos := strings.Index(ss[1].SQL, "\nBEGIN")
			header, body := strings.TrimPrefix(ss[1].SQL[:pos], "CREATE TRIGGER "), ss[1].SQL[pos+1:]
			values := []driver.Value{"APP", "TRG", "APP", "T", "BEFORE EACH ROW", "INSERT", "ENABLED", header, body}
			switch field {
			case "owner":
				values[0] = "OTHER"
			case "name":
				values[1] = "OTHER"
			case "table_owner":
				values[2] = "OTHER"
			case "table":
				values[3] = "OTHER"
			case "timing":
				values[4] = "AFTER EACH ROW"
			case "event":
				values[5] = "INSERT OR UPDATE"
			case "status":
				values[6] = "DISABLED"
			case "condition":
				values[7] = header + " WHEN (NEW.ID IS NULL)"
			case "full-header-condition":
				values[8] = ss[1].SQL[:pos] + " WHEN (NEW.ID IS NULL)" + ss[1].SQL[pos:]
			case "full-header-table":
				values[8] = strings.Replace(ss[1].SQL, `ON "APP"."T"`, `ON "APP"."OTHER"`, 1)
			case "missing-header":
				values[7] = nil
			case "body":
				values[8] = strings.Replace(body, "END;", "INSERT INTO AUDIT VALUES(1); END;", 1)
			}
			m.ExpectQuery("SELECT OWNER,TRIGGER_NAME,TABLE_OWNER,TABLE_NAME,TRIGGERING_TYPE,TRIGGERING_EVENT,STATUS,DESCRIPTION,TRIGGER_BODY").WithArgs("APP", "TRG").WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "table_owner", "table", "timing", "event", "status", "description", "body"}).AddRow(values...))
			err := w.CreateSequence(context.Background(), s, "")
			require.ErrorContains(t, err, "不兼容")
			require.True(t, IsOracleDamengSequencePreflight(err))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestOracleDamengPlanRejectsWrongKindOrOrphanBeforeWrites(t *testing.T) {
	for _, kind := range []string{"TABLE", "orphan"} {
		t.Run(kind, func(t *testing.T) {
			w, m := oracleDMRecoveryMock(t)
			if kind == "orphan" {
				expectOracleDMSequenceObject(m, "S", "")
				expectOracleDMSequenceObject(m, "TRG", "TRIGGER")
			} else {
				expectOracleDMSequenceObject(m, "S", kind)
			}
			err := w.CreateSequence(context.Background(), oracleDMNativeSequence(), "")
			require.Error(t, err)
			require.True(t, IsOracleDamengSequencePreflight(err))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestOracleDamengPlanExhaustionDoesNotCreateMissingTrigger(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	expectOracleDMSequenceState(t, m, s, "1", []bool{false})
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "1000")
	err := w.CreateSequence(context.Background(), s, "")
	require.ErrorContains(t, err, "边界")
	require.True(t, IsOracleDamengSequencePreflight(err))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengCreateResumesAfterPartialTriggerFailure(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	s.Native.Bindings = append(s.Native.Bindings, source.SequenceBinding{TableName: "T2", ColumnName: "ID2", TriggerName: "TRG2", Kind: "trigger", OnlyWhenNull: true})
	expectOracleDMSequenceState(t, m, s, "", []bool{false, false})
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", nil)
	expectOracleDMSequenceExtreme(m, "T2", "ID2", "MAX", nil)
	m.ExpectExec("CREATE SEQUENCE").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(regexp.QuoteMeta(`CREATE TRIGGER "APP"."TRG"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(regexp.QuoteMeta(`CREATE TRIGGER "APP"."TRG2"`)).WillReturnError(errors.New("temporary trigger error"))
	require.ErrorContains(t, w.CreateSequence(context.Background(), s, ""), "temporary trigger error")
	expectOracleDMSequenceState(t, m, s, "1", []bool{true, false})
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", nil)
	expectOracleDMSequenceExtreme(m, "T2", "ID2", "MAX", nil)
	m.ExpectExec(regexp.QuoteMeta(`CREATE TRIGGER "APP"."TRG2"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, w.CreateSequence(context.Background(), s, ""))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengCalibrationRequiresExistingBinding(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	expectOracleDMSequenceState(t, m, s, "11", []bool{false})
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "42")
	err := w.CalibrateOracleDameng(context.Background(), s)
	require.ErrorContains(t, err, "目标绑定不存在")
	require.True(t, IsOracleDamengSequencePreflight(err))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengPreflightRetainsConnectionFailureCause(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	m.ExpectQuery("SELECT OBJECT_TYPE FROM ALL_OBJECTS").WithArgs("APP", "S").WillReturnError(io.EOF)
	_, err := w.PlanOracleDamengSequence(context.Background(), oracleDMNativeSequence())
	require.True(t, IsOracleDamengSequencePreflight(err))
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengExecutionFailureIsNotPreflight(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	expectOracleDMSequenceState(t, m, s, "51", []bool{false})
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "40")
	errCreate := errors.New("trigger rejected")
	m.ExpectExec("CREATE TRIGGER").WillReturnError(errCreate)
	err := w.CreateSequence(context.Background(), s, "")
	require.ErrorIs(t, err, errCreate)
	require.False(t, IsOracleDamengSequencePreflight(err))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengTriggerBodyComparisonPreservesQuotedWhitespace(t *testing.T) {
	require.Equal(t, oracleDamengSequenceBodyKey(`BEGIN SELECT "APP"."S".NEXTVAL INTO :NEW."I  D" FROM DUAL; END;`), oracleDamengSequenceBodyKey("begin\nSELECT \"APP\".\"S\" . NEXTVAL INTO :NEW . \"I  D\" FROM DUAL ; end ;"))
	require.NotEqual(t, oracleDamengSequenceBodyKey(`BEGIN SELECT "APP"."S".NEXTVAL INTO :NEW."I  D" FROM DUAL; END;`), oracleDamengSequenceBodyKey(`BEGIN SELECT "APP"."S".NEXTVAL INTO :NEW."I D" FROM DUAL; END;`))
}

func TestOracleDamengTriggerDescriptionAcceptsOnlyEquivalentHeader(t *testing.T) {
	for _, tc := range []struct {
		description, expected string
		compatible            bool
	}{
		{`TRG BEFORE INSERT ON T FOR EACH ROW`, `CREATE TRIGGER "APP"."TRG" BEFORE INSERT ON "APP"."T" FOR EACH ROW`, true},
		{`触发器 BEFORE INSERT ON 数据表 FOR EACH ROW`, `CREATE TRIGGER "APP"."触发器" BEFORE INSERT ON "APP"."数据表" FOR EACH ROW`, true},
		{`"APP"."Trg" BEFORE INSERT ON T FOR EACH ROW`, `CREATE TRIGGER "APP"."TRG" BEFORE INSERT ON "APP"."T" FOR EACH ROW`, false},
		{`TRG BEFORE INSERT ON T FOR EACH ROW WHEN (NEW.ID IS NULL)`, `CREATE TRIGGER "APP"."TRG" BEFORE INSERT ON "APP"."T" FOR EACH ROW`, false},
	} {
		require.Equal(t, tc.compatible, oracleDamengSequenceTriggerHeader(tc.description, tc.expected), tc.description)
	}
}
