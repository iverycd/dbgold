package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

type compatibilityFunctionSpec struct {
	name        string
	signature   string
	inputs      []string
	minDefaults int
}

var builtinCompatibilitySpecs = []compatibilityFunctionSpec{
	{name: "auto_increase_itemid", signature: "auto_increase_itemid()"},
	{name: "count_rows", signature: "count_rows(table_name varchar, owner varchar DEFAULT NULL)", inputs: []string{"character varying", "character varying"}, minDefaults: 1},
	{name: "fun_getpy", signature: "fun_getpy(p_name varchar)", inputs: []string{"character varying"}},
}

func (s compatibilityFunctionSpec) inputPredicate() string {
	predicate := fmt.Sprintf("p.pronargs=%d", len(s.inputs))
	// Input types are fixed application constants. Compare their resolved OIDs
	// rather than vendor-specific type names or implicit cast compatibility.
	for i, typ := range s.inputs {
		predicate += fmt.Sprintf(" AND p.proargtypes[%d]='%s'::regtype", i, typ)
	}
	// pg_proc changed these fields in PG 11. Reading optional catalog keys
	// avoids referencing a missing column on older PostgreSQL-derived engines.
	predicate += " AND COALESCE(row_to_json(p)->>'prokind','f')='f'" +
		" AND COALESCE(row_to_json(p)->>'proisagg','false')='false'" +
		" AND COALESCE(row_to_json(p)->>'proiswindow','false')='false'"
	return predicate
}

// Overloads with trailing defaults (or variadic arguments) can make a fully
// typed canonical call ambiguous even when its complete signature is unique.
// Inspect the catalog only; calling or planning a business function can execute
// user code. Check every documented arity, including count_rows(table_name).
func (s compatibilityFunctionSpec) conflictingCallsPredicate() string {
	var calls []string
	for argc := len(s.inputs) - s.minDefaults; argc <= len(s.inputs); argc++ {
		predicate := fmt.Sprintf("(p.pronargs>=%d OR p.provariadic<>0) AND p.pronargs-p.pronargdefaults-CASE WHEN p.provariadic<>0 THEN 1 ELSE 0 END<=%d", argc, argc)
		for i := 0; i < argc; i++ {
			predicate += fmt.Sprintf(" AND CASE WHEN p.provariadic<>0 AND %d>=p.pronargs-1 THEN p.provariadic ELSE p.proargtypes[%d] END='%s'::regtype", i, i, s.inputs[i])
		}
		calls = append(calls, "("+predicate+")")
	}
	return "(" + strings.Join(calls, " OR ") + ")"
}

func ensurePGBuiltinCompatibility(ctx context.Context, db *sql.DB, schema string, spec compatibilityFunctionSpec) (*CompatibilityFunctionsReport, error) {
	var report *CompatibilityFunctionsReport
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		report, err = preparePGBuiltinCompatibility(ctx, db, schema, spec)
		if err == nil || !compatibilityRace(err) || ctx.Err() != nil {
			break
		}
	}
	return report, err
}

