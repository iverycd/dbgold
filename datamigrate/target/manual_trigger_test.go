package target

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const manualTriggerSQL = `CREATE OR REPLACE TRIGGER APP.A AFTER INSERT ON APP.T FOR EACH ROW BEGIN INSERT INTO APP.LOG VALUES(:NEW.ID); END;`

func manualTriggerMock(t *testing.T) (*DaMengWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &DaMengWriter{db: db, schema: "APP", dia: dialect.NewDaMeng(), identCache: map[string]bool{}}, m
}
func manualTriggerObject() repair.Object {
	original := repair.Object{ID: "trigger", Kind: "trigger", Schema: "APP", TargetName: "A", Table: "T", DDL: manualTriggerSQL, Status: "failed"}
	o := original
	o.DiagnosticOnly = false
	o.ManualTrigger = &repair.ManualTriggerPlan{SQL: manualTriggerSQL, Revision: 1, Original: &original}
	o.Units = []repair.Unit{{ID: "create", SQL: manualTriggerSQL, OriginalSQL: manualTriggerSQL, Status: "failed", Selected: true}}
	return o
}
func expectManualTable(m sqlmock.Sqlmock) {
	m.ExpectQuery("SELECT COUNT.*FROM ALL_TABLES").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
}
func expectManualCatalog(m sqlmock.Sqlmock, exists bool, ddl, compile, state string) {
	r := sqlmock.NewRows([]string{"id", "type", "status"})
	if exists {
		r.AddRow(9, "TRIGGER", compile)
	}
	m.ExpectQuery("SELECT OBJECT_ID,OBJECT_TYPE,STATUS FROM ALL_OBJECTS").WithArgs("APP", "A").WillReturnRows(r)
	if exists {
		m.ExpectQuery("SELECT TABLE_OWNER,TABLE_NAME,STATUS FROM ALL_TRIGGERS").WithArgs("APP", "A").WillReturnRows(sqlmock.NewRows([]string{"owner", "table", "state"}).AddRow("APP", "T", state))
		m.ExpectQuery(regexp.QuoteMeta(`SELECT DBMS_METADATA.GET_DDL(?,?,?)`)).WithArgs("TRIGGER", "A", "APP").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(ddl))
	}
}

func expectManualCompileError(m sqlmock.Sqlmock) {
	m.ExpectQuery("SELECT LINE,POSITION,TEXT FROM ALL_ERRORS").WithArgs("APP", "A", "TRIGGER").WillReturnRows(sqlmock.NewRows([]string{"line", "position", "text"}).AddRow(3, 7, "unknown column"))
}

func manualNativeObject() repair.Object {
	o := manualTriggerObject()
	disable := `ALTER TRIGGER "APP"."A" DISABLE`
	o.Native = &damengnative.Object{ID: o.ID, Kind: o.Kind, Schema: o.Schema, Name: o.TargetName, Table: o.Table, Status: "DISABLED", Units: []damengnative.Unit{{SQL: manualTriggerSQL, Phase: "after_data"}, {SQL: disable, Phase: "restore"}}}
	o.NativeFingerprint = damengnative.Fingerprint(o.Native)
	o.Units = append(o.Units, repair.Unit{ID: "restore", SQL: disable, OriginalSQL: disable, Status: "pending", Selected: true})
	return o
}

