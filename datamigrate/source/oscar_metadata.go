package source

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"dbgold/datamigrate/failure"
	"dbgold/internal/jdbcbridge"
)

func (r *OscarReader) ListTables(ctx context.Context) ([]string, error) {
	rows, err := r.query(ctx, `SELECT c.RELNAME FROM SYS_CLASS c JOIN SYS_NAMESPACE n ON n.OID=c.RELNAMESPACE
WHERE n.NSPNAME=? AND c.RELKIND IN ('r','f') ORDER BY c.RELNAME`, r.schema)
	if err != nil {
		return nil, fmt.Errorf("读取神通表清单: %w", err)
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		name, err := oscarSingleText([][]any{row})
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

func (r *OscarReader) columns(ctx context.Context, table string) ([]jdbcbridge.MigrationColumn, error) {
	cols, err := r.bridge.MigrationColumns(ctx, r.session, r.schema, table)
	if err != nil {
		return nil, fmt.Errorf("读取神通 %s 的列定义: %w", table, err)
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].Ordinal < cols[j].Ordinal })
	seen := make(map[string]bool, len(cols))
	for i, c := range cols {
		if c.Name == "" || c.Type == "" || c.Ordinal <= 0 || i > 0 && c.Ordinal <= cols[i-1].Ordinal || seen[c.Name] {
			return nil, fmt.Errorf("神通 %s 的列名、类型或序号缺失或重复", table)
		}
		seen[c.Name] = true
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("神通 %s 没有可见列定义", table)
	}
	return cols, nil
}

func oscarColumnInfo(c jdbcbridge.MigrationColumn) ColumnInfo {
	info := ColumnInfo{
		Name: c.Name, DataType: oscarMigrationColumnType(c), Length: c.Size, DataLength: c.OctetLength,
		Precision: c.Size, Scale: c.Scale, IsNullable: c.Nullable, Default: c.Default,
		Ordinal: int(c.Ordinal), DateTimePrecision: max(c.Scale, 0), Comment: c.Comment,
	}
	if strings.EqualFold(c.Generated, "YES") {
		info.Generated = "YES"
	}
	info.FormattedType = FormatColumnType(info)
	return info
}

func oscarMigrationColumnType(c jdbcbridge.MigrationColumn) string {
	// Some compatibility modes expose a DATE declaration with JDBC TIMESTAMP
	// storage. Preserve its time of day in the target DATETIME column.
	if strings.EqualFold(c.Type, "DATE") && c.SQLType == 93 { // java.sql.Types.TIMESTAMP
		return "TIMESTAMP"
	}
	return c.Type
}

func (r *OscarReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	raw, err := r.columns(ctx, table)
	if err != nil {
		return nil, err
	}
	cols := make([]ColumnInfo, 0, len(raw))
	for _, c := range raw {
		col := oscarColumnInfo(c)
		seq, related, err := r.oscarAutoIncrement(ctx, table, c)
		if err != nil {
			return nil, err
		}
		if related {
			if seq.Unsupported != "" {
				return nil, fmt.Errorf("神通表 %s 自增列 %s: %s", table, c.Name, seq.Unsupported)
			}
			col.Extra, col.Default = "auto_increment", nil
		}
		cols = append(cols, col)
	}
	return &TableDDLInfo{TableName: table, Columns: cols}, nil
}

func (r *OscarReader) GetTableSourceDDL(ctx context.Context, table string) (string, error) {
	rows, err := r.query(ctx, `SELECT SYS_GET_TABLEDEF FROM V_SYS_TABLE WHERE SCHEMANAME=? AND TABLENAME=?`, r.schema, table)
	if err != nil {
		return "", err
	}
	return oscarSingleText(rows)
}

func (r *OscarReader) GetPrimaryKey(ctx context.Context, table string) ([]string, error) {
	keys, err := r.primaryKeys(ctx, table)
	if err != nil || len(keys) == 0 {
		return nil, err
	}
	if len(keys) != 1 {
		return nil, fmt.Errorf("神通表 %s 返回多组主键", table)
	}
	return keys[0].Columns, nil
}

