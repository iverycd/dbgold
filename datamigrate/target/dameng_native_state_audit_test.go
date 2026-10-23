package target

import (
	"context"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// A schema may legitimately contain a stand-alone change log before any MV is
// defined. Its creator needs the table privilege, not the separate MV privilege.
func TestDamengNativeLogPreflightUsesTablePrivilege(t *testing.T) {
	for _, tc := range []struct {
		name, username, privilege string
		allowed                   bool
	}{
		{name: "own_schema", username: "MixedOwner", privilege: "CREATE TABLE", allowed: true},
		{name: "other_schema", username: "Operator", privilege: "CREATE ANY TABLE", allowed: true},
		{name: "mv_privilege_is_insufficient", username: "MixedOwner", privilege: "CREATE MATERIALIZED VIEW"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := mockNativeDM(t)
			mock.ExpectQuery("SELECT USER FROM DUAL").WillReturnRows(sqlmock.NewRows([]string{"user"}).AddRow(tc.username))
			if tc.username != w.schema {
				mock.ExpectQuery(`SELECT COUNT\(\*\) FROM SYS.SYSOBJECTS s JOIN SYS.SYSOBJECTS u`).WithArgs(w.schema, tc.username).WillReturnRows(sqlmock.NewRows([]string{"owned"}).AddRow(0))
			}
			mock.ExpectQuery("SELECT PRIVILEGE FROM SESSION_PRIVS").WillReturnRows(sqlmock.NewRows([]string{"privilege"}).AddRow(tc.privilege))
			issues := w.ValidateDamengNativeObjects(context.Background(), []damengnative.Object{{
				Kind: "materialized_view_log", Name: "ChangeLog", Schema: "MixedOwner", Table: "Base",
				Units: []damengnative.Unit{{SQL: `CREATE MATERIALIZED VIEW LOG ON "MixedOwner"."Base" WITH ROWID`, Phase: "after_data"}},
			}})
			if tc.allowed {
				require.Empty(t, issues)
			} else {
				require.Len(t, issues, 1)
				require.Contains(t, issues[0].Reason, "CREATE TABLE")
			}
			require.NoError(t, mock.ExpectationsWereMet(), "preflight must issue reads only")
		})
	}
}

