package target

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"gitee.com/chunanyong/dm"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

type nativeLinkFixtureReader struct {
	ddl  string
	name string
}

func (r *nativeLinkFixtureReader) ReadDamengNativeLinkDDL(_ context.Context, name string) (string, error) {
	r.name = name
	return r.ddl, nil
}

func TestNativeReloadedLinkRepairRehydratesOnlyFrozenSource(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("source_changed_%t", changed), func(t *testing.T) {
			w, mock := mockNativeDM(t)
			defer w.Close()
			ddl := `CREATE LINK "App"."L" CONNECT WITH U IDENTIFIED BY 'secret-password' USING 'PWD=connection-password'`
			u, err := damengnative.FreezeLinkUnit(ddl, "before_data")
			require.NoError(t, err)
			u, _, err = damengnative.RewriteUnit(u, "App", "MixedOwner")
			require.NoError(t, err)
			native := damengnative.Object{Kind: "db_link", Name: "L", Schema: "MixedOwner", Units: []damengnative.Unit{u}}
			actual, err := damengnative.ExecutionSQL(context.Background(), native, u)
			require.NoError(t, err)
			object := repair.Object{Kind: "db_link", Name: "L", TargetName: "L", Schema: "MixedOwner", Native: &native, NativeFingerprint: damengnative.Fingerprint(native), Units: []repair.Unit{{SQL: u.SQL, OriginalSQL: u.SQL, Status: "failed", Selected: true}}}
			encoded, err := json.Marshal(object)
			require.NoError(t, err)
			object = repair.Object{}
			require.NoError(t, json.Unmarshal(encoded, &object))
			r := &nativeLinkFixtureReader{ddl: ddl}
			if changed {
				r.ddl = strings.Replace(ddl, "connection-password", "changed-secret", 1)
			} else {
				mock.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "MixedOwner"`)).WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectExec(regexp.QuoteMeta(actual)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			ctx := damengnative.WithSourceLinkResolver(context.Background(), r, "App")
			var persisted [][]repair.Unit
			units, err := ExecuteRepair(ctx, w, object, object.Units, func(step []repair.Unit) error {
				persisted = append(persisted, step)
				b, e := json.Marshal(step)
				require.NoError(t, e)
				for _, secret := range []string{"secret-password", "connection-password", "changed-secret"} {
					require.NotContains(t, string(b), secret)
				}
				return nil
			})
			require.Equal(t, "L", r.name)
			require.NotEmpty(t, persisted)
			if changed {
				require.Error(t, err)
				require.Equal(t, "failed", units[0].Status)
			} else {
				require.NoError(t, err)
				require.Equal(t, "succeeded", units[0].Status)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestNativeLinkObserverNeverReceivesCredentialsOrServerEcho(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	defer db.Close()
	actual := `CREATE LINK "Target"."Remote" CONNECT WITH U IDENTIFIED BY 'secret-password' USING 'DSN=host;PWD=connection-password' OPTION(CASE_OPT='option-secret')`
	display, err := damengnative.PublicLinkSQL(actual)
	require.NoError(t, err)
	mock.ExpectExec(actual).WillReturnError(errors.New("connection lost while executing " + actual))
	var events []repair.Event
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { events = append(events, e); return nil })
	err = execNativeLinkObserved(ctx, db, display, actual)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-password")
	require.NotContains(t, err.Error(), "connection-password")
	require.ErrorIs(t, err, driver.ErrBadConn)
	require.Len(t, events, 2)
	for _, event := range events {
		require.NotContains(t, event.SQL, "secret-password")
		require.NotContains(t, event.Error, "secret-password")
		require.NotContains(t, event.SQL, "connection-password")
		require.NotContains(t, event.Error, "connection-password")
		require.NotContains(t, event.SQL, "option-secret")
	}
	require.Equal(t, "unknown", events[1].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeLinkDriverCommunicationCodesAreUnknownWithoutServerDetails(t *testing.T) {
	for _, code := range []int32{dm.ECGO_COMMUNITION_ERROR.ErrCode, dm.ECGO_CONNECTION_CLOSED.ErrCode} {
		err := nativeLinkError(&dm.DmError{ErrCode: code, ErrText: "secret-password / Connection details"})
		require.ErrorIs(t, err, driver.ErrBadConn)
		require.Equal(t, "unknown", repairErrorStatus(err))
		require.NotContains(t, err.Error(), "secret-password")
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		err := nativeLinkError(cause)
		require.ErrorIs(t, err, cause)
		require.Equal(t, "unknown", repairErrorStatus(err))
	}
}

func TestNativeMalformedLinkNeverReachesGenericObserver(t *testing.T) {
	w, mock := mockNativeDM(t)
	defer w.Close()
	var events []repair.Event
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { events = append(events, e); return nil })
	for _, statement := range []string{
		`CREATE LINK "MixedOwner"."L" CONNECT WITH U IDENTIFIED BY 'secret-password`,
		`CREATE DATABASE LINK "MixedOwner"."L" IDENTIFIED BY VALUES 'secret-password' USING 'host'`,
		`ALTER LINK "MixedOwner"."L" IDENTIFIED BY 'secret-password'`,
		`CREATE FORCE LINK "MixedOwner"."L" IDENTIFIED BY 'secret-password`,
		`CREATE LINK "MixedOwner"."L" IDENTIFIED BY '__DBGOLD_DBLINK_CREDENTIAL__' USING 'host'`,
	} {
		err := w.ExecuteDamengNative(ctx, statement)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret-password")
	}
	require.Empty(t, events)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeLinkFrozenRepairIsCredentialFreeAndTamperEvident(t *testing.T) {
	u, err := damengnative.FreezeLinkUnit(`CREATE LINK "App"."L" CONNECT WITH U IDENTIFIED BY 'secret-password' USING 'PWD=connection-password'`, "before_data")
	require.NoError(t, err)
	u, _, err = damengnative.RewriteUnit(u, "App", "MixedOwner")
	require.NoError(t, err)
	native := damengnative.Object{Kind: "db_link", Name: "L", Schema: "MixedOwner", Units: []damengnative.Unit{u}}
	object := repair.Object{Kind: "db_link", Name: "L", Schema: "MixedOwner", TargetName: "L", Native: &native, NativeFingerprint: damengnative.Fingerprint(native), Units: []repair.Unit{{SQL: u.SQL, OriginalSQL: u.SQL, Status: "failed", Selected: true}}}
	require.True(t, object.TrustedNative())
	encoded, err := json.Marshal(object)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret-password")
	require.NotContains(t, string(encoded), "connection-password")
	var reloaded repair.Object
	require.NoError(t, json.Unmarshal(encoded, &reloaded))
	require.True(t, reloaded.TrustedNative())
	reloaded.Native.Units[0].SourceSQLHash = "another-source"
	require.False(t, reloaded.TrustedNative())
}

func TestNativeLinkObservationFailurePreventsExecution(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	ctx := repair.WithObserver(context.Background(), func(repair.Event) error { return errors.New("ledger unavailable") })
	err = execNativeLinkObserved(ctx, db, "public", "sensitive")
	require.ErrorContains(t, err, "ledger unavailable")
	require.NoError(t, mock.ExpectationsWereMet())
}
