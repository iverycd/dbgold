package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"dbgold/datamigrate/source"
)

const compatibilityProcedureName = "changedatatohistory"
const compatibilityProcedureSignature = "changedatatohistory(p_processversioninstanceguid varchar)"
const compatibilityMainProcedureName = "changedatatomain"
const compatibilityMainProcedureSignature = "changedatatomain(p_processversioninstanceguid varchar)"
const compatibilityCreatePVIProcedureName = "createpvi"
const compatibilityCreatePVIProcedureSignature = "createpvi(p_processguid varchar, p_processversioninstanceguid varchar, p_userguid varchar, p_username varchar, p_workitemguid varchar, p_ouguid varchar, INOUT p_handleurl varchar DEFAULT NULL)"

type compatibilityProcedureSpec struct {
	name, signature string
	inputs          []string
	modes           string
	minDefaults     int
	maxDefaults     int
	writableCTE     bool
	ddl             func(schema, catalog string, native bool) string
}

var compatibilityProcedureSpecs = []compatibilityProcedureSpec{
	{name: compatibilityProcedureName, signature: compatibilityProcedureSignature, inputs: []string{"character varying"}, modes: "i", maxDefaults: 1, writableCTE: true, ddl: changeDataToHistoryDDL},
	{name: compatibilityMainProcedureName, signature: compatibilityMainProcedureSignature, inputs: []string{"character varying"}, modes: "i", maxDefaults: 1, writableCTE: true, ddl: changeDataToMainDDL},
	{name: compatibilityCreatePVIProcedureName, signature: compatibilityCreatePVIProcedureSignature,
		inputs: []string{"character varying", "character varying", "character varying", "character varying", "character varying", "character varying", "character varying"},
		modes:  "i,i,i,i,i,i,b", minDefaults: 1, maxDefaults: 1, ddl: createPVIDDL},
}

func (s compatibilityProcedureSpec) inputPredicate() string {
	predicate := fmt.Sprintf("p.pronargs=%d", len(s.inputs))
	for i, typ := range s.inputs {
		predicate += fmt.Sprintf(" AND p.proargtypes[%d]='%s'::regtype", i, typ)
	}
	return predicate + " AND p.prokind='p'"
}

func (s compatibilityProcedureSpec) identityArguments() string {
	return strings.Join(s.inputs, ",")
}

func (s compatibilityProcedureSpec) compatibleMetadata(defaults int, variadic int64, modes string) bool {
	if modes == "" {
		modes = strings.TrimSuffix(strings.Repeat("i,", len(s.inputs)), ",")
	}
	return variadic == 0 && modes == s.modes && defaults >= s.minDefaults && defaults <= s.maxDefaults
}

var compatibilityProcedureProbeSequence atomic.Uint64

