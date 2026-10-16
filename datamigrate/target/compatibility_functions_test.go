package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	opengauss "gitee.com/opengauss/openGauss-connector-go-pq"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func expectUUIDProbe(m sqlmock.Sqlmock, failed bool) {
	if failed {
		m.ExpectExec("ROLLBACK TO SAVEPOINT uuid_probe").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectExec("RELEASE SAVEPOINT uuid_probe").WillReturnResult(sqlmock.NewResult(0, 0))
}
func expectUUIDSavepoint(m sqlmock.Sqlmock) {
	m.ExpectExec("SAVEPOINT uuid_probe").WillReturnResult(sqlmock.NewResult(0, 0))
}
func expectUUIDHeader(m sqlmock.Sqlmock, schema string) {
	m.ExpectBegin()
	if schema == "" {
		m.ExpectQuery(regexp.QuoteMeta("SELECT current_schema()")).WillReturnRows(sqlmock.NewRows([]string{"schema"}).AddRow("app"))
		schema = "app"
	}
	expectUUIDSavepoint(m)
	m.ExpectQuery("SELECT EXISTS .*pg_catalog.pg_proc").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	expectUUIDProbe(m, false)
	m.ExpectExec(regexp.QuoteMeta("SET LOCAL search_path TO " + source.QuotePGIdent(schema) + ", pg_catalog, public")).WillReturnResult(sqlmock.NewResult(0, 0))
}
func expectUUIDExisting(m sqlmock.Sqlmock, schema, name string, exists bool) {
	q := m.ExpectQuery("SELECT p.oid FROM .*pg_proc").WithArgs(schema, name)
	if !exists {
		q.WillReturnRows(sqlmock.NewRows([]string{"oid"}))
		return
	}
	q.WillReturnRows(sqlmock.NewRows([]string{"oid"}).AddRow(42))
	expectUUIDSavepoint(m)
	m.ExpectQuery(regexp.QuoteMeta("SELECT pg_get_functiondef($1::oid)")).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow("EXISTING " + name))
	expectUUIDProbe(m, false)
}
func expectUUIDReused(m sqlmock.Sqlmock) {
	expectUUIDHeader(m, "app")
	expectUUIDExisting(m, "app", "get_uuid", true)
	m.ExpectRollback()
}
func expectBuiltinFunctionsReused(m sqlmock.Sqlmock, schema string) {
	for _, spec := range builtinCompatibilitySpecs {
		expectUUIDHeader(m, schema)
		m.ExpectQuery("SELECT p.oid,p.pronargdefaults,p.proretset,p.provariadic,p.prorettype FROM").
			WithArgs(schema, spec.name).
			WillReturnRows(sqlmock.NewRows([]string{"oid", "defaults", "set", "variadic", "return_type"}).AddRow(42, spec.minDefaults, false, 0, 1043))
		m.ExpectQuery("SELECT EXISTS .*p.proname=COALESCE").WithArgs(schema, spec.name, int64(42)).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		expectUUIDSavepoint(m)
		m.ExpectQuery(regexp.QuoteMeta("SELECT pg_get_functiondef($1::oid)")).WithArgs(int64(42)).
			WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow("EXISTING " + spec.name))
		expectUUIDProbe(m, false)
		m.ExpectRollback()
	}
}
func expectUUIDMissing(m sqlmock.Sqlmock, schema string) {
	expectUUIDHeader(m, schema)
	if schema == "" {
		schema = "app"
	}
	expectUUIDExisting(m, schema, "get_uuid", false)
	expectUUIDResolution(m, schema, 0, &pq.Error{Code: "42883", Message: "function does not exist"})
}
func expectUUIDResolution(m sqlmock.Sqlmock, schema string, oid int64, resolveErr error) {
	expectUUIDSavepoint(m)
	q := m.ExpectQuery(regexp.QuoteMeta(`SELECT $1::regprocedure::oid`)).
		WithArgs(qualifiedFunction(schema, "get_uuid") + "()")
	if resolveErr != nil {
		q.WillReturnError(resolveErr)
	} else {
		q.WillReturnRows(sqlmock.NewRows([]string{"oid"}).AddRow(oid))
	}
	expectUUIDProbe(m, resolveErr != nil)
}
func expectUUIDCandidate(m sqlmock.Sqlmock, name, schema, typ string, sampleErr error) {
	expectUUIDCandidateForSchema(m, "app", name, schema, typ, sampleErr)
}
func expectUUIDCandidateForSchema(m sqlmock.Sqlmock, targetSchema, name, schema, typ string, sampleErr error) {
	expectUUIDSavepoint(m)
	q := m.ExpectQuery("SELECT n.nspname,t.typname FROM .*pg_proc").WithArgs(name, "pg_catalog", targetSchema)
	rows := sqlmock.NewRows([]string{"schema", "type"})
	if schema != "" {
		rows.AddRow(schema, typ)
	}
	q.WillReturnRows(rows)
	expectUUIDProbe(m, false)
	if schema == "" {
		return
	}
	expectUUIDSavepoint(m)
	probe := m.ExpectQuery(regexp.QuoteMeta("SELECT " + uuidNormalizer(typ, "v") + " FROM (SELECT " + qualifiedFunction(schema, name) + "() AS v OFFSET 0) uuid_sample"))
	if sampleErr != nil {
		probe.WillReturnError(sampleErr)
	} else {
		probe.WillReturnRows(sqlmock.NewRows([]string{"guid"}).AddRow("0123456789abcdef0123456789abcdef"))
	}
	expectUUIDProbe(m, sampleErr != nil)
}
func expectUUIDCreates(m sqlmock.Sqlmock, generator, typ string, createErr error) {
	q := m.ExpectExec(regexp.QuoteMeta(uuidFunctionDDL("app", "get_uuid", generator, typ)))
	if createErr != nil {
		q.WillReturnError(createErr)
		m.ExpectRollback()
	} else {
		q.WillReturnResult(sqlmock.NewResult(0, 0))
		m.ExpectCommit()
	}
}

