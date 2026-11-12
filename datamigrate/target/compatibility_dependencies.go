package target

import (
	"context"
	"database/sql"
	"strings"

	"dbgold/datamigrate/source"
)

// Inspect the exact callable dependency without executing a user routine.
// A missing dependency is expected when the source catalog selected only the caller.
func hasCompatibilityDependency(ctx context.Context, tx *sql.Tx, schema, catalog, name, result string, inputs ...string) (bool, error) {
	spec := compatibilityFunctionSpec{name: name, inputs: inputs}
	identity := qualifiedFunction(schema, name) + "(" + strings.Join(inputs, ",") + ")"
	query := `SELECT EXISTS (SELECT 1 FROM ` + catalog + `.pg_proc p JOIN ` + catalog + `.pg_namespace n ON n.oid=p.pronamespace
		WHERE p.oid=$1::regprocedure::oid AND n.nspname=$2 AND ` + spec.inputPredicate() +
		` AND NOT p.proretset AND p.provariadic=0 AND p.prorettype=$3::regtype AND has_function_privilege(p.oid,'EXECUTE')
		AND NOT EXISTS (SELECT 1 FROM ` + catalog + `.pg_proc p WHERE p.pronamespace=n.oid
		AND p.proname=(SELECT proname FROM ` + catalog + `.pg_proc WHERE oid=$1::regprocedure::oid)
		AND p.oid<>$1::regprocedure::oid AND ` + spec.conflictingCallsPredicate() + `))`
	var exists bool
	err := compatibilityProbe(ctx, tx, func() error {
		return tx.QueryRowContext(ctx, query, identity, schema, result).Scan(&exists)
	})
	if compatibilitySQLState(err) == "42883" {
		return false, nil
	}
	return exists, err
}

func prepareCompatibilityFunctionDDL(ctx context.Context, tx *sql.Tx, schema, catalog, product, name string, report *CompatibilityFunctionsReport) (string, error) {
	var dependency, result string
	var inputs []string
	switch name {
	case "convertguidlist":
		dependency, result = "convertguid", "character varying"
		inputs = []string{"character varying", "character varying"}
	case "getcategoryscore":
		dependency, result = "getscore", "integer"
		inputs = []string{"character varying"}
	case "newid":
		dependency, result = "get_uuid", "character varying"
	default:
		return builtinFunctionDDL(schema, catalog, name, product), nil
	}
	exists, err := hasCompatibilityDependency(ctx, tx, schema, catalog, dependency, result, inputs...)
	if err != nil {
		return "", err
	}
	if name != "newid" {
		return businessFunctionDDLWithDependency(schema, catalog, name, exists), nil
	}
	generator, typ := qualifiedFunction(schema, dependency), "text"
	if !exists {
		generator, typ, err = findUUIDGenerator(ctx, tx, schema, catalog, product, report)
		if err != nil {
			return "", err
		}
	}
	report.Items[0].Generator = generator
	return uuidFunctionDDL(schema, name, generator, typ), nil
}

func prepareCompatibilityProcedureDDL(ctx context.Context, tx *sql.Tx, schema, catalog, product string, spec compatibilityProcedureSpec, report *CompatibilityFunctionsReport) (func(bool) string, error) {
	switch spec.name {
	case compatibilityCreatePVIProcedureName:
		exists, err := hasCompatibilityDependency(ctx, tx, schema, catalog, "get_uuid", "character varying")
		if err != nil {
			return nil, err
		}
		expression := qualifiedFunction(schema, "get_uuid") + "()"
		if !exists {
			generator, typ, err := findUUIDGenerator(ctx, tx, schema, catalog, product, report)
			if err != nil {
				return nil, err
			}
			report.Items[0].Generator = generator
			expression = "CAST(coalesce(" + uuidNormalizer(typ, generator+"()") + ",'invalid UUID') AS " + source.QuotePGIdent(catalog) + ".uuid)"
		}
		return func(native bool) string { return createPVIDDLWithUUID(schema, catalog, native, expression) }, nil
	case "automarkcount":
		exists, err := hasCompatibilityDependency(ctx, tx, schema, catalog, "getdate", "timestamp without time zone")
		if err != nil {
			return nil, err
		}
		if exists {
			return func(native bool) string { return spec.ddl(schema, catalog, native) }, nil
		}
		expression := getDateExpression(source.QuotePGIdent(catalog), product)
		return func(native bool) string { return autoMarkCountDDLWithDate(schema, catalog, native, expression) }, nil
	default:
		return func(native bool) string { return spec.ddl(schema, catalog, native) }, nil
	}
}
