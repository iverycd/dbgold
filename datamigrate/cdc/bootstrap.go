package cdc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"dbgold/datamigrate"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

func BuildBootstrapReview(position Position, expected []string, report datamigrate.MigrationReport, lower bool, compatibility map[string]string) BootstrapReview {
	issues := make(map[string]BootstrapIssue, len(expected))
	for _, item := range report.Tables.Items {
		issues[item.Name] = BootstrapIssue{Table: item.Name, Stage: "schema", Error: item.Error, DDL: item.DDL}
	}
	for _, item := range report.Data.Items {
		if _, failed := issues[item.Name]; !failed {
			issues[item.Name] = BootstrapIssue{Table: item.Name, Stage: "data", Error: item.Error}
		}
	}
	targetToSource := make(map[string]string, len(expected))
	for _, table := range expected {
		targetName := table
		if lower {
			targetName = strings.ToLower(table)
		}
		targetToSource[targetName] = table
	}
	for _, item := range report.PrimaryKeys.Items {
		if table, found := targetToSource[item.Name]; found {
			if _, failed := issues[table]; !failed {
				issues[table] = BootstrapIssue{Table: table, Stage: "cdc_compatibility", Error: "创建源主键对应约束失败: " + item.Error, DDL: item.DDL}
			}
		}
	}
	rowCounts := make(map[string]datamigrate.TableRowCount, len(report.RowCounts))
	for _, row := range report.RowCounts {
		rowCounts[row.Table] = row
	}
	for _, table := range expected {
		if _, failed := issues[table]; failed {
			continue
		}
		targetName := table
		if lower {
			targetName = strings.ToLower(table)
		}
		row, exists := rowCounts[targetName]
		if !exists {
			issues[table] = BootstrapIssue{Table: table, Stage: "row_count", Error: "全量快照未完成该表行数校验"}
			continue
		}
		if !row.Match {
			issues[table] = BootstrapIssue{Table: table, Stage: "row_count", Error: fmt.Sprintf("源目标行数不一致: source=%d target=%d", row.Src, row.Dst)}
			continue
		}
		if message := compatibility[table]; message != "" {
			issues[table] = BootstrapIssue{Table: table, Stage: "cdc_compatibility", Error: message}
		}
	}

	review := BootstrapReview{
		BootstrapRecord: BootstrapRecord{
			State:                "review_pending",
			Position:             position,
			FailedObjects:        BuildBootstrapFailedObjects(expected, report, lower, compatibility),
			FailureReportVersion: 1,
		},
		RequestedCount: len(expected),
		Warnings:       bootstrapObjectWarnings(report),
	}
	for _, table := range expected {
		if issue, failed := issues[table]; failed {
			review.ExcludedTables = append(review.ExcludedTables, issue)
		} else {
			review.EffectiveTables = append(review.EffectiveTables, table)
		}
	}
	review.ManifestHash = HashBootstrapManifest(review.BootstrapRecord)
	return review
}

