// datamigrate/target/dameng_ops.go
package target

import (
	"context"
	"database/sql/driver"
	"dbgold/datamigrate/valueconv"
	"errors"
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
)

// CopyData 用参数化批量 INSERT 写入(达梦占位符为 ?)。
// 含自增列的表写入前后开关 IDENTITY_INSERT,以便导入源库原始 id。
func (w *DaMengWriter) CopyData(ctx context.Context, table string, cols []string, colTypes []string, rows [][]interface{}) (retErr error) {
	if len(rows) == 0 {
		return nil
	}
	oracleFloatKinds := make([]string, len(cols))
	var oracleRows [][]any
	if w.srcType == "oracle" {
		if len(colTypes) != len(cols) {
			return fmt.Errorf("表 %s 缺少源列类型", table)
		}
		for i, dt := range colTypes {
			switch typemap.OracleDamengBaseType(dt) {
			case "FLOAT", "REAL", "DOUBLE PRECISION", "BINARY_FLOAT", "BINARY_DOUBLE":
				kind, err := w.OracleDamengColumnFloatKind(ctx, table, cols[i])
				if err != nil {
					return fmt.Errorf("%s.%s: %w", table, cols[i], err)
				}
				if kind != "decimal" && kind != "real" && kind != "double" {
					return fmt.Errorf("%s.%s: 无法确认达梦目标浮点类型 %q", table, cols[i], kind)
				}
				oracleFloatKinds[i] = kind
			}
		}
		var err error
		oracleRows, err = w.prepareOracleDMLoadPage(ctx, table, cols, colTypes, rows, oracleFloatKinds)
		if err != nil {
			return err
		}
	}
	hasIdentity, err := w.tableHasIdentity(ctx, table)
	if err != nil {
		return err
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	identityEnabled := hasIdentity
	if hasIdentity {
		// Always clean the same physical session, including failed INSERT/cancellation.
		defer func() {
			if !identityEnabled {
				return
			}
			_ = tx.Rollback()
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, e := conn.ExecContext(cleanup, "SET IDENTITY_INSERT "+w.qualifiedTable(table)+" OFF"); e != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				retErr = errors.Join(retErr, fmt.Errorf("清理 IDENTITY_INSERT: %w", e))
			}
		}()
		if _, err = tx.ExecContext(ctx, "SET IDENTITY_INSERT "+w.qualifiedTable(table)+" ON"); err != nil {
			return err
		}
	}

	quotedCols := make([]string, len(cols))
	placeholders := make([]string, len(cols))
	for i, c := range cols {
		quotedCols[i] = w.dia.QuoteIdent(c)
		placeholders[i] = "?"
		if w.srcType == "sqlserver" && i < len(colTypes) {
			placeholders[i] = valueconv.SQLServerDamengPlaceholder(colTypes[i])
		}
		if w.srcType == "mysql" && i < len(colTypes) {
			placeholders[i] = valueconv.MySQLDamengPlaceholder(colTypes[i])
		}
		if w.srcType == "oracle" && i < len(colTypes) {
			placeholders[i] = valueconv.OracleDamengPlaceholder(colTypes[i])
		}
	}
	insert := "INSERT INTO"
	if w.srcType == "oracle" {
		insert = "INSERT /*+ MY_STRICT_TABLES(7) */ INTO"
	}
	insertSQL := fmt.Sprintf(`%s %s (%s) VALUES (%s)`, insert,
		w.qualifiedTable(table), strings.Join(quotedCols, ", "), strings.Join(placeholders, ", "))
	stmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for rowIndex, row := range rows {
		if len(row) != len(cols) {
			return fmt.Errorf("表 %s 行列数不匹配", table)
		}
		var conv []interface{}
		if w.srcType == "oracle" {
			conv = oracleRows[rowIndex]
		} else {
			conv = w.convertRow(row, colTypes)
		}
		if w.srcType == "mysql" {
			if len(colTypes) != len(cols) {
				return fmt.Errorf("表 %s 缺少源列类型", table)
			}
			for i, v := range row {
				converted, e := valueconv.MySQLDamengDate(v, colTypes[i])
				if e != nil {
					return fmt.Errorf("%s.%s: %w", table, cols[i], e)
				}
				switch strings.ToUpper(colTypes[i]) {
				case "DATE", "DATETIME", "TIMESTAMP":
					conv[i] = converted
				}
			}
		}
		if w.srcType == "sqlserver" {
			if len(colTypes) != len(cols) {
				return fmt.Errorf("表 %s 缺少源列类型", table)
			}
			conv = make([]interface{}, len(row))
			for i, v := range row {
				var e error
				conv[i], e = valueconv.SQLServerDameng(v, colTypes[i])
				if e != nil {
					return fmt.Errorf("%s.%s: %w", table, cols[i], e)
				}
			}
		}
		if _, err := stmt.ExecContext(ctx, conv...); err != nil {
			return err
		}
	}

	if hasIdentity {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET IDENTITY_INSERT %s OFF`, w.qualifiedTable(table))); err != nil {
			return err
		}
		identityEnabled = false
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
func (w *DaMengWriter) tableHasIdentity(ctx context.Context, table string) (bool, error) {
	table = strings.ToUpper(table)
	w.identMu.Lock()
	if v, ok := w.identCache[table]; ok {
		w.identMu.Unlock()
		return v, nil
	}
	w.identMu.Unlock()

	var cnt int
	err := w.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM SYS.SYSCOLUMNS sc
		 JOIN ALL_OBJECTS o ON o.object_id = sc.id
		 WHERE o.owner = ? AND o.object_name = ? AND sc.INFO2 & 0x01 = 0x01`,
		w.schema, table).Scan(&cnt)
	if err != nil {
		return false, fmt.Errorf("检测 %s 自增列: %w", table, err)
	}
	has := cnt > 0

	w.identMu.Lock()
	w.identCache[table] = has
	w.identMu.Unlock()
	return has, nil
}

// CreateSequence reports every identity continuation failure to the job.
func (w *DaMengWriter) CreateSequence(ctx context.Context, seq source.SequenceInfo, owner string) error {
	if seq.Native != nil {
		return w.createOracleDamengSequence(ctx, seq, owner)
	}
	return w.execStatements(ctx, w.dia.SequenceStatements(w.schema, seq, owner))
}

func (w *DaMengWriter) CreateIndex(ctx context.Context, idx source.IndexInfo) error {
	return w.execStatements(ctx, w.dia.IndexStatements(w.schema, idx))
}

func (w *DaMengWriter) CreateForeignKey(ctx context.Context, fk source.FKInfo) error {
	return w.execStatements(ctx, w.dia.ForeignKeyStatements(w.schema, fk))
}

func (w *DaMengWriter) CreateComment(ctx context.Context, cm source.CommentInfo) error {
	return w.execStatements(ctx, w.dia.CommentStatements(w.schema, cm))
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
