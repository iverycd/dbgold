package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
)

func precheck(ctx context.Context, cfg Config) error {
	mysqlPassword, err := envRequired("CDCSTRESS_MYSQL_PASSWORD")
	if err != nil {
		return err
	}
	if _, err = envRequired("CDCSTRESS_GAUSSDB_PASSWORD"); err != nil {
		return err
	}
	admin, err := sql.Open("mysql", mysqlAdminDSN(cfg, mysqlPassword))
	if err != nil {
		return err
	}
	defer admin.Close()
	if err = admin.PingContext(ctx); err != nil {
		return fmt.Errorf("mysql ping: %w", err)
	}
	vars := map[string]string{}
	for _, name := range []string{"log_bin", "binlog_format", "binlog_row_image", "gtid_mode"} {
		var value string
		if e := admin.QueryRowContext(ctx, "SELECT @@"+name).Scan(&value); e != nil {
			return fmt.Errorf("read MySQL %s: %w", name, e)
		}
		vars[name] = value
	}
	retention, retentionErr := readRetentionWithFallback(ctx, func(ctx context.Context, name string) (string, error) {
		var value string
		err := admin.QueryRowContext(ctx, "SELECT @@"+name).Scan(&value)
		return value, err
	})
	if retentionErr != nil {
		retention = "无法确认"
		log.Printf("warning: 无法读取 MySQL binlog 保留时间，将继续由 dbgold 预检确认: %v", retentionErr)
	}
	if !mysqlVariableEnabled(vars["log_bin"]) || !strings.EqualFold(vars["binlog_format"], "ROW") || !strings.EqualFold(vars["binlog_row_image"], "FULL") {
		return fmt.Errorf("MySQL CDC settings invalid: log_bin=%s format=%s row_image=%s", vars["log_bin"], vars["binlog_format"], vars["binlog_row_image"])
	}
	var grants string
	rows, e := admin.QueryContext(ctx, "SHOW GRANTS")
	if e == nil {
		for rows.Next() {
			var item string
			if rows.Scan(&item) == nil {
				grants += " " + strings.ToUpper(item)
			}
		}
		rows.Close()
	}
	for _, permission := range []string{"SELECT", "REPLICATION"} {
		if !strings.Contains(grants, permission) && !strings.Contains(grants, "ALL PRIVILEGES") {
			return fmt.Errorf("MySQL account grants do not show %s privilege", permission)
		}
	}
	dbs, err := openDatabases(ctx, cfg, false)
	if err != nil {
		return err
	}
	dbs.close()
	client, err := newAPIClient(ctx, cfg)
	if err != nil {
		return err
	}
	source, target, err := resolveConnections(ctx, client, cfg)
	if err != nil {
		return err
	}
	if source.Host != cfg.MySQL.Host || source.Port != cfg.MySQL.Port {
		return fmt.Errorf("dbgold source connection points to %s:%d, direct test config points to %s:%d", source.Host, source.Port, cfg.MySQL.Host, cfg.MySQL.Port)
	}
	if target.Host != cfg.GaussDB.Host || target.Port != cfg.GaussDB.Port || target.Database != cfg.GaussDB.Database {
		return fmt.Errorf("dbgold target connection does not match direct GaussDB test config")
	}
	log.Printf("precheck passed: source=%s target=%s MySQL=%s GTID=%s retention=%s", source.Name, target.Name, redactedDSN(cfg.MySQL), vars["gtid_mode"], retention)
	return nil
}

type mysqlVariableLookup func(context.Context, string) (string, error)

func mysqlVariableEnabled(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "ON", "1", "TRUE", "YES":
		return true
	default:
		return false
	}
}

// readRetentionWithFallback supports both MySQL 8's second-level variable and
// MySQL 5.7's day-level variable. A zero value means automatic expiry is off.
func readRetentionWithFallback(ctx context.Context, lookup mysqlVariableLookup) (string, error) {
	secondsText, secondsErr := lookup(ctx, "binlog_expire_logs_seconds")
	if secondsErr == nil {
		seconds, err := parseRetentionValue("binlog_expire_logs_seconds", secondsText)
		if err != nil {
			secondsErr = err
		} else if seconds > 0 {
			return fmt.Sprintf("%ds", seconds), nil
		}
	}

	daysText, daysErr := lookup(ctx, "expire_logs_days")
	if daysErr == nil {
		days, err := parseRetentionValue("expire_logs_days", daysText)
		if err != nil {
			daysErr = err
		} else if days == 0 {
			return "不自动清理（expire_logs_days=0）", nil
		} else {
			return fmt.Sprintf("%ds（expire_logs_days=%d）", days*24*60*60, days), nil
		}
	}
	return "", fmt.Errorf("binlog_expire_logs_seconds: %v；expire_logs_days: %v", secondsErr, daysErr)
}

func parseRetentionValue(name, value string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s 值无效: %q", name, strings.TrimSpace(value))
	}
	return parsed, nil
}
