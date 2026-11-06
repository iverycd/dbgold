package source

import (
	"context"
	"database/sql"
	"fmt"
)

const sqlServerPGViewsQuery = `SELECT v.name,m.definition FROM sys.views v
 LEFT JOIN sys.sql_modules m ON v.object_id=m.object_id
 WHERE v.is_ms_shipped=0 ORDER BY v.name`

// GetViewsForTarget leaves the original T-SQL intact for the PG conversion
// planner. In particular, lowercasing the definition would change literal values
// and remove the original identifier spelling needed for metadata binding.
func (r *SQLServerReader) GetViewsForTarget(ctx context.Context, target string) ([]ViewInfo, error) {
	if !IsPGFamily(target) {
		return r.GetViews(ctx)
	}
	rows, err := r.db.QueryContext(ctx, sqlServerPGViewsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []ViewInfo
	for rows.Next() {
		var v ViewInfo
		var definition sql.NullString
		if err := rows.Scan(&v.ViewName, &definition); err != nil {
			return nil, err
		}
		// Keep unreadable/encrypted objects in the inventory so the planner can
		// report an object failure instead of silently omitting the view.
		v.Definition = definition.String
		views = append(views, v)
	}
	return views, rows.Err()
}

// The embedded base reader's new optional capability must not bypass the
// existing SQL Server -> Dameng reader's raw, dbo-scoped view inventory.
func (r *SQLServerDamengReader) GetViewsForTarget(ctx context.Context, _ string) ([]ViewInfo, error) {
	return r.GetViews(ctx)
}

const sqlServerViewRelationQuery = `SELECT s.name,o.name,o.object_id FROM sys.objects o
 JOIN sys.schemas s ON s.schema_id=o.schema_id
 WHERE o.type IN ('U','V') AND o.name=@p1`

const sqlServerViewRelationColumnsQuery = `SELECT c.name,ty.name,
 CASE WHEN ty.name IN ('nchar','nvarchar') AND c.max_length>0 THEN c.max_length/2 ELSE c.max_length END,
 c.max_length,c.precision,c.scale,c.is_nullable,c.column_id,ty.is_user_defined
 FROM sys.columns c JOIN sys.types ty ON c.user_type_id=ty.user_type_id
 WHERE c.object_id=@p1 ORDER BY c.column_id`

// GetViewRelationColumns resolves tables and views without reading business
// rows. Missing or ambiguous identities are object-level preparation errors.
// TableName is the actual catalog spelling, even when the reference used a
// different case in a case-insensitive source database.
func (r *SQLServerReader) GetViewRelationColumns(ctx context.Context, schema, relation string) (*TableDDLInfo, error) {
	query := sqlServerViewRelationQuery
	args := []any{relation}
	if schema != "" {
		query += ` AND s.name=@p2`
		args = append(args, schema)
	}
	rows, err := r.db.QueryContext(ctx, query+` ORDER BY s.name,o.name`, args...)
	if err != nil {
		return nil, err
	}
	var actualSchema, actualName string
	var objectID int64
	count := 0
	for rows.Next() {
		if err = rows.Scan(&actualSchema, &actualName, &objectID); err != nil {
			rows.Close()
			return nil, err
		}
		count++
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, fmt.Errorf("SQL Server 视图依赖对象 %s.%s 的 schema 身份不唯一或不存在", schema, relation)
	}
	// The identity result is closed before issuing this query, which also
	// supports source connections configured with a one-connection pool.
	rows, err = r.db.QueryContext(ctx, sqlServerViewRelationColumnsQuery, objectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	info := &TableDDLInfo{TableName: actualName}
	for rows.Next() {
		var col ColumnInfo
		var userDefined bool
		if err := rows.Scan(&col.Name, &col.DataType, &col.Length, &col.DataLength, &col.Precision, &col.Scale, &col.IsNullable, &col.Ordinal, &userDefined); err != nil {
			return nil, err
		}
		if userDefined {
			col.TypeKind = "user_defined"
		}
		col.DateTimePrecision = col.Scale
		info.Columns = append(info.Columns, col)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(info.Columns) == 0 {
		return nil, fmt.Errorf("SQL Server 视图依赖对象 %s.%s 的列元数据不可用", actualSchema, actualName)
	}
	return info, nil
}
