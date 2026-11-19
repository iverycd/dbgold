package diff

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"dbgold/datamigrate/source"
	"github.com/lib/pq"
)

type pgCatalog struct {
	name   string
	fields map[string]bool
}

func (p pgCatalog) sql(text string) string {
	return strings.ReplaceAll(text, "$catalog", p.name)
}

func (p pgCatalog) field(name, column, fallback string) string {
	if p.fields[name] {
		return column
	}
	return fallback
}

func detectPGCatalog(ctx context.Context, q Queryer) (pgCatalog, error) {
	const probe = `SELECT co.relname,a.attname FROM $catalog.pg_attribute a JOIN $catalog.pg_class co ON co.oid=a.attrelid
JOIN $catalog.pg_namespace ns ON ns.oid=co.relnamespace WHERE ns.nspname=$1 AND a.attnum>0 AND NOT a.attisdropped`
	var lastErr error
	for _, name := range []string{"pg_catalog", "sys_catalog"} {
		p := pgCatalog{name: name, fields: make(map[string]bool)}
		err := catalogRows(ctx, q, "探测 PG 目录", p.sql(probe), func(rows *sql.Rows) error {
			var table, column string
			if err := rows.Scan(&table, &column); err != nil {
				return err
			}
			p.fields[table+"."+column] = true
			return nil
		}, name)
		if err != nil {
			lastErr = err
			continue
		}
		if p.fields["pg_class.relname"] && p.fields["pg_attribute.attname"] {
			return p, nil
		}
		lastErr = fmt.Errorf("%s 缺少表或列目录", name)
	}
	return pgCatalog{}, lastErr
}

func readPGCatalog(ctx context.Context, q Queryer, c *Catalog, selected map[string]bool) error {
	p, err := detectPGCatalog(ctx, q)
	if err != nil {
		return err
	}
	const profile = `SELECT current_database(),d.oid::text,n.oid::text,COALESCE(inet_server_addr()::text,''),COALESCE(inet_server_port(),0),version(),current_setting('max_identifier_length')
FROM $catalog.pg_database d,$catalog.pg_namespace n WHERE d.datname=current_database() AND n.nspname=$1`
	var db, dbID, namespaceID, port, version string
	var addr sql.NullString
	if err := q.QueryRowContext(ctx, p.sql(profile), c.Namespace).Scan(&db, &dbID, &namespaceID, &addr, &port, &version, &c.IdentifierLimit); err != nil {
		return fmt.Errorf("确认 PG 数据库和 schema: %w", err)
	}
	c.TransactionalDDL = c.Product == "postgres"
	c.Identity = strings.Join([]string{db, dbID, namespaceID, addr.String, port, version, p.name}, "\n")
	const settings = `SELECT name,setting FROM $catalog.pg_settings WHERE name IN ('sql_compatibility','behavior_compat_options','standard_conforming_strings') ORDER BY name`
	if err := catalogRows(ctx, q, "读取 PG 兼容模式", p.sql(settings), func(rows *sql.Rows) error {
		var name string
		var value sql.NullString
		if err := rows.Scan(&name, &value); err != nil {
			return err
		}
		c.Mode += name + "=" + value.String + "\n"
		return nil
	}); err != nil {
		return err
	}
	persistence := p.field("pg_class.relpersistence", "co.relpersistence::text", "'p'")
	partition := p.field("pg_class.parttype", "COALESCE(co.parttype IN ('p','s'),false)", "false")
	rowSecurity := p.field("pg_class.relrowsecurity", "co.relrowsecurity", "false")
	relOptions := p.field("pg_class.reloptions", "COALESCE(co.reloptions::text,'')", "''")
	query := `SELECT co.relname,co.relkind::text,co.oid::text,COALESCE(obj_description(co.oid,'pg_class'),''),` + persistence + `,
EXISTS(SELECT 1 FROM $catalog.pg_inherits h WHERE h.inhrelid=co.oid OR h.inhparent=co.oid),` + partition + `,` + rowSecurity + `,` + relOptions + `
FROM $catalog.pg_class co JOIN $catalog.pg_namespace ns ON ns.oid=co.relnamespace WHERE ns.nspname=$1 ORDER BY co.relname`
	if err := catalogRows(ctx, q, "读取 PG 关系目录", p.sql(query), func(rows *sql.Rows) error {
		var name, kind, id, persistence string
		var comment, options sql.NullString
		var inherits, partitioned, rowSecurity bool
		if err := rows.Scan(&name, &kind, &id, &comment, &persistence, &inherits, &partitioned, &rowSecurity, &options); err != nil {
			return err
		}
		c.Occupied[name] = kind
		if kind != "r" && kind != "p" && kind != "f" || !catalogSelected(selected, name) {
			return nil
		}
		t := &Table{Name: name, Identity: id, Comment: comment.String}
		if kind != "r" || inherits || partitioned || persistence != "p" || rowSecurity || strings.Contains(options.String, "orientation=column") {
			t.Unsupported = "分区、继承、临时、外部、列存或启用行级安全的表需要手工处理"
		}
		c.Tables[name] = t
		return nil
	}, c.Namespace); err != nil {
		return err
	}
	if len(c.Tables) == 0 {
		return nil
	}
	scope := pgCatalogScope{args: []any{c.Namespace}}
	if selected != nil {
		names := make([]string, 0, len(c.Tables))
		for name := range c.Tables {
			names = append(names, name)
		}
		sort.Strings(names)
		scope.filter = " AND $table.relname::text=ANY($2::text[])"
		scope.args = append(scope.args, pq.Array(names))
	}
	attnums, err := readPGColumns(ctx, q, p, c, scope)
	if err != nil {
		return err
	}
	if err := readPGIndexes(ctx, q, p, c, scope, attnums); err != nil {
		return err
	}
	return readPGConstraints(ctx, q, p, c, scope, attnums)
}