// Procedures have different catalog, signature and syntax capabilities from
// functions. Keep this transaction independent so unsupported older engines
// retain all compatibility functions that were initialized successfully.
func ensurePGProcedureCompatibility(ctx context.Context, db *sql.DB, schema, product string) (*CompatibilityFunctionsReport, error) {
	report := &CompatibilityFunctionsReport{}
	var failures []error
	for _, spec := range compatibilityProcedureSpecs {
		itemReport, err := ensurePGProcedureSpecCompatibility(ctx, db, schema, product, spec)
		report.Items = append(report.Items, itemReport.Items...)
		if schema == "" && len(itemReport.Items) > 0 {
			schema = itemReport.Items[0].Schema
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	report.finish(nil)
	return report, errors.Join(failures...)
}

func ensurePGProcedureSpecCompatibility(ctx context.Context, db *sql.DB, schema, product string, spec compatibilityProcedureSpec) (*CompatibilityFunctionsReport, error) {
	var report *CompatibilityFunctionsReport
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		report, err = preparePGProcedureCompatibility(ctx, db, schema, product, spec)
		if err == nil || !compatibilityRace(err) || ctx.Err() != nil {
			break
		}
	}
	return report, err
}

func preparePGProcedureCompatibility(ctx context.Context, db *sql.DB, schema, product string, spec compatibilityProcedureSpec) (report *CompatibilityFunctionsReport, resultErr error) {
	report = &CompatibilityFunctionsReport{Items: []CompatibilityFunctionResult{{
		Name: spec.name, Kind: "procedure", Signature: spec.signature, Schema: schema,
	}}}
	defer func() { report.finish(resultErr) }()
	item := &report.Items[0]
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("%s 兼容存储过程初始化失败: %w", item.Name, err)
	}
	defer tx.Rollback()
	if schema == "" {
		var current sql.NullString
		if err = tx.QueryRowContext(ctx, "SELECT current_schema()").Scan(&current); err != nil {
			return report, fmt.Errorf("解析 %s 目标 schema 失败: %w", item.Name, err)
		}
		if !current.Valid || current.String == "" {
			return report, fmt.Errorf("%s 兼容存储过程初始化失败: 目标 current_schema() 为空", item.Name)
		}
		schema = current.String
	}
	item.Schema = schema
	catalog, err := compatibilityCatalog(ctx, tx)
	if err != nil {
		return report, fmt.Errorf("探测 %s 系统目录失败: %w", item.Name, err)
	}
	if _, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+source.QuotePGIdent(schema)+", "+catalog+", public"); err != nil {
		return report, err
	}
	var hasProcedureKind bool
	query := `SELECT EXISTS (SELECT 1 FROM ` + catalog + `.pg_attribute WHERE attrelid='` + catalog + `.pg_proc'::regclass AND attname='prokind' AND attnum>0 AND NOT attisdropped)`
	if err = tx.QueryRowContext(ctx, query).Scan(&hasProcedureKind); err != nil {
		return report, fmt.Errorf("检查 %s 存储过程目录能力失败: %w", item.Name, err)
	}
	if !hasProcedureKind {
		return report, fmt.Errorf("%s 初始化失败: 目标系统目录缺少 prokind，无法安全区分存储过程与函数", item.Name)
	}
	selectMetadata := `SELECT p.oid,p.pronargdefaults,p.provariadic,COALESCE(` + source.QuotePGIdent(catalog) + `.array_to_string(p.proargmodes,','),'') FROM ` + catalog + `.pg_proc p JOIN ` + catalog + `.pg_namespace n ON n.oid=p.pronamespace WHERE `
	query = selectMetadata + `n.nspname=$1 AND p.proname=$2 AND ` + spec.inputPredicate()
	var oid, variadic int64
	var defaults int
	var modes string
	err = tx.QueryRowContext(ctx, query, schema, item.Name).Scan(&oid, &defaults, &variadic, &modes)
	if errors.Is(err, sql.ErrNoRows) {
		// Some compatibility modes resolve the canonical quoted name to an
		// uppercase catalog entry. Validate the resulting OID and schema again;
		// regprocedure itself does not distinguish functions from procedures.
		err = compatibilityProbe(ctx, tx, func() error {
			return tx.QueryRowContext(ctx, `SELECT $1::regprocedure::oid`, qualifiedFunction(schema, item.Name)+"("+spec.identityArguments()+")").Scan(&oid)
		})
		if compatibilitySQLState(err) == "42883" {
			err, oid = nil, 0
		} else if err == nil {
			query = selectMetadata + `p.oid=$1 AND n.nspname=$2 AND ` + spec.inputPredicate()
			err = tx.QueryRowContext(ctx, query, oid, schema).Scan(&oid, &defaults, &variadic, &modes)
			if errors.Is(err, sql.ErrNoRows) {
				return report, fmt.Errorf("已有 %s.%s 不是兼容的存储过程签名，已保留原定义", schema, item.Signature)
			}
		}
	}
	if err != nil {
		return report, fmt.Errorf("检查 %s.%s 失败: %w", schema, item.Signature, err)
	}
	var conflicting bool
	query = `SELECT EXISTS (SELECT 1 FROM ` + catalog + `.pg_proc p JOIN ` + catalog + `.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname=COALESCE((SELECT proname FROM ` + catalog + `.pg_proc WHERE oid=$3::oid),$2) AND p.oid<>$3::oid AND NOT (` + spec.inputPredicate() + `) AND ` + compatibilityProcedureConflictingCallsPredicate(catalog, spec) + `)`
	if err = tx.QueryRowContext(ctx, query, schema, item.Name, oid).Scan(&conflicting); err != nil {
		return report, fmt.Errorf("检查 %s.%s 的重载失败: %w", schema, item.Signature, err)
	}
	if conflicting {
		return report, fmt.Errorf("已有 %s.%s 重载使标准 varchar 或字符串字面量调用不能唯一指向目标过程，已保留原定义", schema, item.Name)
	}
	if oid != 0 {
		if !spec.compatibleMetadata(defaults, variadic, modes) {
			return report, fmt.Errorf("已有 %s.%s 的参数模式或默认参数结构不兼容，已保留原定义", schema, item.Signature)
		}
		item.Status = "reused"
		definitionErr := compatibilityProbe(ctx, tx, func() error {
			var err error
			item.DDL, err = readCompatibilityProcedureDefinition(ctx, tx, catalog, oid)
			return err
		})
		if definitionErr != nil {
			item.DDL = ""
			item.DefinitionWarning = "无法读取已有存储过程定义: " + definitionErr.Error()
		}
		return report, nil
	}
	if spec.writableCTE {
		if err = probeCompatibilityProcedureWritableCTE(ctx, tx, catalog); err != nil {
			return report, fmt.Errorf("%s 初始化失败: 无法验证 DELETE RETURNING 数据修改 CTE 能力: %w", item.Name, err)
		}
	}
	preferNative := product == "gaussdb" || product == "vastbase" || product == "gbase" || product == "seabox"
	var preferredErr error
	for attempt, native := range []bool{preferNative, !preferNative} {
		item.DDL = spec.ddl(schema, catalog, native)
		err = compatibilityProbe(ctx, tx, func() error {
			_, createErr := tx.ExecContext(ctx, item.DDL)
			return createErr
		})
		if err == nil {
			break
		}
		// Only a syntax error can justify trying the other procedure header.
		// Permission, schema and catalog errors retain their original meaning.
		if compatibilitySQLState(err) != "42601" || attempt == 1 {
			if preferredErr != nil {
				// Keep both diagnostics while wrapping only the terminal error:
				// its SQLSTATE still determines whether a bounded retry is safe.
				err = fmt.Errorf("首选存储过程语法失败: %v；备用语法失败: %w", preferredErr, err)
			}
			return report, fmt.Errorf("创建 %s.%s 失败: %w", schema, item.Signature, err)
		}
		preferredErr = err
	}
	if err = tx.Commit(); err != nil {
		return report, fmt.Errorf("提交 %s 初始化事务失败，执行结果需核验: %w", item.Name, err)
	}
	item.Status = "created"
	return report, nil
}

