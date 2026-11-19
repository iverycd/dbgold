package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/validation"
)

// ValidateCounts compares selected source/target tables after the cutover
// boundary has been reached. The caller must keep source writes stopped.
func ValidateCounts(ctx context.Context, cfg Config) ([]CountValidation, bool, error) {
	src, err := sql.Open("mysql", cfg.SourceDSN)
	if err != nil {
		return nil, false, err
	}
	defer src.Close()
	dst, err := openTargetDB(cfg)
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
	if len(tables) == 0 {
		return nil, false, fmt.Errorf("有效 CDC 表清单为空，无法校验行数")
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
		targetName := targetObjectName(cfg, t.Name)
		target := quoteIdent(targetSchemaName(cfg)) + "." + quoteIdent(targetName)
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

// CutoverTableValidation keeps the historical count-report fields alongside
// the complete content scan and bounded, non-value difference details.
type CutoverTableValidation struct {
	CountValidation
	Content *validation.TableResult `json:"content,omitempty"`
}

// ValidateCutoverContents compares the exact table and column mapping from
// the completed checkpoint. Both databases must remain quiescent.
func ValidateCutoverContents(ctx context.Context, cfg Config, counts []CountValidation) ([]CutoverTableValidation, bool, bool) {
	results := make([]CutoverTableValidation, len(counts))
	for i, count := range counts {
		results[i].CountValidation = count
	}
	src, err := sql.Open("mysql", cfg.SourceDSN)
	if err != nil {
		markCutoverContentError(cfg, results, fmt.Errorf("读取源库失败: %w", err))
		return results, false, false
	}
	defer src.Close()
	dst, err := openTargetDB(cfg)
	if err != nil {
		markCutoverContentError(cfg, results, fmt.Errorf("读取目标库失败: %w", err))
		return results, false, false
	}
	defer dst.Close()
	tables, err := cfg.SourceSchema.TableInfos(cfg.SourceDatabase, cfg.TableNames)
	if err != nil {
		markCutoverContentError(cfg, results, err)
		return results, false, false
	}
	dialect := "postgres"
	if cfg.TargetDBType == "dameng" {
		dialect = "dameng"
	}
	srcScan := validation.NewSQLScanner(src, "mysql", cfg.SourceDatabase)
	dstScan := validation.NewSQLScanner(dst, dialect, targetSchemaName(cfg))
	return validateCutoverContents(ctx, cfg, results, tables, srcScan, dstScan)
}

func markCutoverContentError(cfg Config, results []CutoverTableValidation, err error) {
	for i := range results {
		name := results[i].Table
		results[i].Content = &validation.TableResult{SourceTable: name, TargetTable: targetObjectName(cfg, name), Status: validation.StatusError, Error: err.Error()}
	}
}

func validateCutoverContents(ctx context.Context, cfg Config, results []CutoverTableValidation, tables []TableInfo, src, dst validation.Scanner) ([]CutoverTableValidation, bool, bool) {
	byName := tableMap(tables)
	allMatch, countsChanged := len(results) > 0, false
	for i := range results {
		item := &results[i]
		name, mapped := item.Table, targetObjectName(cfg, item.Table)
		result := validation.TableResult{SourceTable: name, TargetTable: mapped}
		plan, err := cutoverContentPlan(ctx, cfg, byName[name], mapped, src, dst)
		if err != nil {
			result.Status, result.Error = validation.StatusError, err.Error()
		} else {
			result = validation.Validate(ctx, src, dst, plan)
			for i := range result.Samples {
				result.Samples[i].Key, result.Samples[i].Fields = nil, nil
			}
			if result.Status == validation.StatusPassed || result.Status == validation.StatusMismatch {
				if result.SourceRows != result.TargetRows || result.SourceRows != item.Source || result.TargetRows != item.Target {
					countsChanged = true
				}
			}
		}
		item.Content = &result
		allMatch = allMatch && result.Status == validation.StatusPassed
	}
	return results, allMatch, countsChanged
}

func cutoverContentPlan(ctx context.Context, cfg Config, snapshot *TableInfo, targetTable string, src, dst validation.Scanner) (validation.Plan, error) {
	if snapshot == nil {
		return validation.Plan{}, fmt.Errorf("表不在冻结的 CDC 源结构中")
	}
	plan := validation.Plan{SourceTable: snapshot.Name, TargetTable: targetTable}
	srcMeta, err := src.DescribeValidationTable(ctx, plan.SourceTable)
	if err != nil {
		return plan, fmt.Errorf("读取源表校验元数据: %w", err)
	}
	dstMeta, err := dst.DescribeValidationTable(ctx, plan.TargetTable)
	if err != nil {
		return plan, fmt.Errorf("读取目标表校验元数据: %w", err)
	}
	for _, i := range snapshot.PrimaryKey {
		plan.PrimaryKey = append(plan.PrimaryKey, snapshot.Columns[i])
	}
	if !slices.Equal(srcMeta.PrimaryKey, plan.PrimaryKey) || len(srcMeta.Columns) != len(snapshot.Columns) {
		return plan, fmt.Errorf("表 %s 当前源结构与冻结的 CDC 结构不一致", snapshot.Name)
	}
	dstColumns := make(map[string]validation.Column, len(dstMeta.Columns))
	for _, col := range dstMeta.Columns {
		dstColumns[col.Name] = col
	}
	for i, name := range snapshot.Columns {
		if srcMeta.Columns[i].Name != name {
			return plan, fmt.Errorf("表 %s 当前源列顺序与冻结的 CDC 结构不一致", snapshot.Name)
		}
		targetName := targetObjectName(cfg, name)
		col, ok := dstColumns[targetName]
		if !ok {
			return plan, fmt.Errorf("目标表 %s 缺少映射列 %s", targetTable, targetName)
		}
		plan.SourceColumns = append(plan.SourceColumns, srcMeta.Columns[i])
		plan.TargetColumns = append(plan.TargetColumns, col)
	}
	return plan, nil
}
