package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

type pgUniqueConstraintQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func createPGUniqueConstraint(ctx context.Context, db *sql.DB, d dialect.Dialect, schema string, idx source.IndexInfo) error {
	// Resolve an unqualified table against the same search path for execution
	// and duplicate verification, even if the pool has only one connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	obj := repair.Object{Schema: schema, Table: idx.TableName, TargetName: dialect.IndexName(d, idx), Index: &idx}
	query := dialect.JoinSQL(d.IndexStatements(schema, idx))
	var verificationErr error
	_, err = execObservedAllow(ctx, conn, query, func(createErr error) bool {
		var matched bool
		matched, verificationErr = verifyPGUniqueConstraintDuplicate(ctx, conn, obj, createErr)
		return matched
	})
	return errors.Join(err, verificationErr)
}

// IsPGUniqueConstraintViolation detects duplicate data, not a same-name object.
func IsPGUniqueConstraintViolation(err error) bool {
	return compatibilitySQLState(err) == "23505"
}

func verifyPGUniqueConstraintDuplicate(ctx context.Context, q pgUniqueConstraintQueryer, obj repair.Object, createErr error) (bool, error) {
	switch compatibilitySQLState(createErr) {
	case "42710", "42P07":
		result, err := inspectPGUniqueConstraint(ctx, q, obj)
		if err != nil {
			return false, err
		}
		if result.Status != "succeeded" {
			return false, fmt.Errorf("核验同名唯一约束失败: %s", result.Message)
		}
		return true, nil
	default:
		return false, nil
	}
}

// A backing index alone cannot satisfy an ADD CONSTRAINT plan. Look up the
// constraint itself and ensure it enforces the expected B-tree key immediately.
// JSON probes preserve compatibility with older catalogs and PostgreSQL forks.
const pgUniqueConstraintSQL = `SELECT a.attname,
       c.convalidated AND NOT c.condeferrable AND NOT c.condeferred
       AND COALESCE((pg_catalog.row_to_json(c)->>'conenforced')::boolean, true)
       AND NOT COALESCE((pg_catalog.row_to_json(c)->>'consoft')::boolean, false)
       AND ix.indisunique AND NOT ix.indisprimary AND ix.indisvalid AND ix.indisready AND ix.indimmediate
       AND COALESCE((pg_catalog.row_to_json(ix)->>'indislive')::boolean, true)
       AND COALESCE((pg_catalog.row_to_json(ix)->>'indisusable')::boolean, true)
       AND NOT COALESCE((pg_catalog.row_to_json(ix)->>'indnullsnotdistinct')::boolean, false)
       AND ix.indpred IS NULL AND ix.indexprs IS NULL AND am.amname IN ('btree','ubtree')
       AND ix.indnatts=pg_catalog.array_length(c.conkey, 1)
       AND ix.indkey[k.pos-1]=c.conkey[k.pos] AND ix.indoption[k.pos-1]=0
       AND ix.indcollation[k.pos-1]=a.attcollation
FROM pg_catalog.pg_constraint c
JOIN pg_catalog.pg_class t ON t.oid=c.conrelid
JOIN pg_catalog.pg_namespace ns ON ns.oid=t.relnamespace
JOIN pg_catalog.pg_index ix ON ix.indexrelid=c.conindid AND ix.indrelid=t.oid
JOIN pg_catalog.pg_class i ON i.oid=ix.indexrelid
JOIN pg_catalog.pg_am am ON am.oid=i.relam
CROSS JOIN pg_catalog.generate_subscripts(c.conkey, 1) AS k(pos)
LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid=t.oid AND a.attnum=c.conkey[k.pos] AND NOT a.attisdropped
WHERE c.contype='u' AND t.relname=$2 AND c.conname=$3
  AND CASE WHEN $1::name IS NULL THEN pg_catalog.pg_table_is_visible(t.oid) ELSE ns.nspname=$1 END
ORDER BY k.pos`

func inspectPGUniqueConstraint(ctx context.Context, q pgUniqueConstraintQueryer, obj repair.Object) (repair.Verification, error) {
	idx := obj.Index
	if idx == nil || !idx.PGUniqueConstraint || !idx.IsUnique || idx.IsPrimary || len(idx.Columns) == 0 {
		return verification("unknown", "缺少可核验的唯一约束键列定义")
	}
	table := obj.Table
	if table == "" {
		table = idx.TableName
	}
	var schema any
	if obj.Schema != "" {
		schema = obj.Schema
	}
	rows, err := q.QueryContext(ctx, pgUniqueConstraintSQL, schema, table, obj.TargetName)
	if err != nil {
		return repair.Verification{Status: "unknown", Message: err.Error()}, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name sql.NullString
		var enforced sql.NullBool
		if err := rows.Scan(&name, &enforced); err != nil {
			return repair.Verification{}, err
		}
		if !name.Valid || !enforced.Valid || !enforced.Bool {
			return verification("failed", "唯一约束的有效状态、索引属性、检查时机或 NULL 语义与原计划不符")
		}
		columns = append(columns, name.String)
	}
	if err := rows.Err(); err != nil {
		return repair.Verification{}, err
	}
	if len(columns) == 0 {
		return verification("failed", "目标表不存在该名称的唯一约束")
	}
	if !slices.Equal(columns, idx.Columns) {
		return verification("failed", "唯一约束键列或顺序与原计划不符")
	}
	return verification("succeeded", "唯一约束所属表、键列顺序和强制规则核验通过")
}