// Compatibility initialization is deliberately per function: a failed pinyin
// function must never roll back a successfully committed UUID or row counter.
func preparePGBuiltinCompatibility(ctx context.Context, db *sql.DB, schema string, spec compatibilityFunctionSpec) (report *CompatibilityFunctionsReport, resultErr error) {
	report = &CompatibilityFunctionsReport{Items: []CompatibilityFunctionResult{{Name: spec.name, Signature: spec.signature, Schema: schema}}}
	defer func() { report.finish(resultErr) }()
	item := &report.Items[0]
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("%s 兼容函数初始化失败: %w", spec.name, err)
	}
	defer tx.Rollback()
	if schema == "" {
		var current sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT current_schema()").Scan(&current); err != nil {
			return report, fmt.Errorf("解析 %s 目标 schema 失败: %w", spec.name, err)
		}
		if !current.Valid || current.String == "" {
			return report, fmt.Errorf("%s 兼容函数初始化失败: 目标 current_schema() 为空", spec.name)
		}
		schema = current.String
	}
	item.Schema = schema
	catalog, err := compatibilityCatalog(ctx, tx)
	if err != nil {
		return report, fmt.Errorf("探测 %s 系统目录失败: %w", spec.name, err)
	}
	if _, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+source.QuotePGIdent(schema)+", "+catalog+", public"); err != nil {
		return report, err
	}
	query := `SELECT p.oid,p.pronargdefaults,p.proretset,p.provariadic,p.prorettype FROM ` + catalog + `.pg_proc p JOIN ` + catalog + `.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname=$2 AND ` + spec.inputPredicate()
	var oid, variadic, returnType int64
	var defaults int
	var returnsSet bool
	err = tx.QueryRowContext(ctx, query, schema, spec.name).Scan(&oid, &defaults, &returnsSet, &variadic, &returnType)
	if errors.Is(err, sql.ErrNoRows) {
		// Some Oracle compatibility modes fold a quoted lowercase call to an
		// existing catalog entry. Resolve only the exact, fully typed signature.
		err = compatibilityProbe(ctx, tx, func() error {
			return tx.QueryRowContext(ctx, `SELECT $1::regprocedure::oid`, qualifiedFunction(schema, spec.name)+"("+strings.Join(spec.inputs, ",")+")").Scan(&oid)
		})
		if compatibilitySQLState(err) == "42883" {
			err, oid = nil, 0
		} else if err == nil {
			query = `SELECT p.oid,p.pronargdefaults,p.proretset,p.provariadic,p.prorettype FROM ` + catalog + `.pg_proc p JOIN ` + catalog + `.pg_namespace n ON n.oid=p.pronamespace WHERE p.oid=$1 AND n.nspname=$2 AND ` + spec.inputPredicate()
			err = tx.QueryRowContext(ctx, query, oid, schema).Scan(&oid, &defaults, &returnsSet, &variadic, &returnType)
			if errors.Is(err, sql.ErrNoRows) {
				return report, fmt.Errorf("已有 %s.%s 不是兼容的普通标量函数，已保留原定义", schema, spec.signature)
			}
		}
	}
	if err != nil {
		return report, fmt.Errorf("检查 %s.%s 失败: %w", schema, spec.signature, err)
	}
	var conflicting bool
	// A concurrent initializer may commit the exact function after discovery.
	// It is not a conflicting overload: CREATE will detect that race and the
	// bounded retry will discover and reuse it in a fresh transaction.
	conflictQuery := `SELECT EXISTS (SELECT 1 FROM ` + catalog + `.pg_proc p JOIN ` + catalog + `.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname=COALESCE((SELECT proname FROM ` + catalog + `.pg_proc WHERE oid=$3::oid),$2) AND p.oid<>$3::oid AND NOT (` + spec.inputPredicate() + `) AND ` + spec.conflictingCallsPredicate() + `)`
	if err = tx.QueryRowContext(ctx, conflictQuery, schema, spec.name, oid).Scan(&conflicting); err != nil {
		return report, fmt.Errorf("检查 %s.%s 的重载失败: %w", schema, spec.signature, err)
	}
	if conflicting {
		return report, fmt.Errorf("已有 %s.%s 重载会导致默认参数或标准签名调用不明确，已保留原定义", schema, spec.name)
	}
	if oid != 0 {
		// Return types do not participate in an overload signature. Preserve
		// existing scalar business functions; reject set/void/variadic routines
		// and a counter that cannot be called with its optional owner omitted.
		if defaults < spec.minDefaults || returnsSet || variadic != 0 || returnType == 0 || returnType == 2278 {
			return report, fmt.Errorf("已有 %s.%s 的参数默认值或返回结构不兼容，已保留原定义", schema, spec.signature)
		}
		item.Status = "reused"
		definitionErr := compatibilityProbe(ctx, tx, func() error {
			return tx.QueryRowContext(ctx, `SELECT pg_get_functiondef($1::oid)`, oid).Scan(&item.DDL)
		})
		if definitionErr != nil {
			item.DDL = ""
			item.DefinitionWarning = "无法读取已有函数定义: " + definitionErr.Error()
		}
		return report, nil
	}
	item.DDL = builtinFunctionDDL(schema, catalog, spec.name)
	if _, err = tx.ExecContext(ctx, item.DDL); err != nil {
		return report, fmt.Errorf("创建 %s.%s 失败: %w", schema, spec.signature, err)
	}
	if err = tx.Commit(); err != nil {
		return report, fmt.Errorf("提交 %s 初始化事务失败，执行结果需核验: %w", spec.name, err)
	}
	item.Status = "created"
	return report, nil
}

func compatibilityCatalog(ctx context.Context, tx *sql.Tx) (string, error) {
	var lastErr error
	for _, catalog := range []string{"pg_catalog", "sys_catalog"} {
		err := compatibilityProbe(ctx, tx, func() error {
			var exists bool
			return tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+catalog+".pg_proc)").Scan(&exists)
		})
		if err == nil {
			return catalog, nil
		}
		lastErr = err
	}
	return "", lastErr
}
