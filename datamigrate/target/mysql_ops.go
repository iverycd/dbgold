// datamigrate/target/mysql_ops.go
package target

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

// CopyData 用参数化批量 INSERT 写入(MySQL 占位符为 ?)。
// 自增列允许显式插入源库原始 id,无需开关(MySQL 默认允许)。
func (w *MySQLWriter) CopyData(ctx context.Context, table string, cols []string, colTypes []string, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	quotedCols := make([]string, len(cols))
	placeholders := make([]string, len(cols))
	for i, c := range cols {
		quotedCols[i] = w.quoteIdent(c)
		placeholders[i] = "?"
	}
	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		w.qualifiedTable(table), strings.Join(quotedCols, ", "), strings.Join(placeholders, ", "))
	stmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, row := range rows {
		conv := w.convertRow(row, colTypes)
		if _, err := stmt.ExecContext(ctx, conv...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// convertRow 对一行的每个值按列类型经 ValueConverter 落地为 MySQL 形态。
func (w *MySQLWriter) convertRow(row []interface{}, colTypes []string) []interface{} {
	if w.valueConv == nil {
		return row
	}
	out := make([]interface{}, len(row))
	for i, v := range row {
		dt := ""
		if i < len(colTypes) {
			dt = colTypes[i]
		}
		out[i] = w.valueConv.Convert(v, w.srcType, dt)
	}
	return out
}

// CreateSequence 修正自增列种子(MySQL 自增已在建表声明 AUTO_INCREMENT)。
// 若该列实际非自增,ALTER 会失败,降级忽略(种子未修正只影响后续应用插入)。
func (w *MySQLWriter) CreateSequence(ctx context.Context, seq source.SequenceInfo) error {
	stmts := w.dia.SequenceStatements(w.schema, seq)
	for _, s := range stmts {
		_, _ = w.db.ExecContext(ctx, s.SQL) // 降级:忽略错误
	}
	return nil
}

func (w *MySQLWriter) CreateIndex(ctx context.Context, idx source.IndexInfo) error {
	return w.execStatements(ctx, w.dia.IndexStatements(w.schema, idx))
}

func (w *MySQLWriter) CreateForeignKey(ctx context.Context, fk source.FKInfo) error {
	return w.execStatements(ctx, w.dia.ForeignKeyStatements(w.schema, fk))
}

func (w *MySQLWriter) CreateComment(ctx context.Context, cm source.CommentInfo) error {
	return w.execStatements(ctx, w.dia.CommentStatements(w.schema, cm))
}

func (w *MySQLWriter) CreateView(ctx context.Context, view source.ViewInfo) error {
	return w.execStatements(ctx, w.dia.ViewStatements(w.schema, view))
}

// CountRows 返回指定表的行数。
func (w *MySQLWriter) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := w.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", w.qualifiedTable(table))).Scan(&count)
	return count, err
}

// AlterDistribute MySQL 非分布式,无操作。
func (w *MySQLWriter) AlterDistribute(_ context.Context, _ string, _ []string) error {
	return nil
}

// SchemaExists 检查目标 database 是否存在。
func (w *MySQLWriter) SchemaExists(ctx context.Context, schema string) (bool, error) {
	var cnt int
	err := w.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?`,
		schema).Scan(&cnt)
	if err != nil {
		return false, err
	}
	return cnt > 0, nil
}

// ChangeOwner MySQL 对象 owner 即所在 database,无 ALTER ... OWNER TO,空操作。
func (w *MySQLWriter) ChangeOwner(_ context.Context, _, _, _ string) error {
	return nil
}
