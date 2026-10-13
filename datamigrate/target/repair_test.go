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
	"github.com/stretchr/testify/require"
)

func repairPGMock(t *testing.T) (*PostgresWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	t.Cleanup(func() { _ = db.Close() })
	return &PostgresWriter{db: db, schema: "app", dbType: "postgres", dia: dialect.NewPostgres("postgres")}, m
}
func repairTestUnits() []repair.Unit {
	return []repair.Unit{{ID: "a", SQL: "CREATE TABLE t(a int)", Selected: true, Status: "pending"}, {ID: "b", SQL: "ALTER TABLE t ADD b int", Selected: true, Status: "pending"}, {ID: "c", SQL: "ALTER TABLE t ADD c int", Selected: true, Status: "pending"}}
}

func TestRepairStopsAfterFailedUnitAndPreservesCommittedStep(t *testing.T) {
	w, m := repairPGMock(t)
	m.ExpectExec("SET search_path").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec("CREATE TABLE").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec("ALTER TABLE t ADD b").WillReturnError(errors.New("bad type"))
	m.ExpectClose()
	var snapshots [][]repair.Unit
	u, e := ExecuteRepair(context.Background(), w, repair.Object{Kind: "table", Schema: "app"}, repairTestUnits(), func(v []repair.Unit) error { snapshots = append(snapshots, v); return nil })
	require.ErrorContains(t, e, "bad type")
	require.Equal(t, "succeeded", u[0].Status)
	require.Equal(t, "failed", u[1].Status)
	require.Equal(t, "pending", u[2].Status)
	require.Equal(t, "running", snapshots[0][0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRepairPersistenceFailurePreventsSQL(t *testing.T) {
	w, m := repairPGMock(t)
	m.ExpectExec("SET search_path").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectClose()
	u, e := ExecuteRepair(context.Background(), w, repair.Object{Kind: "table", Schema: "app"}, repairTestUnits(), func([]repair.Unit) error { return errors.New("disk full") })
	require.ErrorContains(t, e, "disk full")
	require.Equal(t, "pending", u[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRepairTransportErrorDoesNotClaimDefiniteFailure(t *testing.T) {
	w, m := repairPGMock(t)
	m.ExpectExec("SET search_path").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec("CREATE TABLE").WillReturnError(errors.New("connection reset by peer"))
	m.ExpectClose()
	u, e := ExecuteRepair(context.Background(), w, repair.Object{Kind: "table", Schema: "app"}, repairTestUnits(), nil)
	require.Error(t, e)
	require.Equal(t, "unknown", u[0].Status)
	require.Equal(t, "pending", u[1].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRepairRejectsChangedSchemaBeforeExecution(t *testing.T) {
	w, m := repairPGMock(t)
	_, e := ExecuteRepair(context.Background(), w, repair.Object{Schema: "other"}, repairTestUnits(), nil)
	require.ErrorContains(t, e, "schema")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRepairSQLServerRollbackAndAmbiguousCommit(t *testing.T) {
	for _, commitFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "ambiguous commit"}[commitFails], func(t *testing.T) {
			w, m := sqlServerMock(t)
			m.ExpectExec("SET ANSI_NULLS").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectBegin()
			m.ExpectExec("CREATE TABLE").WillReturnResult(sqlmock.NewResult(0, 0))
			if commitFails {
				m.ExpectExec("ALTER TABLE t ADD b").WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectCommit().WillReturnError(errors.New("commit connection lost"))
			} else {
				m.ExpectExec("ALTER TABLE t ADD b").WillReturnError(errors.New("constraint error"))
				m.ExpectRollback()
			}
			m.ExpectClose()
			u, e := ExecuteRepair(context.Background(), w, repair.Object{Kind: "table", Schema: "dbo"}, repairTestUnits()[:2], nil)
			require.Error(t, e)
			if commitFails {
				require.Equal(t, "unknown", u[0].Status)
				require.Equal(t, "unknown", u[1].Status)
			} else {
				require.Equal(t, "rolled_back", u[0].Status)
				require.Equal(t, "failed", u[1].Status)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestRepairOracleBlockIsOneExecutionUnit(t *testing.T) {
	w, m := oracleMockWriter(t)
	block := "BEGIN EXECUTE IMMEDIATE 'CREATE SEQUENCE APP.S'; NULL; END;"
	m.ExpectExec("ALTER SESSION SET CURRENT_SCHEMA").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(regexp.QuoteMeta(block)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectClose()
	u, e := ExecuteRepair(context.Background(), w, repair.Object{Kind: "sequence", Schema: "APP"}, []repair.Unit{{ID: "a", SQL: block, Selected: true}}, nil)
	require.NoError(t, e)
	require.Equal(t, "succeeded", u[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOriginalDDLObserverTracksCommittedAndRolledBackUnits(t *testing.T) {
	w, m := sqlServerMock(t)
	m.ExpectExec("SET ANSI_NULLS").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectBegin()
	m.ExpectExec("CREATE TABLE").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec("ALTER TABLE").WillReturnError(errors.New("bad"))
	m.ExpectRollback()
	m.ExpectClose()
	var events []repair.Event
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { events = append(events, e); return nil })
	e := w.CreateTable(ctx, []dialect.Statement{{SQL: "CREATE TABLE t(a int)"}, {SQL: "ALTER TABLE t ADD bad invalid"}})
	require.Error(t, e)
	for _, event := range events {
		require.NotEqual(t, "succeeded", event.Status)
	}
	require.Equal(t, "rolled_back", events[len(events)-1].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOriginalDDLObserverFailureIsNotIgnoredAsMissingTable(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	w := &DaMengWriter{db: db, schema: "APP", srcType: "mysql", dia: dialect.NewDaMeng()}
	ctx := repair.WithObserver(context.Background(), func(repair.Event) error { return errors.New("no durable state") })
	require.ErrorContains(t, w.CreateTable(ctx, []dialect.Statement{{SQL: "DROP TABLE APP.T"}}), "no durable state")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRepairViewUsesOriginalSchemaAndCommit(t *testing.T) {
	w, m := repairPGMock(t)
	m.ExpectBegin()
	m.ExpectExec(regexp.QuoteMeta(`SET LOCAL search_path TO "app"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec("CREATE OR REPLACE VIEW").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectCommit()
	m.ExpectClose()
	u, e := ExecuteRepair(context.Background(), w, repair.Object{Kind: "view", Schema: "app"}, []repair.Unit{{SQL: "CREATE OR REPLACE VIEW app.v AS SELECT * FROM t", Selected: true}}, nil)
	require.NoError(t, e)
	require.Equal(t, "succeeded", u[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestInspectIndexDoesNotAcceptSameNameWrongDefinition(t *testing.T) {
	w, m := repairPGMock(t)
	m.ExpectQuery("SELECT a.attname,ix.indisunique").WithArgs("app", "t", false, "ix").WillReturnRows(sqlmock.NewRows([]string{"column", "unique", "primary", "valid", "special", "desc"}).AddRow("wrong", false, false, true, false, false))
	v, e := InspectRepair(context.Background(), w, repair.Object{Kind: "index", Schema: "app", TargetName: "ix", Table: "t", Index: &source.IndexInfo{TableName: "t", IndexName: "ix", Columns: []string{"expected"}}})
	require.NoError(t, e)
	require.Equal(t, "failed", v.Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestInspectTableUsesOriginalCaseColumnMapping(t *testing.T) {
	w, m := repairPGMock(t)
	m.ExpectQuery("SELECT COUNT.*FROM pg_class").WithArgs("app", "mixed").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	m.ExpectQuery("SELECT column_name,data_type").WithArgs("app", "mixed").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "length", "precision", "scale", "nullable", "default", "extra", "ordinal", "time_precision", "generated"}).AddRow("new_id", "integer", 0, 32, 0, "NO", nil, "", 1, 0, ""))
	v, e := InspectRepair(context.Background(), w, repair.Object{Kind: "table", Schema: "app", TargetName: "mixed", Columns: []source.ColumnInfo{{Name: "mixed_id", DataType: "INTEGER"}}, SourceColumns: []source.ColumnInfo{{Name: "Mixed_ID", DataType: "int"}}, ColumnMapping: map[string]string{"Mixed_ID": "new_id"}})
	require.NoError(t, e)
	require.Equal(t, "succeeded", v.Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestResolveRepairSchemaBindsPGWrapper(t *testing.T) {
	w, m := repairPGMock(t)
	w.schema = ""
	m.ExpectQuery(regexp.QuoteMeta("SELECT current_schema()")).WillReturnRows(sqlmock.NewRows([]string{"schema"}).AddRow("resolved"))
	schema, e := ResolveRepairSchema(context.Background(), &VastbaseWriter{PostgresWriter: w})
	require.NoError(t, e)
	require.Equal(t, "resolved", schema)
	require.Equal(t, "resolved", w.schema)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRepairNeverReplaysCommittedUnit(t *testing.T) {
	w, m := repairPGMock(t)
	m.ExpectExec("SET search_path").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec("ALTER TABLE t ADD b").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectClose()
	u := repairTestUnits()[:2]
	u[0].Status = "succeeded"
	result, e := ExecuteRepair(context.Background(), w, repair.Object{Kind: "table", Schema: "app"}, u, nil)
	require.NoError(t, e)
	require.False(t, result[0].Selected)
	require.Equal(t, "succeeded", result[1].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRecoveryRefusesTriggerAndIncomingForeignKey(t *testing.T) {
	for _, trigger := range []bool{true, false} {
		t.Run(map[bool]string{true: "trigger", false: "incoming foreign key"}[trigger], func(t *testing.T) {
			w, m := repairPGMock(t)
			m.ExpectQuery("SELECT COUNT.*FROM pg_class").WithArgs("app", "t").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
			n := 0
			if trigger {
				n = 1
			}
			m.ExpectQuery("SELECT COUNT.*FROM pg_trigger").WithArgs("app", "t").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(n))
			if !trigger {
				m.ExpectQuery("SELECT COUNT.*FROM pg_constraint").WithArgs("app", "t").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
			}
			require.Error(t, TruncateRecovery(context.Background(), w, "t"))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestRecoveryTruncateDoesNotCascade(t *testing.T) {
	w, m := repairPGMock(t)
	m.ExpectQuery("SELECT COUNT.*FROM pg_class").WithArgs("app", "t").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	m.ExpectQuery("SELECT COUNT.*FROM pg_trigger").WithArgs("app", "t").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	m.ExpectQuery("SELECT COUNT.*FROM pg_constraint").WithArgs("app", "t").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	m.ExpectExec(`^TRUNCATE TABLE "app"\."t"$`).WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, TruncateRecovery(context.Background(), w, "t"))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestRecoverySQLServerUsesRepairedTypeForCast(t *testing.T) {
	w, m := sqlServerMock(t)
	m.ExpectQuery("SELECT COUNT.*FROM sys.tables").WithArgs("dbo", "T").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	m.ExpectQuery("SELECT COUNT.*FROM sys.triggers").WithArgs("dbo", "T").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	m.ExpectQuery("SELECT c.name,t.name,CASE").WithArgs("dbo", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "length", "precision", "scale", "nullable", "default", "extra", "ordinal", "time_precision", "generated"}).AddRow("Name", "nvarchar", 200, 0, 0, "YES", nil, "", 1, 0, ""))
	require.NoError(t, PrepareRecovery(context.Background(), w, "T", []source.ColumnInfo{{Name: "Name", DataType: "varchar", Length: 20, IsNullable: true}}))
	m.ExpectExec("SET ANSI_NULLS").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectBegin()
	m.ExpectExec(regexp.QuoteMeta("INSERT INTO [dbo].[T] ([Name]) VALUES (CAST(@p1 AS NVARCHAR(200)))")).WithArgs("abc").WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	m.ExpectClose()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"Name"}, nil, [][]interface{}{{"abc"}}))
	require.NoError(t, m.ExpectationsWereMet())
}
