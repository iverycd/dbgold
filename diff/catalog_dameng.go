package diff

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

func readDamengCatalog(ctx context.Context, q Queryer, c *Catalog, selected map[string]bool) error {
	const scope = `SELECT ID,NAME FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?`
	var id, actualName string
	if err := q.QueryRowContext(ctx, scope, c.Namespace).Scan(&id, &actualName); err != nil {
		return fmt.Errorf("确认达梦 schema: %w", err)
	}
	if actualName != c.Namespace {
		return fmt.Errorf("请使用目录中的准确 schema 名称 %q", actualName)
	}
	const mode = `SELECT CAST(SF_GET_UNICODE_FLAG() AS VARCHAR(20)),CAST(SF_GET_CASE_SENSITIVE_FLAG() AS VARCHAR(20))`
	var charset, caseSensitive string
	if err := q.QueryRowContext(ctx, mode).Scan(&charset, &caseSensitive); err != nil {
		return fmt.Errorf("读取达梦字符集和大小写策略: %w", err)
	}
	var databaseID string
	if err := q.QueryRowContext(ctx, `SELECT CAST(DB_MAGIC AS VARCHAR(80)) FROM V$RLOG`).Scan(&databaseID); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	c.Identity = id + "\n" + databaseID
	c.Mode = "CHARSET=" + charset + "\nCASE_SENSITIVE=" + caseSensitive
	c.IdentifierLimit = 128
	const settings = `SELECT PARA_NAME,PARA_VALUE FROM V$DM_INI WHERE PARA_NAME IN ('COMPATIBLE_MODE','LENGTH_IN_CHAR','CHARACTER_LENGTH_IN_CHAR','NUMBER_MODE') ORDER BY PARA_NAME`
	var profile strings.Builder
	if err := catalogRows(ctx, q, "读取达梦兼容设置", settings, func(rows *sql.Rows) error {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return err
		}
		profile.WriteString("\n" + name + "=" + value)
		return nil
	}); err == nil {
		c.Mode += profile.String()
	} else if ctx.Err() != nil {
		return ctx.Err()
	}
	const inventory = `SELECT o.OBJECT_NAME,o.OBJECT_TYPE,o.OBJECT_ID,t.TEMPORARY,
CASE WHEN EXISTS (SELECT 1 FROM ALL_PART_TABLES p WHERE p.OWNER=o.OWNER AND p.TABLE_NAME=o.OBJECT_NAME) THEN 1 ELSE 0 END
FROM ALL_OBJECTS o LEFT JOIN ALL_TABLES t ON t.OWNER=o.OWNER AND t.TABLE_NAME=o.OBJECT_NAME AND o.OBJECT_TYPE='TABLE'
WHERE o.OWNER=? ORDER BY o.OBJECT_NAME,o.OBJECT_TYPE,o.OBJECT_ID`
	if err := catalogRows(ctx, q, "读取达梦表和对象", inventory, func(rows *sql.Rows) error {
		var name, kind, id string
		var temporary sql.NullString
		var partitioned int
		if err := rows.Scan(&name, &kind, &id, &temporary, &partitioned); err != nil {
			return err
		}
		if kind == "TABLE PARTITION" || kind == "TABLE SUBPARTITION" || kind == "INDEX PARTITION" || kind == "INDEX SUBPARTITION" {
			return nil
		}
		if _, exists := c.Occupied[name]; !exists || kind == "TABLE" {
			c.Occupied[name] = kind
		}
		if kind != "TABLE" || !catalogSelected(selected, name) {
			return nil
		}
		t := &Table{Name: name, Identity: id}
		if temporary.String == "Y" || partitioned != 0 {
			t.Unsupported = "临时或分区表需要手工处理"
		}
		c.Tables[name] = t
		return nil
	}, c.Namespace); err != nil {
		return err
	}
	if len(c.Tables) == 0 {
		return nil
	}
	if err := readDamengColumns(ctx, q, c); err != nil {
		return err
	}
	if err := readDamengComments(ctx, q, c); err != nil {
		return err
	}
	constraintIndexes, err := readDamengConstraints(ctx, q, c)
	if err != nil {
		return err
	}
	return readDamengIndexes(ctx, q, c, constraintIndexes)
}

