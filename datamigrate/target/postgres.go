// datamigrate/target/postgres.go
package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	_ "github.com/lib/pq"
)

// ConnPoolConfig 连接池配置，零值表示使用默认值（MaxOpenConns=50, MaxIdleConns=25, ConnMaxLifetime=1h）
type ConnPoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

func (c ConnPoolConfig) applyTo(db *sql.DB) {
	maxOpen := c.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 50
	}
	maxIdle := c.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 25
	}
	lifetime := c.ConnMaxLifetime
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(lifetime)
}

// PostgresWriter 实现 Writer 接口，写入到 PostgreSQL 数据库
type PostgresWriter struct {
	db        *sql.DB
	schema    string
	srcType   string                   // 源库类型，用于 ValueConverter 落地中立值
	valueConv valueconv.ValueConverter // 把 Reader 中立值落地为 PG 形态
	dia       dialect.Dialect          // SQL 生成方言
}

// SetSourceType 由 Migrator 注入源库类型(reader.DBType())。
func (w *PostgresWriter) SetSourceType(srcType string) { w.srcType = srcType }

// Dialect 返回 PostgreSQL 方言。
func (w *PostgresWriter) Dialect() dialect.Dialect { return w.dia }

// NewPostgres 创建并连接 PostgreSQL Writer
// dsn 格式：host=... port=... user=... password=... dbname=... sslmode=disable
// schema 为空时使用连接默认 search_path
func NewPostgres(dsn, schema string, pool ConnPoolConfig) (*PostgresWriter, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &PostgresWriter{db: db, schema: schema, valueConv: valueconv.NewPostgres(), dia: dialect.NewPostgres("postgres")}, nil
}

// qualifiedTable 返回带 schema 前缀的表名，schema 为空时直接返回表名
func (w *PostgresWriter) qualifiedTable(table string) string {
	if w.schema == "" {
		return fmt.Sprintf(`"%s"`, table)
	}
	return fmt.Sprintf(`"%s"."%s"`, w.schema, table)
}

func (w *PostgresWriter) Close() error   { return w.db.Close() }
func (w *PostgresWriter) DBType() string { return "postgres" }

func (w *PostgresWriter) CreateTable(ctx context.Context, ddl string) error {
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

// CopyData 使用 PostgreSQL COPY 协议批量写入行数据。
// 写入前经 ValueConverter 把 Reader 输出的中立值落地为 pq COPY 协议能接受的形态。
func (w *PostgresWriter) CopyData(ctx context.Context, table string, cols []string, colTypes []string, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// pq.CopyIn 直接拼表名进 SQL，必须用带引号的 qualified 形式保证大小写正确
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
		conv := w.convertRow(row, colTypes)
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
func (w *PostgresWriter) convertRow(row []interface{}, colTypes []string) []interface{} {
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

func (w *PostgresWriter) execStatements(ctx context.Context, stmts []dialect.Statement) error {
	for _, s := range stmts {
		if _, err := w.db.ExecContext(ctx, s.SQL); err != nil {
			return err
		}
	}
	return nil
}

func (w *PostgresWriter) CreateSequence(ctx context.Context, seq source.SequenceInfo) error {
	return w.execStatements(ctx, w.dia.SequenceStatements(w.schema, seq))
}

func (w *PostgresWriter) CreateIndex(ctx context.Context, idx source.IndexInfo) error {
	return w.execStatements(ctx, w.dia.IndexStatements(w.schema, idx))
}

func (w *PostgresWriter) CreateForeignKey(ctx context.Context, fk source.FKInfo) error {
	return w.execStatements(ctx, w.dia.ForeignKeyStatements(w.schema, fk))
}

func (w *PostgresWriter) CreateComment(ctx context.Context, cm source.CommentInfo) error {
	return w.execStatements(ctx, w.dia.CommentStatements(w.schema, cm))
}

func (w *PostgresWriter) CreateView(ctx context.Context, view source.ViewInfo) error {
	stmts := w.dia.ViewStatements(w.schema, view)
	if w.schema == "" {
		return w.execStatements(ctx, stmts)
	}
	// 视图定义中的非限定表名需要能解析到目标 schema，用 SET LOCAL 避免影响其他连接
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL search_path TO "%s"`, w.schema)); err != nil {
		return err
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s.SQL); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CountRows 返回指定表的行数
func (w *PostgresWriter) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := w.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, w.qualifiedTable(table))).Scan(&count)
	return count, err
}

func (w *PostgresWriter) AlterDistribute(_ context.Context, _ string, _ []string) error {
	return nil
}

func (w *PostgresWriter) SchemaExists(ctx context.Context, schema string) (bool, error) {
	var exists bool
	err := w.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname = $1)`,
		schema,
	).Scan(&exists)
	return exists, err
}

func (w *PostgresWriter) ChangeOwner(ctx context.Context, objType, name, owner string) error {
	_, err := w.db.ExecContext(ctx,
		fmt.Sprintf(`ALTER %s %s OWNER TO "%s"`, objType, w.qualifiedTable(name), owner))
	return err
}
