package diff

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func readMySQLCatalog(ctx context.Context, q Queryer, c *Catalog, selected map[string]bool) error {
	const profile = `SELECT @@hostname,@@port,@@server_id,@@version,@@lower_case_table_names,@@session.sql_mode,DEFAULT_CHARACTER_SET_NAME,DEFAULT_COLLATION_NAME
FROM information_schema.SCHEMATA WHERE BINARY SCHEMA_NAME=BINARY ?`
	var host, port, server, version, charset, collation string
	if err := q.QueryRowContext(ctx, profile, c.Namespace).Scan(&host, &port, &server, &version, &c.CaseMode, &c.Mode, &charset, &collation); err != nil {
		return fmt.Errorf("确认数据库及大小写策略: %w", err)
	}
	c.Identity = strings.Join([]string{host, port, server, version, charset, collation}, "\n")
	c.IdentifierLimit = 64
	const inventory = `SELECT t.TABLE_NAME,t.TABLE_TYPE,t.ENGINE,t.CREATE_TIME,t.CREATE_OPTIONS,t.TABLE_COMMENT,t.TABLE_COLLATION,co.CHARACTER_SET_NAME
FROM information_schema.TABLES t LEFT JOIN information_schema.COLLATIONS co ON co.COLLATION_NAME=t.TABLE_COLLATION
WHERE BINARY t.TABLE_SCHEMA=BINARY ? ORDER BY BINARY t.TABLE_NAME`
	if err := catalogRows(ctx, q, "读取表和视图", inventory, func(rows *sql.Rows) error {
		var name, kind string
		var engine, created, options, comment, collation, charset sql.NullString
		if err := rows.Scan(&name, &kind, &engine, &created, &options, &comment, &collation, &charset); err != nil {
			return err
		}
		c.Occupied[name] = kind
		if kind != "BASE TABLE" || !catalogSelected(selected, name) {
			return nil
		}
		t := &Table{Name: name, Identity: created.String, Comment: comment.String, Engine: engine.String, Charset: charset.String, Collation: collation.String}
		if !strings.EqualFold(engine.String, "InnoDB") {
			t.Unsupported = "存储引擎暂不支持自动应用"
		}
		if strings.Contains(strings.ToLower(options.String), "partitioned") {
			t.Unsupported = catalogReason(t.Unsupported, "分区表需要手工处理")
		}
		c.Tables[name] = t
		return nil
	}, c.Namespace); err != nil {
		return err
	}
	if len(c.Tables) == 0 {
		return nil
	}
	if err := readMySQLColumns(ctx, q, c); err != nil {
		return err
	}
	if err := readMySQLKeys(ctx, q, c); err != nil {
		return err
	}
	return readMySQLForeignKeys(ctx, q, c)
}

func readMySQLColumns(ctx context.Context, q Queryer, c *Catalog) error {
	const query = `SELECT TABLE_NAME,COLUMN_NAME,DATA_TYPE,COLUMN_TYPE,ORDINAL_POSITION,CHARACTER_MAXIMUM_LENGTH,NUMERIC_PRECISION,NUMERIC_SCALE,DATETIME_PRECISION,
IS_NULLABLE,COLUMN_DEFAULT,EXTRA,GENERATION_EXPRESSION,COLUMN_COMMENT,CHARACTER_SET_NAME,COLLATION_NAME
FROM information_schema.COLUMNS WHERE BINARY TABLE_SCHEMA=BINARY ? ORDER BY BINARY TABLE_NAME,ORDINAL_POSITION`
	return catalogRows(ctx, q, "读取列定义", query, func(rows *sql.Rows) error {
		var table, nullable string
		var column Column
		var length, precision, scale, temporal sql.NullInt64
		var def, extra, generated, comment, charset, collation sql.NullString
		if err := rows.Scan(&table, &column.Name, &column.DataType, &column.FormattedType, &column.Ordinal, &length, &precision, &scale, &temporal,
			&nullable, &def, &extra, &generated, &comment, &charset, &collation); err != nil {
			return err
		}
		t := c.Tables[table]
		if t == nil {
			return nil
		}
		column.Length, column.Precision, column.Scale, column.DateTimePrecision = length.Int64, precision.Int64, scale.Int64, temporal.Int64
		column.IsNullable, column.Extra, column.Generated = nullable == "YES", extra.String, generated.String
		column.Comment, column.Charset, column.Collation = comment.String, charset.String, collation.String
		if def.Valid {
			column.Default = &def.String
		}
		if strings.Contains(strings.ToUpper(extra.String), "GENERATED") && !strings.EqualFold(extra.String, "DEFAULT_GENERATED") && generated.String == "" {
			column.Unsupported = "生成列定义不可见"
		}
		t.Columns = append(t.Columns, column)
		return nil
	}, c.Namespace)
}