func readDamengColumns(ctx context.Context, q Queryer, c *Catalog) error {
	const query = `SELECT c.TABLE_NAME,c.COLUMN_NAME,c.DATA_TYPE,c.COLUMN_ID,c.CHAR_LENGTH,c.DATA_LENGTH,c.DATA_PRECISION,c.DATA_SCALE,c.CHAR_USED,
c.NULLABLE,c.DATA_DEFAULT,CASE WHEN sc.INFO2 & 0x01=0x01 THEN 1 ELSE 0 END,NVL(ci.INFO1,0)
FROM ALL_TAB_COLUMNS c JOIN ALL_OBJECTS o ON o.OBJECT_NAME=c.TABLE_NAME AND o.OWNER=c.OWNER AND o.OBJECT_TYPE='TABLE'
JOIN SYS.SYSCOLUMNS sc ON sc.ID=o.OBJECT_ID AND sc.NAME=c.COLUMN_NAME
LEFT JOIN SYS.SYSCOLINFOS ci ON ci.ID=o.OBJECT_ID AND ci.COLID=sc.COLID
WHERE c.OWNER=? ORDER BY c.TABLE_NAME,c.COLUMN_ID`
	return catalogRows(ctx, q, "读取达梦列定义", query, func(rows *sql.Rows) error {
		var table, nullable string
		var column Column
		var length, bytes, precision, scale sql.NullInt64
		var def, units sql.NullString
		var identity, flags int64
		if err := rows.Scan(&table, &column.Name, &column.DataType, &column.Ordinal, &length, &bytes, &precision, &scale, &units,
			&nullable, &def, &identity, &flags); err != nil {
			return err
		}
		t := c.Tables[table]
		if t == nil {
			return nil
		}
		column.Length, column.DataLength, column.Precision, column.Scale = length.Int64, bytes.Int64, precision.Int64, scale.Int64
		column.CharUsed, column.IsNullable = units.String, nullable == "Y"
		if identity == 1 {
			column.Extra = "auto_increment"
		}
		if flags&1 != 0 {
			column.Generated = strings.TrimSpace(def.String)
			if column.Generated == "" {
				column.Unsupported = "生成列定义不可见"
			}
		} else if def.Valid && strings.TrimSpace(def.String) != "" {
			value := strings.TrimSpace(def.String)
			column.Default = &value
		}
		column.FormattedType = damengFormattedType(&column)
		t.Columns = append(t.Columns, column)
		return nil
	}, c.Namespace)
}

func damengFormattedType(c *Column) string {
	typ := c.DataType
	if strings.Contains(typ, "(") {
		return typ
	}
	switch strings.ToUpper(typ) {
	case "CHAR", "CHARACTER", "VARCHAR", "VARCHAR2", "CHAR VARYING", "CHARACTER VARYING":
		if c.CharUsed == "C" && c.Length > 0 {
			return fmt.Sprintf("%s(%d CHAR)", typ, c.Length)
		}
		if c.DataLength > 0 {
			return fmt.Sprintf("%s(%d)", typ, c.DataLength)
		}
	case "NCHAR", "NVARCHAR", "NVARCHAR2":
		if c.Length > 0 {
			return fmt.Sprintf("%s(%d)", typ, c.Length)
		}
	case "BINARY", "VARBINARY":
		if c.DataLength > 0 {
			return fmt.Sprintf("%s(%d)", typ, c.DataLength)
		}
	case "DECIMAL", "DEC", "NUMERIC", "NUMBER":
		if c.Precision > 0 {
			return fmt.Sprintf("%s(%d,%d)", typ, c.Precision, c.Scale)
		}
	case "TIME", "TIMESTAMP", "DATETIME", "TIME WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE":
		c.DateTimePrecision = c.Scale
		if i := strings.IndexByte(typ, ' '); i >= 0 {
			return fmt.Sprintf("%s(%d)%s", typ[:i], c.Scale, typ[i:])
		}
		return fmt.Sprintf("%s(%d)", typ, c.Scale)
	}
	return typ
}

func readDamengComments(ctx context.Context, q Queryer, c *Catalog) error {
	const comments = `SELECT TVNAME,CAST(NULL AS VARCHAR(128)),COMMENT$ FROM SYS.SYSTABLECOMMENTS WHERE SCHNAME=? AND COMMENT$ IS NOT NULL
UNION ALL SELECT TVNAME,COLNAME,COMMENT$ FROM SYS.SYSCOLUMNCOMMENTS WHERE SCHNAME=? AND COMMENT$ IS NOT NULL`
	return catalogRows(ctx, q, "读取达梦表和字段注释", comments, func(rows *sql.Rows) error {
		var table, comment string
		var column sql.NullString
		if err := rows.Scan(&table, &column, &comment); err != nil {
			return err
		}
		t := c.Tables[table]
		if t == nil {
			return nil
		}
		if !column.Valid {
			t.Comment = comment
			return nil
		}
		for i := range t.Columns {
			if t.Columns[i].Name == column.String {
				t.Columns[i].Comment = comment
				break
			}
		}
		return nil
	}, c.Namespace, c.Namespace)
}

