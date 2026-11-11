// datamigrate/target/gaussdb.go
package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	_ "gitee.com/opengauss/openGauss-connector-go-pq"
)

// GaussDBWriter 实现 Writer 接口，写入到 GaussDB 数据库
// GaussDB 与 PostgreSQL 语法完全兼容，仅驱动不同
type GaussDBWriter struct {
	db        *sql.DB
	schema    string
	srcType   string
	valueConv valueconv.CheckedValueConverter
	dia       dialect.Dialect
}

// SetSourceType 由 Migrator 注入源库类型(reader.DBType())。
func (w *GaussDBWriter) SetSourceType(srcType string) { w.srcType = srcType }

// Dialect 返回 GaussDB 方言。
func (w *GaussDBWriter) Dialect() dialect.Dialect { return w.dia }

// NewGaussDB 创建并连接 GaussDB Writer
// dsn 格式：host=... port=... user=... password=... dbname=... sslmode=disable
// schema 为空时使用连接默认 search_path
func NewGaussDB(dsn, schema string, pool ConnPoolConfig) (*GaussDBWriter, error) {
	db, err := sql.Open("opengauss", dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &GaussDBWriter{db: db, schema: schema, valueConv: valueconv.NewPostgres(), dia: dialect.NewPostgres("gaussdb")}, nil
}

// qualifiedTable 返回带 schema 前缀的表名，schema 为空时直接返回表名
func (w *GaussDBWriter) qualifiedTable(table string) string {
	return dialect.NewPostgres("gaussdb").QualifyTable(w.schema, table)
}

func (w *GaussDBWriter) Close() error   { return w.db.Close() }
func (w *GaussDBWriter) DBType() string { return "gaussdb" }

func (w *GaussDBWriter) PoolStats() sql.DBStats { return w.db.Stats() }

func (w *GaussDBWriter) CreateTable(ctx context.Context, stmts []dialect.Statement) error {
	return w.execStatements(ctx, stmts)
}

// CopyData 使用 GaussDB COPY 协议批量写入行数据。
// 写入前经 ValueConverter 把 Reader 输出的中立值落地为 COPY 协议能接受的形态。
func (w *GaussDBWriter) CopyData(ctx context.Context, table string, cols []string, colTypes []string, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// ogpq.CopyIn 直接拼表名进 SQL，必须用带引号的 qualified 形式保证大小写正确
	quotedCols := make([]string, len(cols))
	for i, c := range cols {
		quotedCols[i] = dialect.NewPostgres("gaussdb").QuoteIdent(c)
	}
	copySQL := fmt.Sprintf(`COPY %s (%s) FROM STDIN`, w.qualifiedTable(table), strings.Join(quotedCols, ", "))
	stmt, err := tx.PrepareContext(ctx, copySQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, row := range rows {
		conv, err := w.convertRow(row, colTypes, cols...)
		if err != nil {
			return fmt.Errorf("表 %s 数据转换失败: %w", table, err)
		}
		if _, err := stmt.ExecContext(ctx, conv...); err != nil {
			return err
		}
	}
	if _, err := stmt.ExecContext(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

// convertRow 对一行的每个值按列类型经 ValueConverter 落地。
func (w *GaussDBWriter) convertRow(row []interface{}, colTypes []string, cols ...string) ([]interface{}, error) {
	if w.valueConv == nil {
		return row, nil
	}
	out := make([]interface{}, len(row))
	for i, v := range row {
		dt := ""
		if i < len(colTypes) {
			dt = colTypes[i]
		}
		var err error
		out[i], err = w.valueConv.Convert(v, w.srcType, dt)
		if err != nil {
			label := fmt.Sprintf("第 %d 列", i+1)
			if i < len(cols) {
				label += " " + cols[i]
			}
			return nil, fmt.Errorf("%s (%s): %w", label, dt, err)
		}
	}
	return out, nil
}

func (w *GaussDBWriter) execStatements(ctx context.Context, stmts []dialect.Statement) error {
	for _, s := range stmts {
		if _, err := execObserved(ctx, w.db, s.SQL); err != nil {
			return err
		}
	}
	return nil
}

func (w *GaussDBWriter) CreateSequence(ctx context.Context, seq source.SequenceInfo, owner string) error {
	return w.execStatements(ctx, w.dia.SequenceStatements(w.schema, seq, owner))
}

func (w *GaussDBWriter) CreateIndex(ctx context.Context, idx source.IndexInfo) error {
	if idx.GaussDB != nil {
		return w.createGaussDBIndex(ctx, idx)
	}
	return createPGIndex(ctx, w.db, w.dia, w.schema, idx)
}

func (w *GaussDBWriter) CreateForeignKey(ctx context.Context, fk source.FKInfo) error {
	return w.execStatements(ctx, w.dia.ForeignKeyStatements(w.schema, fk))
}

func (w *GaussDBWriter) CreateComment(ctx context.Context, cm source.CommentInfo) error {
	return w.execStatements(ctx, w.dia.CommentStatements(w.schema, cm))
}

func (w *GaussDBWriter) CreateView(ctx context.Context, view source.ViewInfo) error {
	stmts := w.dia.ViewStatements(w.schema, view)
	if w.schema == "" {
		return w.execStatements(ctx, stmts)
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL search_path TO "%s"`, w.schema)); err != nil {
		return err
	}
	return execObservedTransaction(ctx, tx, stmts)
}

// CountRows 返回指定表的行数
func (w *GaussDBWriter) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := w.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, w.qualifiedTable(table))).Scan(&count)
	return count, err
}

// AlterDistribute 将表的分布列设置为指定列，适用于 GaussDB 分布式版
func (w *GaussDBWriter) AlterDistribute(ctx context.Context, table string, cols []string) error {
	ddl := fmt.Sprintf("ALTER TABLE %s DISTRIBUTE BY hash (%s);", w.qualifiedTable(table), strings.Join(cols, ", "))
	_, err := execObserved(ctx, w.db, ddl)
	return err
}

func (w *GaussDBWriter) SchemaExists(ctx context.Context, schema string) (bool, error) {
	var exists bool
	err := w.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname = $1)`,
		schema,
	).Scan(&exists)
	return exists, err
}

func (w *GaussDBWriter) ChangeOwner(ctx context.Context, objType, name, owner string) error {
	_, err := execObserved(ctx, w.db,
		fmt.Sprintf(`ALTER %s %s OWNER TO "%s"`, objType, w.qualifiedTable(name), owner))
	return err
}
