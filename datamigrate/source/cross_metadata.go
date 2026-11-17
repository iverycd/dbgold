package source

import (
	"context"
	"database/sql"
	"strings"
)

func (r *SQLServerOracleReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	views, err := r.SQLServerDamengReader.GetViews(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT v.name,c.name,ty.name,CASE WHEN ty.name IN ('nchar','nvarchar') AND c.max_length>0 THEN c.max_length/2 ELSE c.max_length END,c.precision,c.scale,c.is_nullable FROM sys.views v JOIN sys.columns c ON c.object_id=v.object_id JOIN sys.types ty ON ty.user_type_id=c.user_type_id WHERE v.schema_id=SCHEMA_ID('dbo') AND v.is_ms_shipped=0 ORDER BY v.name,c.column_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var c ColumnInfo
		if err = rows.Scan(&name, &c.Name, &c.DataType, &c.Length, &c.Precision, &c.Scale, &c.IsNullable); err != nil {
			return nil, err
		}
		c.DateTimePrecision = c.Scale
		for i := range views {
			if views[i].ViewName == name {
				views[i].Columns = append(views[i].Columns, c)
			}
		}
	}
	return views, rows.Err()
}
func (r *SQLServerOracleReader) GetViewsForTarget(ctx context.Context, _ string) ([]ViewInfo, error) {
	return r.GetViews(ctx)
}
func (r *DaMengCrossReader) crossViewColumns(ctx context.Context, views []ViewInfo) ([]ViewInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.TABLE_NAME,c.COLUMN_NAME,c.DATA_TYPE,COALESCE(c.CHAR_LENGTH,0),COALESCE(c.DATA_PRECISION,0),COALESCE(c.DATA_SCALE,0),c.NULLABLE FROM ALL_TAB_COLUMNS c JOIN ALL_VIEWS v ON v.OWNER=c.OWNER AND v.VIEW_NAME=c.TABLE_NAME WHERE c.OWNER=? ORDER BY c.TABLE_NAME,c.COLUMN_ID`, r.schema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, nullable string
		var c ColumnInfo
		if err = rows.Scan(&name, &c.Name, &c.DataType, &c.Length, &c.Precision, &c.Scale, &nullable); err != nil {
			rows.Close()
			return nil, err
		}
		c.IsNullable = nullable == "Y"
		c.DateTimePrecision = c.Scale
		for i := range views {
			if views[i].ViewName == name {
				views[i].Columns = append(views[i].Columns, c)
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range views {
		info := &TableDDLInfo{TableName: views[i].ViewName, Columns: views[i].Columns}
		if e := r.describeFloatStorage(ctx, info); e != nil {
			views[i].Unsupported = e.Error()
		}
	}
	return views, nil
}
func (r *DaMengCrossReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT i.TABLE_NAME,i.INDEX_NAME,ic.COLUMN_NAME,i.UNIQUENESS,i.INDEX_TYPE,i.STATUS,ic.DESCEND FROM ALL_INDEXES i JOIN ALL_IND_COLUMNS ic ON i.OWNER=ic.INDEX_OWNER AND i.INDEX_NAME=ic.INDEX_NAME WHERE i.OWNER=? AND NOT EXISTS (SELECT 1 FROM ALL_CONSTRAINTS c WHERE c.OWNER=i.OWNER AND c.INDEX_NAME=i.INDEX_NAME AND c.CONSTRAINT_TYPE='P') ORDER BY i.TABLE_NAME,i.INDEX_NAME,ic.COLUMN_POSITION`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexInfo
	for rows.Next() {
		var table, name, unique, kind, status, desc string
		var col sql.NullString
		if err = rows.Scan(&table, &name, &col, &unique, &kind, &status, &desc); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].TableName != table || out[len(out)-1].IndexName != name {
			out = append(out, IndexInfo{TableName: table, IndexName: name, IsUnique: unique == "UNIQUE"})
		}
		idx := &out[len(out)-1]
		if kind != "NORMAL" || status != "VALID" || !col.Valid {
			idx.Unsupported = "特殊、表达式或失效索引需要人工适配"
		}
		if col.Valid {
			idx.Columns = append(idx.Columns, col.String)
			idx.Descending = append(idx.Descending, strings.EqualFold(desc, "DESC"))
		}
	}
	return out, rows.Err()
}
func (r *DaMengCrossReader) GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.TABLE_NAME,c.CONSTRAINT_NAME,cc.COLUMN_NAME,c.STATUS FROM ALL_CONSTRAINTS c JOIN ALL_CONS_COLUMNS cc ON cc.OWNER=c.OWNER AND cc.CONSTRAINT_NAME=c.CONSTRAINT_NAME WHERE c.OWNER=? AND c.CONSTRAINT_TYPE='P' ORDER BY c.TABLE_NAME,c.CONSTRAINT_NAME,cc.POSITION`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexInfo
	for rows.Next() {
		var table, name, col, status string
		if err = rows.Scan(&table, &name, &col, &status); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].TableName != table || out[len(out)-1].IndexName != name {
			out = append(out, IndexInfo{TableName: table, IndexName: name, IsPrimary: true, IsUnique: true})
		}
		idx := &out[len(out)-1]
		idx.Columns = append(idx.Columns, col)
		if status != "ENABLED" {
			idx.Unsupported = "禁用主键需要人工适配"
		}
	}
	return out, rows.Err()
}
