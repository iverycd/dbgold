package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"dbgold/datamigrate/source"
)

// CompatibilityFunctionsReport is an immutable snapshot of target preparation.
// Success includes newly created and reused functions or procedures. Keep the
// existing report name and wire key for older report consumers.
type CompatibilityFunctionsReport struct {
	Total      int                            `json:"total"`
	Success    int                            `json:"success"`
	Failed     int                            `json:"failed"`
	Created    int                            `json:"created"`
	Reused     int                            `json:"reused"`
	Items      []CompatibilityFunctionResult  `json:"items"`
	Extensions []CompatibilityExtensionResult `json:"extensions,omitempty"`
}

type CompatibilityFunctionResult struct {
	Name              string `json:"name"`
	Kind              string `json:"kind,omitempty"`
	Signature         string `json:"signature,omitempty"`
	Schema            string `json:"schema"`
	Status            string `json:"status"` // created | reused | failed
	Generator         string `json:"generator,omitempty"`
	DDL               string `json:"ddl"`
	Error             string `json:"error"`
	DefinitionWarning string `json:"definition_warning,omitempty"`
}

type CompatibilityExtensionResult struct {
	Name   string `json:"name"`
	Schema string `json:"schema"`
	Status string `json:"status"` // installed | reused | failed | rolled_back | unknown
	SQL    string `json:"sql,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Optional capability: existing Writer implementations and third-party writers
// do not acquire a new required method.
type compatibilityInitializer interface {
	EnsureCompatibilityFunctions(context.Context) (*CompatibilityFunctionsReport, error)
}

func EnsureCompatibilityFunctions(ctx context.Context, w Writer) (*CompatibilityFunctionsReport, error) {
	if initializer, ok := w.(compatibilityInitializer); ok && source.IsPGFamily(w.DBType()) {
		return initializer.EnsureCompatibilityFunctions(ctx)
	}
	return nil, nil
}

func (w *PostgresWriter) EnsureCompatibilityFunctions(ctx context.Context) (*CompatibilityFunctionsReport, error) {
	return ensurePGCompatibility(ctx, w.db, w.schema, w.DBType())
}
func (w *GaussDBWriter) EnsureCompatibilityFunctions(ctx context.Context) (*CompatibilityFunctionsReport, error) {
	return ensurePGCompatibility(ctx, w.db, w.schema, w.DBType())
}
func (w *HighGoWriter) EnsureCompatibilityFunctions(ctx context.Context) (*CompatibilityFunctionsReport, error) {
	return ensurePGCompatibility(ctx, w.db, w.schema, w.DBType())
}
func (w *KingbaseWriter) EnsureCompatibilityFunctions(ctx context.Context) (*CompatibilityFunctionsReport, error) {
	return ensurePGCompatibility(ctx, w.db, w.schema, w.DBType())
}

func uuidCandidates(product string) []string {
	switch product {
	case "kingbase":
		return []string{"sys_guid", "uuid", "gen_random_uuid", "uuid_generate_v4"}
	case "gaussdb", "vastbase", "gbase", "seabox":
		return []string{"uuid", "sys_guid", "gen_random_uuid", "uuid_generate_v4"}
	default:
		return []string{"gen_random_uuid", "uuid_generate_v4", "sys_guid", "uuid"}
	}
}

func compatibilitySQLState(err error) string {
	// A probe can join its query error with a savepoint cleanup failure.
	// Never classify that combined failure by only the query's SQL state.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 1 {
			return compatibilitySQLState(causes[0])
		}
		return ""
	}
	if state, ok := err.(interface{ SQLState() string }); ok {
		if code := state.SQLState(); code != "" {
			return code
		}
	}
	if legacy, ok := err.(interface{ Get(byte) string }); ok {
		return legacy.Get('C')
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return compatibilitySQLState(wrapped.Unwrap())
	}
	return ""
}

func compatibilityRace(err error) bool {
	code := compatibilitySQLState(err)
	return code == "42723" || code == "42710" || code == "23505"
}

func ensurePGCompatibility(ctx context.Context, db *sql.DB, schema, product string) (*CompatibilityFunctionsReport, error) {
	report, uuidErr := ensurePGUUIDCompatibility(ctx, db, schema, product)
	var failures []error
	if uuidErr != nil {
		failures = append(failures, uuidErr)
	}
	// Resolve a default schema once so all routines belong to the same target,
	// even though each routine owns an independent transaction.
	if schema == "" && len(report.Items) > 0 {
		schema = report.Items[0].Schema
	}
	for _, spec := range builtinCompatibilitySpecs {
		itemReport, err := ensurePGBuiltinCompatibility(ctx, db, schema, spec)
		report.Items = append(report.Items, itemReport.Items...)
		if schema == "" && len(itemReport.Items) > 0 {
			schema = itemReport.Items[0].Schema
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	procedureReport, procedureErr := ensurePGProcedureCompatibility(ctx, db, schema, product)
	report.Items = append(report.Items, procedureReport.Items...)
	if procedureErr != nil {
		failures = append(failures, procedureErr)
	}
	// Per-routine reports have already accounted for their own rollback. An
	// unrelated failure must not relabel committed routines or UUID extensions.
	report.finish(nil)
	return report, errors.Join(failures...)
}

func ensurePGUUIDCompatibility(ctx context.Context, db *sql.DB, schema, product string) (*CompatibilityFunctionsReport, error) {
	var report *CompatibilityFunctionsReport
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		report, err = preparePGUUIDCompatibility(ctx, db, schema, product)
		if err == nil || !compatibilityRace(err) || ctx.Err() != nil {
			break
		}
	}
	return report, err
}

func (r *CompatibilityFunctionsReport) finish(err error) {
	r.Total, r.Success, r.Failed, r.Created, r.Reused = len(r.Items), 0, 0, 0, 0
	for i := range r.Items {
		item := &r.Items[i]
		if item.Status != "reused" && err != nil {
			item.Status = "failed"
			item.Error = err.Error()
		}
		switch item.Status {
		case "created":
			r.Created++
			r.Success++
		case "reused":
			r.Reused++
			r.Success++
		default:
			r.Failed++
		}
	}
	if err != nil {
		for i := range r.Extensions {
			if r.Extensions[i].Status == "installed" {
				r.Extensions[i].Status = "rolled_back"
				r.Extensions[i].Error = err.Error()
			}
		}
	}
}

// Failed probes must not poison the surrounding PostgreSQL transaction.
func compatibilityProbe(ctx context.Context, tx *sql.Tx, fn func() error) error {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT uuid_probe"); err != nil {
		return err
	}
	err := fn()
	if err != nil {
		if _, e := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT uuid_probe"); e != nil {
			return errors.Join(err, e)
		}
	}
	_, releaseErr := tx.ExecContext(ctx, "RELEASE SAVEPOINT uuid_probe")
	return errors.Join(err, releaseErr)
}

func preparePGUUIDCompatibility(ctx context.Context, db *sql.DB, schema, product string) (report *CompatibilityFunctionsReport, resultErr error) {
	report = &CompatibilityFunctionsReport{Items: []CompatibilityFunctionResult{
		{Name: "get_uuid", Schema: schema},
	}}
	defer func() { report.finish(resultErr) }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("get_uuid 兼容函数初始化失败: %w", err)
	}
	defer tx.Rollback()
	if schema == "" {
		var current sql.NullString
		if err = tx.QueryRowContext(ctx, "SELECT current_schema()").Scan(&current); err != nil {
			return report, err
		}
		if !current.Valid || current.String == "" {
			return report, fmt.Errorf("get_uuid 兼容函数初始化失败: 目标 current_schema() 为空")
		}
		schema = current.String
	}
	item := &report.Items[0]
	item.Schema = schema
	// Kingbase versions can expose their compatibility catalogs in sys_catalog.
	catalog := ""
	var catalogErr error
	for _, candidate := range []string{"pg_catalog", "sys_catalog"} {
		catalogErr = compatibilityProbe(ctx, tx, func() error {
			var exists bool
			return tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+candidate+".pg_proc)").Scan(&exists)
		})
		if catalogErr == nil {
			catalog = candidate
			break
		}
	}
	if catalog == "" {
		return report, fmt.Errorf("探测 get_uuid 系统目录失败: %w", catalogErr)
	}
	// Transaction-local only; extension installation defaults to the target schema.
	if _, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+source.QuotePGIdent(schema)+", "+catalog+", public"); err != nil {
		return report, err
	}
	var oid int64
	err = tx.QueryRowContext(ctx, `SELECT p.oid FROM `+catalog+`.pg_proc p JOIN `+catalog+`.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname=$2 AND p.pronargs=0 AND p.prorettype<>0`, schema, item.Name).Scan(&oid)
	if errors.Is(err, sql.ErrNoRows) {
		// Compatibility modes can resolve a lowercase call to an existing
		// uppercase catalog entry. Resolve only our schema-qualified lowercase
		// name, without creating or changing an uppercase alias.
		err = compatibilityProbe(ctx, tx, func() error {
			return tx.QueryRowContext(ctx, `SELECT $1::regprocedure::oid`, qualifiedFunction(schema, item.Name)+"()").Scan(&oid)
		})
		if compatibilitySQLState(err) == "42883" {
			err = nil // No function resolves to the lowercase call.
		}
	}
	if err != nil {
		return report, fmt.Errorf("检查 %s.%s 失败: %w", schema, item.Name, err)
	}
	if oid != 0 {
		item.Status = "reused"
		definitionErr := compatibilityProbe(ctx, tx, func() error {
			return tx.QueryRowContext(ctx, `SELECT pg_get_functiondef($1::oid)`, oid).Scan(&item.DDL)
		})
		if definitionErr != nil {
			item.DDL = ""
			item.DefinitionWarning = "无法读取已有函数定义: " + definitionErr.Error()
		}
		return report, nil // Read-only discovery; the deferred rollback releases the transaction.
	}
	generator, typ, findErr := findUUIDGenerator(ctx, tx, schema, catalog, product, report)
	if findErr != nil {
		return report, findErr
	}
	item.Generator = generator
	item.DDL = uuidFunctionDDL(schema, item.Name, generator, typ)
	if _, err = tx.ExecContext(ctx, item.DDL); err != nil {
		return report, fmt.Errorf("创建 %s.%s 失败: %w", schema, item.Name, err)
	}
	item.Status = "created"
	if err = tx.Commit(); err != nil {
		for i := range report.Extensions {
			if report.Extensions[i].Status == "installed" {
				report.Extensions[i].Status = "unknown"
				report.Extensions[i].Error = err.Error()
			}
		}
		return report, fmt.Errorf("提交 get_uuid 初始化事务失败，执行结果需核验: %w", err)
	}
	return report, nil
}

func qualifiedFunction(schema, name string) string {
	return source.QuotePGIdent(schema) + "." + source.QuotePGIdent(name)
}

// sqlFunctionBody preserves quotes and backslashes without escape-string parsing.
func sqlFunctionBody(body string) string {
	delimiter := "$dbgold$"
	for suffix := 1; strings.Contains(body, delimiter); suffix++ {
		delimiter = fmt.Sprintf("$dbgold_%d$", suffix)
	}
	return delimiter + body + delimiter
}

func uuidNormalizer(typ, value string) string {
	if typ == "bytea" {
		value = fmt.Sprintf("CASE WHEN octet_length(%s)=16 THEN encode(%s,'hex') WHEN octet_length(%s)=32 THEN convert_from(%s,'UTF8') ELSE NULL END", value, value, value, value)
	} else {
		value = "CAST(" + value + " AS text)"
	}
	// Kingbase TRIM only accepts one character; LTRIM/RTRIM support a set.
	return "lower(replace(ltrim(rtrim((" + value + "), '{}'), '{}'), '-', ''))"
}

func uuidFunctionDDL(schema, name, generator, typ string) string {
	rawType := "text"
	if typ == "bytea" {
		rawType = "bytea"
	}
	body := fmt.Sprintf(`DECLARE
 raw_guid %s;
 guid text;
BEGIN
 raw_guid := %s();
 guid := %s;
 IF guid IS NULL OR guid !~ '^[0-9a-f]{32}$' THEN
  RAISE EXCEPTION 'UUID generator returned an unsupported value';
 END IF;
 RETURN substr(guid,1,8)||'-'||substr(guid,9,4)||'-'||substr(guid,13,4)||'-'||substr(guid,17,4)||'-'||substr(guid,21,12);
END;`, rawType, generator, uuidNormalizer(typ, "raw_guid"))
	return "CREATE FUNCTION " + qualifiedFunction(schema, name) + "() RETURNS varchar LANGUAGE plpgsql VOLATILE AS " + sqlFunctionBody(body)
}

var uuidHex = regexp.MustCompile(`^[0-9a-f]{32}$`)

type uuidGenerator struct{ schema, typ string }

func listUUIDGenerators(ctx context.Context, tx *sql.Tx, catalog, schema, name string) ([]uuidGenerator, error) {
	rows, err := tx.QueryContext(ctx, `SELECT n.nspname,t.typname FROM `+catalog+`.pg_proc p JOIN `+catalog+`.pg_namespace n ON n.oid=p.pronamespace JOIN `+catalog+`.pg_type t ON t.oid=p.prorettype WHERE p.proname=$1 AND p.pronargs=0 AND NOT p.proretset AND has_function_privilege(p.oid,'EXECUTE') AND has_schema_privilege(n.oid,'USAGE') AND n.nspname NOT LIKE 'pg_temp_%' ORDER BY CASE WHEN n.nspname=$2 THEN 0 WHEN n.nspname=$3 THEN 1 WHEN n.nspname='public' THEN 2 ELSE 3 END,n.nspname`, name, catalog, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuidGenerator
	for rows.Next() {
		var g uuidGenerator
		if err = rows.Scan(&g.schema, &g.typ); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func findUUIDGenerator(ctx context.Context, tx *sql.Tx, schema, catalog, product string, report *CompatibilityFunctionsReport) (string, string, error) {
	var attempts []string
	for _, name := range uuidCandidates(product) {
		if product == "highgo" && name == "uuid_generate_v4" {
			if err := prepareUUIDExtension(ctx, tx, catalog, schema, report); err != nil {
				return "", "", err
			}
		}
		var candidates []uuidGenerator
		err := compatibilityProbe(ctx, tx, func() error {
			var e error
			candidates, e = listUUIDGenerators(ctx, tx, catalog, schema, name)
			return e
		})
		if err != nil {
			attempts = append(attempts, name+": "+err.Error())
			continue
		}
		if len(candidates) == 0 {
			attempts = append(attempts, name+": 不存在或无执行/模式访问权限")
		}
		for _, candidate := range candidates {
			full := qualifiedFunction(candidate.schema, name)
			var sample sql.NullString
			err = compatibilityProbe(ctx, tx, func() error {
				return tx.QueryRowContext(ctx, "SELECT "+uuidNormalizer(candidate.typ, "v")+" FROM (SELECT "+full+"() AS v OFFSET 0) uuid_sample").Scan(&sample)
			})
			if err == nil && sample.Valid && uuidHex.MatchString(sample.String) {
				return full, candidate.typ, nil
			}
			if err == nil {
				err = fmt.Errorf("返回值不是 32 位十六进制 UUID")
			}
			attempts = append(attempts, full+": "+err.Error())
		}
	}
	return "", "", fmt.Errorf("get_uuid 兼容函数初始化失败 [%s]: 无可用 UUID 函数；%s", schema, strings.Join(attempts, "；"))
}

func prepareUUIDExtension(ctx context.Context, tx *sql.Tx, catalog, schema string, report *CompatibilityFunctionsReport) error {
	extension := CompatibilityExtensionResult{Name: "uuid-ossp", Schema: schema, Status: "failed"}
	// uuid-ossp is database-wide, so serialize installers across target schemas.
	// These database-local transaction lock keys identify dbgold/uuid preparation.
	lockSQL := "SELECT " + catalog + ".pg_advisory_xact_lock(1684170607, 1970628964)"
	if _, err := tx.ExecContext(ctx, lockSQL); err != nil {
		extension.Error = "锁定 uuid-ossp 扩展初始化失败: " + err.Error()
		report.Extensions = append(report.Extensions, extension)
		return fmt.Errorf("%s: %w", extension.Error, err)
	}
	var installedSchema string
	err := tx.QueryRowContext(ctx, `SELECT n.nspname FROM `+catalog+`.pg_extension e JOIN `+catalog+`.pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='uuid-ossp'`).Scan(&installedSchema)
	if err == nil {
		extension.Schema, extension.Status = installedSchema, "reused"
		report.Extensions = append(report.Extensions, extension)
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		extension.Error = err.Error()
		report.Extensions = append(report.Extensions, extension)
		return err
	}
	extension.SQL = `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`
	_, err = tx.ExecContext(ctx, extension.SQL)
	if err != nil {
		extension.Error = err.Error()
		report.Extensions = append(report.Extensions, extension)
		return fmt.Errorf("HighGo get_uuid 初始化失败，执行 %s: %w", extension.SQL, err)
	}
	extension.Status = "installed"
	// A concurrent installer may have chosen another schema. Always resolve it.
	err = tx.QueryRowContext(ctx, `SELECT n.nspname FROM `+catalog+`.pg_extension e JOIN `+catalog+`.pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='uuid-ossp'`).Scan(&extension.Schema)
	report.Extensions = append(report.Extensions, extension)
	return err
}