func (r *OscarReader) GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error) {
	return r.primaryKeys(ctx, "")
}

func (r *OscarReader) primaryKeys(ctx context.Context, table string) ([]IndexInfo, error) {
	query := `SELECT TABLE_NAME,PK_NAME,COLUMN_NAME,KEY_SEQ FROM V_SYS_PRIMARY_KEYS WHERE TABLE_SCHEM=?`
	args := []string{r.schema}
	if table != "" {
		query += " AND TABLE_NAME=?"
		args = append(args, table)
	}
	rows, err := r.query(ctx, query+" ORDER BY TABLE_NAME,PK_NAME,KEY_SEQ", args...)
	if err != nil {
		return nil, err
	}
	keys := []IndexInfo{}
	type keyID struct{ table, name string }
	index := make(map[keyID]int)
	for _, row := range rows {
		if len(row) != 4 {
			return nil, fmt.Errorf("神通主键目录列数不完整")
		}
		tableName, _ := row[0].(string)
		keyName, _ := row[1].(string)
		column, _ := row[2].(string)
		if tableName == "" || keyName == "" || column == "" {
			return nil, fmt.Errorf("神通主键目录存在空对象名称")
		}
		id := keyID{tableName, keyName}
		i, found := index[id]
		if !found {
			i = len(keys)
			index[id] = i
			keys = append(keys, IndexInfo{TableName: id.table, IndexName: id.name, IsPrimary: true, IsUnique: true})
		}
		position, err := oscarInteger(row[3])
		if err != nil || position != int64(len(keys[i].Columns)+1) {
			return nil, fmt.Errorf("神通主键 %s.%s 的列序号不完整", id.table, id.name)
		}
		keys[i].Columns = append(keys[i].Columns, column)
	}
	return keys, nil
}

func (r *OscarReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	pks, err := r.GetPrimaryKeys(ctx)
	if err != nil {
		return nil, err
	}
	type keyID struct{ table, name string }
	primary := make(map[keyID]bool, len(pks))
	for _, pk := range pks {
		primary[keyID{pk.TableName, pk.IndexName}] = true
	}
	rows, err := r.query(ctx, `SELECT TABLE_NAME,INDEX_NAME,NON_UNIQUE,TYPE,ORDINAL_POSITION,COLUMN_NAME,ASC_OR_DESC,FILTER_CONDITION
FROM V_SYS_INDEX_STATISTICS WHERE TABLE_SCHEM=? ORDER BY TABLE_NAME,INDEX_NAME,ORDINAL_POSITION`, r.schema)
	if err != nil {
		return nil, err
	}
	indexes := []IndexInfo{}
	index := make(map[keyID]int)
	for _, row := range rows {
		if len(row) != 8 {
			return nil, fmt.Errorf("神通索引目录列数不完整")
		}
		kind, err := oscarInteger(row[3])
		if err != nil {
			return nil, err
		}
		if kind == 0 { // JDBC tableIndexStatistic
			continue
		}
		tableName, _ := row[0].(string)
		indexName, _ := row[1].(string)
		if tableName == "" || indexName == "" {
			return nil, fmt.Errorf("神通索引目录存在空对象名称")
		}
		id := keyID{tableName, indexName}
		if primary[id] {
			continue
		}
		i, found := index[id]
		if !found {
			nonUnique, err := oscarBoolean(row[2])
			if err != nil {
				return nil, err
			}
			i = len(indexes)
			index[id] = i
			indexes = append(indexes, IndexInfo{TableName: tableName, IndexName: indexName, IsUnique: !nonUnique})
		}
		idx := &indexes[i]
		position, err := oscarInteger(row[4])
		if err != nil || position != int64(len(idx.Columns)+1) {
			idx.Unsupported = "神通索引列顺序不完整"
		}
		col, _ := row[5].(string)
		direction, _ := row[6].(string)
		filter, _ := row[7].(string)
		if col == "" || filter != "" || kind == 2 || direction != "" && direction != "A" && direction != "D" {
			idx.Unsupported = "神通表达式、部分或哈希索引无法等价转换为普通索引"
		}
		idx.Columns = append(idx.Columns, col)
		idx.Descending = append(idx.Descending, direction == "D")
	}
	return indexes, nil
}

