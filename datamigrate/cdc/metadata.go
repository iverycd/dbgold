package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"dbgold/datamigrate"
	_ "github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
)

func OpenSource(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func LoadTables(ctx context.Context, db *sql.DB, database, mode, filter string) ([]TableInfo, error) {
	return loadTables(ctx, db, database, mode, filter, nil)
}

func LoadExactTables(ctx context.Context, db *sql.DB, database string, exact []string) ([]TableInfo, error) {
	return loadTables(ctx, db, database, "all", "", exact)
}

func loadTables(ctx context.Context, db *sql.DB, database, mode, filter string, exact []string) ([]TableInfo, error) {
	rows, err := db.QueryContext(ctx, `SELECT c.TABLE_NAME, c.COLUMN_NAME, c.DATA_TYPE, c.EXTRA, t.ENGINE
		FROM information_schema.COLUMNS c JOIN information_schema.TABLES t
		ON t.TABLE_SCHEMA=c.TABLE_SCHEMA AND t.TABLE_NAME=c.TABLE_NAME
		WHERE c.TABLE_SCHEMA = ? AND t.TABLE_TYPE='BASE TABLE' ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION`, database)
	if err != nil {
		return nil, err
	}
	tableMap := map[string]*TableInfo{}
	var order []string
	for rows.Next() {
		var table, col, typ, extra string
		var engine sql.NullString
		if err := rows.Scan(&table, &col, &typ, &extra, &engine); err != nil {
			return nil, err
		}
		if tableMap[table] == nil {
			tableMap[table] = &TableInfo{Name: table, Engine: engine.String}
			order = append(order, table)
		}
		t := tableMap[table]
		t.Columns = append(t.Columns, col)
		t.ColumnTypes = append(t.ColumnTypes, strings.ToUpper(typ))
		if strings.Contains(strings.ToLower(extra), "auto_increment") {
			t.AutoIncrement = append(t.AutoIncrement, len(t.Columns)-1)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	pkRows, err := db.QueryContext(ctx, `SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = ? AND CONSTRAINT_NAME = 'PRIMARY' ORDER BY TABLE_NAME, ORDINAL_POSITION`, database)
	if err != nil {
		return nil, err
	}
	for pkRows.Next() {
		var table, col string
		if err := pkRows.Scan(&table, &col); err != nil {
			return nil, err
		}
		t := tableMap[table]
		if t == nil {
			continue
		}
		for i, name := range t.Columns {
			if name == col {
				t.PrimaryKey = append(t.PrimaryKey, i)
				break
			}
		}
	}
	if err := pkRows.Err(); err != nil {
		pkRows.Close()
		return nil, err
	}
	if err := pkRows.Close(); err != nil {
		return nil, err
	}
	if err := loadUniqueIndexes(ctx, db, database, tableMap); err != nil {
		return nil, err
	}
	selected := datamigrate.FilterTables(order, mode, filter)
	if exact != nil {
		available := make(map[string]bool, len(order))
		for _, name := range order {
			available[name] = true
		}
		selected = make([]string, 0, len(exact))
		for _, name := range exact {
			if !available[name] {
				return nil, fmt.Errorf("有效 CDC 表在源库中不存在: %s", name)
			}
			selected = append(selected, name)
		}
	}
	result := make([]TableInfo, 0, len(selected))
	for _, name := range selected {
		if t := tableMap[name]; t != nil {
			result = append(result, *t)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("没有匹配的源表")
	}
	return result, nil
}

type uniqueIndexBuilder struct {
	info  UniqueIndexInfo
	valid bool
}

func loadUniqueIndexes(ctx context.Context, db *sql.DB, database string, tables map[string]*TableInfo) error {
	rows, err := db.QueryContext(ctx, `SELECT s.TABLE_NAME, s.INDEX_NAME, s.COLUMN_NAME, s.SUB_PART, s.INDEX_TYPE, c.IS_NULLABLE
		FROM information_schema.STATISTICS s
		LEFT JOIN information_schema.COLUMNS c ON c.TABLE_SCHEMA=s.TABLE_SCHEMA AND c.TABLE_NAME=s.TABLE_NAME AND c.COLUMN_NAME=s.COLUMN_NAME
		WHERE s.TABLE_SCHEMA=? AND s.NON_UNIQUE=0 AND s.INDEX_NAME<>'PRIMARY'
		ORDER BY s.TABLE_NAME, s.INDEX_NAME, s.SEQ_IN_INDEX`, database)
	if err != nil {
		return err
	}
	defer rows.Close()
	builders := map[string]*uniqueIndexBuilder{}
	var keys []string
	for rows.Next() {
		var table, index, indexType string
		var column, nullable sql.NullString
		var subPart sql.NullInt64
		if err := rows.Scan(&table, &index, &column, &subPart, &indexType, &nullable); err != nil {
			return err
		}
		key := table + "\x00" + index
		builder := builders[key]
		if builder == nil {
			builder = &uniqueIndexBuilder{info: UniqueIndexInfo{Name: index}, valid: true}
			builders[key] = builder
			keys = append(keys, key)
		}
		// COLUMN_NAME is NULL for functional/expression key parts. SUB_PART is
		// non-NULL for prefix indexes. Both are unsafe row locators.
		if !eligibleUniqueIndexPart(column.Valid, column.String, subPart.Valid, indexType, nullable.Valid, nullable.String) {
			builder.valid = false
			continue
		}
		builder.info.Columns = append(builder.info.Columns, column.String)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, key := range keys {
		parts := strings.SplitN(key, "\x00", 2)
		builder := builders[key]
		if builder.valid && len(builder.info.Columns) > 0 && tables[parts[0]] != nil {
			tables[parts[0]].UniqueIndexes = append(tables[parts[0]].UniqueIndexes, builder.info)
		}
	}
	for _, table := range tables {
		sort.Slice(table.UniqueIndexes, func(i, j int) bool {
			if len(table.UniqueIndexes[i].Columns) != len(table.UniqueIndexes[j].Columns) {
				return len(table.UniqueIndexes[i].Columns) < len(table.UniqueIndexes[j].Columns)
			}
			return table.UniqueIndexes[i].Name < table.UniqueIndexes[j].Name
		})
	}
	return nil
}

func eligibleUniqueIndexPart(columnValid bool, column string, hasPrefix bool, indexType string, nullableValid bool, nullable string) bool {
	return columnValid && column != "" && !hasPrefix && strings.EqualFold(indexType, "BTREE") && nullableValid && strings.EqualFold(nullable, "NO")
}

func LocatorStrategiesFromTables(tables []TableInfo) []LocatorStrategy {
	result := make([]LocatorStrategy, 0, len(tables))
	for _, table := range tables {
		result = append(result, LocatorStrategy{Table: table.Name, Strategy: table.LocatorStrategy, Index: table.LocatorIndex, Columns: append([]string(nil), table.LocatorColumns...)})
	}
	return result
}

func ApplyLocatorStrategies(tables []TableInfo, strategies []LocatorStrategy) error {
	byTable := make(map[string]LocatorStrategy, len(strategies))
	for _, strategy := range strategies {
		byTable[strategy.Table] = strategy
	}
	for i := range tables {
		strategy, ok := byTable[tables[i].Name]
		if !ok {
			return fmt.Errorf("表 %s 缺少冻结的 CDC 定位策略", tables[i].Name)
		}
		if strategy.Strategy != LocatorPrimaryKey && strategy.Strategy != LocatorUniqueKey && strategy.Strategy != LocatorFullRow {
			return fmt.Errorf("表 %s 的 CDC 定位策略无效: %s", tables[i].Name, strategy.Strategy)
		}
		for _, column := range strategy.Columns {
			found := false
			for _, actual := range tables[i].Columns {
				if actual == column {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("表 %s 的冻结定位列已不存在: %s", tables[i].Name, column)
			}
		}
		tables[i].LocatorStrategy = strategy.Strategy
		tables[i].LocatorIndex = strategy.Index
		tables[i].LocatorColumns = append([]string(nil), strategy.Columns...)
	}
	return nil
}

// ResolveLocatorStrategies chooses a stable locator for every table. PostgreSQL
// unique indexes (including indexes not backed by information_schema
// constraints) are considered, because the full migrator creates both forms.
func ResolveLocatorStrategies(ctx context.Context, targetDSN, schema string, lower bool, tables []TableInfo) ([]TableInfo, error) {
	db, err := sql.Open("postgres", targetDSN)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err = db.PingContext(ctx); err != nil {
		return nil, err
	}
	result := append([]TableInfo(nil), tables...)
	for i := range result {
		table := &result[i]
		if len(table.PrimaryKey) > 0 {
			table.LocatorStrategy = LocatorPrimaryKey
			table.LocatorIndex = "PRIMARY"
			table.LocatorColumns = columnNamesAt(table, table.PrimaryKey)
			continue
		}
		targetName := table.Name
		if lower {
			targetName = strings.ToLower(targetName)
		}
		uniqueSets, queryErr := loadPostgresUniqueColumnSets(ctx, db, schema, targetName)
		if queryErr != nil {
			return nil, fmt.Errorf("读取目标表唯一索引失败 %s: %w", targetName, queryErr)
		}
		selectUniqueLocator(table, uniqueSets, lower)
		if table.LocatorStrategy == "" {
			table.LocatorStrategy = LocatorFullRow
			table.LocatorColumns = append([]string(nil), table.Columns...)
			table.LocatorWarning = "没有可同时在源端和目标端确认的非空普通唯一键，UPDATE/DELETE 将按更新前整行匹配；大表可能产生全表扫描"
		}
	}
	return result, nil
}

func selectUniqueLocator(table *TableInfo, targetUniqueSets [][]string, lower bool) bool {
	candidates := append([]UniqueIndexInfo(nil), table.UniqueIndexes...)
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i].Columns) != len(candidates[j].Columns) {
			return len(candidates[i].Columns) < len(candidates[j].Columns)
		}
		return candidates[i].Name < candidates[j].Name
	})
	for _, candidate := range candidates {
		expected := make([]string, len(candidate.Columns))
		for columnIndex, column := range candidate.Columns {
			expected[columnIndex] = column
			if lower {
				expected[columnIndex] = strings.ToLower(column)
			}
		}
		for _, actual := range targetUniqueSets {
			if sameColumnSet(expected, actual) {
				table.LocatorStrategy = LocatorUniqueKey
				table.LocatorIndex = candidate.Name
				table.LocatorColumns = append([]string(nil), candidate.Columns...)
				return true
			}
		}
	}
	return false
}

func columnNamesAt(table *TableInfo, indexes []int) []string {
	columns := make([]string, 0, len(indexes))
	for _, index := range indexes {
		if index >= 0 && index < len(table.Columns) {
			columns = append(columns, table.Columns[index])
		}
	}
	return columns
}

func loadPostgresUniqueColumnSets(ctx context.Context, db *sql.DB, schema, table string) ([][]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT array_agg(a.attname ORDER BY key_column.ordinality)
		FROM pg_index i
		JOIN pg_class target_table ON target_table.oid=i.indrelid
		JOIN pg_namespace target_schema ON target_schema.oid=target_table.relnamespace
		CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS key_column(attnum, ordinality)
		JOIN pg_attribute a ON a.attrelid=target_table.oid AND a.attnum=key_column.attnum
		WHERE target_schema.nspname=$1 AND target_table.relname=$2
		AND i.indisunique AND i.indisvalid AND i.indimmediate AND i.indpred IS NULL AND i.indexprs IS NULL
		AND key_column.ordinality <= i.indnkeyatts
		GROUP BY i.indexrelid`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result [][]string
	for rows.Next() {
		var columns []string
		if err := rows.Scan(pq.Array(&columns)); err != nil {
			return nil, err
		}
		result = append(result, columns)
	}
	return result, rows.Err()
}

func LoadConfiguredTables(ctx context.Context, db *sql.DB, cfg Config) ([]TableInfo, error) {
	if cfg.TableNames != nil {
		if len(cfg.TableNames) == 0 {
			return nil, fmt.Errorf("有效 CDC 表清单为空或损坏，已拒绝回退到原始表过滤条件")
		}
		return LoadExactTables(ctx, db, cfg.SourceDatabase, cfg.TableNames)
	}
	return LoadTables(ctx, db, cfg.SourceDatabase, cfg.Mode, cfg.Filter)
}

func tableMap(tables []TableInfo) map[string]*TableInfo {
	m := make(map[string]*TableInfo, len(tables))
	for i := range tables {
		m[tables[i].Name] = &tables[i]
	}
	return m
}
