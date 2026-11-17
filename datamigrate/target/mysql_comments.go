package target

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"unicode/utf8"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
)

// PrepareComment preserves the actual target column definition when MySQL
// requires MODIFY COLUMN to change a comment. Preparing never executes DDL.
func (w *MySQLWriter) PrepareComment(ctx context.Context, cm source.CommentInfo) ([]dialect.Statement, error) {
	if w.srcType == "oscar" {
		limit := 2048
		if cm.ColumnName != "" {
			limit = 1024
		}
		if !utf8.ValidString(cm.Comment) || strings.ContainsRune(cm.Comment, 0) || utf8.RuneCountInString(cm.Comment) > limit {
			return nil, fmt.Errorf("目标 MySQL 注释必须是有效文本且最多 %d 个字符", limit)
		}
	}
	return GenerateRepairComment(ctx, w, cm)
}

// ExecuteCommentStatements executes the prepared definition using the string
// escaping mode expected by GenerateRepairComment. The private session is
// discarded so its SQL mode cannot affect another operation in the pool.
func (w *MySQLWriter) ExecuteCommentStatements(ctx context.Context, statements []dialect.Statement) error {
	if len(statements) == 0 {
		return nil
	}
	if w.srcType == "oscar" {
		return w.execStatements(ctx, statements)
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, "SET SESSION sql_mode=CONCAT(@@SESSION.sql_mode, ',NO_BACKSLASH_ESCAPES')"); err != nil {
		return fmt.Errorf("设置 MySQL 注释 SQL 模式: %w", err)
	}
	for _, statement := range statements {
		if _, err := execObserved(ctx, conn, statement.SQL); err != nil {
			return fmt.Errorf("执行 MySQL 注释 DDL [%s]: %w", statement.SQL, err)
		}
	}
	return nil
}