func oscarBoolean(value any) (bool, error) {
	if b, ok := value.(bool); ok {
		return b, nil
	}
	n, err := oscarInteger(value)
	if err == nil && (n == 0 || n == 1) {
		return n == 1, nil
	}
	return false, fmt.Errorf("神通目录返回无效布尔值")
}

func oscarFKAction(value any) (string, bool) {
	n, err := oscarInteger(value)
	if err != nil {
		return "", false
	}
	switch n {
	case 0:
		return "CASCADE", true
	case 1:
		return "RESTRICT", true
	case 2:
		return "SET NULL", true
	case 3:
		return "NO ACTION", true
	case 4:
		return "SET DEFAULT", false
	default:
		return "", false
	}
}

func (r *OscarReader) GetForeignKeys(ctx context.Context) ([]FKInfo, error) {
	rows, err := r.query(ctx, `SELECT FKTABLE_NAME,FK_NAME,FKCOLUMN_NAME,PKTABLE_SCHEM,PKTABLE_NAME,PKCOLUMN_NAME,
KEY_SEQ,DELETE_RULE,UPDATE_RULE,DEFERRABILITY FROM V_SYS_FOREIGN_KEYS WHERE FKTABLE_SCHEM=?
ORDER BY FKTABLE_NAME,FK_NAME,KEY_SEQ`, r.schema)
	if err != nil {
		return nil, err
	}
	type keyID struct{ table, name string }
	keys := []FKInfo{}
	index := make(map[keyID]int)
	for _, row := range rows {
		if len(row) != 10 {
			return nil, fmt.Errorf("神通外键目录列数不完整")
		}
		table, _ := row[0].(string)
		name, _ := row[1].(string)
		column, _ := row[2].(string)
		refSchema, _ := row[3].(string)
		refTable, _ := row[4].(string)
		refColumn, _ := row[5].(string)
		if table == "" || name == "" || column == "" || refSchema == "" || refTable == "" || refColumn == "" {
			return nil, fmt.Errorf("神通外键目录存在空对象名称")
		}
		id := keyID{table, name}
		i, found := index[id]
		if !found {
			onDelete, delOK := oscarFKAction(row[7])
			onUpdate, updOK := oscarFKAction(row[8])
			deferrability, _ := oscarInteger(row[9])
			i = len(keys)
			index[id] = i
			fk := FKInfo{TableName: table, ConstraintName: name, RefSchema: refSchema, RefTable: refTable, OnDelete: onDelete, OnUpdate: onUpdate}
			switch {
			case refSchema != r.schema:
				fk.Unsupported = "神通跨 Schema 外键没有目标对象的确定映射"
			case !delOK || !updOK || deferrability != 7:
				fk.Unsupported = "目标外键无法等价保留神通引用动作或延迟校验语义"
			}
			keys = append(keys, fk)
		}
		fk := &keys[i]
		position, err := oscarInteger(row[6])
		if err != nil || position != int64(len(fk.Columns)+1) || fk.RefSchema != refSchema || fk.RefTable != refTable {
			fk.Unsupported = "神通复合外键的列顺序或引用关系不完整"
		}
		fk.Columns = append(fk.Columns, column)
		fk.RefColumns = append(fk.RefColumns, refColumn)
	}
	return keys, nil
}

func (r *OscarReader) OscarPartitionHasForeignKeys(ctx context.Context, table string) (bool, error) {
	rows, err := r.query(ctx, `SELECT COUNT(*) FROM V_SYS_FOREIGN_KEYS
WHERE (FKTABLE_SCHEM=? AND FKTABLE_NAME=?) OR (PKTABLE_SCHEM=? AND PKTABLE_NAME=?)`, r.schema, table, r.schema, table)
	if err != nil {
		return false, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return false, fmt.Errorf("神通分区外键目录未返回唯一计数")
	}
	count, err := oscarInteger(rows[0][0])
	return count > 0, err
}

