package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	mysql "github.com/go-sql-driver/mysql"
)

// MySQL 8 caches TABLES.AUTO_INCREMENT by default. Pin a connection and bypass
// that cache for both planning and verification, restoring its original TTL.
// MySQL 5.7 does not expose the variable and already reads the live counter.
func (w *MySQLWriter) oracleMySQLAutoCounter(ctx context.Context, schema, table string) (*big.Int, error) {
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var ttl string
	err = conn.QueryRowContext(ctx, "SELECT @@SESSION.information_schema_stats_expiry").Scan(&ttl)
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1193 {
			return nil, err
		}
	} else if ttl != "0" {
		if _, err = conn.ExecContext(ctx, "SET SESSION information_schema_stats_expiry=0"); err != nil {
			return nil, err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(cleanup, "SET SESSION information_schema_stats_expiry=?", ttl); err != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}()
	}
	var value sql.NullString
	if err = conn.QueryRowContext(ctx, `SELECT AUTO_INCREMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?`, schema, table).Scan(&value); err != nil {
		return nil, err
	}
	if !value.Valid {
		return nil, fmt.Errorf("目标表缺少 AUTO_INCREMENT 起点")
	}
	return repairInteger(value.String)
}

func (w *MySQLWriter) oracleLoadSession(ctx context.Context) (*sql.Conn, func(), error) {
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	var original string
	if err = conn.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&original); err != nil {
		conn.Close()
		return nil, nil, err
	}
	mode := original
	for _, value := range []string{"STRICT_ALL_TABLES", "NO_AUTO_VALUE_ON_ZERO"} {
		if !strings.Contains(","+strings.ToUpper(mode)+",", ","+value+",") {
			if mode != "" {
				mode += ","
			}
			mode += value
		}
	}
	if _, err = conn.ExecContext(ctx, "SET SESSION sql_mode=?", mode); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		conn.Close()
		return nil, nil, err
	}
	restore := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, "SET SESSION sql_mode=?", original); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		conn.Close()
	}
	return conn, restore, nil
}

func (w *MySQLWriter) oracleMySQLAutoColumn(ctx context.Context, s source.SequenceInfo) (string, *big.Int, error) {
	schema, err := w.OracleMySQLSchema(ctx)
	if err != nil {
		return "", nil, err
	}
	var kind, columnType, extra, nullable string
	err = w.db.QueryRowContext(ctx, `SELECT DATA_TYPE,COLUMN_TYPE,EXTRA,IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME=?`, schema, s.TableName, s.ColumnName).Scan(&kind, &columnType, &extra, &nullable)
	if err != nil {
		return "", nil, fmt.Errorf("读取目标自增列 %s.%s: %w", s.TableName, s.ColumnName, err)
	}
	bits, ok := map[string]uint{"tinyint": 8, "smallint": 16, "mediumint": 24, "int": 32, "bigint": 64}[kind]
	if !ok || !strings.Contains(extra, "auto_increment") || nullable != "NO" {
		return "", nil, fmt.Errorf("目标 %s.%s 必须是 NOT NULL 整数 AUTO_INCREMENT 列", s.TableName, s.ColumnName)
	}
	if !strings.Contains(columnType, "unsigned") {
		bits--
	}
	bound := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bits), big.NewInt(1))
	return columnType, bound, nil
}