func TestDamengNativeTableRecipePreflightChecksItsExplicitIndex(t *testing.T) {
	for _, tc := range []struct {
		name, username string
		grants         []string
		allowed        bool
	}{
		{name: "owner", username: "MixedOwner", grants: []string{"CREATE TABLE"}, allowed: true},
		{name: "missing_index_privilege", username: "Operator", grants: []string{"CREATE ANY TABLE"}},
		{name: "both_privileges", username: "Operator", grants: []string{"CREATE ANY TABLE", "CREATE ANY INDEX"}, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := mockNativeDM(t)
			m.ExpectQuery("SELECT USER FROM DUAL").WillReturnRows(sqlmock.NewRows([]string{"user"}).AddRow(tc.username))
			if tc.username != w.schema {
				m.ExpectQuery(`SELECT COUNT\(\*\) FROM SYS.SYSOBJECTS s JOIN SYS.SYSOBJECTS u`).WithArgs(w.schema, tc.username).WillReturnRows(sqlmock.NewRows([]string{"owned"}).AddRow(0))
			}
			grants := sqlmock.NewRows([]string{"privilege"})
			for _, p := range tc.grants {
				grants.AddRow(p)
			}
			m.ExpectQuery("SELECT PRIVILEGE FROM SESSION_PRIVS").WillReturnRows(grants)
			issues := w.ValidateDamengNativeObjects(context.Background(), []damengnative.Object{{Kind: "table", Name: "T", Schema: "MixedOwner", Units: []damengnative.Unit{
				{SQL: `CREATE TABLE "MixedOwner"."T" ("Id" INT)`, Phase: "before_data"},
				{SQL: `CREATE UNIQUE INDEX "MixedOwner"."ExplicitKeyIndex" ON "MixedOwner"."T"("Id")`, Phase: "before_data"},
				{SQL: `ALTER TABLE "MixedOwner"."T" ADD CONSTRAINT "PK" PRIMARY KEY("Id") USING INDEX "MixedOwner"."ExplicitKeyIndex"`, Phase: "before_data"},
			}}})
			if tc.allowed {
				require.Empty(t, issues)
			} else {
				require.Len(t, issues, 1)
				require.Contains(t, issues[0].Reason, "CREATE ANY INDEX")
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestDamengNativePreflightUsesAuthorizedSchemaOwner(t *testing.T) {
	w, m := mockNativeDM(t)
	m.ExpectQuery("SELECT USER FROM DUAL").WillReturnRows(sqlmock.NewRows([]string{"user"}).AddRow("SchemaOwner"))
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM SYS.SYSOBJECTS s JOIN SYS.SYSOBJECTS u`).WithArgs(w.schema, "SchemaOwner").WillReturnRows(sqlmock.NewRows([]string{"owned"}).AddRow(1))
	m.ExpectQuery("SELECT PRIVILEGE FROM SESSION_PRIVS").WillReturnRows(sqlmock.NewRows([]string{"privilege"}).AddRow("CREATE TABLE"))
	issues := w.ValidateDamengNativeObjects(context.Background(), []damengnative.Object{{Kind: "table", Name: "T", Schema: w.schema, Units: []damengnative.Unit{
		{SQL: `CREATE TABLE "MixedOwner"."T" ("Id" INT)`},
		{SQL: `CREATE UNIQUE INDEX "MixedOwner"."PKIndex" ON "MixedOwner"."T"("Id")`},
	}}})
	require.Empty(t, issues, "AUTHORIZATION owner must not need ANY privileges for its own named schema")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestDamengNativeIndexPrivilegeRecognizesNativeModifiers(t *testing.T) {
	for _, head := range []string{"CLUSTER", "CLUSTER UNIQUE", "NOT CLUSTER", "UNIQUE", "BITMAP", "NOT PARTIAL"} {
		tokens, err := nativeSQLTokens(`CREATE OR REPLACE ` + head + ` INDEX "MixedOwner"."I" ON "MixedOwner"."T"("Id")`)
		require.NoError(t, err)
		require.Equal(t, "CREATE INDEX", nativeCreateIndexPrivilege(tokens), head)
	}
}

func TestDamengNativeDeferredVerificationRequiresNativeUninitializedState(t *testing.T) {
	for _, tc := range []struct {
		name, state, last string
		wantErr           bool
	}{
		{name: "unchanged", state: "UNUSEABLE", last: "1900-01-01 00:00:00.000000"},
		{name: "unnecessary_refresh_of_empty_base", state: "FRESH", last: "2026-10-23 10:00:00.000000", wantErr: true},
		{name: "lost_uninitialized_state", state: "NEEDS_FULL_REFRESH", last: "1900-01-01 00:00:00.000000", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := mockNativeDM(t)
			m.ExpectQuery(`SELECT STALENESS,TO_CHAR\(LAST_REFRESH_DATE`).WithArgs(w.schema, "Deferred").WillReturnRows(sqlmock.NewRows([]string{"state", "last"}).AddRow(tc.state, tc.last))
			err := w.verifyNativeDeferredMaterialized(context.Background(), damengnative.Object{Name: "Deferred", Materialized: &damengnative.MaterializedState{Staleness: "UNUSEABLE", LastRefresh: "1900-01-01 00:00:00.000000"}})
			if tc.wantErr {
				require.ErrorContains(t, err, "原生状态或未初始化水位")
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
	w, m := mockNativeDM(t)
	require.NoError(t, w.verifyNativeDeferredMaterialized(context.Background(), damengnative.Object{Name: "Current", Materialized: &damengnative.MaterializedState{Staleness: "PROVEN_CURRENT", LastRefresh: "2026-10-23 10:00:00.000000"}}))
	require.NoError(t, m.ExpectationsWereMet(), "initialized views keep their separate baseline/history proof")
}
