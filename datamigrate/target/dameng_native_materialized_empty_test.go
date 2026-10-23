package target

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func historyEmptyTestPlan() *damengnative.MaterializedEmptyTouch {
	return &damengnative.MaterializedEmptyTouch{Table: "Empty", DefinitionSQL: `CREATE TABLE "Dst"."Empty"("V" INT NOT NULL)`, Columns: []damengnative.MaterializedEmptyColumn{{Name: "V", NotNull: true}}}
}

func historyEmptyTestConn(t *testing.T) (*sql.Conn, sqlmock.Sqlmock) {
	t.Helper()
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn, m
}

func expectEmptyDDL(m sqlmock.Sqlmock, ddl string) {
	m.ExpectQuery(regexp.QuoteMeta(`SELECT DBMS_METADATA.GET_DDL('TABLE',?,?) FROM DUAL`)).WithArgs("Empty", "Dst").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(ddl))
}

func TestMaterializedEmptyTouchCancellationRestoresNullable(t *testing.T) {
	conn, m := historyEmptyTestConn(t)
	p := historyEmptyTestPlan()
	m.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM "Dst"."Empty"`)).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	expectEmptyDDL(m, p.DefinitionSQL)
	m.ExpectQuery(`SELECT c.OWNER,c.CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "table", "kind", "status", "validated", "column"}))
	m.ExpectExec(regexp.QuoteMeta(`ALTER TABLE "Dst"."Empty" ALTER COLUMN "V" SET NULL`)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(regexp.QuoteMeta(`CALL SET_SESSION_CONS_CHK(0)`)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectBegin()
	m.ExpectExec(regexp.QuoteMeta(`INSERT INTO "Dst"."Empty"("V") VALUES(NULL)`)).WillReturnError(context.Canceled)
	m.ExpectRollback()
	m.ExpectExec(regexp.QuoteMeta(`ALTER TABLE "Dst"."Empty" ALTER COLUMN "V" SET NOT NULL`)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(regexp.QuoteMeta(`CALL SET_SESSION_CONS_CHK(1)`)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectEmptyDDL(m, p.DefinitionSQL)
	m.ExpectQuery(`SELECT c.OWNER,c.CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "table", "kind", "status", "validated", "column"}))
	m.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	require.ErrorIs(t, nativeHistoryEmptyTouch(context.Background(), conn, "Dst", p), context.Canceled)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedEmptyTouchUnknownCommitNeverClaimsSuccess(t *testing.T) {
	conn, m := historyEmptyTestConn(t)
	p := historyEmptyTestPlan()
	m.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	expectEmptyDDL(m, p.DefinitionSQL)
	m.ExpectQuery(`SELECT c.OWNER,c.CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "table", "kind", "status", "validated", "column"}))
	for _, s := range []string{`ALTER TABLE "Dst"."Empty" ALTER COLUMN "V" SET NULL`, `CALL SET_SESSION_CONS_CHK(0)`} {
		m.ExpectExec(regexp.QuoteMeta(s)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectBegin()
	m.ExpectExec(`INSERT INTO`).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit().WillReturnError(context.DeadlineExceeded)
	m.ExpectExec(`ALTER TABLE`).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(`CALL SET_SESSION_CONS_CHK`).WillReturnResult(sqlmock.NewResult(0, 0))
	expectEmptyDDL(m, p.DefinitionSQL)
	m.ExpectQuery(`SELECT c.OWNER,c.CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "table", "kind", "status", "validated", "column"}))
	m.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	require.ErrorIs(t, nativeHistoryEmptyTouch(context.Background(), conn, "Dst", p), context.DeadlineExceeded)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedEmptyTouchRejectsChangedDefinitionWithoutWrites(t *testing.T) {
	for _, ddl := range []string{`CREATE TABLE "Dst"."Empty"("V" BIGINT NOT NULL)`, `CREATE TABLE "Dst"."Empty"("V" INT NOT NULL DEFAULT 99)`} {
		t.Run(ddl, func(t *testing.T) {
			conn, m := historyEmptyTestConn(t)
			m.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
			expectEmptyDDL(m, ddl)
			require.ErrorContains(t, nativeHistoryEmptyTouch(context.Background(), conn, "Dst", historyEmptyTestPlan()), "定义与冻结计划不同")
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestMaterializedEmptyTouchRejectsExternalFKWithoutWrites(t *testing.T) {
	conn, m := historyEmptyTestConn(t)
	p := historyEmptyTestPlan()
	m.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	expectEmptyDDL(m, p.DefinitionSQL)
	m.ExpectQuery(`SELECT c.OWNER,c.CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "table", "kind", "status", "validated", "column"}).AddRow("Other", "FK", "Child", "R", "ENABLED", "VALIDATED", "Parent"))
	require.ErrorContains(t, nativeHistoryEmptyTouch(context.Background(), conn, "Dst", p), "模式外")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedEmptyTouchCleanupFailureIsReported(t *testing.T) {
	conn, m := historyEmptyTestConn(t)
	p := historyEmptyTestPlan()
	m.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	expectEmptyDDL(m, p.DefinitionSQL)
	m.ExpectQuery(`SELECT c.OWNER,c.CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "table", "kind", "status", "validated", "column"}))
	m.ExpectExec(`ALTER TABLE`).WillReturnError(errors.New("DDL response lost"))
	m.ExpectExec(`ALTER TABLE`).WillReturnError(errors.New("restore permission lost"))
	m.ExpectExec(`CALL SET_SESSION_CONS_CHK`).WillReturnResult(sqlmock.NewResult(0, 0))
	err := nativeHistoryEmptyTouch(context.Background(), conn, "Dst", p)
	require.ErrorContains(t, err, "DDL response lost")
	require.ErrorContains(t, err, "恢复空基表冻结定义失败")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedEmptyDefinitionPreservesExpressionAndStorage(t *testing.T) {
	p := historyEmptyTestPlan()
	p.Columns[0].DefaultOnNull = true
	p.Columns[0].DefaultSQL = `'BEGIN NOT NULL, X'`
	a := `CREATE TABLE "Dst"."Empty"("V" VARCHAR(40) DEFAULT ON NULL 'BEGIN NOT NULL, X' NOT NULL) STORAGE(ON "Mixed",CLUSTERBTR)`
	b := `CREATE TABLE "Dst"."Empty"("V" VARCHAR(40)) STORAGE(ON "Mixed",CLUSTERBTR)`
	x, e := historyEmptyDefinition(a, p)
	require.NoError(t, e)
	y, e := historyEmptyDefinition(b, p)
	require.NoError(t, e)
	require.True(t, nativeDefinitionsEqual(x, y))
	_, e = historyEmptyDefinition(`CREATE TABLE "Dst"."Empty"("V" VARCHAR(40) DEFAULT ON NULL 'changed' NOT NULL)`, p)
	require.Error(t, e)
}

func TestMaterializedEmptyGeneratedForeignKeysMatchFullReference(t *testing.T) {
	p := historyEmptyTestPlan()
	for _, parent := range []string{"P1", "P2"} {
		p.Constraints = append(p.Constraints, damengnative.MaterializedEmptyConstraint{Table: "Child", Kind: "R", Name: "Source" + parent, Generated: true, Columns: []string{"X"}, Enabled: true, DefinitionSQL: `ALTER TABLE "Dst"."Child" ADD CONSTRAINT "Source` + parent + `" FOREIGN KEY("X") REFERENCES "` + parent + `"("Id") ON DELETE CASCADE ENABLE`})
	}
	actual := []historyEmptyConstraint{{table: "Child", kind: "R", name: "Target2", columns: []string{"X"}, definition: `ALTER TABLE "Dst"."Child" ADD CONSTRAINT "Target2" FOREIGN KEY("X") REFERENCES "P2"("Id") ON DELETE CASCADE DISABLE`}, {table: "Child", kind: "R", name: "Target1", columns: []string{"X"}, definition: `ALTER TABLE "Dst"."Child" ADD CONSTRAINT "Target1" FOREIGN KEY("X") REFERENCES "P1"("Id") ON DELETE CASCADE DISABLE`}}
	resolved, err := historyResolveEmptyConstraints(actual, p)
	require.NoError(t, err)
	require.Equal(t, "Target1", resolved[0].name)
	require.Equal(t, "Target2", resolved[1].name)
	actual[1].definition = `ALTER TABLE "Dst"."Child" ADD CONSTRAINT "Target1" FOREIGN KEY("X") REFERENCES "OtherParent"("Id") ON DELETE CASCADE DISABLE`
	_, err = historyResolveEmptyConstraints(actual, p)
	require.ErrorContains(t, err, "定义与冻结计划不同")
}

func TestMaterializedEmptyGeneratedChecksPreserveEnabledMultiplicity(t *testing.T) {
	p := historyEmptyTestPlan()
	p.Constraints = []damengnative.MaterializedEmptyConstraint{{Table: "Empty", Kind: "C", Generated: true, Columns: []string{"V"}, Enabled: false}, {Table: "Empty", Kind: "C", Generated: true, Columns: []string{"V"}, Enabled: true}}
	actual := []historyEmptyConstraint{{table: "Empty", kind: "C", name: "C1", columns: []string{"V"}, enabled: true}, {table: "Empty", kind: "C", name: "C2", columns: []string{"V"}, enabled: false}}
	resolved, err := historyResolveEmptyConstraints(actual, p)
	require.NoError(t, err)
	require.Equal(t, "C2", resolved[0].name)
	require.Equal(t, "C1", resolved[1].name)
}

func TestMaterializedEmptyIdentityNormalizesOnlyFrozenImplicitNullability(t *testing.T) {
	p := &damengnative.MaterializedEmptyTouch{Table: "Base", Columns: []damengnative.MaterializedEmptyColumn{{Name: "Id", NotNull: true, ValueSQL: "CAST(0 AS INT)"}, {Name: "A", NotNull: true, ValueSQL: "CAST(5 AS INT)"}}, IdentityRestore: []string{`ALTER TABLE "Dst"."Base" DROP IDENTITY`, `ALTER TABLE "Dst"."Base" ADD COLUMN "Id" IDENTITY(10,3)`}}
	want, err := historyEmptyDefinition(`CREATE TABLE "Dst"."Base"("Id" INT IDENTITY(10,3) NOT NULL,"A" INT NOT NULL)`, p)
	require.NoError(t, err)
	pending, err := historyEmptyDefinition(`CREATE TABLE "Dst"."Base"("Id" INT,"A" INT NOT NULL)`, p)
	require.NoError(t, err)
	require.True(t, nativeDefinitionsEqual(want, pending))
	changed, err := historyEmptyDefinition(`CREATE TABLE "Dst"."Base"("Id" INT,"A" INT)`, p)
	require.NoError(t, err)
	require.False(t, nativeDefinitionsEqual(want, changed), "其它typed列不能被泛化为可变状态")
	changed, err = historyEmptyDefinition(`CREATE TABLE "Dst"."Base"("Id" INT,"A" INT IDENTITY(10,3) NOT NULL)`, p)
	require.NoError(t, err)
	require.False(t, nativeDefinitionsEqual(want, changed), "不能把identity移到另一列当作临时状态")
	p.Constraints = []damengnative.MaterializedEmptyConstraint{{Table: "Base", Kind: "P", Enabled: true, Columns: []string{"Id", "A"}}}
	want, err = historyEmptyDefinition(`CREATE TABLE "Dst"."Base"("Id" INT IDENTITY(10,3) NOT NULL,"A" INT NOT NULL)`, p)
	require.NoError(t, err)
	changed, err = historyEmptyDefinition(`CREATE TABLE "Dst"."Base"("Id" INT,"A" INT)`, p)
	require.NoError(t, err)
	require.True(t, nativeDefinitionsEqual(want, changed), "仅冻结会DISABLE的复合非聚集PK列可处于临时nullable状态")
}