type pgCatalogScope struct {
	filter string
	args   []any
}

func (s pgCatalogScope) forAlias(alias string) string {
	return strings.ReplaceAll(s.filter, "$table", alias)
}

func readPGColumns(ctx context.Context, q Queryer, p pgCatalog, c *Catalog, scope pgCatalogScope) (map[string]map[int]string, error) {
	identity := p.field("pg_attribute.attidentity", "a.attidentity::text", "''")
	generated := p.field("pg_attribute.attgenerated", "a.attgenerated::text", "''")
	collation, joins := "''", ""
	if p.fields["pg_attribute.attcollation"] && p.fields["pg_collation.collname"] {
		collation = `CASE WHEN col.oid IS NULL OR cn.nspname IN ('pg_catalog','sys_catalog') AND col.collname='default' THEN '' ELSE cn.nspname||'.'||col.collname END`
		joins = ` LEFT JOIN $catalog.pg_collation col ON col.oid=a.attcollation LEFT JOIN $catalog.pg_namespace cn ON cn.oid=col.collnamespace`
	}
	const serial = `EXISTS(SELECT 1 FROM $catalog.pg_depend dep JOIN $catalog.pg_class seq ON seq.oid=dep.objid AND seq.relkind='S'
JOIN $catalog.pg_depend df ON df.refobjid=seq.oid AND df.objid=d.oid AND df.classid='$catalog.pg_attrdef'::regclass AND df.refclassid='$catalog.pg_class'::regclass
WHERE dep.classid='$catalog.pg_class'::regclass AND dep.refclassid='$catalog.pg_class'::regclass AND dep.refobjid=co.oid AND dep.refobjsubid=a.attnum AND dep.deptype='a')`
	query := `SELECT co.relname,a.attnum,a.attname,t.typname,t.typtype::text,tn.nspname,a.atttypmod,NOT a.attnotnull,pg_get_expr(d.adbin,d.adrelid),` + identity + `,` + generated + `,
COALESCE(col_description(co.oid,a.attnum),''),format_type(a.atttypid,a.atttypmod),` + collation + `,` + serial + `
FROM $catalog.pg_class co JOIN $catalog.pg_namespace n ON n.oid=co.relnamespace JOIN $catalog.pg_attribute a ON a.attrelid=co.oid
JOIN $catalog.pg_type t ON t.oid=a.atttypid JOIN $catalog.pg_namespace tn ON tn.oid=t.typnamespace LEFT JOIN $catalog.pg_attrdef d ON d.adrelid=co.oid AND d.adnum=a.attnum` + joins + `
WHERE n.nspname=$1 AND co.relkind IN ('r','p','f') AND a.attnum>0 AND NOT a.attisdropped` + scope.forAlias("co") + ` ORDER BY co.relname,a.attnum`
	names := make(map[string]map[int]string)
	err := catalogRows(ctx, q, "读取 PG 列定义", p.sql(query), func(rows *sql.Rows) error {
		var table string
		var column Column
		var typmod int64
		var def, identity, generated, comment, collation sql.NullString
		var owned bool
		if err := rows.Scan(&table, &column.Ordinal, &column.Name, &column.DataType, &column.TypeKind, &column.TypeSchema, &typmod, &column.IsNullable,
			&def, &identity, &generated, &comment, &column.FormattedType, &collation, &owned); err != nil {
			return err
		}
		if names[table] == nil {
			names[table] = make(map[int]string)
		}
		names[table][column.Ordinal] = column.Name
		t := c.Tables[table]
		if t == nil {
			return nil
		}
		column.Identity = strings.Trim(identity.String, "\x00")
		column.Comment, column.Collation = comment.String, collation.String
		kind := strings.Trim(generated.String, "\x00")
		if kind != "" && kind != "0" {
			column.Generated = def.String
			if !def.Valid {
				column.Unsupported = "生成列定义不可见"
			}
		} else if def.Valid {
			column.Default = &def.String
		}
		if column.Identity == "a" || column.Identity == "d" || owned && strings.HasPrefix(def.String, "nextval(") {
			column.Extra = "auto_increment"
		}
		fillPGTypeModifiers(&column, typmod)
		t.Columns = append(t.Columns, column)
		return nil
	}, scope.args...)
	return names, err
}

