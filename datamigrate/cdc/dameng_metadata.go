package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"dbgold/datamigrate/damengnames"
	"dbgold/datamigrate/tsqldameng"
)

const loadDamengTargetTablesSQL = `SELECT TABLE_NAME FROM ALL_TABLES WHERE OWNER=?`
const loadDamengTargetColumnsSQL = `SELECT c.TABLE_NAME,c.COLUMN_NAME,c.NULLABLE,c.DATA_DEFAULT,c.DATA_TYPE,
CASE WHEN sc.INFO2 & 0x01 = 0x01 THEN 'YES' ELSE 'NO' END
FROM ALL_TAB_COLUMNS c
JOIN ALL_OBJECTS o ON o.OWNER=c.OWNER AND o.OBJECT_NAME=c.TABLE_NAME AND o.OBJECT_TYPE='TABLE'
JOIN SYS.SYSCOLUMNS sc ON sc.ID=o.OBJECT_ID AND sc.NAME=c.COLUMN_NAME
WHERE c.OWNER=? ORDER BY c.TABLE_NAME,c.COLUMN_ID`
const loadDamengTargetIndexesSQL = `SELECT OWNER,INDEX_NAME,TABLE_OWNER,TABLE_NAME,UNIQUENESS,STATUS,INDEX_TYPE
FROM ALL_INDEXES WHERE TABLE_OWNER=?`
const loadDamengTargetIndexColumnsSQL = `SELECT INDEX_OWNER,INDEX_NAME,TABLE_OWNER,TABLE_NAME,COLUMN_NAME,COLUMN_POSITION
FROM ALL_IND_COLUMNS WHERE TABLE_OWNER=?`

type targetMetadataReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Read compatibility views separately: joining the expanded DM index views is
// expensive on schemas with many tables. Never normalize names read from DM.
func loadDamengTargetSchemaMetadata(ctx context.Context, db targetMetadataReader, schema string) (targetSchemaMetadata, error) {
	metadata := targetSchemaMetadata{Tables: map[string]*targetTableMetadata{}}
	rows, err := db.QueryContext(ctx, loadDamengTargetTablesSQL, schema)
	if err != nil {
		return metadata, fmt.Errorf("读取达梦目标表清单失败: %w", err)
	}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return metadata, err
		}
		metadata.Tables[name] = &targetTableMetadata{ColumnNames: map[string]bool{}}
	}
	if err = closeRows(rows); err != nil {
		return metadata, err
	}
	rows, err = db.QueryContext(ctx, loadDamengTargetColumnsSQL, schema)
	if err != nil {
		return metadata, fmt.Errorf("读取达梦目标列失败: %w", err)
	}
	for rows.Next() {
		var table string
		var column targetColumnMetadata
		if err = rows.Scan(&table, &column.Name, &column.Nullable, &column.DefaultValue, &column.DataType, &column.Identity); err != nil {
			rows.Close()
			return metadata, err
		}
		if column.Nullable == "Y" {
			column.Nullable = "YES"
		} else {
			column.Nullable = "NO"
		}
		if target := metadata.Tables[table]; target != nil {
			target.Columns = append(target.Columns, column)
			target.ColumnNames[column.Name] = true
		}
	}
	if err = closeRows(rows); err != nil {
		return metadata, err
	}
	type indexKey struct{ owner, name, tableOwner, table string }
	type index struct {
		valid   bool
		columns map[int]string
	}
	indexes := map[indexKey]*index{}
	rows, err = db.QueryContext(ctx, loadDamengTargetIndexesSQL, schema)
	if err != nil {
		return metadata, fmt.Errorf("读取达梦目标索引失败: %w", err)
	}
	for rows.Next() {
		var key indexKey
		var uniqueness, status, kind string
		if err = rows.Scan(&key.owner, &key.name, &key.tableOwner, &key.table, &uniqueness, &status, &kind); err != nil {
			rows.Close()
			return metadata, err
		}
		if previous := indexes[key]; previous != nil {
			previous.valid = false
			continue
		}
		indexes[key] = &index{valid: key.tableOwner == schema && uniqueness == "UNIQUE" && status == "VALID" && (kind == "NORMAL" || kind == "CLUSTER"), columns: map[int]string{}}
	}
	if err = closeRows(rows); err != nil {
		return metadata, err
	}
	rows, err = db.QueryContext(ctx, loadDamengTargetIndexColumnsSQL, schema)
	if err != nil {
		return metadata, fmt.Errorf("读取达梦目标索引列失败: %w", err)
	}
	for rows.Next() {
		var key indexKey
		var name sql.NullString
		var position int
		if err = rows.Scan(&key.owner, &key.name, &key.tableOwner, &key.table, &name, &position); err != nil {
			rows.Close()
			return metadata, err
		}
		if idx := indexes[key]; idx != nil {
			table := metadata.Tables[key.table]
			if table == nil || !name.Valid || !table.ColumnNames[name.String] || position < 1 || idx.columns[position] != "" {
				idx.valid = false
			}
			idx.columns[position] = name.String
		}
	}
	if err = closeRows(rows); err != nil {
		return metadata, err
	}
	for key, idx := range indexes {
		table := metadata.Tables[key.table]
		if !idx.valid || table == nil || len(idx.columns) == 0 {
			continue
		}
		columns := make([]string, len(idx.columns))
		for i := range columns {
			columns[i] = idx.columns[i+1]
			if columns[i] == "" {
				idx.valid = false
			}
		}
		if idx.valid {
			table.UniqueSets = append(table.UniqueSets, columns)
		}
	}
	for _, table := range metadata.Tables {
		sort.Slice(table.UniqueSets, func(i, j int) bool {
			if len(table.UniqueSets[i]) != len(table.UniqueSets[j]) {
				return len(table.UniqueSets[i]) < len(table.UniqueSets[j])
			}
			return strings.Join(table.UniqueSets[i], "\x00") < strings.Join(table.UniqueSets[j], "\x00")
		})
	}
	return metadata, nil
}

