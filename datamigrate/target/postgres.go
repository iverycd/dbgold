// datamigrate/target/postgres.go
package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
	"github.com/lib/pq"
)

// PostgresWriter 实现 Writer 接口，写入到 PostgreSQL 数据库
type PostgresWriter struct {
	db *sql.DB
}

// NewPostgres 创建并连接 PostgreSQL Writer
// dsn 格式：host=... port=... user=... password=... dbname=... sslmode=disable
func NewPostgres(dsn string) (*PostgresWriter, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &PostgresWriter{db: db}, nil
}

func (w *PostgresWriter) Close() error   { return w.db.Close() }
func (w *PostgresWriter) DBType() string { return "postgres" }

func (w *PostgresWriter) CreateTable(ctx context.Context, ddl string) error {
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

// CopyData 使用 PostgreSQL COPY 协议批量写入行数据
func (w *PostgresWriter) CopyData(ctx context.Context, table string, cols []string, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, pq.CopyIn(table, cols...))
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

func (w *PostgresWriter) CreateSequence(ctx context.Context, seq source.SequenceInfo) error {
	seqName := fmt.Sprintf("seq_%s_%s", seq.TableName, seq.ColumnName)
	createSQL := fmt.Sprintf("CREATE SEQUENCE IF NOT EXISTS %s INCREMENT BY 1 START %d", seqName, seq.StartValue)
	if _, err := w.db.ExecContext(ctx, createSQL); err != nil {
		return err
	}
	alterSQL := fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT nextval('%s')", seq.TableName, seq.ColumnName, seqName)
	_, err := w.db.ExecContext(ctx, alterSQL)
	return err
}

func (w *PostgresWriter) CreateIndex(ctx context.Context, idx source.IndexInfo) error {
	cols := strings.Join(idx.Columns, ", ")
	var ddl string
	if idx.IsPrimary {
		ddl = fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s);", idx.TableName, cols)
	} else if idx.IsUnique {
		ddl = fmt.Sprintf("CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s (%s);",
			idx.IndexName, idx.TableName, cols)
	} else {
		ddl = fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s);",
			idx.IndexName, idx.TableName, cols)
	}
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

func (w *PostgresWriter) CreateForeignKey(ctx context.Context, fk source.FKInfo) error {
	ddl := fmt.Sprintf(
		"ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE %s ON UPDATE %s;",
		fk.TableName, fk.ConstraintName,
		strings.Join(fk.Columns, ", "),
		fk.RefTable,
		strings.Join(fk.RefColumns, ", "),
		fk.OnDelete, fk.OnUpdate)
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

func (w *PostgresWriter) CreateView(ctx context.Context, view source.ViewInfo) error {
	ddl := fmt.Sprintf("CREATE OR REPLACE VIEW %s AS %s;", view.ViewName, view.Definition)
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

// CountRows 返回指定表的行数
func (w *PostgresWriter) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := w.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, table)).Scan(&count)
	return count, err
}

func (w *PostgresWriter) AlterDistribute(_ context.Context, _ string, _ []string) error {
	return nil
}
