package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

const damengMetadataBatchSize = 128

// scopeDamengMetadataQuery only accepts the catalog queries used below. Names
// are parameters; folding has already happened at the target boundary.
func scopeDamengMetadataQuery(query string, names []string) (string, error) {
	var column string
	switch query {
	case loadDamengTargetTablesSQL:
		column = "TABLE_NAME"
	case loadDamengTargetColumnsSQL:
		column = "c.TABLE_NAME"
	case loadDamengTargetIndexesSQL, loadDamengTargetIndexColumnsSQL:
		column = "TABLE_NAME"
	case damengBusinessTriggersSQL:
		column = "TABLE_NAME"
	default:
		return "", fmt.Errorf("未知达梦 CDC 元数据查询")
	}
	if len(names) == 0 || len(names) > damengMetadataBatchSize {
		return "", fmt.Errorf("达梦 CDC 元数据批量大小无效")
	}
	predicate := " AND " + column + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(names)), ",") + ")"
	if i := strings.Index(query, " ORDER BY "); i >= 0 {
		return query[:i] + predicate + query[i:], nil
	}
	return query + predicate, nil
}

type scopedDamengMetadata struct {
	db    targetMetadataReader
	names []string
}

func (s scopedDamengMetadata) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	query, err := scopeDamengMetadataQuery(query, s.names)
	if err != nil {
		return nil, err
	}
	args = append([]any(nil), args...)
	for _, name := range s.names {
		args = append(args, name)
	}
	return s.db.QueryContext(ctx, query, args...)
}
func damengTargetNames(cfg Config, tables []TableInfo) []string {
	seen := map[string]bool{}
	var names []string
	for _, table := range tables {
		n := targetObjectName(cfg, table.Name)
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}
func loadDamengTransactionMetadata(ctx context.Context, db targetMetadataReader, cfg Config, tables []TableInfo) (targetSchemaMetadata, error) {
	result := targetSchemaMetadata{Tables: map[string]*targetTableMetadata{}}
	names := damengTargetNames(cfg, tables)
	for start := 0; start < len(names); start += damengMetadataBatchSize {
		scoped := scopedDamengMetadata{db: db, names: names[start:min(start+damengMetadataBatchSize, len(names))]}
		if err := checkDamengEnabledTriggers(ctx, scoped, cfg, tables); err != nil {
			return result, err
		}
		batch, err := loadDamengTargetSchemaMetadata(ctx, scoped, targetSchemaName(cfg))
		if err != nil {
			return result, err
		}
		for name, table := range batch.Tables {
			result.Tables[name] = table
		}
	}
	return result, nil
}