// Check both explicitly typed and unknown-string calls at every documented
// arity. Procedures count OUT positions as CALL arguments, while functions use
// input arguments only. Discovery never calls or plans a business routine.
func compatibilityProcedureConflictingCallsPredicate(catalog string, spec compatibilityProcedureSpec) string {
	cat := source.QuotePGIdent(catalog)
	argc := "CASE WHEN p.prokind='p' THEN COALESCE(" + cat + ".array_length(p.proallargtypes,1),p.pronargs) ELSE p.pronargs END"
	var calls []string
	for arity := len(spec.inputs) - spec.minDefaults; arity <= len(spec.inputs); arity++ {
		callable := fmt.Sprintf("((%s)>=%d OR p.provariadic<>0) AND (%s)-p.pronargdefaults-CASE WHEN p.provariadic<>0 THEN 1 ELSE 0 END<=%d", argc, arity, argc, arity)
		var typed, literal []string
		for i := 0; i < arity; i++ {
			argument := fmt.Sprintf("CASE WHEN p.prokind='p' THEN COALESCE(p.proallargtypes[%d],p.proargtypes[%d]) ELSE p.proargtypes[%d] END", i+1, i, i)
			argument = fmt.Sprintf("CASE WHEN p.provariadic<>0 AND %d>=(%s)-1 THEN p.provariadic ELSE %s END", i, argc, argument)
			typed = append(typed, "("+argument+")='"+spec.inputs[i]+"'::regtype")
			literal = append(literal, "EXISTS (SELECT 1 FROM "+catalog+".pg_type argument_type WHERE argument_type.oid=("+argument+") AND argument_type.typcategory='S')")
		}
		calls = append(calls, "("+callable+" AND (("+strings.Join(typed, " AND ")+") OR ("+strings.Join(literal, " AND ")+")))")
	}
	return "(" + strings.Join(calls, " OR ") + ")"
}