// BuildBootstrapFailedObjects converts the migrator report into a durable,
// category-aware artifact. It deliberately remains separate from the
// exclusion manifest: an index or view failure must be exportable without
// changing which tables participate in CDC.
func BuildBootstrapFailedObjects(expected []string, report datamigrate.MigrationReport, lower bool, compatibility map[string]string) []BootstrapFailedObject {
	items := make([]BootstrapFailedObject, 0)
	seen := make(map[string]bool)
	failedTables := make(map[string]bool)
	appendItems := func(category, stage string, failures []datamigrate.ObjectResult) {
		for _, item := range failures {
			failure := BootstrapFailedObject{Category: category, Name: item.Name, Error: item.Error, DDL: item.DDL, Stage: stage}
			key := strings.Join([]string{failure.Category, failure.Name, failure.Error, failure.DDL, failure.Stage}, "\x00")
			if !seen[key] {
				seen[key] = true
				items = append(items, failure)
			}
		}
	}
	appendItems("table", "schema", report.Tables.Items)
	appendItems("data", "data", report.Data.Items)
	appendItems("primary_key", "objects", report.PrimaryKeys.Items)
	appendItems("sequence", "objects", report.Sequences.Items)
	appendItems("index", "objects", report.Indexes.Items)
	appendItems("foreign_key", "objects", report.Constraints.Items)
	appendItems("view", "objects", report.Views.Items)
	appendItems("comment", "objects", report.Comments.Items)
	for _, item := range report.Tables.Items {
		failedTables[item.Name] = true
	}
	for _, item := range report.Data.Items {
		failedTables[item.Name] = true
	}
	targetToSource := make(map[string]string, len(expected))
	for _, table := range expected {
		targetName := table
		if lower {
			targetName = strings.ToLower(table)
		}
		targetToSource[targetName] = table
	}
	for _, item := range report.PrimaryKeys.Items {
		if table, found := targetToSource[item.Name]; found {
			failedTables[table] = true
		}
	}

	targetRows := make(map[string]datamigrate.TableRowCount, len(report.RowCounts))
	for _, row := range report.RowCounts {
		targetRows[row.Table] = row
	}
	for _, table := range expected {
		if failedTables[table] {
			continue
		}
		targetName := table
		if lower {
			targetName = strings.ToLower(table)
		}
		row, exists := targetRows[targetName]
		if !exists {
			items = append(items, BootstrapFailedObject{Category: "row_count", Name: table, Error: "全量快照未完成该表行数校验", Stage: "validation"})
		} else if !row.Match {
			items = append(items, BootstrapFailedObject{Category: "row_count", Name: table, Error: fmt.Sprintf("源目标行数不一致: source=%d target=%d", row.Src, row.Dst), Stage: "validation"})
		}
		if message := compatibility[table]; message != "" {
			items = append(items, BootstrapFailedObject{Category: "cdc_compatibility", Name: table, Error: message, Stage: "validation"})
		}
	}
	return items
}

func bootstrapObjectWarnings(report datamigrate.MigrationReport) []string {
	var warnings []string
	appendItems := func(kind string, items []datamigrate.ObjectResult) {
		for _, item := range items {
			warnings = append(warnings, fmt.Sprintf("%s创建失败 [%s]: %s", kind, item.Name, item.Error))
		}
	}
	appendItems("索引", report.Indexes.Items)
	appendItems("外键", report.Constraints.Items)
	appendItems("序列", report.Sequences.Items)
	appendItems("注释", report.Comments.Items)
	appendItems("视图", report.Views.Items)
	return warnings
}

