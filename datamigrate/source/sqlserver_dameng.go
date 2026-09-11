package source

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// SQLServerDamengReader is scoped to dbo and preserves source values and DDL.
// Other SQL Server migration pairs retain their historical reader behavior.
type SQLServerDamengReader struct{ *SQLServerReader }

func NewSQLServerForDameng(dsn, database string, pool ConnPoolConfig) (*SQLServerDamengReader, error) {
	r, err := NewSQLServer(dsn, database, pool)
	if err != nil {
		return nil, err
	}
	return &SQLServerDamengReader{r}, nil
}
func quoteMSSQL(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }
func dboTable(s string) string   { return "[dbo]." + quoteMSSQL(s) }

func (r *SQLServerDamengReader) ListTables(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT t.name FROM sys.tables t
		 INNER JOIN sys.schemas s ON t.schema_id = s.schema_id
		 WHERE t.type = 'U' AND s.name = 'dbo' AND t.is_ms_shipped = 0
		 ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func (r *SQLServerDamengReader) GetPrimaryKey(ctx context.Context, table string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT kcu.COLUMN_NAME
		 FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu
		 JOIN INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc
		   ON kcu.CONSTRAINT_NAME = tc.CONSTRAINT_NAME
		  AND kcu.TABLE_SCHEMA = tc.TABLE_SCHEMA
		 WHERE tc.CONSTRAINT_TYPE = 'PRIMARY KEY'
		   AND kcu.TABLE_CATALOG = DB_NAME() AND kcu.TABLE_SCHEMA = 'dbo'
		   AND kcu.TABLE_NAME = @p1
		 ORDER BY kcu.ORDINAL_POSITION`,
		table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pks []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		pks = append(pks, col)
	}
	return pks, rows.Err()
}

func (r *SQLServerDamengReader) GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT t.name,i.name,c.name,i.is_disabled
 FROM sys.tables t JOIN sys.indexes i ON i.object_id=t.object_id
 JOIN sys.index_columns ic ON ic.object_id=i.object_id AND ic.index_id=i.index_id
 JOIN sys.columns c ON c.object_id=ic.object_id AND c.column_id=ic.column_id
 WHERE t.schema_id=SCHEMA_ID('dbo') AND t.is_ms_shipped=0 AND i.is_primary_key=1
 ORDER BY t.name,i.name,ic.key_ordinal`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IndexInfo{}
	positions := map[string]int{}
	for rows.Next() {
		var table, name, col string
		var disabled bool
		if err = rows.Scan(&table, &name, &col, &disabled); err != nil {
			return nil, err
		}
		pos, ok := positions[table]
		if !ok {
			pos = len(out)
			positions[table] = pos
			out = append(out, IndexInfo{TableName: table, IndexName: name, IsPrimary: true, IsUnique: true})
		}
		out[pos].Columns = append(out[pos].Columns, col)
		if disabled {
			out[pos].Unsupported = "不支持禁用的主键"
		}
	}
	return out, rows.Err()
}

func (r *SQLServerDamengReader) GetComments(ctx context.Context) ([]CommentInfo, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT t.name, '' AS COLUMN_NAME, CAST(ep.value AS NVARCHAR(MAX))
		 FROM sys.tables t
		 JOIN sys.extended_properties ep
		   ON ep.major_id = t.object_id AND ep.minor_id = 0 AND ep.class = 1
		 WHERE ep.name = 'MS_Description' AND t.schema_id = SCHEMA_ID('dbo') AND t.is_ms_shipped = 0
		 UNION ALL
		 SELECT t.name, c.name, CAST(ep.value AS NVARCHAR(MAX))
		 FROM sys.tables t
		 JOIN sys.columns c ON c.object_id = t.object_id
		 JOIN sys.extended_properties ep
		   ON ep.major_id = t.object_id AND ep.minor_id = c.column_id AND ep.class = 1
		 WHERE ep.name = 'MS_Description' AND t.schema_id = SCHEMA_ID('dbo') AND t.is_ms_shipped = 0
		 ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var comments []CommentInfo
	for rows.Next() {
		var cm CommentInfo
		if err := rows.Scan(&cm.TableName, &cm.ColumnName, &cm.Comment); err != nil {
			return nil, err
		}
		comments = append(comments, cm)
	}
	return comments, rows.Err()
}

func (r *SQLServerDamengReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.name, ty.name,
 CASE WHEN ty.name IN ('nchar','nvarchar') AND c.max_length > 0 THEN c.max_length/2 ELSE c.max_length END,
 c.precision,c.scale,c.is_nullable,dc.definition,c.is_computed,ty.is_user_defined,
 CONVERT(bigint,ic.seed_value),CONVERT(bigint,ic.increment_value),CONVERT(bigint,ic.last_value)
 FROM sys.tables t JOIN sys.columns c ON c.object_id=t.object_id
 JOIN sys.types ty ON c.user_type_id=ty.user_type_id
 LEFT JOIN sys.default_constraints dc ON dc.object_id=c.default_object_id
 LEFT JOIN sys.identity_columns ic ON ic.object_id=c.object_id AND ic.column_id=c.column_id
 WHERE t.schema_id=SCHEMA_ID('dbo') AND t.is_ms_shipped=0 AND t.name=@p1 ORDER BY c.column_id`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	info := &TableDDLInfo{TableName: table}
	for rows.Next() {
		var c ColumnInfo
		var def sql.NullString
		var computed, userType bool
		var seed, step, last sql.NullInt64
		if err = rows.Scan(&c.Name, &c.DataType, &c.Length, &c.Precision, &c.Scale, &c.IsNullable, &def, &computed, &userType, &seed, &step, &last); err != nil {
			return nil, err
		}
		if def.Valid {
			c.Default = &def.String
		}
		if computed {
			c.Generated = "computed"
		}
		if userType {
			c.TypeKind = "user_defined"
		}
		c.DateTimePrecision = c.Scale
		if seed.Valid {
			c.Extra = "auto_increment"
			c.IdentityInfo = &IdentityInfo{Seed: seed.Int64, Increment: step.Int64}
			if last.Valid {
				c.IdentityInfo.Last = &last.Int64
			}
		}
		info.Columns = append(info.Columns, c)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(info.Columns) == 0 {
		return nil, fmt.Errorf("dbo.%s 不存在或无权读取列元数据", table)
	}
	return info, nil
}

func (r *SQLServerDamengReader) ReadPage(ctx context.Context, table string, pks []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	order := "(SELECT NULL)"
	if len(pks) > 0 {
		q := make([]string, len(pks))
		for i, c := range pks {
			q[i] = quoteMSSQL(c)
		}
		order = strings.Join(q, ", ")
	}
	query := fmt.Sprintf("SELECT * FROM %s ORDER BY %s OFFSET @p1 ROWS FETCH NEXT @p2 ROWS ONLY", dboTable(table), order)
	rows, err := r.db.QueryContext(ctx, query, offset, limit)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, nil, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, nil, err
	}
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = strings.ToUpper(t.DatabaseTypeName())
	}
	data := [][]interface{}{}
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			return nil, nil, nil, err
		}
		// No NUL stripping, date rounding, or lossy text conversion in this reader.
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = append([]byte{}, b...)
			}
		}
		data = append(data, vals)
	}
	return cols, names, data, rows.Err()
}
func (r *SQLServerDamengReader) CountRows(ctx context.Context, table string) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, "SELECT COUNT_BIG(*) FROM "+dboTable(table)).Scan(&n)
	return n, err
}
func (r *SQLServerDamengReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT t.name,c.name,CONVERT(varchar(100),c.seed_value),CONVERT(varchar(100),c.increment_value),CONVERT(varchar(100),c.last_value)
 FROM sys.tables t JOIN sys.identity_columns c ON t.object_id=c.object_id
 WHERE t.schema_id=SCHEMA_ID('dbo') AND t.is_ms_shipped=0 ORDER BY t.name,c.column_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SequenceInfo{}
	for rows.Next() {
		var seq SequenceInfo
		var seed, step string
		var last sql.NullString
		if err = rows.Scan(&seq.TableName, &seq.ColumnName, &seed, &step, &last); err != nil {
			return nil, err
		}
		parse := func(s string) int64 {
			n, e := strconv.ParseInt(s, 10, 64)
			if e != nil {
				seq.Unsupported = "自增状态超出 BIGINT 范围"
			}
			return n
		}
		id := &IdentityInfo{Seed: parse(seed), Increment: parse(step)}
		if last.Valid {
			n := parse(last.String)
			id.Last = &n
		}
		seq.IdentityInfo = id
		seq.StartValue = id.Seed
		out = append(out, seq)
	}
	return out, rows.Err()
}

func (r *SQLServerDamengReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT t.name,i.name,i.is_unique,i.type,i.has_filter,i.is_disabled,i.is_hypothetical,
 c.name,ic.is_included_column,ic.is_descending_key
 FROM sys.tables t JOIN sys.indexes i ON i.object_id=t.object_id
 LEFT JOIN sys.index_columns ic ON ic.object_id=i.object_id AND ic.index_id=i.index_id
 LEFT JOIN sys.columns c ON c.object_id=ic.object_id AND c.column_id=ic.column_id
 WHERE t.schema_id=SCHEMA_ID('dbo') AND t.is_ms_shipped=0 AND i.is_primary_key=0 AND i.type>0
 ORDER BY t.name,i.index_id,ic.key_ordinal,ic.index_column_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IndexInfo{}
	positions := map[string]int{}
	for rows.Next() {
		var table, name string
		var unique, filter, disabled, hypothetical bool
		var kind int
		var col sql.NullString
		var included, desc sql.NullBool
		if err = rows.Scan(&table, &name, &unique, &kind, &filter, &disabled, &hypothetical, &col, &included, &desc); err != nil {
			return nil, err
		}
		key := table + "\x00" + name
		pos, ok := positions[key]
		if !ok {
			pos = len(out)
			positions[key] = pos
			out = append(out, IndexInfo{TableName: table, IndexName: name, IsUnique: unique})
		}
		idx := &out[pos]
		if kind != 1 && kind != 2 || filter || disabled || hypothetical || included.Bool || !col.Valid {
			idx.Unsupported = "仅支持启用的普通/唯一 B-tree 索引；不支持过滤、包含列或特殊索引"
		}
		if col.Valid && !included.Bool {
			idx.Columns = append(idx.Columns, col.String)
			idx.Descending = append(idx.Descending, desc.Bool)
		}
	}
	return out, rows.Err()
}
func (r *SQLServerDamengReader) GetForeignKeys(ctx context.Context) ([]FKInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT p.name,f.name,pc.name,rs.name,r.name,rc.name,
 f.delete_referential_action_desc,f.update_referential_action_desc,f.is_disabled,f.is_not_trusted
 FROM sys.foreign_keys f JOIN sys.foreign_key_columns fc ON f.object_id=fc.constraint_object_id
 JOIN sys.tables p ON p.object_id=f.parent_object_id JOIN sys.tables r ON r.object_id=f.referenced_object_id
 JOIN sys.schemas rs ON rs.schema_id=r.schema_id
 JOIN sys.columns pc ON pc.object_id=p.object_id AND pc.column_id=fc.parent_column_id
 JOIN sys.columns rc ON rc.object_id=r.object_id AND rc.column_id=fc.referenced_column_id
 WHERE p.schema_id=SCHEMA_ID('dbo') AND p.is_ms_shipped=0 ORDER BY p.name,f.name,fc.constraint_column_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FKInfo{}
	positions := map[string]int{}
	for rows.Next() {
		var f FKInfo
		var c, rc string
		var disabled, untrusted bool
		if err = rows.Scan(&f.TableName, &f.ConstraintName, &c, &f.RefSchema, &f.RefTable, &rc, &f.OnDelete, &f.OnUpdate, &disabled, &untrusted); err != nil {
			return nil, err
		}
		f.OnDelete = strings.ReplaceAll(f.OnDelete, "_", " ")
		f.OnUpdate = strings.ReplaceAll(f.OnUpdate, "_", " ")
		if f.RefSchema != "dbo" || f.OnUpdate != "NO ACTION" || (f.OnDelete != "NO ACTION" && f.OnDelete != "CASCADE" && f.OnDelete != "SET NULL") || disabled || untrusted {
			f.Unsupported = "不支持跨 schema、ON UPDATE、SET DEFAULT 或禁用/未受信任外键"
		}
		key := f.TableName + "\x00" + f.ConstraintName
		pos, ok := positions[key]
		if !ok {
			pos = len(out)
			positions[key] = pos
			out = append(out, f)
		}
		out[pos].Columns = append(out[pos].Columns, c)
		out[pos].RefColumns = append(out[pos].RefColumns, rc)
	}
	return out, rows.Err()
}
func (r *SQLServerDamengReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT v.name,m.definition FROM sys.views v LEFT JOIN sys.sql_modules m ON v.object_id=m.object_id
 WHERE v.schema_id=SCHEMA_ID('dbo') AND v.is_ms_shipped=0 ORDER BY v.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ViewInfo{}
	for rows.Next() {
		var v ViewInfo
		var def sql.NullString
		if err = rows.Scan(&v.ViewName, &def); err != nil {
			return nil, err
		}
		v.Definition = def.String
		out = append(out, v)
	}
	return out, rows.Err()
}
func (r *SQLServerDamengReader) GetTriggerCount(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.triggers tr JOIN sys.tables t ON tr.parent_id=t.object_id WHERE t.schema_id=SCHEMA_ID('dbo') AND tr.is_ms_shipped=0`).Scan(&n)
	return n, err
}

// IdentityBounds is read outside metadata result sets so a one-connection pool works.
func (r *SQLServerDamengReader) IdentityBounds(ctx context.Context, table, column string) (*int64, *int64, error) {
	var lo, hi sql.NullInt64
	err := r.db.QueryRowContext(ctx, "SELECT MIN("+quoteMSSQL(column)+"), MAX("+quoteMSSQL(column)+") FROM "+dboTable(table)).Scan(&lo, &hi)
	if err != nil {
		return nil, nil, err
	}
	if !lo.Valid {
		return nil, nil, nil
	}
	return &lo.Int64, &hi.Int64, nil
}
