package target

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func pgOracleSequenceFixture(bound bool) pgoracle.SequencePlan {
	p := pgoracle.SequencePlan{Schema: "APP", Name: "SEQ", Source: source.PGSequence{OID: 11, Name: "Seq", Schema: "Mixed", Increment: 3, Min: 1, Max: 1000, LastValue: 31, IsCalled: true, Cache: 1}}
	if bound {
		p.Source.Bindings = []source.PGSequenceBinding{{Schema: "Mixed", TableName: "Orders", ColumnName: "Id", Owned: true}}
		p.Bindings = []pgoracle.Binding{{SourceTable: "Orders", SourceColumn: "Id", Table: "ORDERS", Column: "ID", Trigger: "TRG_ORDERS_ID"}}
	}
	return p
}

func pgOracleSequenceRepairObject(t *testing.T, p *pgoracle.SequencePlan) repair.Object {
	t.Helper()
	statements, err := p.SequenceStatements()
	require.NoError(t, err)
	return repair.Object{Kind: "sequence", Schema: p.Schema, Name: p.Name, TargetName: p.Name, PGOracleSequence: p, Units: []repair.Unit{{ID: "create", SQL: statements[0].SQL, OriginalSQL: statements[0].SQL, Status: "pending", Selected: true}}}
}

func expectPGOracleIdentity(mock sqlmock.Sqlmock, p pgoracle.SequencePlan, id int64) {
	mock.ExpectQuery(`SELECT OBJECT_ID FROM ALL_OBJECTS`).WithArgs(p.Schema, p.Name).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(id))
	cycle := "N"
	if p.Source.Cycle {
		cycle = "Y"
	}
	cache := p.Source.Cache
	if cache < 2 {
		cache = 0
	}
	mock.ExpectQuery(`SELECT TO_CHAR\(INCREMENT_BY,'TM9',.*ORDER_FLAG FROM ALL_SEQUENCES`).WithArgs(p.Schema, p.Name).WillReturnRows(sqlmock.NewRows([]string{"increment", "min", "max", "cache", "cycle", "order"}).AddRow(p.Source.Increment, p.Source.Min, p.Source.Max, cache, cycle, "N"))
}

func expectPGOracleMissingTrigger(mock sqlmock.Sqlmock, p pgoracle.SequencePlan, b pgoracle.Binding) {
	mock.ExpectQuery(`SELECT t.TABLE_OWNER,t.TABLE_NAME,t.TRIGGERING_EVENT`).WithArgs(p.Schema, b.Trigger).WillReturnError(sql.ErrNoRows)
}

func expectPGOracleKnownSequence(mock sqlmock.Sqlmock, p pgoracle.SequencePlan) {
	mock.ExpectQuery(`SELECT OBJECT_TYPE FROM ALL_OBJECTS`).WithArgs(p.Schema, p.Name).WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("SEQUENCE"))
	expectPGOracleIdentity(mock, p, p.Identity.ObjectID)
	mock.ExpectQuery(`SELECT OWNER,NAME,TYPE FROM ALL_DEPENDENCIES`).WithArgs(p.Schema, p.Name).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "type"}))
	for _, b := range p.Bindings {
		expectPGOracleMissingTrigger(mock, p, b)
	}
}

func expectPGOracleBindingColumns(mock sqlmock.Sqlmock, p pgoracle.SequencePlan) {
	for _, b := range p.Bindings {
		mock.ExpectQuery(`SELECT DATA_TYPE,DATA_SCALE,VIRTUAL_COLUMN,DATA_DEFAULT`).WithArgs(p.Schema, b.Table, b.Column).WillReturnRows(sqlmock.NewRows([]string{"type", "scale", "virtual", "default"}).AddRow("NUMBER", 0, "NO", nil))
		expectPGOracleMissingTrigger(mock, p, b)
	}
}

