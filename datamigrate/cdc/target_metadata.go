package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const loadTargetTablesSQL = `SELECT table_name
	FROM information_schema.tables
	WHERE table_schema=$1 AND table_type='BASE TABLE'
	ORDER BY table_name`

const loadTargetColumnsSQL = `SELECT table_name, column_name, is_nullable, column_default, is_identity
	FROM information_schema.columns
	WHERE table_schema=$1
	ORDER BY table_name, ordinal_position`

// Keep this query compatible with the GaussDB catalog dialect. In particular,
// do not use WITH ORDINALITY, LATERAL, generate_subscripts, or indnkeyatts.
// indkey is returned as text so Go can restore index-key order without a
// correlated set-returning function in the FROM clause.
const loadTargetUniqueIndexesSQL = `SELECT target_table.relname, i.indexrelid, i.indkey::text, a.attnum, a.attname
	FROM pg_index i
	JOIN pg_class target_table ON target_table.oid=i.indrelid
	JOIN pg_namespace target_schema ON target_schema.oid=target_table.relnamespace
	JOIN pg_attribute a ON a.attrelid=target_table.oid AND a.attnum=ANY(i.indkey)
	WHERE target_schema.nspname=$1
	AND i.indisunique AND i.indisvalid AND i.indimmediate AND i.indpred IS NULL AND i.indexprs IS NULL
	AND a.attnum > 0
	ORDER BY target_table.relname, i.indexrelid, a.attnum`

type targetColumnMetadata struct {
	Name         string
	Nullable     string
	DefaultValue sql.NullString
	Identity     string
}

type targetTableMetadata struct {
	Columns     []targetColumnMetadata
	ColumnNames map[string]bool
	UniqueSets  [][]string
}

type targetSchemaMetadata struct {
	Tables map[string]*targetTableMetadata
}

type targetUniqueIndexBuilder struct {
	Table    string
	KeyOrder []int
	Columns  map[int]string
}

func loadTargetSchemaMetadata(ctx context.Context, db *sql.DB, schema string) (targetSchemaMetadata, error) {
	metadata := targetSchemaMetadata{Tables: map[string]*targetTableMetadata{}}
	tableRows, err := db.QueryContext(ctx, loadTargetTablesSQL, schema)
	if err != nil {
		return metadata, fmt.Errorf("读取目标表清单失败: %w", err)
	}
	for tableRows.Next() {
		var name string
		if err = tableRows.Scan(&name); err != nil {
			tableRows.Close()
			return metadata, fmt.Errorf("读取目标表清单失败: %w", err)
		}
		metadata.Tables[name] = &targetTableMetadata{ColumnNames: map[string]bool{}}
	}
	if err = closeRows(tableRows); err != nil {
		return metadata, fmt.Errorf("读取目标表清单失败: %w", err)
	}

	columnRows, err := db.QueryContext(ctx, loadTargetColumnsSQL, schema)
	if err != nil {
		return metadata, fmt.Errorf("读取目标列失败: %w", err)
	}
	for columnRows.Next() {
		var table string
		var column targetColumnMetadata
		if err = columnRows.Scan(&table, &column.Name, &column.Nullable, &column.DefaultValue, &column.Identity); err != nil {
			columnRows.Close()
			return metadata, fmt.Errorf("读取目标列失败: %w", err)
		}
		if target := metadata.Tables[table]; target != nil {
			target.Columns = append(target.Columns, column)
			target.ColumnNames[column.Name] = true
		}
	}
	if err = closeRows(columnRows); err != nil {
		return metadata, fmt.Errorf("读取目标列失败: %w", err)
	}

	indexRows, err := db.QueryContext(ctx, loadTargetUniqueIndexesSQL, schema)
	if err != nil {
		return metadata, fmt.Errorf("读取目标唯一索引失败: %w", err)
	}
	builders := map[string]*targetUniqueIndexBuilder{}
	var builderKeys []string
	for indexRows.Next() {
		var table, indkey, column string
		var indexOID int64
		var attributeNumber int
		if err = indexRows.Scan(&table, &indexOID, &indkey, &attributeNumber, &column); err != nil {
			indexRows.Close()
			return metadata, fmt.Errorf("读取目标唯一索引失败: %w", err)
		}
		if metadata.Tables[table] == nil {
			continue
		}
		key := table + "\x00" + strconv.FormatInt(indexOID, 10)
		builder := builders[key]
		if builder == nil {
			order, parseErr := parseIndexKeyOrder(indkey)
			if parseErr != nil {
				indexRows.Close()
				return metadata, fmt.Errorf("解析目标唯一索引列顺序失败 %s: %w", table, parseErr)
			}
			builder = &targetUniqueIndexBuilder{Table: table, KeyOrder: order, Columns: map[int]string{}}
			builders[key] = builder
			builderKeys = append(builderKeys, key)
		}
		builder.Columns[attributeNumber] = column
	}
	if err = closeRows(indexRows); err != nil {
		return metadata, fmt.Errorf("读取目标唯一索引失败: %w", err)
	}
	for _, key := range builderKeys {
		builder := builders[key]
		columns := make([]string, 0, len(builder.KeyOrder))
		for _, attributeNumber := range builder.KeyOrder {
			if column := builder.Columns[attributeNumber]; column != "" {
				columns = append(columns, column)
			}
		}
		if len(columns) > 0 {
			metadata.Tables[builder.Table].UniqueSets = append(metadata.Tables[builder.Table].UniqueSets, columns)
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

func closeRows(rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}

func parseIndexKeyOrder(indkey string) ([]int, error) {
	fields := strings.Fields(indkey)
	result := make([]int, 0, len(fields))
	for _, field := range fields {
		attributeNumber, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("无效的 pg_index.indkey %q", indkey)
		}
		if attributeNumber > 0 {
			result = append(result, attributeNumber)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("pg_index.indkey 为空: %q", indkey)
	}
	return result, nil
}

func targetTableName(cfg Config, sourceName string) string {
	if cfg.LowerCaseNames {
		return strings.ToLower(sourceName)
	}
	return sourceName
}

func resolveLocatorStrategiesFromMetadata(cfg Config, tables []TableInfo, metadata targetSchemaMetadata) []TableInfo {
	result := append([]TableInfo(nil), tables...)
	for i := range result {
		table := &result[i]
		if len(table.PrimaryKey) > 0 {
			table.LocatorStrategy = LocatorPrimaryKey
			table.LocatorIndex = "PRIMARY"
			table.LocatorColumns = columnNamesAt(table, table.PrimaryKey)
			continue
		}
		var uniqueSets [][]string
		if target := metadata.Tables[targetTableName(cfg, table.Name)]; target != nil {
			uniqueSets = target.UniqueSets
		}
		selectUniqueLocator(table, uniqueSets, cfg.LowerCaseNames)
		if table.LocatorStrategy == "" {
			table.LocatorStrategy = LocatorFullRow
			table.LocatorColumns = append([]string(nil), table.Columns...)
			table.LocatorWarning = "没有可同时在源端和目标端确认的非空普通唯一键，UPDATE/DELETE 将按更新前整行匹配；大表可能产生全表扫描"
		}
	}
	return result
}
