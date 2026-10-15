package target

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestUUIDFunctionDollarQuotedDDL(t *testing.T) {
	// These complete SQL fixtures intentionally do not use the production
	// normalizer or quoting helpers to construct their expected bodies.
	const textDDL = `CREATE FUNCTION "app"."get_uuid"() RETURNS varchar LANGUAGE plpgsql VOLATILE AS $dbgold$DECLARE
 raw_guid text;
 guid text;
BEGIN
 raw_guid := "pg_catalog"."sys_guid"();
 guid := lower(replace(ltrim(rtrim((CAST(raw_guid AS text)), '{}'), '{}'), '-', ''));
 IF guid IS NULL OR guid !~ '^[0-9a-f]{32}$' THEN
  RAISE EXCEPTION 'UUID generator returned an unsupported value';
 END IF;
 RETURN substr(guid,1,8)||'-'||substr(guid,9,4)||'-'||substr(guid,13,4)||'-'||substr(guid,17,4)||'-'||substr(guid,21,12);
END;$dbgold$`
	const byteaDDL = `CREATE FUNCTION "app"."get_uuid"() RETURNS varchar LANGUAGE plpgsql VOLATILE AS $dbgold$DECLARE
 raw_guid bytea;
 guid text;
BEGIN
 raw_guid := "pg_catalog"."sys_guid"();
 guid := lower(replace(ltrim(rtrim((CASE WHEN octet_length(raw_guid)=16 THEN encode(raw_guid,'hex') WHEN octet_length(raw_guid)=32 THEN convert_from(raw_guid,'UTF8') ELSE NULL END), '{}'), '{}'), '-', ''));
 IF guid IS NULL OR guid !~ '^[0-9a-f]{32}$' THEN
  RAISE EXCEPTION 'UUID generator returned an unsupported value';
 END IF;
 RETURN substr(guid,1,8)||'-'||substr(guid,9,4)||'-'||substr(guid,13,4)||'-'||substr(guid,17,4)||'-'||substr(guid,21,12);
END;$dbgold$`
	for _, tt := range []struct{ typ, want string }{
		{"text", textDDL},
		{"name", textDDL},
		{"bytea", byteaDDL},
	} {
		t.Run(tt.typ, func(t *testing.T) {
			require.Equal(t, tt.want, uuidFunctionDDL("app", "get_uuid", `"pg_catalog"."sys_guid"`, tt.typ))
		})
	}
}

func TestUUIDFunctionDollarQuotedIdentifiers(t *testing.T) {
	const want = `CREATE FUNCTION "target'""\$$$dbgold$"."get'""\$$$dbgold_1$"() RETURNS varchar LANGUAGE plpgsql VOLATILE AS $dbgold_2$DECLARE
 raw_guid text;
 guid text;
BEGIN
 raw_guid := "native'""\$$$dbgold$"."sys'""\$$$dbgold_1$"();
 guid := lower(replace(ltrim(rtrim((CAST(raw_guid AS text)), '{}'), '{}'), '-', ''));
 IF guid IS NULL OR guid !~ '^[0-9a-f]{32}$' THEN
  RAISE EXCEPTION 'UUID generator returned an unsupported value';
 END IF;
 RETURN substr(guid,1,8)||'-'||substr(guid,9,4)||'-'||substr(guid,13,4)||'-'||substr(guid,17,4)||'-'||substr(guid,21,12);
END;$dbgold_2$`
	generator := qualifiedFunction(`native'"\$$$dbgold$`, `sys'"\$$$dbgold_1$`)
	require.Equal(t, want, uuidFunctionDDL(`target'"\$$$dbgold$`, `get'"\$$$dbgold_1$`, generator, "name"))

	// A delimiter in only the CREATE header cannot terminate the function body.
	ddl := uuidFunctionDDL("app$dbgold$", "get_uuid$dbgold_1$", `"native"."sys_guid"`, "text")
	require.Contains(t, ddl, "VOLATILE AS $dbgold$DECLARE")
	require.True(t, strings.HasSuffix(ddl, "END;$dbgold$"))
}

