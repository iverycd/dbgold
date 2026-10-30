package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
)

type PartitionKeyColumnVerifier interface {
	VerifyPartitionKeyColumns(context.Context, string, *partition.Plan) error
}

// Expression equality does not prove the input column's storage or NULL domain.
// The check is required even without data validation and on empty target tables.
func VerifyPartitionKeyColumns(ctx context.Context, w Writer, table string, plan *partition.Plan) error {
	if plan == nil {
		return nil
	}
	if len(plan.KeyColumns) == 0 {
		if strings.Contains(plan.Rule, "mysql-innodb-key-bytes-to-native-list") || partitionConvertedKeyNeedsColumns(plan.Source, plan.Target) {
			return fmt.Errorf("跨库 KEY 冻结计划缺少完整目标列域快照，请重新创建迁移任务")
		}
		return nil
	}
	verifier, ok := w.(PartitionKeyColumnVerifier)
	if !ok {
		return fmt.Errorf("目标缺少 KEY 表达式输入列域的原生核验能力，禁止写入")
	}
	return verifier.VerifyPartitionKeyColumns(ctx, table, plan)
}

func partitionConvertedKeyNeedsColumns(src, dst *source.PartitionSpec) bool {
	if src == nil || dst == nil {
		return false
	}
	method := strings.ToUpper(strings.TrimSpace(src.Method))
	if (method == "KEY" || method == "LINEAR KEY") && strings.EqualFold(dst.Method, "LIST") {
		for _, key := range dst.Keys {
			if key.Expression != "" {
				return true
			}
		}
	}
	for i, p := range src.Partitions {
		if i < len(dst.Partitions) && partitionConvertedKeyNeedsColumns(p.Subpartition, dst.Partitions[i].Subpartition) {
			return true
		}
	}
	return false
}

// LEFT JOIN retains a row for missing/dropped columns. Native type OID equality
// excludes domains over int4; information_schema's base-type names do not.
// JSON access handles pre-12 catalogs, which cannot contain generated columns.
const pgPartitionKeyColumnsSQL = `WITH RECURSIVE tree(oid,parent_oid) AS (
 SELECT c.oid,0::oid FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2
 UNION ALL SELECT i.inhrelid,i.inhparent FROM tree t JOIN pg_catalog.pg_inherits i ON i.inhparent=t.oid
)
SELECT t.oid,t.parent_oid,n.nspname,c.relname,c.relkind,c.relispartition,a.attname,a.atttypid,a.attnotnull,
 COALESCE(pg_catalog.to_jsonb(a)->>'attidentity',''),COALESCE(pg_catalog.to_jsonb(a)->>'attgenerated',''),
 a.atttypid='pg_catalog.int4'::pg_catalog.regtype,
 EXISTS(SELECT 1 FROM pg_catalog.pg_constraint k WHERE k.conrelid=t.oid AND k.contype='c')
FROM tree t JOIN pg_catalog.pg_class c ON c.oid=t.oid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid AND a.attname=$3 AND a.attnum>0 AND NOT a.attisdropped
ORDER BY t.oid`

type partitionKeyRelation struct {
	schema, table, kind string
	parent              string
}

func partitionKeyRelationID(schema, table string) string { return schema + "\x00" + table }

func (w *PostgresWriter) VerifyPartitionKeyColumns(ctx context.Context, table string, plan *partition.Plan) error {
	if plan == nil || len(plan.KeyColumns) == 0 {
		return nil
	}
	if w.db == nil || w.DBType() != "postgres" || table == "" || plan.Target == nil {
		return fmt.Errorf("KEY 目标列域核验缺少原生 PostgreSQL 连接或完整分区计划")
	}
	schema := w.schema
	if schema == "" {
		var current sql.NullString
		if err := w.db.QueryRowContext(ctx, `SELECT pg_catalog.current_schema()`).Scan(&current); err != nil {
			return err
		}
		if !current.Valid || current.String == "" {
			return fmt.Errorf("KEY 目标列域无法确认有效 schema")
		}
		schema = current.String
	}
	rootID := partitionKeyRelationID(schema, table)
	want := map[string]partitionKeyRelation{rootID: {schema: schema, table: table, kind: "p"}}
	var add func(*source.PartitionSpec, string) error
	add = func(spec *source.PartitionSpec, parent string) error {
		for _, child := range spec.Partitions {
			if child.Schema == "" || child.Name == "" {
				return fmt.Errorf("KEY 目标分区身份未完整冻结")
			}
			id := partitionKeyRelationID(child.Schema, child.Name)
			if _, duplicate := want[id]; duplicate {
				return fmt.Errorf("KEY 目标分区身份重复")
			}
			kind := "r"
			if child.Subpartition != nil {
				kind = "p"
			}
			want[id] = partitionKeyRelation{schema: child.Schema, table: child.Name, kind: kind, parent: parent}
			if child.Subpartition != nil {
				if err := add(child.Subpartition, id); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := add(plan.Target, rootID); err != nil {
		return err
	}
	seenColumns := map[string]bool{}
	for _, key := range plan.KeyColumns {
		if key.Column == "" || key.DataType != "integer" || seenColumns[key.Column] {
			return fmt.Errorf("KEY 目标列域快照含未知类型、空名或重复列")
		}
		seenColumns[key.Column] = true
		rows, err := w.db.QueryContext(ctx, pgPartitionKeyColumnsSQL, schema, table, key.Column)
		if err != nil {
			return fmt.Errorf("读取 KEY 根表及物理分区列域: %w", err)
		}
		seen := map[string]int64{}
		parents := map[string]int64{}
		for rows.Next() {
			var oid, parent int64
			var owner, name, kind, identity, generated string
			var isPartition, hasChecks bool
			var column sql.NullString
			var typeOID sql.NullInt64
			var notNull, nativeInt sql.NullBool
			if err = rows.Scan(&oid, &parent, &owner, &name, &kind, &isPartition, &column, &typeOID, &notNull, &identity, &generated, &nativeInt, &hasChecks); err != nil {
				break
			}
			id := partitionKeyRelationID(owner, name)
			expected, exists := want[id]
			if !exists || seen[id] != 0 || oid <= 0 || kind != expected.kind || isPartition != (id != rootID) || (id == rootID && parent != 0) {
				err = fmt.Errorf("KEY 目标根表/物理分区 %s.%s 的完整身份或层级与冻结计划不同", owner, name)
				break
			}
			if !column.Valid || column.String != key.Column || !typeOID.Valid || !nativeInt.Valid || !nativeInt.Bool || !notNull.Valid || notNull.Bool == key.Nullable || identity != "" || generated != "" {
				err = fmt.Errorf("KEY 目标列 %s.%s.%s 的原生 integer 类型、可空性或生成属性已偏离冻结计划", owner, name, key.Column)
				break
			}
			if hasChecks {
				err = fmt.Errorf("KEY 目标分区表 %s.%s 含未冻结的 CHECK 约束，无法证明未来完整输入域等价", owner, name)
				break
			}
			seen[id], parents[id] = oid, parent
		}
		if err == nil {
			err = rows.Err()
		}
		closeErr := rows.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if len(seen) != len(want) {
			return fmt.Errorf("KEY 根表及全部物理分区的列域目录不完整")
		}
		for id, expected := range want {
			if expected.parent != "" && parents[id] != seen[expected.parent] {
				return fmt.Errorf("KEY 物理分区 %s.%s 的父表身份不同", expected.schema, expected.table)
			}
		}
	}
	return ctx.Err()
}
