package source

import (
	"context"
	"fmt"
	"strings"
)

// pg_get_partkeydef prints visible functions/operators without their schema.
// Their spelling cannot prove built-in identity: a user search_path can make a
// custom abs(numeric) or +(numeric,numeric) visible instead of pg_catalog's.
// PostgreSQL records those expression dependencies against the root pg_class
// OID (objsubid=0). Pinned initdb functions/operators have no pg_depend entry:
// https://www.postgresql.org/docs/16/catalog-pg-depend.html
// Reject every recorded executable/type dependency, including extensions or
// custom objects placed in pg_catalog; namespace alone is not proof of native
// semantics. A visible user domain named int8 deparses exactly like a built-in
// cast, but may reject values through domain constraints. Include column type
// dependencies too: their objsubid is the attribute number rather than zero.
func (r *PostgresReader) pgPartitionExpressionDependencies(ctx context.Context, oid int64, attributes string, spec *PartitionSpec) error {
	hasExpression := false
	for _, attribute := range strings.Fields(attributes) {
		hasExpression = hasExpression || attribute == "0"
	}
	for _, key := range spec.Keys {
		hasExpression = hasExpression || key.Expression != ""
	}
	if !hasExpression {
		return nil
	}
	proc, operator, typ := r.qcat("pg_proc"), r.qcat("pg_operator"), r.qcat("pg_type")
	query := `SELECT CASE WHEN d.refclassid='` + proc + `'::regclass THEN 'function' WHEN d.refclassid='` + operator + `'::regclass THEN 'operator' ELSE 'type' END,d.refobjid,COALESCE(pn.nspname,onsp.nspname,tn.nspname,''),COALESCE(p.proname,o.oprname,t.typname,'') FROM ` + r.qcat("pg_depend") + ` d LEFT JOIN ` + proc + ` p ON d.refclassid='` + proc + `'::regclass AND p.oid=d.refobjid LEFT JOIN ` + r.qcat("pg_namespace") + ` pn ON pn.oid=p.pronamespace LEFT JOIN ` + operator + ` o ON d.refclassid='` + operator + `'::regclass AND o.oid=d.refobjid LEFT JOIN ` + r.qcat("pg_namespace") + ` onsp ON onsp.oid=o.oprnamespace LEFT JOIN ` + typ + ` t ON d.refclassid='` + typ + `'::regclass AND t.oid=d.refobjid LEFT JOIN ` + r.qcat("pg_namespace") + ` tn ON tn.oid=t.typnamespace WHERE d.classid='` + r.qcat("pg_class") + `'::regclass AND d.objid=$1 AND (d.objsubid=0 OR d.refclassid='` + typ + `'::regclass) AND d.refclassid IN ('` + proc + `'::regclass,'` + operator + `'::regclass,'` + typ + `'::regclass) ORDER BY d.refclassid,d.refobjid`
	rows, err := r.db.QueryContext(ctx, query, oid)
	if err != nil {
		return fmt.Errorf("读取分区表达式或列类型的真实依赖: %w", err)
	}
	for rows.Next() {
		var kind, owner, name string
		var dependencyOID int64
		if err = rows.Scan(&kind, &dependencyOID, &owner, &name); err != nil {
			break
		}
		if dependencyOID <= 0 || owner == "" || name == "" || (kind != "function" && kind != "operator" && kind != "type") {
			err = fmt.Errorf("分区表达式或列类型的依赖目录身份不完整")
			break
		}
		label, subject := "函数", "分区表达式"
		if kind == "operator" {
			label = "运算符"
		} else if kind == "type" {
			label, subject = "类型", "分区表达式或列类型"
		}
		catalogPartitionUnsupported(spec, fmt.Sprintf("%s依赖自定义或扩展%s %s.%s（OID %d），不能仅凭反解析名称证明原生路由等价", subject, label, owner, name, dependencyOID))
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
