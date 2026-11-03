package target

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func expectDMProceduresReused(m sqlmock.Sqlmock, schema string) {
	for _, spec := range dmBusinessProcedures() {
		expectDMProcedureExists(m, schema, spec.name, 1)
	}
}
func expectDMProcedureExists(m sqlmock.Sqlmock, schema, name string, count int) {
	m.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=? AND OBJECT_TYPE='PROCEDURE'`)).
		WithArgs(schema, name).WillReturnRows(sqlmock.NewRows([]string{"N"}).AddRow(count))
}
func expectDMProcedureCreate(m sqlmock.Sqlmock, w *DaMengWriter, spec dmBusinessRoutine, status string) {
	expectDMFunctionDependencies(m, spec.dmBusinessFunction)
	m.ExpectExec(regexp.QuoteMeta(w.dmBusinessRoutineDDL(spec))).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT STATUS FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=? AND OBJECT_TYPE='PROCEDURE'`)).
		WithArgs(w.schema, spec.name).WillReturnRows(sqlmock.NewRows([]string{"STATUS"}).AddRow(status))
	if status != "VALID" {
		m.ExpectQuery(regexp.QuoteMeta(`SELECT LINE,POSITION,TEXT FROM ALL_ERRORS WHERE OWNER=? AND NAME=? AND TYPE='PROCEDURE' ORDER BY SEQUENCE`)).
			WithArgs(w.schema, spec.name).WillReturnRows(sqlmock.NewRows([]string{"LINE", "POSITION", "TEXT"}).AddRow(8, 3, "procedure compile error"))
	}
}

func TestDamengBusinessProceduresPreserveOriginalSQL(t *testing.T) {
	w, _ := mockDM(t)
	w.schema = `Mixed"Schema`
	w.dia = &damengNativeDialect{DaMengDialect: dialect.NewDaMeng()}
	originals := map[string]string{"EPOINT_PROC_ALTER": dmEpointProcAlterOriginal, "GETCURRENTNUMBER": dmGetCurrentNumberOriginal}
	hashes := map[string]string{
		"EPOINT_PROC_ALTER": "2040625e5cc174f282be59277ff9314879014ade84afb290e0c96e63b1eeab7a",
		"GETCURRENTNUMBER":  "e481b0b19119bc16d9ce182e7a9a62319c5e5c799a8b0d28221d8ded49d37f04",
	}
	for _, spec := range dmBusinessProcedures() {
		original := originals[spec.name]
		require.Equal(t, hashes[spec.name], fmt.Sprintf("%x", sha256.Sum256([]byte(original))), "supplied SQL is immutable")
		header, body, ok := strings.Cut(original, "\n")
		require.True(t, ok)
		_, params, ok := strings.Cut(header, "(")
		require.True(t, ok)
		require.Equal(t, spec.parameters, strings.TrimSuffix(params, ")"))
		ddl := w.dmBusinessRoutineDDL(spec)
		require.Equal(t, `CREATE PROCEDURE "Mixed""Schema"."`+spec.name+`"(`+spec.parameters+")\n"+body, ddl)
		require.NotContains(t, ddl, "OR REPLACE")
		require.NotContains(t, ddl, "RETURN VARCHAR")
	}
	require.Contains(t, dmEpointProcAlterOriginal, "SF_GET_SCHEMA_NAME_BY_ID(CURRENT_SCHID())")
	require.Contains(t, dmEpointProcAlterOriginal, "EXECUTE IMMEDIATE 'ALTER TABLE API_MANAGE_CONFIG ADD CONSTRAINT UK_API_MAN_CON_APIGUID UNIQUE (APIGUID)'")
	require.Contains(t, dmGetCurrentNumberOriginal, "dbms_output.put_line(' maxValues is null;');")
	require.Contains(t, dmGetCurrentNumberOriginal, "            commit;")
	require.Contains(t, dmGetCurrentNumberOriginal, "cnumber nvarchar2(50);")
	// The first seven definitions continue to use the original generator.
	routines := dmBusinessRoutines()
	require.Len(t, routines, 9)
	for _, fn := range dmBusinessFunctions() {
		for _, r := range routines {
			if r.name == fn.name {
				require.Equal(t, w.dmBusinessFunctionDDL(fn), w.dmBusinessRoutineDDL(r))
			}
		}
	}
	require.Equal(t, []string{"EPOINT_PROC_ALTER", "GETCURRENTNUMBER"}, []string{routines[7].name, routines[8].name})
}