func readMySQLKeys(ctx context.Context, q Queryer, c *Catalog) error {
	const supported = `SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='information_schema' AND TABLE_NAME='STATISTICS' AND COLUMN_NAME='IS_VISIBLE'`
	var visibleField int
	if err := q.QueryRowContext(ctx, supported).Scan(&visibleField); err != nil {
		return fmt.Errorf("读取索引目录能力: %w", err)
	}
	visible := "'YES'"
	if visibleField > 0 {
		visible = "IS_VISIBLE"
	}
	query := `SELECT TABLE_NAME,INDEX_NAME,COLUMN_NAME,NON_UNIQUE,SUB_PART,INDEX_TYPE,COLLATION,` + visible + `
FROM information_schema.STATISTICS WHERE BINARY TABLE_SCHEMA=BINARY ? ORDER BY BINARY TABLE_NAME,BINARY INDEX_NAME,SEQ_IN_INDEX`
	type indexID struct{ table, name string }
	byID := make(map[indexID]*Key)
	var order []indexID
	err := catalogRows(ctx, q, "读取主键、唯一键和索引", query, func(rows *sql.Rows) error {
		var id indexID
		var column, direction sql.NullString
		var prefix sql.NullInt64
		var nonUnique int
		var method, visibility string
		if err := rows.Scan(&id.table, &id.name, &column, &nonUnique, &prefix, &method, &direction, &visibility); err != nil {
			return err
		}
		if c.Tables[id.table] == nil {
			return nil
		}
		key := byID[id]
		if key == nil {
			kind := "index"
			switch {
			case id.name == "PRIMARY":
				kind = "primary"
			case nonUnique == 0:
				kind = "unique"
			}
			key = &Key{Name: id.name, Kind: kind, Method: strings.ToLower(method)}
			byID[id] = key
			order = append(order, id)
		}
		if !column.Valid {
			key.Unsupported = catalogReason(key.Unsupported, "表达式索引需要手工处理")
		}
		if key.Method != "btree" || direction.String != "A" && direction.String != "D" {
			key.Unsupported = catalogReason(key.Unsupported, "特殊索引方法或排序规则需要手工处理")
		}
		if visibility != "YES" {
			key.Unsupported = catalogReason(key.Unsupported, "不可见索引需要手工处理")
		}
		key.Columns = append(key.Columns, column.String)
		key.Descending = append(key.Descending, direction.String == "D")
		if prefix.Valid || key.PrefixLengths != nil {
			if key.PrefixLengths == nil {
				key.PrefixLengths = make([]int, len(key.Columns)-1)
			}
			key.PrefixLengths = append(key.PrefixLengths, int(prefix.Int64))
		}
		return nil
	}, c.Namespace)
	if err != nil {
		return err
	}
	for _, id := range order {
		catalogAppendKey(c.Tables[id.table], *byID[id])
	}
	return nil
}

func readMySQLForeignKeys(ctx context.Context, q Queryer, c *Catalog) error {
	const query = `SELECT tc.TABLE_NAME,tc.CONSTRAINT_NAME,tc.CONSTRAINT_TYPE,ku.COLUMN_NAME,ku.REFERENCED_TABLE_SCHEMA,ku.REFERENCED_TABLE_NAME,
ku.REFERENCED_COLUMN_NAME,rc.UPDATE_RULE,rc.DELETE_RULE
FROM information_schema.TABLE_CONSTRAINTS tc
LEFT JOIN information_schema.KEY_COLUMN_USAGE ku ON ku.CONSTRAINT_SCHEMA=tc.CONSTRAINT_SCHEMA AND ku.TABLE_NAME=tc.TABLE_NAME AND ku.CONSTRAINT_NAME=tc.CONSTRAINT_NAME
AND BINARY ku.CONSTRAINT_SCHEMA=BINARY tc.CONSTRAINT_SCHEMA AND BINARY ku.TABLE_NAME=BINARY tc.TABLE_NAME AND BINARY ku.CONSTRAINT_NAME=BINARY tc.CONSTRAINT_NAME
LEFT JOIN information_schema.REFERENTIAL_CONSTRAINTS rc ON rc.CONSTRAINT_SCHEMA=tc.CONSTRAINT_SCHEMA AND rc.TABLE_NAME=tc.TABLE_NAME AND rc.CONSTRAINT_NAME=tc.CONSTRAINT_NAME
AND BINARY rc.CONSTRAINT_SCHEMA=BINARY tc.CONSTRAINT_SCHEMA AND BINARY rc.TABLE_NAME=BINARY tc.TABLE_NAME AND BINARY rc.CONSTRAINT_NAME=BINARY tc.CONSTRAINT_NAME
WHERE BINARY tc.TABLE_SCHEMA=BINARY ? AND tc.CONSTRAINT_TYPE IN ('FOREIGN KEY','CHECK')
ORDER BY BINARY tc.TABLE_NAME,BINARY tc.CONSTRAINT_NAME,ku.ORDINAL_POSITION`
	type constraintID struct{ table, name string }
	fks := make(map[constraintID]*Key)
	var order []constraintID
	err := catalogRows(ctx, q, "读取外键和检查约束", query, func(rows *sql.Rows) error {
		var id constraintID
		var kind string
		var column, refNamespace, refTable, refColumn, onUpdate, onDelete sql.NullString
		if err := rows.Scan(&id.table, &id.name, &kind, &column, &refNamespace, &refTable, &refColumn, &onUpdate, &onDelete); err != nil {
			return err
		}
		if c.Tables[id.table] == nil {
			return nil
		}
		key := fks[id]
		if key == nil {
			key = &Key{Name: id.name, Kind: "foreign", RefNamespace: refNamespace.String, RefTable: refTable.String, OnUpdate: onUpdate.String, OnDelete: onDelete.String}
			if kind == "CHECK" {
				key.Kind, key.Unsupported = "check", "检查约束需要手工处理"
			}
			fks[id] = key
			order = append(order, id)
		}
		if key.Kind == "foreign" {
			if !column.Valid || !refColumn.Valid || !refTable.Valid || !refNamespace.Valid || !onUpdate.Valid || !onDelete.Valid {
				key.Unsupported = "外键定义不可见或不完整"
			}
			key.Columns = append(key.Columns, column.String)
			key.RefColumns = append(key.RefColumns, refColumn.String)
		}
		return nil
	}, c.Namespace)
	if err != nil {
		return err
	}
	for _, id := range order {
		catalogAppendKey(c.Tables[id.table], *fks[id])
	}
	return nil
}
