package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	_ "github.com/lib/pq"
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
	var retention int64
	if err := src.QueryRowContext(ctx, `SELECT @@binlog_expire_logs_seconds`).Scan(&retention); err == nil && retention > 0 && retention < 86400 {
		r.Warnings = append(r.Warnings, "binlog 保留时间不足 24 小时，全量快照较慢时可能无法追赶")
	}
	if pos, e := currentPosition(ctx, src); e != nil {
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
			if len(t.PrimaryKey) == 0 {
				r.NoPrimaryKey = append(r.NoPrimaryKey, t.Name)
			}
		}
		if len(r.NoPrimaryKey) > 0 {
			r.Warnings = append(r.Warnings, "无主键表仅同步 INSERT，UPDATE/DELETE 将跳过")
		}
	}
	if incrementalOnly {
		if cfg.Start.GTID == "" && (cfg.Start.File == "" || cfg.Start.Pos < 4) {
			r.Errors = append(r.Errors, "仅增量模式必须提供 GTID 或 binlog 文件与位置")
		}
		if cfg.Start.GTID != "" {
			if _, e := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, cfg.Start.GTID); e != nil {
				r.Errors = append(r.Errors, "GTID 格式错误: "+e.Error())
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
				colRows, colErr := dst.QueryContext(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2`, cfg.TargetSchema, name)
				if colErr != nil {
					r.Errors = append(r.Errors, fmt.Sprintf("读取目标表列失败 %s: %v", name, colErr))
					continue
				}
				targetCols := map[string]bool{}
				for colRows.Next() {
					var col string
					if colRows.Scan(&col) == nil {
						targetCols[col] = true
					}
				}
				colRows.Close()
				for _, sourceCol := range t.Columns {
					expected := sourceCol
					if cfg.LowerCaseNames {
						expected = strings.ToLower(expected)
					}
					if !targetCols[expected] {
						r.Errors = append(r.Errors, fmt.Sprintf("目标表缺少列: %s.%s", name, expected))
					}
				}
			}
		}
	}
	r.OK = len(r.Errors) == 0
	return r
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

func currentPosition(ctx context.Context, db *sql.DB) (Position, error) {
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
			p.GTID = string(vals[i])
		}
	}
	return p, nil
}
