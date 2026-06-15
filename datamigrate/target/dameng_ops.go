// datamigrate/target/dameng_ops.go
package target

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

// CopyData 用参数化批量 INSERT 写入(达梦占位符为 ?)。
// 含自增列的表写入前后开关 IDENTITY_INSERT,以便导入源库原始 id。
func (w *DaMengWriter) CopyData(ctx context.Context, table string, cols []string, colTypes []string, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	hasIdentity := w.tableHasIdentity(ctx, table)
	if hasIdentity {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET IDENTITY_INSERT %s ON`, w.qualifiedTable(table))); err != nil {
			return err
		}
	}

	quotedCols := make([]string, len(cols))
	placeholders := make([]string, len(cols))
	for i, c := range cols {
		quotedCols[i] = fmt.Sprintf(`"%s"`, c)
		placeholders[i] = "?"
	}
	insertSQL := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`,
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

	if hasIdentity {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET IDENTITY_INSERT %s OFF`, w.qualifiedTable(table))); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// convertRow 对一行的每个值按列类型经 ValueConverter 落地为达梦形态。
func (w *DaMengWriter) convertRow(row []interface{}, colTypes []string) []interface{} {
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

// tableHasIdentity 判断目标表是否含自增(IDENTITY)列,结果缓存。
// 复用达梦 SYS.SYSCOLUMNS.INFO2 & 0x01 的自增位判定(与源端 Reader 一致)。
func (w *DaMengWriter) tableHasIdentity(ctx context.Context, table string) bool {
	w.identMu.Lock()
	if v, ok := w.identCache[table]; ok {
		w.identMu.Unlock()
		return v
	}
	w.identMu.Unlock()

	var cnt int
	err := w.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM SYS.SYSCOLUMNS sc
		 JOIN ALL_OBJECTS o ON o.object_id = sc.id
		 WHERE o.owner = ? AND o.object_name = ? AND sc.INFO2 & 0x01 = 0x01`,
		w.schema, table).Scan(&cnt)
	has := err == nil && cnt > 0

	w.identMu.Lock()
	w.identCache[table] = has
	w.identMu.Unlock()
	return has
}

// CreateSequence 重置 IDENTITY 种子(达梦自增在建表已声明)。
// 失败降级为不报错,避免使整个迁移失败(种子未修正只影响后续应用插入)。
func (w *DaMengWriter) CreateSequence(ctx context.Context, seq source.SequenceInfo) error {
	stmts := w.dia.SequenceStatements(w.schema, seq)
	for _, s := range stmts {
		_, _ = w.db.ExecContext(ctx, s.SQL) // 降级:忽略错误
	}
	return nil
}

func (w *DaMengWriter) CreateIndex(ctx context.Context, idx source.IndexInfo) error {
	return w.execStatements(ctx, w.dia.IndexStatements(w.schema, idx))
}

func (w *DaMengWriter) CreateForeignKey(ctx context.Context, fk source.FKInfo) error {
	return w.execStatements(ctx, w.dia.ForeignKeyStatements(w.schema, fk))
}

func (w *DaMengWriter) CreateView(ctx context.Context, view source.ViewInfo) error {
	return w.execStatements(ctx, w.dia.ViewStatements(w.schema, view))
}

// CountRows 返回指定表的行数。
func (w *DaMengWriter) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := w.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s`, w.qualifiedTable(table))).Scan(&count)
	return count, err
}

// AlterDistribute 达梦非分布式,无操作。
func (w *DaMengWriter) AlterDistribute(_ context.Context, _ string, _ []string) error {
	return nil
}

// SchemaExists 检查目标 schema(达梦用户)是否存在。
func (w *DaMengWriter) SchemaExists(ctx context.Context, schema string) (bool, error) {
	var cnt int
	err := w.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ALL_USERS WHERE USERNAME = UPPER(?)`, schema).Scan(&cnt)
	if err != nil {
		return false, err
	}
	return cnt > 0, nil
}

// ChangeOwner 达梦对象 owner 即所在 schema,无 ALTER ... OWNER TO,空操作。
func (w *DaMengWriter) ChangeOwner(_ context.Context, _, _, _ string) error {
	return nil
}