func TestDamengBusinessProceduresInstallation(t *testing.T) {
	for _, scenario := range []string{"created", "reused", "missing_table", "missing_column", "blocked", "ddl_error", "invalid", "connection", "cancel_between"} {
		t.Run(scenario, func(t *testing.T) {
			w, m := mockDM(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			for _, f := range dmBusinessFunctions() {
				expectDMFunctionExists(m, f.name, 1)
			}
			blocked := map[string]string{}
			stopped := false
			for _, spec := range dmBusinessProcedures() {
				if stopped {
					continue
				}
				if spec.name == "GETCURRENTNUMBER" {
					expectDMProcedureExists(m, w.schema, spec.name, 0)
					expectDMProcedureCreate(m, w, spec, "VALID")
					continue
				}
				if scenario == "reused" {
					expectDMProcedureExists(m, w.schema, spec.name, 1)
					blocked["API_MANAGE_CONFIG"] = "must be ignored for an existing procedure"
					continue
				}
				expectDMProcedureExists(m, w.schema, spec.name, 0)
				switch scenario {
				case "missing_table", "missing_column":
					rows := sqlmock.NewRows([]string{"COLUMN_NAME"})
					if scenario == "missing_column" {
						rows.AddRow("OTHER_COLUMN")
					}
					m.ExpectQuery("SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS").WithArgs(w.schema, "API_MANAGE_CONFIG").WillReturnRows(rows)
				case "blocked":
					blocked["API_MANAGE_CONFIG"] = "table migration failed"
				case "ddl_error", "connection":
					expectDMFunctionDependencies(m, spec.dmBusinessFunction)
					err := errors.New("permission denied")
					if scenario == "connection" {
						err = io.EOF
						stopped = true
					}
					m.ExpectExec(regexp.QuoteMeta(w.dmBusinessRoutineDDL(spec))).WillReturnError(err)
				case "invalid":
					expectDMProcedureCreate(m, w, spec, "INVALID")
				case "cancel_between":
					expectDMFunctionDependencies(m, spec.dmBusinessFunction)
					m.ExpectExec(regexp.QuoteMeta(w.dmBusinessRoutineDDL(spec))).WillReturnError(context.Canceled)
					stopped = true
				default:
					expectDMProcedureCreate(m, w, spec, "VALID")
				}
			}
			report, err := w.EnsureMySQLDamengBusinessFunctions(ctx, blocked)
			require.Equal(t, 9, report.Total)
			first := dmFunctionResult(t, report, "EPOINT_PROC_ALTER")
			second := dmFunctionResult(t, report, "GETCURRENTNUMBER")
			require.Equal(t, "procedure", first.Kind)
			require.Equal(t, "procedure", second.Kind)
			require.Contains(t, second.Signature, "CurrentNumber out nvarchar2")
			if scenario == "created" || scenario == "reused" {
				require.NoError(t, err)
				require.Equal(t, scenario, first.Status)
			} else {
				require.Error(t, err)
				require.Equal(t, "failed", first.Status)
				require.NotEmpty(t, first.DDL)
			}
			if scenario == "invalid" {
				require.Contains(t, first.Error, "8:3 procedure compile error")
			}
			if scenario == "reused" {
				require.Empty(t, first.DDL)
				require.Equal(t, "已存在，跳过，未校验定义", first.DefinitionWarning)
			}
			if stopped {
				require.Contains(t, second.Error, "任务已终止")
			} else {
				require.Equal(t, "created", second.Status)
			}
		})
	}
}

func TestDamengBusinessProceduresContinueAfterAllFunctionsFail(t *testing.T) {
	w, m := mockDM(t)
	for _, fn := range dmBusinessFunctions() {
		expectDMFunctionExists(m, fn.name, 0)
		expectDMFunctionDependencies(m, fn)
		m.ExpectExec(regexp.QuoteMeta(w.dmBusinessFunctionDDL(fn))).WillReturnError(errors.New("function creation denied"))
	}
	for _, proc := range dmBusinessProcedures() {
		expectDMProcedureExists(m, w.schema, proc.name, 0)
		expectDMProcedureCreate(m, w, proc, "VALID")
	}
	report, err := w.EnsureMySQLDamengBusinessFunctions(context.Background(), nil)
	require.Error(t, err)
	require.Equal(t, 7, report.Failed)
	require.Equal(t, 2, report.Created)
	for _, name := range []string{"EPOINT_PROC_ALTER", "GETCURRENTNUMBER"} {
		require.Equal(t, "created", dmFunctionResult(t, report, name).Status)
	}
}
