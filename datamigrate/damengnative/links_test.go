package damengnative

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLinkCredentialsSurviveOnlyInMemoryAndFreezeAcrossReload(t *testing.T) {
	ddl := `CREATE LINK "App"."Remote" CONNECT 'DPI' WITH "RemoteUser" IDENTIFIED BY "App.p'ass" USING 'remote/App.Name' OPTION (CASE_OPT='SENSITIVE')`
	u, err := FreezeLinkUnit(ddl, "before_data")
	require.NoError(t, err)
	mapped, warnings, err := RewriteUnit(u, "App", "Target")
	require.NoError(t, err)
	require.Contains(t, mapped.SQL, `"Target" . "Remote"`)
	require.NotContains(t, mapped.SQL, `remote/App.Name`)
	require.Empty(t, warnings) // Opaque connection text never enters schema rewriting.
	actual, err := ExecutionSQL(context.Background(), Object{Kind: "db_link"}, mapped)
	require.NoError(t, err)
	require.Contains(t, actual, `IDENTIFIED BY "App.p'ass"`)
	require.Contains(t, actual, `USING 'remote/App.Name'`)
	b, err := json.Marshal(mapped)
	require.NoError(t, err)
	require.NotContains(t, string(b), "p'ass")
	require.NotContains(t, string(b), "remote/App.Name")
	var restored Unit
	require.NoError(t, json.Unmarshal(b, &restored))
	_, err = ExecutionSQL(context.Background(), Object{Kind: "db_link"}, restored)
	require.ErrorContains(t, err, "重新读取")
	hydrated, err := HydrateLinkUnit(restored, ddl, "App", "Target")
	require.NoError(t, err)
	after, err := ExecutionSQL(context.Background(), Object{Kind: "db_link"}, hydrated)
	require.NoError(t, err)
	require.Equal(t, actual, after)
	_, err = HydrateLinkUnit(restored, strings.Replace(ddl, "p'ass", "changed", 1), "App", "Target")
	require.ErrorContains(t, err, "已变化")
	require.NotContains(t, err.Error(), "changed")
}

func TestLinkOpaqueLiteralsAndWhitespaceNeverEnterPublicState(t *testing.T) {
	ddl := " \n\tCREATE LINK \"App\".\"Remote\" CONNECT 'DPI;driver-secret' WITH U IDENTIFIED BY 'password-secret' USING 'DSN=test;PWD=endpoint-secret' OPTION (CASE_OPT='option-secret');\n/\n"
	u, err := FreezeLinkUnit(ddl, "before_data")
	require.NoError(t, err)
	mapped, warnings, err := RewriteUnit(u, "App", "Target")
	require.NoError(t, err)
	require.Empty(t, warnings)
	actual, err := ExecutionSQL(context.Background(), Object{Kind: "db_link"}, mapped)
	require.NoError(t, err)
	for _, secret := range []string{"driver-secret", "password-secret", "endpoint-secret", "option-secret"} {
		require.Contains(t, actual, secret)
		require.NotContains(t, mapped.SQL, secret)
	}
	p := &Preflight{Objects: []Object{{Kind: "db_link", Units: []Unit{mapped}}}, Snapshot: &Snapshot{Objects: []Object{{Units: []Unit{mapped}}}}}
	encoded, err := json.Marshal(p)
	require.NoError(t, err)
	for _, secret := range []string{"driver-secret", "password-secret", "endpoint-secret", "option-secret"} {
		require.NotContains(t, string(encoded), secret)
	}
	var reloaded Preflight
	require.NoError(t, json.Unmarshal(encoded, &reloaded))
	frozen := reloaded.Objects[0].Units[0]
	hydrated, err := HydrateLinkUnit(frozen, ddl, "App", "Target")
	require.NoError(t, err)
	require.True(t, LinkUnitsEquivalent(mapped, hydrated))
	for _, change := range []string{"password-secret", "endpoint-secret", "option-secret"} {
		_, err := HydrateLinkUnit(frozen, strings.Replace(ddl, change, "changed-secret", 1), "App", "Target")
		require.Error(t, err)
		require.NotContains(t, err.Error(), "changed-secret")
	}
}

