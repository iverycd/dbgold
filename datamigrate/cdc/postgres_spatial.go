package cdc

import (
	"context"
	"database/sql"
	"fmt"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/valueconv"
)

// PrepareSpatial is called before starting the binlog reader, and defensively
// from Apply for direct callers. It does no work for ordinary tables and keeps
// capability/table checks scoped to this applier's lifetime.
func (a *PostgresApplier) PrepareSpatial(ctx context.Context, tables []TableInfo) error {
	return a.prepareSpatial(ctx, tables, true)
}

func (a *PostgresApplier) inspectSpatial(ctx context.Context, tables []TableInfo) error {
	return a.prepareSpatial(ctx, tables, false)
}

func (a *PostgresApplier) prepareSpatial(ctx context.Context, tables []TableInfo, install bool) error {
	for _, table := range tables {
		info := &source.TableDDLInfo{TableName: a.name(table.Name)}
		for i, dt := range table.ColumnTypes {
			if _, spatial := source.MySQLSpatialSubtype(dt); !spatial {
				continue
			}
			if i >= len(table.Columns) {
				return fmt.Errorf("表 %s 空间列元数据数量不一致", table.Name)
			}
			column := source.ColumnInfo{Name: a.name(table.Columns[i]), DataType: dt}
			if i < len(table.ColumnSpatialSRIDs) {
				column.SpatialSRID = table.ColumnSpatialSRIDs[i]
			}
			info.Columns = append(info.Columns, column)
		}
		if len(info.Columns) == 0 || a.spatialTables[table.Name] {
			continue
		}
		if a.spatialCaps == nil {
			inspect := target.InspectPGSpatial
			if install {
				inspect = target.PreparePGSpatial
			}
			caps, err := inspect(ctx, a.db)
			if err != nil {
				return fmt.Errorf("CDC 表 %s 空间能力准备失败: %w", table.Name, err)
			}
			a.spatialCaps = caps
		}
		if err := target.CheckPGSpatialTable(ctx, a.db, a.schema, a.name(table.Name), info, a.spatialCaps); err != nil {
			return fmt.Errorf("CDC 表 %s 空间列校验失败: %w", table.Name, err)
		}
		if a.spatialTables == nil {
			a.spatialTables = make(map[string]bool)
		}
		a.spatialTables[table.Name] = true
	}
	return nil
}

func (a *PostgresApplier) prepareSpatialChanges(ctx context.Context, changes []Change) error {
	tables := make([]TableInfo, 0)
	seen := make(map[string]bool)
	for _, change := range changes {
		if change.Table == nil || len(change.Table.Columns) != len(change.Table.ColumnTypes) {
			return fmt.Errorf("CDC 事件缺少完整列类型元数据")
		}
		if !seen[change.Table.Name] {
			tables = append(tables, *change.Table)
			seen[change.Table.Name] = true
		}
	}
	return a.PrepareSpatial(ctx, tables)
}

// The table lock acquired by checkTransactionTriggers also excludes ALTER
// TABLE. Recheck columns inside that lock: cached startup metadata cannot stop
// an external DDL from changing geometry to text and accepting HEXEWKB as text.
func (a *PostgresApplier) checkTransactionSpatialColumns(ctx context.Context, tx *sql.Tx, changes []Change) error {
	seen := make(map[string]bool)
	for _, change := range changes {
		table := change.Table
		if seen[table.Name] {
			continue
		}
		seen[table.Name] = true
		info := &source.TableDDLInfo{TableName: a.name(table.Name)}
		for i, dt := range table.ColumnTypes {
			if _, spatial := source.MySQLSpatialSubtype(dt); !spatial {
				continue
			}
			column := source.ColumnInfo{Name: a.name(table.Columns[i]), DataType: dt}
			if i < len(table.ColumnSpatialSRIDs) {
				column.SpatialSRID = table.ColumnSpatialSRIDs[i]
			}
			info.Columns = append(info.Columns, column)
		}
		if len(info.Columns) == 0 {
			continue
		}
		if err := target.CheckPGSpatialTable(ctx, tx, a.schema, a.name(table.Name), info, a.spatialCaps); err != nil {
			return fmt.Errorf("CDC 表 %s 空间列结构发生变化或不兼容: %w", table.Name, err)
		}
	}
	return nil
}

func (a *PostgresApplier) convertValue(t *TableInfo, column int, value any) (any, error) {
	if column < 0 || column >= len(t.Columns) || column >= len(t.ColumnTypes) {
		return nil, fmt.Errorf("表 %s 第 %d 列缺少类型元数据", t.Name, column+1)
	}
	if a.conv == nil {
		a.conv = valueconv.NewPostgres()
	}
	converted, err := a.conv.Convert(value, "mysql", t.ColumnTypes[column])
	if err != nil {
		return nil, fmt.Errorf("表 %s 列 %s 数据转换失败: %w", t.Name, t.Columns[column], err)
	}
	return converted, nil
}

// Compare canonical binary forms, not geometry's historical bounding-box
// equality operator. Both sides include SRID and preserve NULL semantics.
func (a *PostgresApplier) fullRowPredicate(t *TableInfo, column, parameter int) (string, error) {
	name := quoteIdent(a.name(t.Columns[column]))
	if _, spatial := source.MySQLSpatialSubtype(t.ColumnTypes[column]); !spatial {
		return fmt.Sprintf("%s IS NOT DISTINCT FROM $%d", name, parameter), nil
	}
	if a.spatialCaps == nil || a.spatialCaps.Schema == "" {
		return "", fmt.Errorf("表 %s 列 %s 空间能力尚未准备", t.Name, t.Columns[column])
	}
	ns := quoteIdent(a.spatialCaps.Schema)
	return fmt.Sprintf("%s.ST_AsEWKB(%s,'NDR') IS NOT DISTINCT FROM %s.ST_AsEWKB($%d::%s.geometry,'NDR')", ns, name, ns, parameter, ns), nil
}