func fillPGTypeModifiers(c *Column, typmod int64) {
	switch c.DataType {
	case "varchar", "bpchar", "nvarchar2":
		if typmod >= 4 {
			c.Length = typmod - 4
		}
	case "numeric":
		if typmod >= 4 {
			modifier := typmod - 4
			c.Precision = (modifier >> 16) & 65535
			c.Scale = modifier & 2047
			if c.Scale >= 1024 {
				c.Scale -= 2048
			}
		}
	case "timestamp", "timestamptz", "time", "timetz":
		c.DateTimePrecision = typmod
		if typmod < 0 {
			c.DateTimePrecision = 6
		}
	case "bit", "varbit":
		if typmod >= 0 {
			c.Length = typmod
		}
	}
}

func readPGIndexes(ctx context.Context, q Queryer, p pgCatalog, c *Catalog, scope pgCatalogScope, attnums map[string]map[int]string) error {
	keyCount := p.field("pg_index.indnkeyatts", "ix.indnkeyatts", "ix.indnatts")
	nulls := p.field("pg_index.indnullsnotdistinct", "ix.indnullsnotdistinct", "false")
	ready := p.field("pg_index.indisready", "ix.indisready", "true")
	opClass, collation := "false", "false"
	if p.fields["pg_opclass.opcdefault"] {
		opClass = `EXISTS(SELECT 1 FROM $catalog.pg_opclass op WHERE op.oid=ANY(ix.indclass) AND NOT op.opcdefault)`
	}
	if p.fields["pg_index.indcollation"] && p.fields["pg_collation.collname"] {
		collation = `EXISTS(SELECT 1 FROM $catalog.pg_collation cl JOIN $catalog.pg_namespace cn ON cn.oid=cl.collnamespace
WHERE cl.oid=ANY(ix.indcollation) AND NOT (cn.nspname IN ('pg_catalog','sys_catalog') AND cl.collname='default'))`
	}
	query := `SELECT t.relname,ixr.relname,ix.indkey::text,ix.indoption::text,ix.indisunique,ix.indisprimary,am.amname,
ix.indexprs IS NOT NULL,ix.indpred IS NOT NULL,ix.indisvalid,ix.indimmediate,` + keyCount + `,ix.indnatts,` + nulls + `,` + ready + `,` + opClass + `,` + collation + `,
COALESCE(con.conname,''),COALESCE(con.condeferrable,false),pg_get_indexdef(ix.indexrelid)
FROM $catalog.pg_index ix JOIN $catalog.pg_class ixr ON ixr.oid=ix.indexrelid JOIN $catalog.pg_class t ON t.oid=ix.indrelid
JOIN $catalog.pg_namespace n ON n.oid=t.relnamespace JOIN $catalog.pg_am am ON am.oid=ixr.relam
LEFT JOIN $catalog.pg_constraint con ON con.conindid=ix.indexrelid AND con.contype IN ('p','u')
WHERE n.nspname=$1` + scope.forAlias("t") + ` ORDER BY t.relname,ixr.relname`
	return catalogRows(ctx, q, "读取 PG 主键、唯一键和索引", p.sql(query), func(rows *sql.Rows) error {
		var table, keys string
		var index Key
		var options, constraint, definition sql.NullString
		var unique, primary, expr, partial, valid, immediate, nulls, ready, opClass, collation, deferred bool
		var keyCount, columnCount int
		if err := rows.Scan(&table, &index.Name, &keys, &options, &unique, &primary, &index.Method, &expr, &partial, &valid, &immediate, &keyCount,
			&columnCount, &nulls, &ready, &opClass, &collation, &constraint, &deferred, &definition); err != nil {
			return err
		}
		t := c.Tables[table]
		if t == nil {
			return nil
		}
		index.Kind = "index"
		if unique {
			index.Kind = "unique"
		}
		if primary {
			index.Kind = "primary"
		}
		if constraint.Valid && constraint.String != "" {
			index.Name = constraint.String
		}
		index.Definition = definition.String
		if c.Product == "gaussdb" && index.Method == "ubtree" {
			index.Method = "btree" // GaussDB's default USTORE B-tree access method.
		}
		if index.Method != "btree" || expr || partial || keyCount != columnCount || !valid || !immediate || !ready || nulls || opClass || collation || deferred {
			index.Unsupported = "非普通 B-tree 索引或具有扩展语义的键需要手工处理"
		}
		numbers, err := source.ParsePGNumbers(keys)
		if err != nil {
			return err
		}
		flags, err := source.ParsePGNumbers(options.String)
		if err != nil {
			return err
		}
		if len(numbers) != columnCount || keyCount < 1 || len(flags) != keyCount || keyCount > len(numbers) {
			index.Unsupported = catalogReason(index.Unsupported, "索引列目录不完整")
		}
		for i, number := range numbers {
			if i == keyCount {
				break
			}
			column := attnums[table][number]
			if column == "" {
				index.Unsupported = catalogReason(index.Unsupported, "索引列定义不可见或包含表达式")
			}
			index.Columns = append(index.Columns, column)
			if i < len(flags) {
				descending := flags[i]&1 != 0
				if flags[i]&^3 != 0 || descending != (flags[i]&2 != 0) {
					index.Unsupported = catalogReason(index.Unsupported, "非默认 NULL 排序需要手工处理")
				}
				index.Descending = append(index.Descending, descending)
			}
		}
		catalogAppendKey(t, index)
		return nil
	}, scope.args...)
}