func TestPGOracleSequenceRepairPersistsIdentityWithSuccessfulCreate(t *testing.T) {
	w, m := oracleMockWriter(t)
	p := pgOracleSequenceFixture(false)
	obj := pgOracleSequenceRepairObject(t, &p)
	m.ExpectQuery(`SELECT OBJECT_TYPE FROM ALL_OBJECTS`).WithArgs("APP", "SEQ").WillReturnError(sql.ErrNoRows)
	m.ExpectExec(regexp.QuoteMeta(obj.Units[0].SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectPGOracleIdentity(m, p, 101)
	var saved []string
	out, err := ExecuteRepair(context.Background(), w, obj, obj.Units, func(units []repair.Unit) error {
		saved = append(saved, units[0].Status)
		if units[0].Status == "running" {
			require.Nil(t, obj.PGOracleSequence.Identity)
		} else {
			require.Equal(t, int64(101), obj.PGOracleSequence.Identity.ObjectID)
			require.Equal(t, p.Fingerprint(), obj.PGOracleSequence.Identity.Fingerprint)
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"running", "succeeded"}, saved)
	require.Equal(t, "succeeded", out[0].Status)
	require.False(t, out[0].Selected)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPGOracleSequenceNamesUseTargetCharsetByteLimitBeforeQueries(t *testing.T) {
	for _, field := range []string{"sequence", "table", "column", "trigger"} {
		t.Run(field, func(t *testing.T) {
			w, mock := oracleMockWriter(t)
			p := pgOracleSequenceFixture(true)
			name := strings.Repeat("序", 11)
			switch field {
			case "sequence":
				p.Name = name
			case "table":
				p.Bindings[0].Table = name
			case "column":
				p.Bindings[0].Column = name
			case "trigger":
				p.Bindings[0].Trigger = name
			}
			require.NoError(t, p.Validate()) // Portable plan syntax allows 11 runes.
			require.ErrorContains(t, w.InspectPGOracleSequence(context.Background(), p), "30 字节")
			_, err := w.PGOracleSequenceIdentity(context.Background(), p)
			require.ErrorContains(t, err, "30 字节")
			require.ErrorContains(t, w.ValidatePGOracleSequenceBindings(context.Background(), p, false), "30 字节")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestPGOracleSequenceRepairNeverAdoptsUnownedName(t *testing.T) {
	w, m := oracleMockWriter(t)
	p := pgOracleSequenceFixture(false)
	obj := pgOracleSequenceRepairObject(t, &p)
	m.ExpectQuery(`SELECT OBJECT_TYPE FROM ALL_OBJECTS`).WithArgs("APP", "SEQ").WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("SEQUENCE"))
	_, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
	require.ErrorContains(t, err, "无法证明")
	require.Nil(t, p.Identity)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPGOracleSequenceRepairRejectsRecreatedIdentity(t *testing.T) {
	w, m := oracleMockWriter(t)
	p := pgOracleSequenceFixture(false)
	p.Identity = &pgoracle.SequenceIdentity{ObjectID: 101, Fingerprint: p.Fingerprint()}
	obj := pgOracleSequenceRepairObject(t, &p)
	m.ExpectQuery(`SELECT OBJECT_TYPE FROM ALL_OBJECTS`).WithArgs("APP", "SEQ").WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("SEQUENCE"))
	expectPGOracleIdentity(m, p, 102)
	_, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
	require.ErrorContains(t, err, "身份或定义已经改变")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPGOracleSuccessfulCreateIsVerifiedWithoutReplay(t *testing.T) {
	w, m := oracleMockWriter(t)
	p := pgOracleSequenceFixture(false)
	obj := pgOracleSequenceRepairObject(t, &p)
	p.Identity = &pgoracle.SequenceIdentity{ObjectID: 101, Fingerprint: p.Fingerprint()}
	obj.Units[0].Status = "succeeded"
	expectPGOracleKnownSequence(m, p)
	out, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", out[0].Status)
	require.False(t, out[0].Selected)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPGOraclePendingCalibrationExecutesFrozenForwardUnit(t *testing.T) {
	w, m := oracleMockWriter(t)
	p := pgOracleSequenceFixture(false)
	p.Identity = &pgoracle.SequenceIdentity{ObjectID: 101, Fingerprint: p.Fingerprint()}
	obj := pgOracleSequenceRepairObject(t, &p)
	require.Contains(t, obj.Units[0].SQL, "desired_last")
	require.NotContains(t, obj.Units[0].SQL, "CREATE SEQUENCE")
	expectPGOracleKnownSequence(m, p)
	m.ExpectExec(regexp.QuoteMeta(obj.Units[0].SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectPGOracleIdentity(m, p, 101)
	out, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", out[0].Status)
	require.Equal(t, int64(101), p.Identity.ObjectID)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPGOracleSuccessfulCalibrationIsNotReexecuted(t *testing.T) {
	w, m := oracleMockWriter(t)
	p := pgOracleSequenceFixture(false)
	p.Identity = &pgoracle.SequenceIdentity{ObjectID: 101, Fingerprint: p.Fingerprint()}
	obj := pgOracleSequenceRepairObject(t, &p)
	obj.Units[0].Status = "succeeded"
	expectPGOracleKnownSequence(m, p)
	out, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", out[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPGOracleSequenceRepairRejectsEditedUnitBeforeSQL(t *testing.T) {
	w, m := oracleMockWriter(t)
	p := pgOracleSequenceFixture(false)
	obj := pgOracleSequenceRepairObject(t, &p)
	obj.Units[0].SQL += " -- edited"
	_, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
	require.ErrorContains(t, err, "冻结方案")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPGOracleSequenceRepairPersistenceFailureBoundaries(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "after"}[after], func(t *testing.T) {
			w, m := oracleMockWriter(t)
			p := pgOracleSequenceFixture(false)
			obj := pgOracleSequenceRepairObject(t, &p)
			m.ExpectQuery(`SELECT OBJECT_TYPE FROM ALL_OBJECTS`).WithArgs("APP", "SEQ").WillReturnError(sql.ErrNoRows)
			if after {
				m.ExpectExec(regexp.QuoteMeta(obj.Units[0].SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
				expectPGOracleIdentity(m, p, 101)
			}
			out, err := ExecuteRepair(context.Background(), w, obj, obj.Units, func(units []repair.Unit) error {
				if after && units[0].Status == "running" {
					return nil
				}
				return errors.New("disk full")
			})
			require.ErrorContains(t, err, "disk full")
			if after {
				require.Equal(t, "unknown", out[0].Status)
				require.NotNil(t, p.Identity)
			} else {
				require.Equal(t, "pending", out[0].Status)
				require.Nil(t, p.Identity)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestPGOracleSequenceRejectsAdditionalConsumers(t *testing.T) {
	w, m := oracleMockWriter(t)
	p := pgOracleSequenceFixture(true)
	p.Identity = &pgoracle.SequenceIdentity{ObjectID: 101, Fingerprint: p.Fingerprint()}
	m.ExpectQuery(`SELECT OBJECT_TYPE FROM ALL_OBJECTS`).WithArgs("APP", "SEQ").WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("SEQUENCE"))
	expectPGOracleIdentity(m, p, 101)
	m.ExpectQuery(`SELECT OWNER,NAME,TYPE FROM ALL_DEPENDENCIES`).WithArgs("APP", "SEQ").WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "type"}).AddRow("APP", "BUSINESS_PROC", "PROCEDURE"))
	err := CalibratePGOracleRecovery(context.Background(), w, "ORDERS", []pgoracle.SequencePlan{p})
	require.ErrorContains(t, err, "未冻结的使用对象")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPGOracleCycleRecoveryDoesNotConsumeOrAlterSequence(t *testing.T) {
	w, m := oracleMockWriter(t)
	p := pgOracleSequenceFixture(true)
	p.Source.Cycle = true
	p.Identity = &pgoracle.SequenceIdentity{ObjectID: 101, Fingerprint: p.Fingerprint()}
	expectPGOracleKnownSequence(m, p)
	expectPGOracleBindingColumns(m, p)
	require.NoError(t, CalibratePGOracleRecovery(context.Background(), w, "ORDERS", []pgoracle.SequencePlan{p}))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPGOracleRecoveryChecksEverySharedBindingBeforeMutation(t *testing.T) {
	w, m := oracleMockWriter(t)
	p := pgOracleSequenceFixture(true)
	p.Source.Bindings = append(p.Source.Bindings, source.PGSequenceBinding{Schema: "Mixed", TableName: "Other", ColumnName: "Id"})
	p.Bindings = append(p.Bindings, pgoracle.Binding{SourceTable: "Other", SourceColumn: "Id", Table: "OTHER", Column: "ID", Trigger: "TRG_OTHER_ID"})
	p.Identity = &pgoracle.SequenceIdentity{ObjectID: 101, Fingerprint: p.Fingerprint()}
	expectPGOracleKnownSequence(m, p)
	m.ExpectQuery(`SELECT DATA_TYPE,DATA_SCALE,VIRTUAL_COLUMN,DATA_DEFAULT`).WithArgs("APP", "ORDERS", "ID").WillReturnRows(sqlmock.NewRows([]string{"type", "scale", "virtual", "default"}).AddRow("NUMBER", 0, "NO", nil))
	expectPGOracleMissingTrigger(m, p, p.Bindings[0])
	m.ExpectQuery(`SELECT DATA_TYPE,DATA_SCALE,VIRTUAL_COLUMN,DATA_DEFAULT`).WithArgs("APP", "OTHER", "ID").WillReturnError(sql.ErrNoRows)
	err := CalibratePGOracleRecovery(context.Background(), w, "ORDERS", []pgoracle.SequencePlan{p})
	require.ErrorContains(t, err, "OTHER.ID")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPGOracleForwardCalibrationRestoresDefinition(t *testing.T) {
	for _, descending := range []bool{false, true} {
		t.Run(map[bool]string{false: "ascending", true: "descending"}[descending], func(t *testing.T) {
			w, m := oracleMockWriter(t)
			p := pgOracleSequenceFixture(true)
			if descending {
				p.Source.Increment, p.Source.Min, p.Source.Max, p.Source.LastValue = -3, -1000, -1, -31
			}
			p.Identity = &pgoracle.SequenceIdentity{ObjectID: 101, Fingerprint: p.Fingerprint()}
			expectPGOracleKnownSequence(m, p)
			expectPGOracleBindingColumns(m, p)
			q, err := pgOracleCalibrationSQL(p)
			require.NoError(t, err)
			require.NotContains(t, q, "DROP")
			require.NotContains(t, q, "LAST_NUMBER")
			require.Contains(t, q, "EXCEPTION WHEN OTHERS THEN IF changed THEN EXECUTE IMMEDIATE")
			if descending {
				require.Contains(t, q, `SELECT MIN("ID")`)
				require.Contains(t, q, "IF desired_last < n THEN")
			} else {
				require.Contains(t, q, `SELECT MAX("ID")`)
				require.Contains(t, q, "IF desired_last > n THEN")
			}
			m.ExpectExec(regexp.QuoteMeta(q)).WillReturnResult(sqlmock.NewResult(0, 0))
			expectPGOracleIdentity(m, p, 101)
			require.NoError(t, CalibratePGOracleRecovery(context.Background(), w, "ORDERS", []pgoracle.SequencePlan{p}))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestPGOracleTriggerDefinitionDriftIsRejected(t *testing.T) {
	for _, tc := range []string{"matches", "wrong_table", "disabled", "body_changed"} {
		t.Run(tc, func(t *testing.T) {
			w, m := oracleMockWriter(t)
			p := pgOracleSequenceFixture(true)
			p.Identity = &pgoracle.SequenceIdentity{ObjectID: 101, Fingerprint: p.Fingerprint()}
			binding := p.Bindings[0]
			table, status := "ORDERS", "ENABLED"
			if tc == "wrong_table" {
				table = "OTHER"
			}
			if tc == "disabled" {
				status = "DISABLED"
			}
			m.ExpectQuery(`SELECT t.TABLE_OWNER,t.TABLE_NAME,t.TRIGGERING_EVENT`).WithArgs("APP", binding.Trigger).WillReturnRows(sqlmock.NewRows([]string{"owner", "table", "event", "type", "enabled", "valid"}).AddRow("APP", table, "INSERT", "BEFORE EACH ROW", status, "VALID"))
			if tc == "matches" || tc == "body_changed" {
				body := strings.TrimPrefix(p.TriggerSQL(binding), "CREATE OR REPLACE ")
				if tc == "body_changed" {
					body = strings.Replace(body, ".NEXTVAL", ".CURRVAL", 1)
				}
				m.ExpectQuery(`SELECT TEXT FROM ALL_SOURCE`).WithArgs("APP", binding.Trigger).WillReturnRows(sqlmock.NewRows([]string{"text"}).AddRow(body))
			}
			exists, err := w.inspectPGOracleSequenceTrigger(context.Background(), p, binding)
			require.True(t, exists)
			if tc == "matches" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestPGOracleTriggerRepairExecutesOnlyFrozenBindingAfterSequenceProof(t *testing.T) {
	w, m := oracleMockWriter(t)
	p := pgOracleSequenceFixture(true)
	p.Identity = &pgoracle.SequenceIdentity{ObjectID: 101, Fingerprint: p.Fingerprint()}
	b := p.Bindings[0]
	sqlText := p.TriggerSQL(b)
	obj := repair.Object{Kind: "trigger", Schema: "APP", Table: b.Table, TargetName: b.Trigger, PGOracleSequence: &p, Units: []repair.Unit{{ID: "trigger", SQL: sqlText, OriginalSQL: sqlText, Status: "failed", Selected: true}}}
	expectPGOracleKnownSequence(m, p)
	expectPGOracleBindingColumns(m, p)
	expectPGOracleMissingTrigger(m, p, b)
	m.ExpectExec(regexp.QuoteMeta(sqlText)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery(`SELECT STATUS FROM ALL_OBJECTS`).WithArgs("APP", b.Trigger, "TRIGGER").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("VALID"))
	out, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", out[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}