func TestUUIDCandidateOrder(t *testing.T) {
	for _, product := range []string{"postgres", "highgo", "gaussdb", "vastbase", "gbase", "seabox", "kingbase"} {
		want := []string{"gen_random_uuid", "uuid_generate_v4", "sys_guid", "uuid"}
		if product == "kingbase" {
			want = []string{"sys_guid", "uuid", "gen_random_uuid", "uuid_generate_v4"}
		}
		if product == "gaussdb" || product == "vastbase" || product == "gbase" || product == "seabox" {
			want = []string{"uuid", "sys_guid", "gen_random_uuid", "uuid_generate_v4"}
		}
		require.Equal(t, want, uuidCandidates(product), product)
	}
}
func TestCompatibilityNativeUUIDAcrossProducts(t *testing.T) {
	for _, product := range []string{"postgres", "highgo", "gaussdb", "vastbase", "gbase", "seabox", "kingbase"} {
		t.Run(product, func(t *testing.T) {
			w, m := repairPGMock(t)
			w.dbType = product
			expectUUIDMissing(m, "app")
			native := uuidCandidates(product)[0]
			expectUUIDCandidate(m, native, "native", "uuid", nil)
			expectUUIDCreates(m, qualifiedFunction("native", native), "uuid", nil)
			r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
			require.NoError(t, err)
			require.Equal(t, 1, r.Created)
			require.Equal(t, 1, r.Success)
			require.Zero(t, r.Failed)
			require.Empty(t, r.Extensions)
			require.Contains(t, r.Items[0].DDL, qualifiedFunction("native", native)+"()")
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestCompatibilityKingbaseSysGUIDUsesPortableNormalization(t *testing.T) {
	for _, typ := range []string{"name", "text", "bytea"} {
		t.Run(typ, func(t *testing.T) {
			w, m := repairPGMock(t)
			w.dbType = "kingbase"
			expectUUIDMissing(m, "app")
			expectUUIDSavepoint(m)
			m.ExpectQuery("SELECT n.nspname,t.typname FROM .*pg_proc").
				WithArgs("sys_guid", "pg_catalog", "app").
				WillReturnRows(sqlmock.NewRows([]string{"schema", "type"}).AddRow("pg_catalog", typ))
			expectUUIDProbe(m, false)

			// Keep these SQL expectations independent of the production generators:
			// PostgreSQL accepts multi-character TRIM, but Kingbase rejects it.
			probeExpression := "lower(replace(ltrim(rtrim((CAST(v AS text)), '{}'), '{}'), '-', ''))"
			ddlExpression := "lower(replace(ltrim(rtrim((CAST(raw_guid AS text)), '{}'), '{}'), '-', ''))"
			if typ == "bytea" {
				probeExpression = "lower(replace(ltrim(rtrim((CASE WHEN octet_length(v)=16 THEN encode(v,'hex') WHEN octet_length(v)=32 THEN convert_from(v,'UTF8') ELSE NULL END), '{}'), '{}'), '-', ''))"
				ddlExpression = "lower(replace(ltrim(rtrim((CASE WHEN octet_length(raw_guid)=16 THEN encode(raw_guid,'hex') WHEN octet_length(raw_guid)=32 THEN convert_from(raw_guid,'UTF8') ELSE NULL END), '{}'), '{}'), '-', ''))"
			}
			expectUUIDSavepoint(m)
			m.ExpectQuery(regexp.QuoteMeta("SELECT " + probeExpression + ` FROM (SELECT "pg_catalog"."sys_guid"() AS v OFFSET 0) uuid_sample`)).
				WillReturnRows(sqlmock.NewRows([]string{"guid"}).AddRow("0123456789abcdef0123456789abcdef"))
			expectUUIDProbe(m, false)
			expectUUIDCreates(m, `"pg_catalog"."sys_guid"`, typ, nil)

			r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
			require.NoError(t, err)
			require.Equal(t, 1, r.Created)
			require.Zero(t, r.Failed)
			require.Equal(t, `"pg_catalog"."sys_guid"`, r.Items[0].Generator)
			require.Contains(t, r.Items[0].DDL, "guid := "+ddlExpression+";")
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestCompatibilityExistingFunctionsAreNeverReplaced(t *testing.T) {
	w, m := repairPGMock(t)
	expectUUIDReused(m)
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Equal(t, 1, r.Reused)
	require.Zero(t, r.Created)
	require.Equal(t, "EXISTING get_uuid", r.Items[0].DDL)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestCompatibilityQuotesTargetSchema(t *testing.T) {
	w, m := repairPGMock(t)
	w.schema = `a"b'c\d`
	expectUUIDMissing(m, w.schema)
	expectUUIDCandidateForSchema(m, w.schema, "gen_random_uuid", "pg_catalog", "uuid", nil)
	ddl := uuidFunctionDDL(w.schema, "get_uuid", qualifiedFunction("pg_catalog", "gen_random_uuid"), "uuid")
	m.ExpectExec(regexp.QuoteMeta(ddl)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectCommit()
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Equal(t, 1, r.Total)
	require.Equal(t, 1, r.Created)
	require.Zero(t, r.Reused)
	require.Contains(t, r.Items[0].DDL, `CREATE FUNCTION "a""b'c\d"."get_uuid"()`)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestCompatibilityReadsDefaultSchema(t *testing.T) {
	w, m := repairPGMock(t)
	w.schema = ""
	expectUUIDMissing(m, "")
	expectUUIDCandidate(m, "gen_random_uuid", "pg_catalog", "uuid", nil)
	expectUUIDCreates(m, qualifiedFunction("pg_catalog", "gen_random_uuid"), "uuid", nil)
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Equal(t, "app", r.Items[0].Schema)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestCompatibilityFailedProbeFallsBackWithinTransaction(t *testing.T) {
	w, m := repairPGMock(t)
	expectUUIDMissing(m, "app")
	expectUUIDCandidate(m, "gen_random_uuid", "pg_catalog", "uuid", errors.New("permission denied"))
	expectUUIDCandidate(m, "uuid_generate_v4", "extensions", "uuid", nil)
	expectUUIDCreates(m, qualifiedFunction("extensions", "uuid_generate_v4"), "uuid", nil)
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Equal(t, `"extensions"."uuid_generate_v4"`, r.Items[0].Generator)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestCompatibilityByteaGeneratorAndRollback(t *testing.T) {
	w, m := repairPGMock(t)
	w.dbType = "kingbase"
	expectUUIDMissing(m, "app")
	expectUUIDCandidate(m, "sys_guid", "sys_catalog", "bytea", nil)
	expectUUIDCreates(m, qualifiedFunction("sys_catalog", "sys_guid"), "bytea", errors.New("cannot create function"))
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.ErrorContains(t, err, "cannot create function")
	require.Zero(t, r.Created)
	require.Zero(t, r.Success)
	require.Equal(t, 1, r.Failed)
	require.Contains(t, r.Items[0].DDL, "raw_guid bytea")
	require.Contains(t, r.Items[0].DDL, "encode(raw_guid")
	require.Contains(t, r.Items[0].DDL, "convert_from(raw_guid")
	require.NoError(t, m.ExpectationsWereMet())
}
func expectUUIDExtension(m sqlmock.Sqlmock, installed bool, installErr error) {
	m.ExpectExec("SELECT pg_catalog.pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 0))
	q := "SELECT n.nspname FROM .*pg_extension"
	rows := sqlmock.NewRows([]string{"schema"})
	if installed {
		rows.AddRow("extensions")
	}
	m.ExpectQuery(q).WillReturnRows(rows)
	if installed {
		return
	}
	e := m.ExpectExec(regexp.QuoteMeta(`CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`))
	if installErr != nil {
		e.WillReturnError(installErr)
		return
	}
	e.WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery(q).WillReturnRows(sqlmock.NewRows([]string{"schema"}).AddRow("extensions"))
}
func TestHighGoUUIDExtensionInstallReuseAndRollback(t *testing.T) {
	for _, state := range []string{"install", "reuse", "rollback", "denied", "missing files"} {
		t.Run(state, func(t *testing.T) {
			w, m := repairPGMock(t)
			w.dbType = "highgo"
			expectUUIDMissing(m, "app")
			expectUUIDCandidate(m, "gen_random_uuid", "", "", nil)
			var installErr, errorAfter error
			if state == "denied" {
				installErr = errors.New("permission denied to create extension")
			}
			if state == "missing files" {
				installErr = errors.New("uuid-ossp.control: No such file")
			}
			if state == "rollback" {
				errorAfter = errors.New("failed function creation")
			}
			expectUUIDExtension(m, state == "reuse", installErr)
			if installErr != nil {
				m.ExpectRollback()
			} else {
				expectUUIDCandidate(m, "uuid_generate_v4", "extensions", "uuid", nil)
				expectUUIDCreates(m, qualifiedFunction("extensions", "uuid_generate_v4"), "uuid", errorAfter)
			}
			r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
			if installErr != nil || errorAfter != nil {
				require.Error(t, err)
				require.Equal(t, 1, r.Failed)
				require.Zero(t, r.Created)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, r.Created)
			}
			require.Len(t, r.Extensions, 1)
			want := map[string]string{"install": "installed", "reuse": "reused", "rollback": "rolled_back", "denied": "failed", "missing files": "failed"}[state]
			require.Equal(t, want, r.Extensions[0].Status)
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
func TestCompatibilityConcurrentFunctionCreationRetriesDiscovery(t *testing.T) {
	w, m := repairPGMock(t)
	expectUUIDMissing(m, "app")
	expectUUIDCandidate(m, "gen_random_uuid", "pg_catalog", "uuid", nil)
	expectUUIDCreates(m, qualifiedFunction("pg_catalog", "gen_random_uuid"), "uuid", &pq.Error{Code: "42723", Message: "already exists"})
	expectUUIDReused(m)
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Equal(t, 1, r.Reused)
	require.Zero(t, r.Created)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestCompatibilityNoGeneratorReportsAllAttempts(t *testing.T) {
	w, m := repairPGMock(t)
	expectUUIDMissing(m, "app")
	for _, name := range uuidCandidates("postgres") {
		expectUUIDCandidate(m, name, "", "", nil)
	}
	m.ExpectRollback()
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.Error(t, err)
	require.Equal(t, 1, r.Failed)
	require.Zero(t, r.Success)
	for _, name := range uuidCandidates("postgres") {
		require.Contains(t, err.Error(), name)
	}
	require.NoError(t, m.ExpectationsWereMet())
}
func TestCompatibilityDefinitionReadFailureDoesNotInventDDL(t *testing.T) {
	w, m := repairPGMock(t)
	expectUUIDHeader(m, "app")
	m.ExpectQuery("SELECT p.oid FROM").WithArgs("app", "get_uuid").WillReturnRows(sqlmock.NewRows([]string{"oid"}).AddRow(42))
	expectUUIDSavepoint(m)
	m.ExpectQuery("SELECT pg_get_functiondef").WillReturnError(errors.New("definition denied"))
	expectUUIDProbe(m, true)
	m.ExpectRollback()
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Empty(t, r.Items[0].DDL)
	require.Contains(t, r.Items[0].DefinitionWarning, "definition denied")
	require.Equal(t, 1, r.Reused)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestCompatibilityCommitFailureDoesNotClaimCreated(t *testing.T) {
	w, m := repairPGMock(t)
	expectUUIDMissing(m, "app")
	expectUUIDCandidate(m, "gen_random_uuid", "pg_catalog", "uuid", nil)
	m.ExpectExec("CREATE FUNCTION").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectCommit().WillReturnError(errors.New("connection lost"))
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.ErrorContains(t, err, "需核验")
	require.Equal(t, 1, r.Failed)
	require.Zero(t, r.Created)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestCompatibilityViewRepairPreparationFailureStillExecutesViewSQL(t *testing.T) {
	for _, dependent := range []bool{false, true} {
		t.Run(fmt.Sprint(dependent), func(t *testing.T) {
			w, m := repairPGMock(t)
			for i := 0; i < 1+len(builtinCompatibilitySpecs); i++ {
				m.ExpectBegin().WillReturnError(errors.New("preparation unavailable"))
			}
			m.ExpectBegin()
			m.ExpectExec(regexp.QuoteMeta(`SET LOCAL search_path TO "app"`)).WillReturnResult(sqlmock.NewResult(0, 0))
			q := m.ExpectExec("CREATE VIEW")
			if dependent {
				q.WillReturnError(errors.New("function get_uuid does not exist"))
				m.ExpectRollback()
			} else {
				q.WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectCommit()
			}
			m.ExpectClose()
			statement := "CREATE VIEW app.v AS SELECT 1"
			if dependent {
				statement = "CREATE VIEW app.v AS SELECT get_uuid()"
			}
			units, err := ExecuteRepair(context.Background(), w, repair.Object{Kind: "view", Schema: "app"}, []repair.Unit{{SQL: statement, Selected: true}}, nil)
			if dependent {
				require.ErrorContains(t, err, "get_uuid does not exist")
				require.Equal(t, "failed", units[0].Status)
			} else {
				require.NoError(t, err)
				require.Equal(t, "succeeded", units[0].Status)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

// Opt-in real SQL verification; the DSN must point to a disposable test database.
func TestCompatibilityPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("DBGOLD_TEST_POSTGRES_DSN is not set")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, legacyUppercase := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_uppercase_%t", legacyUppercase), func(t *testing.T) {
			schema := fmt.Sprintf("dbgold_uuid_%d", time.Now().UnixNano())
			_, err := db.ExecContext(ctx, "CREATE SCHEMA "+source.QuotePGIdent(schema))
			require.NoError(t, err)
			defer db.ExecContext(context.Background(), "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
			var uppercaseBefore string
			if legacyUppercase {
				_, err = db.ExecContext(ctx, "CREATE FUNCTION "+qualifiedFunction(schema, "GET_UUID")+"() RETURNS varchar LANGUAGE sql AS $$SELECT 'legacy uppercase'::varchar$$")
				require.NoError(t, err)
				require.NoError(t, db.QueryRowContext(ctx, "SELECT pg_get_functiondef($1::regprocedure::oid)", qualifiedFunction(schema, "GET_UUID")+"()").Scan(&uppercaseBefore))
			}
			w := &PostgresWriter{db: db, schema: schema, dbType: "postgres", dia: dialect.NewPostgres("postgres")}
			r, err := EnsureCompatibilityFunctions(ctx, w)
			require.NoError(t, err)
			require.Equal(t, 1+len(builtinCompatibilitySpecs), r.Total)
			require.Equal(t, 1+len(builtinCompatibilitySpecs), r.Created)
			require.Zero(t, r.Reused)
			require.Len(t, r.Items, 1+len(builtinCompatibilitySpecs))
			require.Equal(t, "get_uuid", r.Items[0].Name)
			var count int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname IN ('get_uuid','GET_UUID')`, schema).Scan(&count))
			if legacyUppercase {
				require.Equal(t, 2, count)
				var uppercaseAfter, uppercaseValue string
				require.NoError(t, db.QueryRowContext(ctx, "SELECT pg_get_functiondef($1::regprocedure::oid)", qualifiedFunction(schema, "GET_UUID")+"()").Scan(&uppercaseAfter))
				require.Equal(t, uppercaseBefore, uppercaseAfter)
				require.NoError(t, db.QueryRowContext(ctx, "SELECT "+qualifiedFunction(schema, "GET_UUID")+"()").Scan(&uppercaseValue))
				require.Equal(t, "legacy uppercase", uppercaseValue)
			} else {
				require.Equal(t, 1, count)
			}
			view := source.ViewInfo{ViewName: "v", Definition: "SELECT get_uuid() AS a, GET_UUID() AS b"}
			require.NoError(t, w.CreateView(ctx, view))
			var a, b string
			require.NoError(t, db.QueryRowContext(ctx, "SELECT a,b FROM "+qualifiedFunction(schema, "v")).Scan(&a, &b))
			require.Regexp(t, `^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`, a)
			require.Regexp(t, `^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`, b)
			require.NotEqual(t, a, b)
			r, err = EnsureCompatibilityFunctions(ctx, w)
			require.NoError(t, err)
			require.Equal(t, 1+len(builtinCompatibilitySpecs), r.Total)
			require.Equal(t, 1+len(builtinCompatibilitySpecs), r.Reused)
			require.Zero(t, r.Created)
			require.True(t, strings.Contains(r.Items[0].DDL, "CREATE"))
		})
	}
}

func TestCompatibilityPostgresByteaAndConcurrencyIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("DBGOLD_TEST_POSTGRES_DSN is not set")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, kind := range []string{"text", "bytea16", "bytea32", "concurrent", "extension", "extension-concurrent"} {
		t.Run(kind, func(t *testing.T) {
			schema := fmt.Sprintf("dbgold_uuid_%d", time.Now().UnixNano())
			_, err := db.ExecContext(ctx, "CREATE SCHEMA "+source.QuotePGIdent(schema))
			require.NoError(t, err)
			defer db.ExecContext(context.Background(), "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
			if kind == "extension-concurrent" {
				type outcome struct {
					status string
					err    error
				}
				outcomes := make(chan outcome, 6)
				var wg sync.WaitGroup
				for i := 0; i < 6; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						tx, e := db.BeginTx(ctx, nil)
						if e != nil {
							outcomes <- outcome{err: e}
							return
						}
						defer tx.Rollback()
						_, e = tx.ExecContext(ctx, "SET LOCAL search_path TO "+source.QuotePGIdent(schema)+", pg_catalog")
						snapshot := &CompatibilityFunctionsReport{}
						if e == nil {
							e = prepareUUIDExtension(ctx, tx, "pg_catalog", schema, snapshot)
						}
						if e == nil {
							e = tx.Commit()
						}
						status := ""
						if len(snapshot.Extensions) > 0 {
							status = snapshot.Extensions[0].Status
						}
						outcomes <- outcome{status: status, err: e}
					}()
				}
				wg.Wait()
				close(outcomes)
				installed, reused := 0, 0
				for outcome := range outcomes {
					require.NoError(t, outcome.err)
					if outcome.status == "installed" {
						installed++
					}
					if outcome.status == "reused" {
						reused++
					}
				}
				require.Equal(t, 1, installed)
				require.Equal(t, 5, reused)
				return
			}
			if kind == "extension" {
				tx, err := db.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer tx.Rollback()
				_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+source.QuotePGIdent(schema)+", pg_catalog")
				require.NoError(t, err)
				snapshot := &CompatibilityFunctionsReport{}
				require.NoError(t, prepareUUIDExtension(ctx, tx, "pg_catalog", schema, snapshot))
				require.Equal(t, "installed", snapshot.Extensions[0].Status)
				require.Equal(t, schema, snapshot.Extensions[0].Schema)
				require.NoError(t, prepareUUIDExtension(ctx, tx, "pg_catalog", "other", snapshot))
				require.Equal(t, "reused", snapshot.Extensions[1].Status)
				require.Equal(t, schema, snapshot.Extensions[1].Schema)
				require.NoError(t, tx.Rollback())
				return
			}
			if kind != "concurrent" {
				expression := `upper(replace(pg_catalog.gen_random_uuid()::text,'-',''))`
				typ := "text"
				if kind == "bytea16" {
					typ = "bytea"
					expression = "decode(" + expression + ",'hex')"
				}
				if kind == "bytea32" {
					typ = "bytea"
					expression = "convert_to(" + expression + ",'UTF8')"
				}
				_, err = db.ExecContext(ctx, "CREATE FUNCTION "+qualifiedFunction(schema, "sys_guid")+"() RETURNS "+typ+" LANGUAGE sql VOLATILE AS "+sqlFunctionBody("SELECT "+expression))
				require.NoError(t, err)
				r, err := ensurePGUUIDCompatibility(ctx, db, schema, "kingbase")
				require.NoError(t, err)
				require.Equal(t, 1, r.Created)
				var a, b string
				require.NoError(t, db.QueryRowContext(ctx, "SELECT "+qualifiedFunction(schema, "get_uuid")+"(), "+qualifiedFunction(schema, "get_uuid")+"()").Scan(&a, &b))
				require.Regexp(t, `^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`, a)
				require.NotEqual(t, a, b)
			} else {
				var wg sync.WaitGroup
				type outcome struct {
					report *CompatibilityFunctionsReport
					err    error
				}
				outcomes := make(chan outcome, 6)
				for i := 0; i < 6; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						r, err := ensurePGUUIDCompatibility(ctx, db, schema, "postgres")
						if err == nil && r.Success != 1 {
							err = fmt.Errorf("unexpected snapshot: %+v", r)
						}
						outcomes <- outcome{report: r, err: err}
					}()
				}
				wg.Wait()
				close(outcomes)
				created := 0
				for outcome := range outcomes {
					require.NoError(t, outcome.err)
					created += outcome.report.Created
				}
				require.Equal(t, 1, created)
			}
		})
	}
}

func TestCompatibilityKingbaseCatalogFallback(t *testing.T) {
	w, m := repairPGMock(t)
	w.dbType = "kingbase"
	m.ExpectBegin()
	expectUUIDSavepoint(m)
	m.ExpectQuery("SELECT EXISTS .*pg_catalog.pg_proc").WillReturnError(errors.New("pg_catalog unavailable"))
	expectUUIDProbe(m, true)
	expectUUIDSavepoint(m)
	m.ExpectQuery("SELECT EXISTS .*sys_catalog.pg_proc").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	expectUUIDProbe(m, false)
	m.ExpectExec(regexp.QuoteMeta(`SET LOCAL search_path TO "app", sys_catalog, public`)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectUUIDExisting(m, "app", "get_uuid", true)
	m.ExpectRollback()
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Equal(t, 1, r.Reused)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestHighGoConcurrentExtensionCreationRetries(t *testing.T) {
	w, m := repairPGMock(t)
	w.dbType = "highgo"
	expectUUIDMissing(m, "app")
	expectUUIDCandidate(m, "gen_random_uuid", "", "", nil)
	expectUUIDExtension(m, false, &pq.Error{Code: "23505", Message: "extension created concurrently"})
	m.ExpectRollback()
	expectUUIDMissing(m, "app")
	expectUUIDCandidate(m, "gen_random_uuid", "", "", nil)
	expectUUIDExtension(m, true, nil)
	expectUUIDCandidate(m, "uuid_generate_v4", "extensions", "uuid", nil)
	expectUUIDCreates(m, qualifiedFunction("extensions", "uuid_generate_v4"), "uuid", nil)
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Equal(t, 1, r.Created)
	require.Equal(t, "reused", r.Extensions[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestCompatibilityCaseEquivalentExistingFunctionIsReused(t *testing.T) {
	w, m := repairPGMock(t)
	w.dbType = "gaussdb"
	expectUUIDHeader(m, "app")
	expectUUIDExisting(m, "app", "get_uuid", false)
	expectUUIDResolution(m, "app", 42, nil)
	expectUUIDSavepoint(m)
	m.ExpectQuery(regexp.QuoteMeta("SELECT pg_get_functiondef($1::oid)")).WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow("EXISTING GET_UUID"))
	expectUUIDProbe(m, false)
	m.ExpectRollback()
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Equal(t, 1, r.Total)
	require.Equal(t, 1, r.Reused)
	require.Zero(t, r.Created)
	require.Zero(t, r.Failed)
	require.Len(t, r.Items, 1)
	require.Equal(t, "get_uuid", r.Items[0].Name)
	require.Equal(t, "EXISTING GET_UUID", r.Items[0].DDL)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestCompatibilityFunctionResolutionFailureDoesNotCreate(t *testing.T) {
	w, m := repairPGMock(t)
	expectUUIDHeader(m, "app")
	expectUUIDExisting(m, "app", "get_uuid", false)
	expectUUIDResolution(m, "app", 0, &pq.Error{Code: "42501", Message: "permission denied"})
	m.ExpectRollback()
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.ErrorContains(t, err, "permission denied")
	require.Equal(t, 1, r.Total)
	require.Equal(t, 1, r.Failed)
	require.Zero(t, r.Created)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestCompatibilityMissingFunctionProbeCleanupFailureDoesNotCreate(t *testing.T) {
	for _, cleanup := range []string{"rollback", "release"} {
		t.Run(cleanup, func(t *testing.T) {
			w, m := repairPGMock(t)
			expectUUIDHeader(m, "app")
			expectUUIDExisting(m, "app", "get_uuid", false)
			expectUUIDSavepoint(m)
			m.ExpectQuery(regexp.QuoteMeta(`SELECT $1::regprocedure::oid`)).
				WithArgs(qualifiedFunction("app", "get_uuid") + "()").
				WillReturnError(&pq.Error{Code: "42883", Message: "function does not exist"})
			cleanupErr := errors.New(cleanup + " savepoint failed")
			if cleanup == "rollback" {
				m.ExpectExec("ROLLBACK TO SAVEPOINT uuid_probe").WillReturnError(cleanupErr)
			} else {
				m.ExpectExec("ROLLBACK TO SAVEPOINT uuid_probe").WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectExec("RELEASE SAVEPOINT uuid_probe").WillReturnError(cleanupErr)
			}
			m.ExpectRollback()
			r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
			require.ErrorIs(t, err, cleanupErr)
			require.Equal(t, 1, r.Total)
			require.Equal(t, 1, r.Failed)
			require.Zero(t, r.Created)
			require.Zero(t, r.Success)
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestCompatibilityOpenGaussMissingFunctionUsesSQLState(t *testing.T) {
	w, m := repairPGMock(t)
	w.dbType = "gaussdb"
	expectUUIDHeader(m, "app")
	expectUUIDExisting(m, "app", "get_uuid", false)
	expectUUIDResolution(m, "app", 0, &opengauss.Error{Code: "42883", Message: "function does not exist"})
	expectUUIDCandidate(m, "uuid", "pg_catalog", "uuid", nil)
	expectUUIDCreates(m, qualifiedFunction("pg_catalog", "uuid"), "uuid", nil)
	r, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Equal(t, 1, r.Total)
	require.Equal(t, 1, r.Created)
	require.Len(t, r.Items, 1)
	require.Equal(t, "get_uuid", r.Items[0].Name)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestCompatibilityViewRepairCancellationStillStopsSQL(t *testing.T) {
	w, m := repairPGMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ExecuteRepair(ctx, w, repair.Object{Kind: "view", Schema: "app"}, []repair.Unit{{SQL: "CREATE VIEW app.v AS SELECT 1", Selected: true}}, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, m.ExpectationsWereMet())
}
