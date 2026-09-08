package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/dialect"
)

type MySQLProfile struct {
	MaxAllowedPacket    int                  `json:"max_allowed_packet"`
	Version             string               `json:"version"`
	Parsed              dialect.MySQLVersion `json:"parsed"`
	SQLMode             string               `json:"sql_mode"`
	Collation           string               `json:"server_collation"`
	TargetCollation     string               `json:"target_collation"`
	LowerCaseTableNames int                  `json:"lower_case_table_names"`
	PageSize            int                  `json:"innodb_page_size"`
	LargePrefix         bool                 `json:"innodb_large_prefix"`
	Database            string               `json:"database"`
	Verification        string               `json:"verification"`
}

// InspectionDB is for read-only catalog/data validation queries in preflight.
func (w *MySQLWriter) InspectionDB() *sql.DB { return w.db }
func (w *MySQLWriter) InspectMySQL(ctx context.Context) (MySQLProfile, error) {
	p := MySQLProfile{TargetCollation: "utf8mb4_bin", LargePrefix: true, Verification: "待验证（能力探测不等于实库认证）"}
	var db sql.NullString
	err := w.db.QueryRowContext(ctx, `SELECT VERSION(),@@SESSION.sql_mode,@@collation_server,@@lower_case_table_names,@@innodb_page_size,DATABASE(),@@max_allowed_packet`).Scan(&p.Version, &p.SQLMode, &p.Collation, &p.LowerCaseTableNames, &p.PageSize, &db, &p.MaxAllowedPacket)
	if err != nil {
		return p, err
	}
	p.Parsed, err = dialect.ParseMySQLVersion(p.Version)
	if err != nil {
		return p, err
	}
	p.Database = w.schema
	if p.Database == "" {
		p.Database = db.String
	}
	if p.Database == "" {
		return p, fmt.Errorf("必须指定目标 MySQL database")
	}
	mode := "," + strings.ToUpper(p.SQLMode) + ","
	if !strings.Contains(mode, ",STRICT_TRANS_TABLES,") && !strings.Contains(mode, ",STRICT_ALL_TABLES,") {
		return p, fmt.Errorf("目标必须启用 STRICT_TRANS_TABLES 或 STRICT_ALL_TABLES，不能在宽松模式迁移")
	}
	if p.Parsed.Major == 5 {
		rows, e := w.db.QueryContext(ctx, `SHOW VARIABLES WHERE Variable_name IN ('innodb_large_prefix','innodb_file_format')`)
		if e != nil {
			return p, e
		}
		for rows.Next() {
			var n, v string
			if e = rows.Scan(&n, &v); e != nil {
				rows.Close()
				return p, e
			}
			if n == "innodb_large_prefix" {
				p.LargePrefix = strings.EqualFold(v, "ON")
			}
			if n == "innodb_file_format" && !strings.EqualFold(v, "Barracuda") {
				rows.Close()
				return p, fmt.Errorf("MySQL 5.7 DYNAMIC 行格式要求 Barracuda")
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return p, e
		}
	}
	var count int
	if err = w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.COLLATIONS WHERE COLLATION_NAME=?`, p.TargetCollation).Scan(&count); err != nil {
		return p, err
	}
	if count != 1 {
		return p, fmt.Errorf("目标缺少排序规则 %s", p.TargetCollation)
	}
	w.schema = p.Database
	return p, nil
}

// A dedicated connection applies and restores session settings. This never
// changes server globals and never leaves settings on an unrelated pooled user.
func (w *MySQLWriter) pgSession(ctx context.Context) (*sql.Conn, func(), error) {
	conn, e := w.db.Conn(ctx)
	if e != nil {
		return nil, nil, e
	}
	var mode, tz string
	if e = conn.QueryRowContext(ctx, `SELECT @@SESSION.sql_mode,@@SESSION.time_zone`).Scan(&mode, &tz); e != nil {
		conn.Close()
		return nil, nil, e
	}
	pgMode := mode
	for _, m := range []string{"STRICT_ALL_TABLES", "NO_AUTO_VALUE_ON_ZERO", "NO_BACKSLASH_ESCAPES"} {
		if !strings.Contains(","+strings.ToUpper(pgMode)+",", ","+m+",") {
			if pgMode != "" {
				pgMode += ","
			}
			pgMode += m
		}
	}
	if _, e = conn.ExecContext(ctx, `SET SESSION sql_mode=?, SESSION time_zone='+00:00'`, pgMode); e != nil {
		conn.Close()
		return nil, nil, e
	}
	restore := func() { // bounded cleanup even after cancellation
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, `SET SESSION sql_mode=?, SESSION time_zone=?`, mode, tz); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		conn.Close()
	}
	return conn, restore, nil
}