// Name folding must be unambiguous before any CREATE/DROP runs. Other per-table
// DDL failures belong in the bootstrap report instead of aborting other tables.
func validateDamengNameCollisions(cfg Config, tables []TableInfo) []string {
	if cfg.TargetDBType != "dameng" {
		return nil
	}
	var issues []string
	if err := tsqldameng.ValidName(targetSchemaName(cfg)); err != nil {
		issues = append(issues, err.Error())
	}
	seen := map[string]string{}
	for _, table := range tables {
		name := targetObjectName(cfg, table.Name)
		if prior, ok := seen[name]; ok {
			issues = append(issues, fmt.Sprintf("源表 %s 与 %s 映射到相同达梦表 %s", prior, table.Name, name))
		}
		seen[name] = table.Name
		if name == strings.ToUpper(CheckpointTableName) {
			issues = append(issues, fmt.Sprintf("源表 %s 与 CDC 检查点表重名", table.Name))
		}
		columns := map[string]string{}
		for _, column := range table.Columns {
			mapped := targetObjectName(cfg, column)
			if prior, ok := columns[mapped]; ok {
				issues = append(issues, fmt.Sprintf("源表 %s 的列 %s 与 %s 映射到相同达梦列 %s", table.Name, prior, column, mapped))
			}
			columns[mapped] = column
		}
	}
	return issues
}

func validateDamengNames(cfg Config, tables []TableInfo) []string {
	if cfg.TargetDBType != "dameng" {
		return nil
	}
	issues := validateDamengNameCollisions(cfg, tables)
	for _, table := range tables {
		if err := tsqldameng.ValidName(targetObjectName(cfg, table.Name)); err != nil {
			issues = append(issues, err.Error())
		}
		for _, column := range table.Columns {
			mapped := targetObjectName(cfg, column)
			if err := tsqldameng.ValidName(mapped); err != nil {
				issues = append(issues, fmt.Sprintf("%s.%s: %v", table.Name, column, err))
			}
			if damengnames.Forbidden(mapped) {
				issues = append(issues, fmt.Sprintf("源列 %s.%s 使用达梦保留伪列名，无法直接进行 CDC", table.Name, column))
			}
		}
	}
	return issues
}