func (r *OscarReader) GetComments(ctx context.Context) ([]CommentInfo, error) {
	rows, err := r.query(ctx, `SELECT TABLE_NAME,REMARKS FROM V_SYS_TABLES WHERE TABLE_SCHEM=?
AND REMARKS IS NOT NULL ORDER BY TABLE_NAME`, r.schema)
	if err != nil {
		return nil, err
	}
	out := []CommentInfo{}
	for _, row := range rows {
		if len(row) != 2 {
			return nil, fmt.Errorf("神通表注释目录列数不完整")
		}
		table, _ := row[0].(string)
		value, _ := row[1].(string)
		if table != "" && value != "" {
			out = append(out, CommentInfo{TableName: table, Comment: value})
		}
	}
	rows, err = r.query(ctx, `SELECT TABLE_NAME,COLUMN_NAME,REMARKS FROM INFO_SCHEM.V_SYS_COLUMNS
WHERE TABLE_SCHEM=? AND ORDINAL_POSITION>0 AND REMARKS IS NOT NULL ORDER BY TABLE_NAME,ORDINAL_POSITION`, r.schema)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if len(row) != 3 {
			return nil, fmt.Errorf("神通列注释目录列数不完整")
		}
		table, _ := row[0].(string)
		column, _ := row[1].(string)
		value, _ := row[2].(string)
		if table != "" && column != "" && value != "" {
			out = append(out, CommentInfo{TableName: table, ColumnName: column, Comment: value})
		}
	}
	return out, nil
}

func (r *OscarReader) GetTriggerCount(ctx context.Context) (int, error) {
	rows, err := r.query(ctx, `SELECT COUNT(*) FROM ALL_TRIGGERS WHERE OWNER=?`, r.schema)
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return 0, fmt.Errorf("神通触发器目录未返回唯一计数")
	}
	count, err := oscarInteger(rows[0][0])
	return int(count), err
}

func (r *OscarReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	rows, err := r.query(ctx, `SELECT TABLE_NAME FROM V_SYS_TABLES WHERE TABLE_SCHEM=? AND TABLE_TYPE='VIEW' ORDER BY TABLE_NAME`, r.schema)
	if err != nil {
		return nil, err
	}
	out := make([]ViewInfo, 0, len(rows))
	for _, row := range rows {
		name, err := oscarSingleText([][]any{row})
		if err != nil {
			return nil, err
		}
		v := ViewInfo{ViewName: name}
		v.Definition, err = r.viewDefinition(ctx, name)
		if err == nil {
			var cols []jdbcbridge.MigrationColumn
			cols, err = r.columns(ctx, name)
			for _, c := range cols {
				v.Columns = append(v.Columns, oscarColumnInfo(c))
			}
		}
		if err != nil {
			if failure.IsTaskError(err) {
				return nil, err
			}
			v.Unsupported = err.Error()
		}
		out = append(out, v)
	}
	return out, nil
}

func (r *OscarReader) viewDefinition(ctx context.Context, name string) (string, error) {
	// Oscar exposes standard catalog metadata through INFO_SCHEM. Some
	// deployments expose view text only through its Oracle-mode catalog.
	// Neither query is accepted as evidence unless the exact view is visible
	// and its full definition can be read successfully.
	var last error
	for _, query := range []string{
		`SELECT VIEW_DEFINITION FROM INFO_SCHEM.V_SYS_VIEWS WHERE TABLE_SCHEM=? AND TABLE_NAME=?`,
		`SELECT VIEW_DEFINITION FROM INFORMATION_SCHEMA.VIEWS WHERE TABLE_SCHEMA=? AND TABLE_NAME=?`,
		`SELECT TEXT FROM ALL_VIEWS WHERE OWNER=? AND VIEW_NAME=?`,
	} {
		rows, err := r.query(ctx, query, r.schema, name)
		if err == nil {
			var definition string
			definition, err = oscarSingleText(rows)
			if err == nil {
				return definition, nil
			}
		}
		if failure.IsTaskError(err) {
			return "", err
		}
		last = err
	}
	return "", fmt.Errorf("读取神通视图 %s.%s 的源定义失败: %w", r.schema, name, last)
}
