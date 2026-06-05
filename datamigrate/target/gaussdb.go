// datamigrate/target/gaussdb.go
package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
	_ "gitee.com/opengauss/openGauss-connector-go-pq"
)

// GaussDBWriter 实现 Writer 接口，写入到 GaussDB 数据库
// GaussDB 与 PostgreSQL 语法完全兼容，仅驱动不同
type GaussDBWriter struct {
	db     *sql.DB
	schema string
}

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
	return &GaussDBWriter{db: db, schema: schema}, nil
}

// qualifiedTable 返回带 schema 前缀的表名，schema 为空时直接返回表名
func (w *GaussDBWriter) qualifiedTable(table string) string {
	if w.schema == "" {
		return fmt.Sprintf(`"%s"`, table)
	}
	return fmt.Sprintf(`"%s"."%s"`, w.schema, table)
}

func (w *GaussDBWriter) Close() error   { return w.db.Close() }
func (w *GaussDBWriter) DBType() string { return "gaussdb" }

func (w *GaussDBWriter) CreateTable(ctx context.Context, ddl string) error {
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

// CopyData 使用 GaussDB COPY 协议批量写入行数据
func (w *GaussDBWriter) CopyData(ctx context.Context, table string, cols []string, rows [][]interface{}) error {
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
		quotedCols[i] = fmt.Sprintf(`"%s"`, c)
	}
	copySQL := fmt.Sprintf(`COPY %s (%s) FROM STDIN`, w.qualifiedTable(table), strings.Join(quotedCols, ", "))
	stmt, err := tx.PrepareContext(ctx, copySQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, row := range rows {
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			return err
		}
	}
	if _, err := stmt.ExecContext(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func (w *GaussDBWriter) CreateSequence(ctx context.Context, seq source.SequenceInfo) error {
	seqBase := fmt.Sprintf("seq_%s_%s", seq.TableName, seq.ColumnName)
	var quotedSeq string
	var nextvalArg string
	if w.schema != "" {
		quotedSeq = fmt.Sprintf(`"%s"."%s"`, w.schema, seqBase)
		nextvalArg = fmt.Sprintf(`%s."%s"`, w.schema, seqBase)
	} else {
		quotedSeq = fmt.Sprintf(`"%s"`, seqBase)
		nextvalArg = fmt.Sprintf(`"%s"`, seqBase)
	}
	createSQL := fmt.Sprintf("CREATE SEQUENCE IF NOT EXISTS %s INCREMENT BY 1 START %d", quotedSeq, seq.StartValue)
	if _, err := w.db.ExecContext(ctx, createSQL); err != nil {
		return err
	}
	alterSQL := fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN "%s" SET DEFAULT nextval('%s')`,
		w.qualifiedTable(seq.TableName), seq.ColumnName, nextvalArg)
	_, err := w.db.ExecContext(ctx, alterSQL)
	return err
}

func (w *GaussDBWriter) CreateIndex(ctx context.Context, idx source.IndexInfo) error {
	quotedCols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		quotedCols[i] = fmt.Sprintf(`"%s"`, c)
	}
	cols := strings.Join(quotedCols, ", ")
	var ddl string
	if idx.IsPrimary {
		ddl = fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s);", w.qualifiedTable(idx.TableName), cols)
	} else if idx.IsUnique {
		ddl = fmt.Sprintf(`CREATE UNIQUE INDEX IF NOT EXISTS "%s" ON %s (%s);`,
			idx.IndexName, w.qualifiedTable(idx.TableName), cols)
	} else {
		ddl = fmt.Sprintf(`CREATE INDEX IF NOT EXISTS "%s" ON %s (%s);`,
			idx.IndexName, w.qualifiedTable(idx.TableName), cols)
	}
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

func (w *GaussDBWriter) CreateForeignKey(ctx context.Context, fk source.FKInfo) error {
	quotedCols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		quotedCols[i] = fmt.Sprintf(`"%s"`, c)
	}
	quotedRefCols := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		quotedRefCols[i] = fmt.Sprintf(`"%s"`, c)
	}
	ddl := fmt.Sprintf(
		`ALTER TABLE %s ADD CONSTRAINT "%s" FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE %s ON UPDATE %s;`,
		w.qualifiedTable(fk.TableName), fk.ConstraintName,
		strings.Join(quotedCols, ", "),
		w.qualifiedTable(fk.RefTable),
		strings.Join(quotedRefCols, ", "),
		fk.OnDelete, fk.OnUpdate)
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

func (w *GaussDBWriter) CreateView(ctx context.Context, view source.ViewInfo) error {
	ddl := fmt.Sprintf("CREATE OR REPLACE VIEW %s AS %s;", w.qualifiedTable(view.ViewName), view.Definition)
	if w.schema == "" {
		_, err := w.db.ExecContext(ctx, ddl)
		return err
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL search_path TO "%s"`, w.schema)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return err
	}
	return tx.Commit()
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
	_, err := w.db.ExecContext(ctx, ddl)
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
	_, err := w.db.ExecContext(ctx,
		fmt.Sprintf(`ALTER %s %s OWNER TO "%s"`, objType, w.qualifiedTable(name), owner))
	return err
}
