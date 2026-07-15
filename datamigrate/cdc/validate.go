package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/lib/pq"
)

// ValidateCounts compares selected source/target tables after the cutover
// boundary has been reached. The caller must keep source writes stopped.
func ValidateCounts(ctx context.Context, cfg Config) ([]CountValidation, bool, error) {
	src, err := OpenSource(cfg.SourceDSN)
	if err != nil {
		return nil, false, err
	}
	defer src.Close()
	dst, err := sql.Open("postgres", cfg.TargetDSN)
	if err != nil {
		return nil, false, err
	}
	defer dst.Close()
	if err = dst.PingContext(ctx); err != nil {
		return nil, false, err
	}
	tables, err := LoadConfiguredTables(ctx, src, cfg)
	if err != nil {
		return nil, false, err
	}
	results := make([]CountValidation, 0, len(tables))
	allMatch := true
	for _, t := range tables {
		item := CountValidation{Table: t.Name}
		srcTable := "`" + strings.ReplaceAll(t.Name, "`", "``") + "`"
		if err = src.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+srcTable).Scan(&item.Source); err != nil {
			item.Error = "源库计数失败: " + err.Error()
			allMatch = false
			results = append(results, item)
			continue
		}
		targetName := t.Name
		if cfg.LowerCaseNames {
			targetName = strings.ToLower(targetName)
		}
		target := quoteIdent(cfg.TargetSchema) + "." + quoteIdent(targetName)
		if err = dst.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", target)).Scan(&item.Target); err != nil {
			item.Error = "目标库计数失败: " + err.Error()
			allMatch = false
			results = append(results, item)
			continue
		}
		item.Match = item.Source == item.Target
		if !item.Match {
			allMatch = false
		}
		results = append(results, item)
	}
	return results, allMatch, nil
}