type dmObjectID struct{ table, name string }

func readDamengConstraints(ctx context.Context, q Queryer, c *Catalog) (map[dmObjectID]bool, error) {
	const query = `SELECT k.TABLE_NAME,k.CONSTRAINT_NAME,k.CONSTRAINT_TYPE,k.INDEX_NAME,k.STATUS,k.VALIDATED,k.SEARCH_CONDITION,
kc.COLUMN_NAME,k.R_OWNER,r.TABLE_NAME,rc.COLUMN_NAME,k.DELETE_RULE
FROM ALL_CONSTRAINTS k
LEFT JOIN ALL_CONS_COLUMNS kc ON kc.OWNER=k.OWNER AND kc.TABLE_NAME=k.TABLE_NAME AND kc.CONSTRAINT_NAME=k.CONSTRAINT_NAME
LEFT JOIN ALL_CONSTRAINTS r ON r.OWNER=k.R_OWNER AND r.CONSTRAINT_NAME=k.R_CONSTRAINT_NAME
LEFT JOIN ALL_CONS_COLUMNS rc ON rc.OWNER=r.OWNER AND rc.CONSTRAINT_NAME=r.CONSTRAINT_NAME AND rc.POSITION=kc.POSITION
WHERE k.OWNER=? AND k.CONSTRAINT_TYPE IN ('P','U','R','C') ORDER BY k.TABLE_NAME,k.CONSTRAINT_NAME,kc.POSITION`
	constraintIndexes := make(map[dmObjectID]bool)
	keys := make(map[dmObjectID]*Key)
	var order []dmObjectID
	err := catalogRows(ctx, q, "读取达梦约束", query, func(rows *sql.Rows) error {
		var id dmObjectID
		var kind string
		var indexName, status, validated, definition, column, refNamespace, refTable, refColumn, onDelete sql.NullString
		if err := rows.Scan(&id.table, &id.name, &kind, &indexName, &status, &validated, &definition, &column, &refNamespace, &refTable, &refColumn, &onDelete); err != nil {
			return err
		}
		if c.Tables[id.table] == nil {
			return nil
		}
		key := keys[id]
		if key == nil {
			key = &Key{Name: id.name, Definition: definition.String}
			switch kind {
			case "P", "U":
				key.Kind, key.Method = "primary", "btree"
				if kind == "U" {
					key.Kind = "unique"
				}
				if indexName.Valid {
					constraintIndexes[dmObjectID{id.table, indexName.String}] = true
				}
			case "R":
				key.Kind, key.OnUpdate, key.OnDelete = "foreign", "NO ACTION", onDelete.String
				key.RefNamespace, key.RefTable = refNamespace.String, refTable.String
			case "C":
				key.Kind, key.Unsupported = "check", "检查约束需要手工处理"
			}
			if status.String != "ENABLED" || validated.Valid && validated.String != "VALIDATED" {
				key.Unsupported = catalogReason(key.Unsupported, "约束未启用或未验证")
			}
			keys[id], order = key, append(order, id)
		}
		if column.Valid {
			key.Columns = append(key.Columns, column.String)
		}
		if key.Kind == "foreign" {
			if !column.Valid || !refNamespace.Valid || !refTable.Valid || !refColumn.Valid || !onDelete.Valid {
				key.Unsupported = catalogReason(key.Unsupported, "外键定义不可见或不完整")
			}
			key.RefColumns = append(key.RefColumns, refColumn.String)
		}
		return nil
	}, c.Namespace)
	if err != nil {
		return nil, err
	}
	for _, id := range order {
		key := keys[id]
		t := c.Tables[id.table]
		if key.Kind == "check" && dmIntrinsicNotNull(t, key) {
			continue
		}
		if key.Kind != "check" && len(key.Columns) == 0 {
			key.Unsupported = catalogReason(key.Unsupported, "约束列定义不可见或不完整")
		}
		catalogAppendKey(t, *key)
	}
	return constraintIndexes, nil
}

