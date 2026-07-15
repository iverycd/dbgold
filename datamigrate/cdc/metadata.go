package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate"
	_ "github.com/go-sql-driver/mysql"
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
	defer rows.Close()
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
		return nil, err
	}
	pkRows, err := db.QueryContext(ctx, `SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = ? AND CONSTRAINT_NAME = 'PRIMARY' ORDER BY TABLE_NAME, ORDINAL_POSITION`, database)
	if err != nil {
		return nil, err
	}
	defer pkRows.Close()
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