func readPGConstraints(ctx context.Context, q Queryer, p pgCatalog, c *Catalog, scope pgCatalogScope, attnums map[string]map[int]string) error {
	validated := p.field("pg_constraint.convalidated", "cn.convalidated", "true")
	query := `SELECT t.relname,cn.conname,cn.contype::text,COALESCE(cn.conkey::text,''),COALESCE(cn.confkey::text,''),
COALESCE(rn.nspname,''),COALESCE(rt.relname,''),COALESCE(rt.oid::text,''),cn.confupdtype::text,cn.confdeltype::text,cn.confmatchtype::text,
cn.condeferrable,` + validated + `,pg_get_constraintdef(cn.oid)
FROM $catalog.pg_constraint cn JOIN $catalog.pg_class t ON t.oid=cn.conrelid JOIN $catalog.pg_namespace n ON n.oid=t.relnamespace
LEFT JOIN $catalog.pg_class rt ON rt.oid=cn.confrelid LEFT JOIN $catalog.pg_namespace rn ON rn.oid=rt.relnamespace
WHERE n.nspname=$1 AND cn.contype IN ('f','c','x')` + scope.forAlias("t") + ` ORDER BY t.relname,cn.conname`
	type constraint struct {
		table, refID string
		key          Key
		refs         []int
	}
	var constraints []constraint
	err := catalogRows(ctx, q, "读取 PG 外键和检查约束", p.sql(query), func(rows *sql.Rows) error {
		var item constraint
		var kind string
		var local, foreign, refNamespace, refTable, refID, update, delete, match, definition sql.NullString
		var deferrable, valid bool
		key := &item.key
		if err := rows.Scan(&item.table, &key.Name, &kind, &local, &foreign, &refNamespace, &refTable, &refID, &update, &delete, &match,
			&deferrable, &valid, &definition); err != nil {
			return err
		}
		if c.Tables[item.table] == nil {
			return nil
		}
		key.Definition, key.RefNamespace, key.RefTable, item.refID = definition.String, refNamespace.String, refTable.String, refID.String
		if kind != "f" {
			key.Kind, key.Unsupported = "check", "检查或排斥约束需要手工处理"
			constraints = append(constraints, item)
			return nil
		}
		key.Kind = "foreign"
		key.OnUpdate, key.OnDelete = pgReferentialAction(update.String), pgReferentialAction(delete.String)
		// Old PG-derived catalogs call default MATCH SIMPLE "unspecified".
		if deferrable || !valid || match.String != "s" && match.String != "u" || key.OnUpdate == "" || key.OnDelete == "" || item.refID == "" {
			key.Unsupported = "外键的延迟、验证或匹配选项需要手工处理"
		}
		localAttrs, err := source.ParsePGNumbers(local.String)
		if err != nil {
			return err
		}
		item.refs, err = source.ParsePGNumbers(foreign.String)
		if err != nil {
			return err
		}
		if len(localAttrs) == 0 || len(localAttrs) != len(item.refs) {
			key.Unsupported = catalogReason(key.Unsupported, "外键列目录不完整")
		}
		for _, attribute := range localAttrs {
			column := attnums[item.table][attribute]
			if column == "" {
				key.Unsupported = catalogReason(key.Unsupported, "外键列定义不可见")
			}
			key.Columns = append(key.Columns, column)
		}
		constraints = append(constraints, item)
		return nil
	}, scope.args...)
	if err != nil {
		return err
	}
	external := make(map[string]map[int]string)
	for _, item := range constraints {
		key := item.key
		refNames := attnums[key.RefTable]
		if (key.RefNamespace != c.Namespace || refNames == nil) && item.refID != "" {
			refNames = external[item.refID]
			if refNames == nil {
				refNames = make(map[int]string)
				const reference = `SELECT attnum,attname FROM $catalog.pg_attribute WHERE attrelid=$1 AND attnum>0 AND NOT attisdropped`
				if err := catalogRows(ctx, q, "读取 PG 外键引用列", p.sql(reference), func(rows *sql.Rows) error {
					var attnum int
					var name string
					if err := rows.Scan(&attnum, &name); err != nil {
						return err
					}
					refNames[attnum] = name
					return nil
				}, item.refID); err != nil {
					return err
				}
				external[item.refID] = refNames
			}
		}
		for _, attribute := range item.refs {
			column := refNames[attribute]
			if column == "" {
				key.Unsupported = catalogReason(key.Unsupported, "外键引用列定义不可见")
			}
			key.RefColumns = append(key.RefColumns, column)
		}
		catalogAppendKey(c.Tables[item.table], key)
	}
	return nil
}

func pgReferentialAction(action string) string {
	switch action {
	case "a":
		return "NO ACTION"
	case "r":
		return "RESTRICT"
	case "c":
		return "CASCADE"
	case "n":
		return "SET NULL"
	case "d":
		return "SET DEFAULT"
	}
	return ""
}
