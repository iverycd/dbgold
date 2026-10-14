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
// Success includes both newly created functions and existing functions reused.
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

func compatibilityRace(err error) bool {
	var state interface{ SQLState() string }
	code := ""
	if errors.As(err, &state) {
		code = state.SQLState()
	}
	var legacy interface{ Get(byte) string }
	if code == "" && errors.As(err, &legacy) {
		code = legacy.Get('C')
	}
	return code == "42723" || code == "42710" || code == "23505"
}

func ensurePGCompatibility(ctx context.Context, db *sql.DB, schema, product string) (*CompatibilityFunctionsReport, error) {
	var report *CompatibilityFunctionsReport
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		report, err = preparePGCompatibility(ctx, db, schema, product)
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

func preparePGCompatibility(ctx context.Context, db *sql.DB, schema, product string) (report *CompatibilityFunctionsReport, resultErr error) {
	report = &CompatibilityFunctionsReport{Items: []CompatibilityFunctionResult{
		{Name: "get_uuid", Schema: schema}, {Name: "GET_UUID", Schema: schema},
	}}
	defer func() { report.finish(resultErr) }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("GET_UUID 兼容函数初始化失败: %w", err)
	}
	defer tx.Rollback()
	if schema == "" {
		var current sql.NullString
		if err = tx.QueryRowContext(ctx, "SELECT current_schema()").Scan(&current); err != nil {
			return report, err
		}
		if !current.Valid || current.String == "" {
			return report, fmt.Errorf("GET_UUID 兼容函数初始化失败: 目标 current_schema() 为空")
		}
		schema = current.String
	}
	for i := range report.Items {
		report.Items[i].Schema = schema
	}
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
		return report, fmt.Errorf("探测 GET_UUID 系统目录失败: %w", catalogErr)
	}
	// Transaction-local only; extension installation defaults to the target schema.
	if _, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+source.QuotePGIdent(schema)+", "+catalog+", public"); err != nil {
		return report, err
	}
	existing := -1
	for i := range report.Items {
		item := &report.Items[i]
		var oid int64
		err = tx.QueryRowContext(ctx, `SELECT p.oid FROM `+catalog+`.pg_proc p JOIN `+catalog+`.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname=$2 AND p.pronargs=0 AND p.prorettype<>0`, schema, item.Name).Scan(&oid)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return report, fmt.Errorf("检查 %s.%s 失败: %w", schema, item.Name, err)
		}
		item.Status = "reused"
		existing = i
		definitionErr := compatibilityProbe(ctx, tx, func() error {
			return tx.QueryRowContext(ctx, `SELECT pg_get_functiondef($1::oid)`, oid).Scan(&item.DDL)
		})
		if definitionErr != nil {
			item.DDL = ""
			item.DefinitionWarning = "无法读取已有函数定义: " + definitionErr.Error()
		}
	}
	if report.Items[0].Status == "reused" && report.Items[1].Status == "reused" {
		return report, nil // Read-only discovery; the deferred rollback releases the transaction.
	}
	if existing >= 0 {
		for i := range report.Items {
			if report.Items[i].Status == "reused" {
				continue
			}
			item := &report.Items[i]
			item.Generator = qualifiedFunction(schema, report.Items[existing].Name)
			item.DDL = uuidWrapperDDL(schema, item.Name, item.Generator)
			if _, err = tx.ExecContext(ctx, item.DDL); err != nil {
				return report, fmt.Errorf("创建 %s.%s 失败: %w", schema, item.Name, err)
			}
			item.Status = "created"
		}
	} else {
		generator, typ, findErr := findUUIDGenerator(ctx, tx, schema, catalog, product, report)
		if findErr != nil {
			return report, findErr
		}
		item := &report.Items[0]
		item.Generator = generator
		item.DDL = uuidFunctionDDL(schema, item.Name, generator, typ)
		if _, err = tx.ExecContext(ctx, item.DDL); err != nil {
			return report, fmt.Errorf("创建 %s.%s 失败: %w", schema, item.Name, err)
		}
		item.Status = "created"
		alias := &report.Items[1]
		alias.Generator = qualifiedFunction(schema, item.Name)
		alias.DDL = uuidWrapperDDL(schema, alias.Name, alias.Generator)
		if _, err = tx.ExecContext(ctx, alias.DDL); err != nil {
			return report, fmt.Errorf("创建 %s.%s 失败: %w", schema, alias.Name, err)
		}
		alias.Status = "created"
	}
	if err = tx.Commit(); err != nil {
		for i := range report.Extensions {
			if report.Extensions[i].Status == "installed" {
				report.Extensions[i].Status = "unknown"
				report.Extensions[i].Error = err.Error()
			}
		}
		return report, fmt.Errorf("提交 GET_UUID 初始化事务失败，执行结果需核验: %w", err)
	}
	return report, nil
}

func qualifiedFunction(schema, name string) string {
	return source.QuotePGIdent(schema) + "." + source.QuotePGIdent(name)
}
func sqlString(s string) string {
	return "E'" + strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "''") + "'"
}

func uuidNormalizer(typ, value string) string {
	if typ == "bytea" {
		value = fmt.Sprintf("CASE WHEN octet_length(%s)=16 THEN encode(%s,'hex') WHEN octet_length(%s)=32 THEN convert_from(%s,'UTF8') ELSE NULL END", value, value, value, value)
	} else {
		value = "CAST(" + value + " AS text)"
	}
	return "lower(replace(trim(both '{}' from (" + value + ")), '-', ''))"
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
	return "CREATE FUNCTION " + qualifiedFunction(schema, name) + "() RETURNS varchar LANGUAGE plpgsql VOLATILE AS " + sqlString(body)
}
func uuidWrapperDDL(schema, name, generator string) string {
	return "CREATE FUNCTION " + qualifiedFunction(schema, name) + "() RETURNS varchar LANGUAGE sql VOLATILE AS " + sqlString("SELECT CAST("+generator+"() AS varchar)")
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
	return "", "", fmt.Errorf("GET_UUID 兼容函数初始化失败 [%s]: 无可用 UUID 函数；%s", schema, strings.Join(attempts, "；"))
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
		return fmt.Errorf("HighGo GET_UUID 初始化失败，执行 %s: %w", extension.SQL, err)
	}
	extension.Status = "installed"
	// A concurrent installer may have chosen another schema. Always resolve it.
	err = tx.QueryRowContext(ctx, `SELECT n.nspname FROM `+catalog+`.pg_extension e JOIN `+catalog+`.pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='uuid-ossp'`).Scan(&extension.Schema)
	report.Extensions = append(report.Extensions, extension)
	return err
}