func TestManualTriggerAbsentCreatesWholeProgramWithoutReplace(t *testing.T) {
	w, m := manualTriggerMock(t)
	o := manualTriggerObject()
	expectManualTable(m)
	expectManualCatalog(m, false, "", "", "")
	m.ExpectExec("SET SCHEMA").WillReturnResult(sqlmock.NewResult(0, 0))
	expectManualCatalog(m, false, "", "", "")
	d, err := damengnative.ParseTableTriggerSQL(manualTriggerSQL, "APP")
	require.NoError(t, err)
	m.ExpectExec(regexp.QuoteMeta(d.CreateSQL(false))).WillReturnResult(sqlmock.NewResult(0, 0))
	expectManualCatalog(m, true, manualTriggerSQL, "VALID", "Y")
	expectManualCatalog(m, true, manualTriggerSQL, "VALID", "Y")
	m.ExpectClose()
	var states []string
	units, err := ExecuteRepair(context.Background(), w, o, o.Units, func(units []repair.Unit) error { states = append(states, units[0].Status); return nil })
	require.NoError(t, err)
	require.Equal(t, "succeeded", units[0].Status)
	require.Equal(t, []string{"running", "succeeded"}, states)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestManualTriggerUnrelatedSameNameIsNeverReplaced(t *testing.T) {
	w, m := manualTriggerMock(t)
	o := manualTriggerObject()
	expectManualTable(m)
	expectManualCatalog(m, true, `CREATE TRIGGER APP.A AFTER INSERT ON APP.T BEGIN DELETE FROM APP.LOG; END;`, "INVALID", "Y")
	_, err := ExecuteRepair(context.Background(), w, o, o.Units, nil)
	require.ErrorContains(t, err, "禁止覆盖")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestManualTriggerUnknownExactDefinitionReusesWithoutDDL(t *testing.T) {
	w, m := manualTriggerMock(t)
	o := manualTriggerObject()
	o.Status = "unknown"
	o.Units[0].Status = "unknown"
	expectManualTable(m)
	expectManualCatalog(m, true, manualTriggerSQL, "VALID", "Y")
	expectManualCatalog(m, true, manualTriggerSQL, "VALID", "Y")
	units, err := ExecuteRepair(context.Background(), w, o, o.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", units[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestManualTriggerPersistenceFailurePreventsDDL(t *testing.T) {
	w, m := manualTriggerMock(t)
	o := manualTriggerObject()
	expectManualTable(m)
	expectManualCatalog(m, false, "", "", "")
	units, err := ExecuteRepair(context.Background(), w, o, o.Units, func([]repair.Unit) error { return errors.New("disk full") })
	require.ErrorContains(t, err, "未提交 SQL")
	require.Equal(t, "pending", units[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestManualTriggerUnknownConflictStaysUnknown(t *testing.T) {
	w, m := manualTriggerMock(t)
	o := manualTriggerObject()
	o.Status = "unknown"
	o.Units[0].Status = "unknown"
	expectManualTable(m)
	expectManualCatalog(m, true, `CREATE TRIGGER APP.A AFTER INSERT ON APP.T BEGIN DELETE FROM APP.LOG; END;`, "VALID", "Y")
	units, err := ExecuteRepair(context.Background(), w, o, o.Units, nil)
	require.Error(t, err)
	require.Equal(t, "unknown", units[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestManualTriggerInvalidCompileShowsErrorsBeforeSuccess(t *testing.T) {
	w, m := manualTriggerMock(t)
	o := manualTriggerObject()
	expectManualTable(m)
	expectManualCatalog(m, false, "", "", "")
	m.ExpectExec("SET SCHEMA").WillReturnResult(sqlmock.NewResult(0, 0))
	expectManualCatalog(m, false, "", "", "")
	d, _ := damengnative.ParseTableTriggerSQL(manualTriggerSQL, "APP")
	m.ExpectExec(regexp.QuoteMeta(d.CreateSQL(false))).WillReturnResult(sqlmock.NewResult(0, 0))
	expectManualCatalog(m, true, manualTriggerSQL, "INVALID", "Y")
	expectManualCompileError(m)
	m.ExpectClose()
	var states []string
	units, err := ExecuteRepair(context.Background(), w, o, o.Units, func(u []repair.Unit) error { states = append(states, u[0].Status); return nil })
	require.ErrorContains(t, err, "3:7 unknown column")
	require.Equal(t, "failed", units[0].Status)
	require.Equal(t, []string{"running", "failed"}, states)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestManualTriggerKnownInvalidCanRecompileExplicitReplacement(t *testing.T) {
	w, m := manualTriggerMock(t)
	o := manualTriggerObject()
	expectManualTable(m)
	expectManualCatalog(m, true, manualTriggerSQL, "INVALID", "Y")
	expectManualCompileError(m)
	m.ExpectExec("SET SCHEMA").WillReturnResult(sqlmock.NewResult(0, 0))
	expectManualCatalog(m, true, manualTriggerSQL, "INVALID", "Y")
	m.ExpectExec(regexp.QuoteMeta(manualTriggerSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectManualCatalog(m, true, manualTriggerSQL, "VALID", "Y")
	expectManualCatalog(m, true, manualTriggerSQL, "VALID", "Y")
	m.ExpectClose()
	units, err := ExecuteRepair(context.Background(), w, o, o.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", units[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestManualTriggerExistingRequiresExplicitReplace(t *testing.T) {
	w, m := manualTriggerMock(t)
	o := manualTriggerObject()
	plain := strings.Replace(manualTriggerSQL, " OR REPLACE", "", 1)
	o.ManualTrigger.SQL = plain
	o.Units[0].SQL = plain
	o.Units[0].OriginalSQL = plain
	old := strings.Replace(manualTriggerSQL, ":NEW.ID", ":NEW.ID+1", 1)
	o.ManualTrigger.PreviousSQL = []string{old}
	expectManualTable(m)
	expectManualCatalog(m, true, old, "VALID", "Y")
	units, err := ExecuteRepair(context.Background(), w, o, o.Units, nil)
	require.ErrorContains(t, err, "CREATE OR REPLACE")
	require.Equal(t, "failed", units[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestManualTriggerUnknownInvalidBecomesKnownFailedWithoutDDL(t *testing.T) {
	w, m := manualTriggerMock(t)
	o := manualTriggerObject()
	o.Status = "unknown"
	o.Units[0].Status = "unknown"
	expectManualTable(m)
	expectManualCatalog(m, true, manualTriggerSQL, "INVALID", "Y")
	expectManualCompileError(m)
	units, err := ExecuteRepair(context.Background(), w, o, o.Units, nil)
	require.ErrorContains(t, err, "编译无效")
	require.Equal(t, "failed", units[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestManualTriggerCatalogFailureAfterCreateStaysUnknown(t *testing.T) {
	w, m := manualTriggerMock(t)
	o := manualTriggerObject()
	expectManualTable(m)
	expectManualCatalog(m, false, "", "", "")
	m.ExpectExec("SET SCHEMA").WillReturnResult(sqlmock.NewResult(0, 0))
	expectManualCatalog(m, false, "", "", "")
	d, _ := damengnative.ParseTableTriggerSQL(manualTriggerSQL, "APP")
	m.ExpectExec(regexp.QuoteMeta(d.CreateSQL(false))).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery("SELECT OBJECT_ID").WillReturnError(errors.New("catalog unavailable"))
	m.ExpectClose()
	units, err := ExecuteRepair(context.Background(), w, o, o.Units, nil)
	require.ErrorContains(t, err, "读取结果失败")
	require.Equal(t, "unknown", units[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestManualTriggerNativeRestoresDisabledStateOnSameSession(t *testing.T) {
	dm, m := manualTriggerMock(t)
	w := &DaMengNativeWriter{dm}
	o := manualNativeObject()
	expectManualTable(m)
	expectManualCatalog(m, false, "", "", "")
	m.ExpectExec("SET SCHEMA").WillReturnResult(sqlmock.NewResult(0, 0))
	expectManualCatalog(m, false, "", "", "")
	d, _ := damengnative.ParseTableTriggerSQL(manualTriggerSQL, "APP")
	m.ExpectExec(regexp.QuoteMeta(d.CreateSQL(false))).WillReturnResult(sqlmock.NewResult(0, 0))
	expectManualCatalog(m, true, manualTriggerSQL, "VALID", "Y")
	m.ExpectExec(regexp.QuoteMeta(o.Native.Units[1].SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectManualCatalog(m, true, manualTriggerSQL, "VALID", "N")
	m.ExpectClose()
	units, err := ExecuteRepair(context.Background(), w, o, o.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", units[0].Status)
	require.Equal(t, "succeeded", units[1].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestManualTriggerNativeUnknownAlreadyRestoredNeedsNoDDL(t *testing.T) {
	dm, m := manualTriggerMock(t)
	w := &DaMengNativeWriter{dm}
	o := manualNativeObject()
	o.Status = "unknown"
	o.Units[0].Status = "unknown"
	o.Units[1].Status = "succeeded"
	o.Units[1].Selected = false
	expectManualTable(m)
	expectManualCatalog(m, true, manualTriggerSQL, "VALID", "N")
	expectManualCatalog(m, true, manualTriggerSQL, "VALID", "N")
	units, err := ExecuteRepair(context.Background(), w, o, o.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", units[0].Status)
	require.Equal(t, "succeeded", units[1].Status)
	require.NoError(t, m.ExpectationsWereMet())
}