func dmIntrinsicNotNull(t *Table, key *Key) bool {
	definition := strings.TrimSpace(key.Definition)
	for len(definition) > 2 && definition[0] == '(' && definition[len(definition)-1] == ')' {
		definition = strings.TrimSpace(definition[1 : len(definition)-1])
	}
	for _, column := range t.Columns {
		if column.IsNullable || len(key.Columns) > 1 || len(key.Columns) == 1 && key.Columns[0] != column.Name {
			continue
		}
		quoted := `"` + strings.ReplaceAll(column.Name, `"`, `""`) + `"`
		for _, prefix := range []string{quoted, column.Name} {
			if strings.HasPrefix(definition, prefix) && strings.EqualFold(strings.Join(strings.Fields(definition[len(prefix):]), " "), "IS NOT NULL") {
				return true
			}
		}
	}
	return false
}

func readDamengIndexes(ctx context.Context, q Queryer, c *Catalog, constraintIndexes map[dmObjectID]bool) error {
	generated, err := readDamengGeneratedIndexes(ctx, q, c.Namespace)
	if err != nil {
		return err
	}
	const headers = `SELECT TABLE_NAME,INDEX_NAME,OWNER,UNIQUENESS,STATUS,INDEX_TYPE FROM ALL_INDEXES WHERE TABLE_OWNER=? ORDER BY TABLE_NAME,INDEX_NAME`
	type indexID struct{ owner, table, name string }
	indexes := make(map[indexID]*Key)
	var order []indexID
	if err := catalogRows(ctx, q, "读取达梦索引定义", headers, func(rows *sql.Rows) error {
		var id indexID
		var unique, status, method sql.NullString
		if err := rows.Scan(&id.table, &id.name, &id.owner, &unique, &status, &method); err != nil {
			return err
		}
		object := dmObjectID{id.table, id.name}
		if c.Tables[id.table] == nil || id.owner == c.Namespace && (constraintIndexes[object] || generated[object]) {
			return nil
		}
		index := &Key{Name: id.name, Kind: "index", Method: "btree"}
		if unique.String == "UNIQUE" {
			index.Kind = "unique"
		}
		if status.String != "VALID" || method.String != "NORMAL" {
			index.Method = strings.ToLower(method.String)
			index.Unsupported = "特殊、全文、聚簇或不可用索引需要手工处理"
		}
		indexes[id], order = index, append(order, id)
		return nil
	}, c.Namespace); err != nil {
		return err
	}
	const columns = `SELECT TABLE_NAME,INDEX_NAME,INDEX_OWNER,COLUMN_NAME,DESCEND FROM ALL_IND_COLUMNS WHERE TABLE_OWNER=? ORDER BY TABLE_NAME,INDEX_NAME,COLUMN_POSITION`
	if err := catalogRows(ctx, q, "读取达梦索引列", columns, func(rows *sql.Rows) error {
		var id indexID
		var column, direction sql.NullString
		if err := rows.Scan(&id.table, &id.name, &id.owner, &column, &direction); err != nil {
			return err
		}
		index := indexes[id]
		if index == nil {
			return nil
		}
		if !column.Valid || direction.String != "ASC" && direction.String != "DESC" {
			index.Unsupported = catalogReason(index.Unsupported, "索引列定义不可见或不完整")
		}
		index.Columns = append(index.Columns, column.String)
		index.Descending = append(index.Descending, direction.String == "DESC")
		return nil
	}, c.Namespace); err != nil {
		return err
	}
	for _, id := range order {
		index := indexes[id]
		if len(index.Columns) == 0 {
			index.Unsupported = catalogReason(index.Unsupported, "索引列定义不可见或不完整")
		}
		catalogAppendKey(c.Tables[id.table], *index)
	}
	return nil
}

func readDamengGeneratedIndexes(ctx context.Context, q Queryer, namespace string) (map[dmObjectID]bool, error) {
	const query = `SELECT i.ID,i.NAME,t.NAME FROM SYS.SYSINDEXES x JOIN SYS.SYSOBJECTS i ON i.ID=x.ID
JOIN SYS.SYSOBJECTS t ON t.ID=i.PID JOIN SYS.SYSOBJECTS s ON s.ID=t.SCHID
WHERE s.NAME=? AND i.SUBTYPE$='INDEX' AND i.ID BETWEEN 33554432 AND 50331647`
	generated := make(map[dmObjectID]bool)
	err := catalogRows(ctx, q, "确认达梦系统索引和所属表", query, func(rows *sql.Rows) error {
		var identifier int64
		var id dmObjectID
		if err := rows.Scan(&identifier, &id.name, &id.table); err != nil {
			return err
		}
		// DM-generated indexes receive their exact dictionary ID in the name.
		// A user-defined INDEX... prefix is not sufficient to omit an index.
		if id.name == "INDEX"+strconv.FormatInt(identifier, 10) {
			generated[id] = true
		}
		return nil
	}, namespace)
	return generated, err
}