func HashBootstrapManifest(record BootstrapRecord) string {
	effective := append([]string(nil), record.EffectiveTables...)
	excluded := append([]BootstrapIssue(nil), record.ExcludedTables...)
	sort.Strings(effective)
	sort.Slice(excluded, func(i, j int) bool {
		if excluded[i].Table != excluded[j].Table {
			return excluded[i].Table < excluded[j].Table
		}
		if excluded[i].Stage != excluded[j].Stage {
			return excluded[i].Stage < excluded[j].Stage
		}
		if excluded[i].Error != excluded[j].Error {
			return excluded[i].Error < excluded[j].Error
		}
		return excluded[i].DDL < excluded[j].DDL
	})
	payload, _ := json.Marshal(struct {
		Position  Position         `json:"position"`
		Effective []string         `json:"effective"`
		Excluded  []BootstrapIssue `json:"excluded"`
	}{record.Position, effective, excluded})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func ValidateTargetTableCompatibility(ctx context.Context, cfg Config, tables []TableInfo) (map[string]string, error) {
	db, err := sql.Open("postgres", cfg.TargetDSN)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err = db.PingContext(ctx); err != nil {
		return nil, err
	}
	failures := make(map[string]string)
	for _, table := range tables {
		targetName := table.Name
		if cfg.LowerCaseNames {
			targetName = strings.ToLower(targetName)
		}
		var exists bool
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables
			WHERE table_schema=$1 AND table_name=$2)`, cfg.TargetSchema, targetName).Scan(&exists); err != nil {
			failures[table.Name] = "检查目标表失败: " + err.Error()
			continue
		}
		if !exists {
			failures[table.Name] = "目标表不存在"
			continue
		}
		rows, queryErr := db.QueryContext(ctx, `SELECT column_name, is_nullable, column_default, is_identity
			FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2`, cfg.TargetSchema, targetName)
		if queryErr != nil {
			failures[table.Name] = "读取目标列失败: " + queryErr.Error()
			continue
		}
		targetColumns := map[string]bool{}
		type targetColumn struct {
			name, nullable, identity string
			defaultValue             sql.NullString
		}
		var targetMetadata []targetColumn
		for rows.Next() {
			var col targetColumn
			if scanErr := rows.Scan(&col.name, &col.nullable, &col.defaultValue, &col.identity); scanErr != nil {
				queryErr = scanErr
				break
			}
			targetColumns[col.name] = true
			targetMetadata = append(targetMetadata, col)
		}
		if closeErr := rows.Close(); queryErr == nil {
			queryErr = closeErr
		}
		if queryErr != nil {
			failures[table.Name] = "读取目标列失败: " + queryErr.Error()
			continue
		}
		sourceColumns := map[string]bool{}
		for _, sourceColumn := range table.Columns {
			expected := sourceColumn
			if cfg.LowerCaseNames {
				expected = strings.ToLower(expected)
			}
			sourceColumns[expected] = true
			if !targetColumns[expected] {
				failures[table.Name] = "目标表缺少列: " + expected
				break
			}
		}
		if failures[table.Name] != "" {
			continue
		}
		for _, col := range targetMetadata {
			if !sourceColumns[col.name] && col.nullable == "NO" && !col.defaultValue.Valid && col.identity != "YES" {
				failures[table.Name] = "目标表存在无默认值的额外必填列: " + col.name
				break
			}
		}
		if failures[table.Name] != "" || len(table.PrimaryKey) == 0 {
			continue
		}
		expectedPK := make([]string, 0, len(table.PrimaryKey))
		for _, index := range table.PrimaryKey {
			column := table.Columns[index]
			if cfg.LowerCaseNames {
				column = strings.ToLower(column)
			}
			expectedPK = append(expectedPK, column)
		}
		constraintSets, constraintErr := loadPostgresUniqueColumnSets(ctx, db, cfg.TargetSchema, targetName)
		if constraintErr != nil {
			failures[table.Name] = "读取目标唯一约束失败: " + constraintErr.Error()
			continue
		}
		matched := false
		for _, columns := range constraintSets {
			if sameColumnSet(expectedPK, columns) {
				matched = true
			}
		}
		if !matched {
			failures[table.Name] = "目标表缺少与源主键列一致的主键/唯一约束"
		}
	}
	return failures, nil
}

func ValidatePositionAvailable(ctx context.Context, cfg Config, position Position) error {
	db, err := OpenSource(cfg.SourceDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	if position.GTID != "" {
		startSet, err := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, position.GTID)
		if err != nil {
			return fmt.Errorf("GTID 格式错误: %w", err)
		}
		var executedText string
		if err = db.QueryRowContext(ctx, `SELECT @@GLOBAL.gtid_executed`).Scan(&executedText); err != nil {
			return fmt.Errorf("读取 gtid_executed 失败: %w", err)
		}
		executedSet, err := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, executedText)
		if err != nil || !executedSet.Contain(startSet) {
			return fmt.Errorf("快照 GTID 不是源库当前 executed GTID 的子集")
		}
		var purgedText string
		if err = db.QueryRowContext(ctx, `SELECT @@GLOBAL.gtid_purged`).Scan(&purgedText); err != nil {
			return fmt.Errorf("读取 gtid_purged 失败: %w", err)
		}
		if strings.TrimSpace(purgedText) != "" {
			purgedSet, parseErr := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, purgedText)
			if parseErr != nil || !startSet.Contain(purgedSet) {
				return fmt.Errorf("快照之后所需 GTID 已被清理")
			}
		}
		return nil
	}
	if position.File == "" || position.Pos < 4 {
		return fmt.Errorf("快照 file/position 无效")
	}
	found, size, err := findBinlogFile(ctx, db, position.File)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("快照 binlog 文件已不存在或已被清理: %s", position.File)
	}
	if uint64(position.Pos) > size {
		return fmt.Errorf("快照 position 超过 binlog 文件大小")
	}
	return nil
}
