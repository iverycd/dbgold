package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const mysqlPGNativeVariableModeSQL = `SELECT current_setting('database_mode',true)`

var mysqlPGNativeVariableExplainSQL = []string{
	`EXPLAIN SELECT @dbgold_trigger_plan_probe := CAST(1 AS bigint)`,
	`EXPLAIN SELECT @dbgold_trigger_plan_probe := CAST(NULL AS bigint)`,
}

type nativeVariableQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// DetectMySQLPGNativeUserVariables only parses assignments: EXPLAIN without
// ANALYZE never writes a user variable or touches business rows.
func DetectMySQLPGNativeUserVariables(ctx context.Context, w Writer) (bool, error) {
	if w.DBType() != "kingbase" {
		return false, nil
	}
	if detector, ok := w.(interface {
		DetectMySQLPGNativeUserVariables(context.Context) (bool, error)
	}); ok {
		return detector.DetectMySQLPGNativeUserVariables(ctx)
	}
	a, err := repairAccess(w)
	if err != nil {
		return false, err
	}
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("检测目标原生用户变量连接: %w", err)
	}
	defer conn.Close()
	return detectMySQLPGNativeUserVariables(ctx, conn)
}

func detectMySQLPGNativeUserVariables(ctx context.Context, q nativeVariableQuerier) (bool, error) {
	var mode sql.NullString
	if err := q.QueryRowContext(ctx, mysqlPGNativeVariableModeSQL).Scan(&mode); err != nil {
		return false, fmt.Errorf("检测 Kingbase 用户变量兼容模式: %w", err)
	}
	if !mode.Valid || !strings.EqualFold(strings.TrimSpace(mode.String), "mysql") {
		return false, nil
	}
	for _, statement := range mysqlPGNativeVariableExplainSQL {
		rows, err := q.QueryContext(ctx, statement)
		if err != nil {
			// Syntax errors prove that this version/mode lacks the primitive.
			// Permission, connection and other failures remain actionable errors.
			var coded interface{ SQLState() string }
			if errors.As(err, &coded) && coded.SQLState() == "42601" {
				return false, nil
			}
			return false, fmt.Errorf("检测 Kingbase 原生用户变量赋值: %w", err)
		}
		for rows.Next() {
			// Drain EXPLAIN so delayed server/protocol errors are not ignored.
		}
		rowErr := rows.Err()
		closeErr := rows.Close()
		if err := errors.Join(rowErr, closeErr); err != nil {
			return false, fmt.Errorf("读取 Kingbase 用户变量能力检查: %w", err)
		}
	}
	return true, nil
}

func validateMySQLPGNativeUserVariables(ctx context.Context, tx *sql.Tx, w Writer) error {
	if w.DBType() != "kingbase" {
		return fmt.Errorf("原生用户变量触发器仅支持 Kingbase MySQL 兼容模式，当前目标为 %s", w.DBType())
	}
	supported, err := detectMySQLPGNativeUserVariables(ctx, tx)
	if err != nil {
		return err
	}
	if !supported {
		return fmt.Errorf("目标 Kingbase 当前模式或版本不支持原生用户变量赋值")
	}
	return nil
}
