package target

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// Integration DSNs explicitly opt into SQL execution against disposable test
// databases. The native Kingbase test also creates and drops its own schema.
func openUUIDIntegrationDB(t *testing.T, variable string) *sql.DB {
	t.Helper()
	dsn := os.Getenv(variable)
	if dsn == "" {
		t.Skip(variable + " is not set")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx))
	return db
}

func TestUUIDNormalizerSQLIntegration(t *testing.T) {
	for _, database := range []struct{ name, variable string }{
		{"postgres", "DBGOLD_TEST_POSTGRES_DSN"},
		{"kingbase", "DBGOLD_TEST_KINGBASE_DSN"},
	} {
		t.Run(database.name, func(t *testing.T) {
			db := openUUIDIntegrationDB(t, database.variable)
			type sample struct {
				name  string
				input any
				want  string
				null  bool
				valid bool
			}
			const normalized = "0123456789abcdef0123456789abcdef"
			textSamples := []sample{
				{name: "lower_hex", input: normalized, want: normalized, valid: true},
				{name: "upper_hex", input: "0123456789ABCDEF0123456789ABCDEF", want: normalized, valid: true},
				{name: "lower_hyphens", input: "01234567-89ab-cdef-0123-456789abcdef", want: normalized, valid: true},
				{name: "upper_hyphens", input: "01234567-89AB-CDEF-0123-456789ABCDEF", want: normalized, valid: true},
				{name: "exterior_braces", input: "{01234567-89AB-CDEF-0123-456789ABCDEF}", want: normalized, valid: true},
				{name: "repeated_braces", input: "{{0123456789ABCDEF0123456789ABCDEF}}", want: normalized, valid: true},
				{name: "mixed_edge_braces", input: "}{0123456789ABCDEF0123456789ABCDEF}{", want: normalized, valid: true},
				{name: "null", input: nil, null: true},
				{name: "empty", input: "", want: ""},
				{name: "nonhex", input: "0123456789ABCDEZ0123456789ABCDEF", want: "0123456789abcdez0123456789abcdef"},
				{name: "short", input: "0123456789ABCDEF", want: "0123456789abcdef"},
				{name: "long", input: "0123456789ABCDEF0123456789ABCDEF0", want: "0123456789abcdef0123456789abcdef0"},
				{name: "internal_braces", input: "01234567{89ABCDEF0123456789ABCDEF}", want: "01234567{89abcdef0123456789abcdef"},
			}
			check := func(typ string, sample sample) {
				t.Run(typ+"/"+sample.name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					// Execute the production expression in SQL; expected values above
					// are literal fixtures, independent of the normalizer helper.
					query := "SELECT guid, COALESCE(guid ~ '^[0-9a-f]{32}$', false) FROM (SELECT " + uuidNormalizer(typ, "$1::"+typ) + " AS guid) normalized"
					var actual sql.NullString
					var valid bool
					require.NoError(t, db.QueryRowContext(ctx, query, sample.input).Scan(&actual, &valid))
					// Oracle-style Kingbase modes can represent an empty string
					// as NULL; either value must still fail UUID validation.
					if database.name != "kingbase" || sample.name != "empty" {
						require.Equal(t, !sample.null, actual.Valid)
					}
					require.Equal(t, sample.want, actual.String)
					require.Equal(t, sample.valid, valid, "SQL UUID validation")
					require.Equal(t, sample.valid, actual.Valid && uuidHex.MatchString(actual.String), "generator probe validation")
				})
			}
			for _, typ := range []string{"text", "name"} {
				for _, sample := range textSamples {
					check(typ, sample)
				}
			}
			for _, sample := range []sample{
				{name: "lower", input: "01234567-89ab-cdef-0123-456789abcdef", want: normalized, valid: true},
				{name: "upper", input: "01234567-89AB-CDEF-0123-456789ABCDEF", want: normalized, valid: true},
				{name: "braces", input: "{01234567-89AB-CDEF-0123-456789ABCDEF}", want: normalized, valid: true},
				{name: "null", input: nil, null: true},
			} {
				check("uuid", sample)
			}
			for _, sample := range []sample{
				{name: "binary16", input: []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}, want: normalized, valid: true},
				{name: "lower32", input: []byte(normalized), want: normalized, valid: true},
				{name: "upper32", input: []byte("0123456789ABCDEF0123456789ABCDEF"), want: normalized, valid: true},
				{name: "nonhex32", input: []byte("0123456789ABCDEZ0123456789ABCDEF"), want: "0123456789abcdez0123456789abcdef"},
				{name: "internal_brace32", input: []byte("01234567{9ABCDEF0123456789ABCDEF"), want: "01234567{9abcdef0123456789abcdef"},
				{name: "invalid_length", input: []byte("0123456789ABCDEF0"), null: true},
				{name: "null", input: nil, null: true},
			} {
				check("bytea", sample)
			}
		})
	}
}

func TestCompatibilityKingbaseNativeIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_KINGBASE_DSN")
	schema := fmt.Sprintf("dbgold_uuid_native_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, err := db.ExecContext(ctx, "CREATE SCHEMA "+source.QuotePGIdent(schema))
	cancel()
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := db.ExecContext(ctx, "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
		require.NoError(t, err)
	})
	w := &KingbaseWriter{db: db, schema: schema}
	for _, phase := range []string{"initialize", "reuse"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			report, err := EnsureCompatibilityFunctions(ctx, w)
			require.NoError(t, err)
			require.Equal(t, 4, report.Total)
			require.Equal(t, 4, report.Success)
			require.Zero(t, report.Failed)
			require.Len(t, report.Items, 4)
			require.Equal(t, "get_uuid", report.Items[0].Name)
			if phase == "reuse" {
				require.Equal(t, 4, report.Reused)
				require.Zero(t, report.Created)
			} else {
				require.Equal(t, 4, report.Created)
				require.Zero(t, report.Reused)
			}
			var first, second string
			function := qualifiedFunction(schema, "get_uuid")
			query := "SELECT " + function + "(), " + function + "()"
			require.NoError(t, db.QueryRowContext(ctx, query).Scan(&first, &second))
			require.Regexp(t, `^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`, first)
			require.Regexp(t, `^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`, second)
			require.NotEqual(t, first, second)
		})
	}
}
