package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

func Preflight(ctx context.Context, cfg Config, incrementalOnly bool) PreflightResult {
	r := PreflightResult{}
	src, err := OpenSource(cfg.SourceDSN)
	if err != nil {
		r.Errors = append(r.Errors, "连接 MySQL 失败: "+err.Error())
		return r
	}
	defer src.Close()
	var logBin, format, image, gtid string
	if err := src.QueryRowContext(ctx, `SELECT @@log_bin, @@binlog_format, @@binlog_row_image, @@gtid_mode`).Scan(&logBin, &format, &image, &gtid); err != nil {
		r.Errors = append(r.Errors, "读取 binlog 配置失败: "+err.Error())
		return r
	}
	r.LogBin = strings.EqualFold(logBin, "ON") || logBin == "1"
	r.BinlogFormat, r.BinlogRowImage, r.GTIDMode = format, image, gtid
	if !r.LogBin {
		r.Errors = append(r.Errors, "MySQL 未启用 binlog")
	}
	if !strings.EqualFold(format, "ROW") {
		r.Errors = append(r.Errors, "binlog_format 必须为 ROW")
	}
	if !strings.EqualFold(image, "FULL") {
		r.Errors = append(r.Errors, "binlog_row_image 必须为 FULL")
	}
	if !strings.EqualFold(gtid, "ON") && !strings.EqualFold(gtid, "OFF") {
		r.Errors = append(r.Errors, "gtid_mode 必须处于稳定的 ON 或 OFF 状态，不支持 PERMISSIVE 过渡状态")
	}
	r.RetentionSecs, err = readBinlogRetention(ctx, func(ctx context.Context, variable string) (string, error) {
		var value string
		query := `SELECT @@GLOBAL.` + variable // variable 仅来自 readBinlogRetention 内部常量。
		if queryErr := src.QueryRowContext(ctx, query).Scan(&value); queryErr != nil {
			return "", queryErr
		}
		return value, nil
	})
	if err != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("无法读取 binlog 保留时间（%s），请人工确认其长于全量快照+追赶耗时", compactError(err)))
	} else if warning := retentionDurationWarning(r.RetentionSecs); warning != "" {
		r.Warnings = append(r.Warnings, warning)
	}
	if pos, e := CurrentPosition(ctx, src); e != nil {
		r.Errors = append(r.Errors, "读取 binlog 位点失败: "+e.Error())
	} else {
		r.CurrentPosition = pos
	}
	if grants, e := readGrants(ctx, src); e != nil {
		r.Warnings = append(r.Warnings, "无法确认 CDC 账号权限: "+e.Error())
	} else {
		upper := strings.ToUpper(grants)
		all := strings.Contains(upper, "ALL PRIVILEGES")
		if !all && !strings.Contains(upper, "REPLICATION SLAVE") && !strings.Contains(upper, "REPLICATION REPLICA") {
			r.Errors = append(r.Errors, "CDC 账号缺少 REPLICATION SLAVE/REPLICA 权限")
		}
		if !incrementalOnly && !all && !strings.Contains(upper, "RELOAD") && !strings.Contains(upper, "FLUSH_TABLES") {
			r.Errors = append(r.Errors, "全量一致快照账号缺少 RELOAD 或 FLUSH_TABLES 权限")
		}
	}
	tables, err := LoadTables(ctx, src, cfg.SourceDatabase, cfg.Mode, cfg.Filter)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
	} else {
		r.Tables = tables
		for _, t := range tables {
			targetName := t.Name
			if cfg.LowerCaseNames {
				targetName = strings.ToLower(targetName)
			}
			if targetName == CheckpointTableName {
				r.Errors = append(r.Errors, "源表名与 CDC 保留表冲突: "+t.Name)
			}
			if !incrementalOnly && !strings.EqualFold(t.Engine, "InnoDB") {
				r.Errors = append(r.Errors, fmt.Sprintf("全量一致快照仅支持 InnoDB 表: %s (engine=%s)", t.Name, t.Engine))
			}
			if len(t.PrimaryKey) == 0 {
				r.NoPrimaryKey = append(r.NoPrimaryKey, t.Name)
			}
		}
	}
	if incrementalOnly {
		if cfg.Start.GTID == "" && (cfg.Start.File == "" || cfg.Start.Pos < 4) {
			r.Errors = append(r.Errors, "仅增量模式必须提供 GTID 或 binlog 文件与位置")
		}
		if cfg.Start.GTID != "" {
			startSet, parseErr := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, cfg.Start.GTID)
			if parseErr != nil {
				r.Errors = append(r.Errors, "GTID 格式错误: "+parseErr.Error())
			} else if !strings.EqualFold(r.GTIDMode, "ON") {
				r.Errors = append(r.Errors, "源库 gtid_mode 未开启，不能使用 GTID 起点")
			} else {
				currentSet, currentErr := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, r.CurrentPosition.GTID)
				if currentErr != nil || !currentSet.Contain(startSet) {
					r.Errors = append(r.Errors, "手工 GTID 起点不是源库当前 executed GTID 的子集")
				}
				var purgedText string
				if purgeErr := src.QueryRowContext(ctx, `SELECT @@GLOBAL.gtid_purged`).Scan(&purgedText); purgeErr != nil {
					r.Errors = append(r.Errors, "读取 gtid_purged 失败: "+purgeErr.Error())
				} else if purgedText != "" {
					purgedSet, purgeParseErr := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, purgedText)
					if purgeParseErr != nil || !startSet.Contain(purgedSet) {
						r.Errors = append(r.Errors, "手工 GTID 起点未包含已清理的 GTID，无法从该位点完整恢复")
					}
				}
			}
		} else if cfg.Start.File != "" {
			found, size, listErr := findBinlogFile(ctx, src, cfg.Start.File)
			if listErr != nil {
				r.Errors = append(r.Errors, "校验 binlog 起点失败: "+listErr.Error())
			} else if !found {
				r.Errors = append(r.Errors, "binlog 文件已不存在或已被清理: "+cfg.Start.File)
			} else if uint64(cfg.Start.Pos) > size {
				r.Errors = append(r.Errors, fmt.Sprintf("binlog 起点超过文件大小: position=%d size=%d", cfg.Start.Pos, size))
			}
		}
	}
	dst, e := sql.Open("postgres", cfg.TargetDSN)
	if e != nil {
		r.Errors = append(r.Errors, "连接 PostgreSQL 失败: "+e.Error())
	} else {
		defer dst.Close()
		if e = dst.PingContext(ctx); e != nil {
			r.Errors = append(r.Errors, "连接 PostgreSQL 失败: "+e.Error())
		} else {
			var schemaExists bool
			if e = dst.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, cfg.TargetSchema).Scan(&schemaExists); e != nil || !schemaExists {
				r.Errors = append(r.Errors, "目标 Schema 不存在: "+cfg.TargetSchema)
			}
		}
		if incrementalOnly {
			for _, t := range tables {
				name := t.Name
				if cfg.LowerCaseNames {
					name = strings.ToLower(name)
				}
				var exists bool
				e = dst.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name=$2)`, cfg.TargetSchema, name).Scan(&exists)
				if e != nil || !exists {
					r.Errors = append(r.Errors, fmt.Sprintf("目标表不存在: %s.%s", cfg.TargetSchema, name))
					continue
				}
				colRows, colErr := dst.QueryContext(ctx, `SELECT column_name, is_nullable, column_default, is_identity
					FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2`, cfg.TargetSchema, name)
				if colErr != nil {
					r.Errors = append(r.Errors, fmt.Sprintf("读取目标表列失败 %s: %v", name, colErr))
					continue
				}
				targetCols := map[string]bool{}
				type extraColumn struct {
					name, nullable, identity string
					defaultValue             sql.NullString
				}
				var targetMetadata []extraColumn
				for colRows.Next() {
					var col extraColumn
					if colRows.Scan(&col.name, &col.nullable, &col.defaultValue, &col.identity) == nil {
						targetCols[col.name] = true
						targetMetadata = append(targetMetadata, col)
					}
				}
				colRows.Close()
				sourceCols := map[string]bool{}
				for _, sourceCol := range t.Columns {
					expected := sourceCol
					if cfg.LowerCaseNames {
						expected = strings.ToLower(expected)
					}
					sourceCols[expected] = true
					if !targetCols[expected] {
						r.Errors = append(r.Errors, fmt.Sprintf("目标表缺少列: %s.%s", name, expected))
					}
				}
				for _, col := range targetMetadata {
					if !sourceCols[col.name] && col.nullable == "NO" && !col.defaultValue.Valid && col.identity != "YES" {
						r.Errors = append(r.Errors, fmt.Sprintf("目标表存在无默认值的额外必填列: %s.%s", name, col.name))
					}
				}
				if len(t.PrimaryKey) > 0 {
					expectedPK := make([]string, 0, len(t.PrimaryKey))
					for _, index := range t.PrimaryKey {
						column := t.Columns[index]
						if cfg.LowerCaseNames {
							column = strings.ToLower(column)
						}
						expectedPK = append(expectedPK, column)
					}
					constraintSets, constraintErr := loadPostgresUniqueColumnSets(ctx, dst, cfg.TargetSchema, name)
					if constraintErr != nil {
						r.Errors = append(r.Errors, fmt.Sprintf("读取目标表唯一约束失败 %s: %v", name, constraintErr))
					} else {
						matched := false
						for _, columns := range constraintSets {
							if sameColumnSet(expectedPK, columns) {
								matched = true
							}
						}
						if !matched {
							r.Errors = append(r.Errors, fmt.Sprintf("目标表缺少与源主键列一致的主键/唯一约束: %s", name))
						}
					}
				}
			}
		}
		if len(tables) > 0 && e == nil {
			resolved, resolveErr := ResolveLocatorStrategies(ctx, cfg.TargetDSN, cfg.TargetSchema, cfg.LowerCaseNames, tables)
			if resolveErr != nil {
				r.Errors = append(r.Errors, "解析 CDC 行定位策略失败: "+resolveErr.Error())
			} else {
				r.Tables = resolved
				var fullRow []string
				for _, table := range resolved {
					if table.LocatorStrategy == LocatorFullRow {
						fullRow = append(fullRow, table.Name)
					}
				}
				if len(fullRow) > 0 {
					r.Warnings = append(r.Warnings, fmt.Sprintf("%d 张表将使用更新前整行匹配 UPDATE/DELETE，目标端可能发生全表扫描: %s", len(fullRow), strings.Join(fullRow, ", ")))
				}
			}
		}
	}
	r.OK = len(r.Errors) == 0
	return r
}

type globalVariableLookup func(context.Context, string) (string, error)

// readBinlogRetention 返回 MySQL 实际的 binlog 自动清理周期。
// nil 表示无法确认，0 表示已确认不自动清理，正数表示保留秒数。
func readBinlogRetention(ctx context.Context, lookup globalVariableLookup) (*int64, error) {
	if autoPurge, err := lookup(ctx, "binlog_expire_logs_auto_purge"); err == nil {
		switch strings.ToUpper(strings.TrimSpace(autoPurge)) {
		case "OFF", "0":
			return int64Ptr(0), nil
		}
	}

	secondsText, secondsErr := lookup(ctx, "binlog_expire_logs_seconds")
	var seconds int64
	if secondsErr == nil {
		seconds, secondsErr = parseNonNegativeInt64("binlog_expire_logs_seconds", secondsText)
		if secondsErr == nil && seconds > 0 {
			return int64Ptr(seconds), nil
		}
	}

	daysText, daysErr := lookup(ctx, "expire_logs_days")
	if daysErr == nil {
		var days int64
		days, daysErr = parseNonNegativeInt64("expire_logs_days", daysText)
		if daysErr == nil {
			if days > math.MaxInt64/(24*60*60) {
				daysErr = fmt.Errorf("expire_logs_days 超出可支持范围: %s", strings.TrimSpace(daysText))
			} else if days > 0 {
				return int64Ptr(days * 24 * 60 * 60), nil
			} else if secondsErr == nil {
				return int64Ptr(0), nil
			} else {
				// MySQL 5.7 没有秒级变量，天级变量为 0 即表示不自动清理。
				return int64Ptr(0), nil
			}
		}
	}

	diagnostics := make([]string, 0, 2)
	if secondsErr != nil {
		diagnostics = append(diagnostics, "binlog_expire_logs_seconds: "+secondsErr.Error())
	} else {
		diagnostics = append(diagnostics, "binlog_expire_logs_seconds=0，必须继续确认 expire_logs_days")
	}
	if daysErr != nil {
		diagnostics = append(diagnostics, "expire_logs_days: "+daysErr.Error())
	}
	return nil, fmt.Errorf("%s", strings.Join(diagnostics, "；"))
}

func parseNonNegativeInt64(name, value string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s 值无效: %q", name, strings.TrimSpace(value))
	}
	return parsed, nil
}

func retentionDurationWarning(seconds *int64) string {
	if seconds != nil && *seconds > 0 && *seconds < 72*3600 {
		return fmt.Sprintf("binlog 仅保留约 %.1f 小时，必须大于全量快照+追赶耗时并预留安全余量", float64(*seconds)/3600)
	}
	return ""
}

func int64Ptr(value int64) *int64 { return &value }

func compactError(err error) string {
	const maxRunes = 240
	message := strings.Join(strings.Fields(err.Error()), " ")
	if utf8.RuneCountInString(message) <= maxRunes {
		return message
	}
	runes := []rune(message)
	return string(runes[:maxRunes]) + "…"
}

func sameColumnSet(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, column := range a {
		set[column] = true
	}
	for _, column := range b {
		if !set[column] {
			return false
		}
	}
	return true
}

func findBinlogFile(ctx context.Context, db *sql.DB, wanted string) (bool, uint64, error) {
	rows, err := db.QueryContext(ctx, "SHOW BINARY LOGS")
	if err != nil {
		return false, 0, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return false, 0, err
	}
	for rows.Next() {
		values := make([]sql.RawBytes, len(columns))
		args := make([]any, len(columns))
		for i := range values {
			args[i] = &values[i]
		}
		if err = rows.Scan(args...); err != nil {
			return false, 0, err
		}
		var name string
		var size uint64
		for i, column := range columns {
			switch strings.ToLower(column) {
			case "log_name":
				name = string(values[i])
			case "file_size":
				size, _ = strconv.ParseUint(string(values[i]), 10, 64)
			}
		}
		if name == wanted {
			return true, size, nil
		}
	}
	return false, 0, rows.Err()
}

func readGrants(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, "SHOW GRANTS FOR CURRENT_USER")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var grants []string
	for rows.Next() {
		var grant string
		if err := rows.Scan(&grant); err != nil {
			return "", err
		}
		grants = append(grants, grant)
	}
	return strings.Join(grants, "\n"), rows.Err()
}

func CurrentPosition(ctx context.Context, db *sql.DB) (Position, error) {
	rows, err := db.QueryContext(ctx, "SHOW MASTER STATUS")
	if err != nil {
		rows, err = db.QueryContext(ctx, "SHOW BINARY LOG STATUS")
	}
	if err != nil {
		return Position{}, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return Position{}, err
	}
	if !rows.Next() {
		return Position{}, fmt.Errorf("binlog 状态为空")
	}
	vals := make([]sql.RawBytes, len(cols))
	args := make([]any, len(cols))
	for i := range vals {
		args[i] = &vals[i]
	}
	if err := rows.Scan(args...); err != nil {
		return Position{}, err
	}
	var p Position
	for i, c := range cols {
		switch strings.ToLower(c) {
		case "file":
			p.File = string(vals[i])
		case "position":
			n, _ := strconv.ParseUint(string(vals[i]), 10, 32)
			p.Pos = uint32(n)
		case "executed_gtid_set":
			p.GTID = strings.TrimSpace(string(vals[i]))
		}
	}
	if p.File == "" || p.Pos < 4 {
		return Position{}, fmt.Errorf("binlog 文件或位置不可用")
	}
	return p, nil
}