func TestLinkResolverCannotReplaceFrozenIdentityOrExposeErrors(t *testing.T) {
	u, err := FreezeLinkUnit(`CREATE LINK "Target"."L" CONNECT WITH U IDENTIFIED BY 'secret' USING 'host'`, "before_data")
	require.NoError(t, err)
	encoded, err := json.Marshal(u)
	require.NoError(t, err)
	var frozen Unit
	require.NoError(t, json.Unmarshal(encoded, &frozen))
	object := Object{Kind: "db_link", Schema: "Target", Name: "L"}
	for _, field := range []string{"sql", "hash", "phase", "actual"} {
		t.Run(field, func(t *testing.T) {
			ctx := WithLinkResolver(context.Background(), func(context.Context, Object, Unit) (Unit, error) {
				resolved := u
				switch field {
				case "sql":
					resolved.SQL += " tampered"
				case "hash":
					resolved.SourceSQLHash = "changed"
				case "phase":
					resolved.Phase = "restore"
				case "actual":
					resolved.executableSQL = strings.Replace(resolved.executableSQL, `"L"`, `"Other"`, 1)
				}
				return resolved, nil
			})
			_, err := ExecutionSQL(ctx, object, frozen)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret")
		})
	}
	ctx := WithLinkResolver(context.Background(), func(context.Context, Object, Unit) (Unit, error) {
		return Unit{}, fmt.Errorf("server echoed secret")
	})
	_, err = ExecutionSQL(ctx, object, frozen)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
}

func TestLinkCredentialsMustMatchForReconciliation(t *testing.T) {
	u, err := FreezeLinkUnit(`CREATE LINK "App"."L" CONNECT WITH U IDENTIFIED BY 'secret' USING 'host'`, "before_data")
	require.NoError(t, err)
	mapped, _, err := RewriteUnit(u, "App", "Target")
	require.NoError(t, err)
	for _, tc := range []struct {
		password, using string
		equal           bool
	}{{"secret", "host", true}, {"other", "host", false}, {"secret", "other", false}} {
		candidate, err := FreezeLinkUnit(`CREATE LINK "Target"."L" CONNECT WITH U IDENTIFIED BY '`+tc.password+`' USING '`+tc.using+`'`, "before_data")
		require.NoError(t, err)
		require.Equal(t, mapped.SQL, candidate.SQL)
		require.Equal(t, tc.equal, LinkUnitsEquivalent(mapped, candidate))
	}
	encoded, err := json.Marshal(mapped)
	require.NoError(t, err)
	var reloaded Unit
	require.NoError(t, json.Unmarshal(encoded, &reloaded))
	require.False(t, LinkUnitsEquivalent(mapped, reloaded))
}

func TestLinkHeadRecognitionDoesNotParseSensitiveTail(t *testing.T) {
	for _, statement := range []string{`CREATE LINK L IDENTIFIED BY 'unterminated`, `CREATE /* nested /* comment */ ok */ DATABASE LINK L`, `ALTER PUBLIC DBLINK L`, `-- comment\nCREATE LINK L`} {
		statement = strings.ReplaceAll(statement, `\n`, "\n")
		require.True(t, LooksLikeLinkDDL(statement), statement)
	}
	for _, statement := range []string{`CREATE TABLE LINK (ID INT)`, `CREATE PROCEDURE P AS BEGIN EXECUTE IMMEDIATE 'CREATE LINK L'; END;`, `COMMENT ON TABLE LINK IS 'x'`} {
		require.False(t, LooksLikeLinkDDL(statement), statement)
		_, err := PublicLinkSQL(statement)
		require.Error(t, err)
	}
}

func TestLinkMaskedOrIncompleteMetadataCannotMigrate(t *testing.T) {
	for _, credential := range []string{`'***'`, `?`, `'__DBGOLD_DBLINK_CREDENTIAL__'`, `VALUES 'cipher'`} {
		_, err := FreezeLinkUnit(`CREATE LINK L CONNECT 'DPI' WITH U IDENTIFIED BY `+credential+` USING 'host'`, "before_data")
		require.Error(t, err, credential)
	}
	_, err := FreezeLinkUnit(`CREATE PUBLIC LINK L CONNECT WITH U IDENTIFIED BY 'secret' USING 'host'`, "before_data")
	require.Error(t, err)
	_, err = FreezeLinkUnit(`CREATE LINK L CONNECT WITH U IDENTIFIED BY 'secret' USING 'host'; DROP TABLE T`, "before_data")
	require.Error(t, err)
	u, err := FreezeLinkUnit(`CREATE LINK L /* secret */ CONNECT WITH U IDENTIFIED BY 'secret' /* secret */ USING 'host'`, "before_data")
	require.NoError(t, err)
	require.NotContains(t, u.SQL, "secret")
}