// ValidateOracleMySQLAutoIncrement is read-only and runs before data-only loads.
func (w *MySQLWriter) ValidateOracleMySQLAutoIncrement(ctx context.Context, s source.SequenceInfo, typeName string, load bool) error {
	kind, _, err := w.oracleMySQLAutoColumn(ctx, s)
	if err != nil {
		return err
	}
	if strings.SplitN(kind, "(", 2)[0] != typeName || strings.Contains(kind, "unsigned") {
		return fmt.Errorf("目标自增列类型 %s 与预检类型 %s 不匹配", kind, typeName)
	}
	if load {
		schema, err := w.OracleMySQLSchema(ctx)
		if err != nil {
			return err
		}
		var count int
		if err = w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=? AND EVENT_OBJECT_TABLE=? AND EVENT_MANIPULATION='INSERT'`, schema, s.TableName).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("目标表存在 INSERT 触发器，无法保证原 ID 与数据不被改写")
		}
	}
	return nil
}

// PrepareOracleMySQLAutoIncrement computes from current target state every time.
// It is also used by retry/recovery; persisted SQL is never used as a stale seed.
func (w *MySQLWriter) PrepareOracleMySQLAutoIncrement(ctx context.Context, s source.SequenceInfo) ([]dialect.Statement, string, error) {
	if s.Unsupported != "" {
		return nil, "", fmt.Errorf("%s", s.Unsupported)
	}
	schema, err := w.OracleMySQLSchema(ctx)
	if err != nil {
		return nil, "", err
	}
	_, bound, err := w.oracleMySQLAutoColumn(ctx, s)
	if err != nil {
		return nil, "", err
	}
	start, err := recoveryStart(s)
	if err != nil {
		return nil, "", err
	}
	desired := farther(big.NewInt(1), start, big.NewInt(1))
	n, err := w.oracleMySQLAutoCounter(ctx, schema, s.TableName)
	if err != nil {
		return nil, "", err
	}
	desired = farther(desired, n, big.NewInt(1))
	extreme, err := recoveryExtreme(ctx, w, w.db, schema, s, big.NewInt(1))
	if err != nil {
		return nil, "", err
	}
	if extreme != nil {
		desired = farther(desired, new(big.Int).Add(extreme, big.NewInt(1)), big.NewInt(1))
	}
	if desired.Cmp(bound) > 0 {
		return nil, "", fmt.Errorf("下一自增值 %s 超出目标列整数范围", desired)
	}
	stmt := dialect.Statement{SQL: "ALTER TABLE " + w.Dialect().QualifyTable(schema, s.TableName) + " AUTO_INCREMENT = " + desired.String()}
	return []dialect.Statement{stmt}, desired.String(), nil
}

func (w *MySQLWriter) ExecuteOracleMySQLAutoIncrement(ctx context.Context, s source.SequenceInfo, statements []dialect.Statement, start string) (callErr error) {
	schema, err := w.OracleMySQLSchema(ctx)
	if err != nil {
		return err
	}
	if len(statements) != 1 {
		return fmt.Errorf("自增校准必须只有一个步骤")
	}
	statement := statements[0].SQL
	if err := repair.Observe(ctx, statement, "running", nil); err != nil {
		return err
	}
	defer func() {
		status := "succeeded"
		if callErr != nil {
			status = "failed"
		}
		callErr = errors.Join(callErr, repair.Observe(ctx, statement, status, callErr))
	}()
	if _, err := w.db.ExecContext(ctx, statement); err != nil {
		return err
	}
	n, err := w.oracleMySQLAutoCounter(ctx, schema, s.TableName)
	if err != nil {
		return err
	}
	want, err := repairInteger(start)
	if err != nil {
		return err
	}
	if n.Cmp(want) < 0 {
		return fmt.Errorf("自增起点复核失败，预期至少 %s，实际 %s", start, n.String())
	}
	return nil
}

func (w *MySQLWriter) executeOracleMySQLAutoRepair(ctx context.Context, obj repair.Object, units []repair.Unit, onStep func([]repair.Unit) error) ([]repair.Unit, error) {
	out := slices.Clone(units)
	if repairDefinitionEdited(obj) {
		return out, fmt.Errorf("自增校准必须按当前目标数据重新计算，不支持手工修改校准 SQL")
	}
	if len(out) != 1 || !out[0].Selected {
		return out, fmt.Errorf("自增校准必须选择唯一的校准步骤")
	}
	statements, start, err := w.PrepareOracleMySQLAutoIncrement(ctx, *obj.Sequence)
	if err != nil {
		out[0].Status, out[0].Error = "failed", err.Error()
		if onStep != nil {
			if e := onStep(out); e != nil {
				return out, e
			}
		}
		return out, err
	}
	out[0].SQL, out[0].OriginalSQL = statements[0].SQL, statements[0].SQL
	out[0].Status, out[0].Error = "running", ""
	if onStep != nil {
		if err = onStep(out); err != nil {
			return out, err
		}
	}
	err = w.ExecuteOracleMySQLAutoIncrement(ctx, *obj.Sequence, statements, start)
	if err != nil {
		out[0].Status, out[0].Error = "failed", err.Error()
	} else {
		out[0].Status, out[0].Selected = "succeeded", false
	}
	if onStep != nil {
		if e := onStep(out); e != nil {
			return out, e
		}
	}
	return out, err
}
