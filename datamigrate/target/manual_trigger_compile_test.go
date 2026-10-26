package target

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestManualTriggerNativePendingCompileContinuesPlainCreate(t *testing.T) {
	dm, mock := manualTriggerMock(t)
	o := manualNativeObject()
	plain := strings.Replace(manualTriggerSQL, " OR REPLACE", "", 1)
	compile := `ALTER TRIGGER "APP"."A" COMPILE`
	o.ManualTrigger.SQL = plain
	o.Native.Status = "ENABLED"
	o.Native.Units = []damengnative.Unit{{SQL: plain, Phase: "after_data"}, {SQL: compile, Phase: "compile"}}
	o.NativeFingerprint = damengnative.Fingerprint(o.Native)
	o.Units = []repair.Unit{{ID: "create", SQL: plain, OriginalSQL: plain, Status: "succeeded"}, {ID: "compile", SQL: compile, OriginalSQL: compile, Status: "failed", Selected: true}}
	expectManualTable(mock)
	expectManualCatalog(mock, true, plain, "INVALID", "Y")
	expectManualCompileError(mock)
	mock.ExpectExec("SET SCHEMA").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(compile)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectManualCatalog(mock, true, plain, "VALID", "Y")
	expectManualCatalog(mock, true, plain, "VALID", "Y")
	mock.ExpectClose()
	units, err := ExecuteRepair(context.Background(), &DaMengNativeWriter{dm}, o, o.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", units[0].Status)
	require.Equal(t, "succeeded", units[1].Status)
	require.NoError(t, mock.ExpectationsWereMet(), "successful CREATE must not be replayed to continue COMPILE")
}

func TestManualTriggerKnownInvalidDiagnosticFailureRemainsEditable(t *testing.T) {
	for _, failure := range []string{"query", "scan", "rows", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			w, mock := manualTriggerMock(t)
			o := manualTriggerObject()
			o.Status, o.Units[0].Status = "unknown", "unknown"
			expectManualTable(mock)
			expectManualCatalog(mock, true, manualTriggerSQL, "INVALID", "Y")
			query := mock.ExpectQuery("SELECT LINE,POSITION,TEXT FROM ALL_ERRORS").WithArgs("APP", "A", "TRIGGER")
			switch failure {
			case "query":
				query.WillReturnError(errors.New("connection lost while reading diagnostics"))
			case "scan":
				query.WillReturnRows(sqlmock.NewRows([]string{"line", "position", "text"}).AddRow("bad line number", 7, "syntax error"))
			case "rows":
				query.WillReturnRows(sqlmock.NewRows([]string{"line", "position", "text"}).AddRow(3, 7, "syntax error").RowError(0, errors.New("connection lost")))
			case "timeout":
				query.WillReturnError(context.DeadlineExceeded)
			}
			units, err := ExecuteRepair(context.Background(), w, o, o.Units, nil)
			require.ErrorContains(t, err, "编译无效，读取详细错误失败")
			require.Equal(t, "failed", units[0].Status)
			require.Equal(t, "failed", repairErrorStatus(err))
			require.False(t, errors.Is(err, context.DeadlineExceeded), "a diagnostic timeout must not turn definite compile failure into uncertain DDL")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