// The procedure defers business-table SQL until invocation, so CREATE alone
// cannot establish support for writable CTEs. Plan the same operation on an
// empty disposable table, without ANALYZE or CALL. Roll back the savepoint even
// after success so no scratch object survives this capability check.
func probeCompatibilityProcedureWritableCTE(ctx context.Context, tx *sql.Tx, catalog string) error {
	const savepoint = "procedure_capability_probe"
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		return err
	}
	name := source.QuotePGIdent(fmt.Sprintf("dbgold_procedure_probe_%d_%d", time.Now().UnixNano(), compatibilityProcedureProbeSequence.Add(1)))
	_, probeErr := tx.ExecContext(ctx, "CREATE TEMP TABLE "+name+" (probe_id integer) ON COMMIT DROP")
	if probeErr == nil {
		table := "pg_temp." + name
		count := source.QuotePGIdent(catalog) + ".count(*)"
		_, probeErr = tx.ExecContext(ctx, "EXPLAIN WITH moved AS (DELETE FROM "+table+" WHERE false RETURNING probe_id), archived AS (INSERT INTO "+table+" (probe_id) SELECT probe_id FROM moved RETURNING 1) SELECT (SELECT "+count+" FROM moved),(SELECT "+count+" FROM archived)")
	}
	_, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint)
	if rollbackErr != nil {
		return errors.Join(probeErr, rollbackErr)
	}
	_, releaseErr := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint)
	return errors.Join(probeErr, releaseErr)
}

// PostgreSQL returns one scalar column, while some derived engines expose a
// multi-column result containing a definition column. Read the actual result
// shape rather than reporting a reconstructed or guessed procedure body.
func readCompatibilityProcedureDefinition(ctx context.Context, tx *sql.Tx, catalog string, oid int64) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT * FROM `+source.QuotePGIdent(catalog)+`.pg_get_functiondef($1::oid)`, oid)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return "", err
	}
	index := -1
	if len(columns) == 1 {
		index = 0
	} else {
		for i, column := range columns {
			if strings.EqualFold(column, "definition") {
				if index >= 0 {
					return "", errors.New("存储过程定义结果包含多个 definition 字段")
				}
				index = i
			}
		}
	}
	if index < 0 {
		return "", errors.New("存储过程定义结果缺少 definition 字段")
	}
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return "", err
		}
		return "", sql.ErrNoRows
	}
	values := make([]any, len(columns))
	destinations := make([]any, len(columns))
	for i := range values {
		destinations[i] = &values[i]
	}
	if err = rows.Scan(destinations...); err != nil {
		return "", err
	}
	var definition string
	switch value := values[index].(type) {
	case string:
		definition = value
	case []byte:
		definition = string(value)
	default:
		return "", errors.New("存储过程定义不是有效文本")
	}
	if rows.Next() {
		return "", errors.New("存储过程定义返回了多行")
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(definition) == "" {
		return "", errors.New("存储过程定义为空")
	}
	return definition, nil
}