func TestUUIDWrapperDollarQuotedDDL(t *testing.T) {
	for _, tt := range []struct {
		name, schema, alias, generatorSchema, generatorName, want string
	}{
		{
			name:   "ordinary",
			schema: "app", alias: "GET_UUID", generatorSchema: "app", generatorName: "get_uuid",
			want: `CREATE FUNCTION "app"."GET_UUID"() RETURNS varchar LANGUAGE sql VOLATILE AS $dbgold$SELECT CAST("app"."get_uuid"() AS varchar)$dbgold$`,
		},
		{
			name:   "quoted_identifiers",
			schema: `app'"\$$`, alias: `GET'"\$$`, generatorSchema: `native'"\$$`, generatorName: `get'"\$$`,
			want: `CREATE FUNCTION "app'""\$$"."GET'""\$$"() RETURNS varchar LANGUAGE sql VOLATILE AS $dbgold$SELECT CAST("native'""\$$"."get'""\$$"() AS varchar)$dbgold$`,
		},
		{
			name:   "schema_delimiter_collision",
			schema: "app$dbgold$", alias: "GET_UUID", generatorSchema: "app$dbgold$", generatorName: "get_uuid",
			want: `CREATE FUNCTION "app$dbgold$"."GET_UUID"() RETURNS varchar LANGUAGE sql VOLATILE AS $dbgold_1$SELECT CAST("app$dbgold$"."get_uuid"() AS varchar)$dbgold_1$`,
		},
		{
			name:   "generator_delimiter_collision",
			schema: "app", alias: "GET_UUID", generatorSchema: "native$dbgold$", generatorName: "get_uuid$dbgold_1$",
			want: `CREATE FUNCTION "app"."GET_UUID"() RETURNS varchar LANGUAGE sql VOLATILE AS $dbgold_2$SELECT CAST("native$dbgold$"."get_uuid$dbgold_1$"() AS varchar)$dbgold_2$`,
		},
		{
			name:   "header_delimiters_only",
			schema: "app$dbgold$", alias: "GET_UUID$dbgold_1$", generatorSchema: "native", generatorName: "get_uuid",
			want: `CREATE FUNCTION "app$dbgold$"."GET_UUID$dbgold_1$"() RETURNS varchar LANGUAGE sql VOLATILE AS $dbgold$SELECT CAST("native"."get_uuid"() AS varchar)$dbgold$`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			generator := qualifiedFunction(tt.generatorSchema, tt.generatorName)
			require.Equal(t, tt.want, uuidWrapperDDL(tt.schema, tt.alias, generator))
		})
	}
}

func TestUUIDFunctionDollarQuotingPostgresIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	// Both delimiter candidates occur in the native function's qualified name.
	// Apostrophes, quotes and backslashes also have to survive the body unchanged.
	schema := fmt.Sprintf(`dbgold_q_%d_'"\$$$dbgold$`, time.Now().UnixNano())
	const nativeName = `sys'"\$$$dbgold_1$`
	const canonicalName = `get'"\$$`
	const aliasName = `GET'"\$$`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, "CREATE SCHEMA "+source.QuotePGIdent(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := db.ExecContext(ctx, "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
		require.NoError(t, err)
	})
	generator := qualifiedFunction(schema, nativeName)
	_, err = db.ExecContext(ctx, "CREATE FUNCTION "+generator+`() RETURNS text LANGUAGE sql AS $native$SELECT '{01234567-89AB-CDEF-0123-456789ABCDEF}'::text$native$`)
	require.NoError(t, err)
	mainDDL := uuidFunctionDDL(schema, canonicalName, generator, "text")
	require.Contains(t, mainDDL, "VOLATILE AS $dbgold_2$DECLARE")
	_, err = db.ExecContext(ctx, mainDDL)
	require.NoError(t, err)
	canonical := qualifiedFunction(schema, canonicalName)
	_, err = db.ExecContext(ctx, uuidWrapperDDL(schema, aliasName, canonical))
	require.NoError(t, err)
	var mainValue, aliasValue string
	err = db.QueryRowContext(ctx, "SELECT "+canonical+"(), "+qualifiedFunction(schema, aliasName)+"()").Scan(&mainValue, &aliasValue)
	require.NoError(t, err)
	const want = "01234567-89ab-cdef-0123-456789abcdef"
	require.Equal(t, want, mainValue)
	require.Equal(t, want, aliasValue)
	var body string
	err = db.QueryRowContext(ctx, `SELECT p.prosrc FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname=$2`, schema, canonicalName).Scan(&body)
	require.NoError(t, err)
	require.Contains(t, body, "raw_guid := "+generator+"();")
	require.Contains(t, body, "'{}'), '{}'), '-', ''))")
	require.Contains(t, body, "guid !~ '^[0-9a-f]{32}$'")
}
