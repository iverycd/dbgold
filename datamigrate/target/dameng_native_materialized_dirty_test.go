package target

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func nativeTouchFixture() []string {
	return []string{`ALTER TABLE "MixedOwner"."Base" DROP IDENTITY`, `UPDATE "MixedOwner"."Base" SET "Id"="Id" WHERE ROWID=(SELECT ROWID FROM "MixedOwner"."Base" WHERE ROWNUM=1)`, `ALTER TABLE "MixedOwner"."Base" ADD COLUMN "Id" IDENTITY(-9, -2)`}
}

func expectNativeTouchIdentity(mock sqlmock.Sqlmock, present bool) {
	flag := 0
	if present {
		flag = 1
	}
	mock.ExpectQuery(`SELECT c.INFO2 FROM SYS.SYSCOLUMNS`).WithArgs("MixedOwner", "Base", "Id").WillReturnRows(sqlmock.NewRows([]string{"flags"}).AddRow(flag))
}

func expectNativeTouchCounter(mock sqlmock.Sqlmock) {
	q := `"MixedOwner"."Base"`
	mock.ExpectQuery(`SELECT IDENT_SEED`).WithArgs(q, q, q).WillReturnRows(sqlmock.NewRows([]string{"seed", "step", "current"}).AddRow(-9, -2, -9))
}

func nativeTouchConnection(t *testing.T) (*sql.Conn, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(); _ = db.Close() })
	return conn, mock
}

func TestNativeHistoryTouchIdentityRestoresExactFrozenSeedAfterSuccess(t *testing.T) {
	conn, mock := nativeTouchConnection(t)
	units := nativeTouchFixture()
	expectNativeTouchIdentity(mock, true)
	expectNativeTouchCounter(mock)
	mock.ExpectExec(regexp.QuoteMeta(units[0])).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(units[1])).WillReturnResult(sqlmock.NewResult(0, 1))
	expectNativeTouchIdentity(mock, false)
	mock.ExpectExec(regexp.QuoteMeta(units[2])).WillReturnResult(sqlmock.NewResult(0, 0))
	expectNativeTouchCounter(mock)
	require.NoError(t, nativeHistoryTouch(context.Background(), conn, "MixedOwner", units))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeHistoryTouchIdentityInterruptedDropIsNotRepeated(t *testing.T) {
	conn, mock := nativeTouchConnection(t)
	units := nativeTouchFixture()
	expectNativeTouchIdentity(mock, false)
	mock.ExpectExec(regexp.QuoteMeta(units[1])).WillReturnResult(sqlmock.NewResult(0, 1))
	expectNativeTouchIdentity(mock, false)
	mock.ExpectExec(regexp.QuoteMeta(units[2])).WillReturnResult(sqlmock.NewResult(0, 0))
	expectNativeTouchCounter(mock)
	require.NoError(t, nativeHistoryTouch(context.Background(), conn, "MixedOwner", units))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeHistoryTouchCancelledUpdateStillRestoresIdentity(t *testing.T) {
	conn, mock := nativeTouchConnection(t)
	units := nativeTouchFixture()
	expectNativeTouchIdentity(mock, true)
	expectNativeTouchCounter(mock)
	mock.ExpectExec(regexp.QuoteMeta(units[0])).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(units[1])).WillReturnError(context.Canceled)
	expectNativeTouchIdentity(mock, false)
	mock.ExpectExec(regexp.QuoteMeta(units[2])).WillReturnResult(sqlmock.NewResult(0, 0))
	expectNativeTouchCounter(mock)
	require.ErrorIs(t, nativeHistoryTouch(context.Background(), conn, "MixedOwner", units), context.Canceled)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeHistoryTouchRestoreFailureIsNotSuccess(t *testing.T) {
	conn, mock := nativeTouchConnection(t)
	units := nativeTouchFixture()
	expectNativeTouchIdentity(mock, false)
	mock.ExpectExec(regexp.QuoteMeta(units[1])).WillReturnResult(sqlmock.NewResult(0, 1))
	expectNativeTouchIdentity(mock, false)
	mock.ExpectExec(regexp.QuoteMeta(units[2])).WillReturnError(errors.New("connection lost"))
	require.ErrorContains(t, nativeHistoryTouch(context.Background(), conn, "MixedOwner", units), "恢复冻结的自增续号失败")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeHistoryTouchRejectsChangedIdentityBeforeWriting(t *testing.T) {
	conn, mock := nativeTouchConnection(t)
	expectNativeTouchIdentity(mock, true)
	q := `"MixedOwner"."Base"`
	mock.ExpectQuery(`SELECT IDENT_SEED`).WithArgs(q, q, q).WillReturnRows(sqlmock.NewRows([]string{"seed", "step", "current"}).AddRow(-9, -2, -11))
	require.ErrorContains(t, nativeHistoryTouch(context.Background(), conn, "MixedOwner", nativeTouchFixture()), "续号已偏离冻结计划")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeHistoryTouchRecipeCannotBecomeGeneralDMLOrAlterAnotherIdentity(t *testing.T) {
	for _, change := range []func([]string) []string{
		func(s []string) []string { s[1] = `UPDATE "MixedOwner"."Base" SET "Id"=42`; return s },
		func(s []string) []string {
			s[2] = `ALTER TABLE "Other"."Base" ADD COLUMN "Id" IDENTITY(-9,-2)`
			return s
		},
		func(s []string) []string { s[2] += `; DROP TABLE "MixedOwner"."Base"`; return s },
		func(s []string) []string {
			s[2] = `ALTER TABLE "MixedOwner"."Base" ADD COLUMN "Other" IDENTITY(-9,-2)`
			return s
		},
	} {
		_, err := parseNativeHistoryTouch("MixedOwner", change(nativeTouchFixture()))
		require.Error(t, err)
	}
}
