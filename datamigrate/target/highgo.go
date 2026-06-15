// datamigrate/target/highgo.go
package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	_ "github.com/lib/pq"
)

// HighGoWriter 实现 Writer 接口，写入到瀚高（HighGo）数据库
// HighGo 与 PostgreSQL 协议兼容，复用 lib/pq 驱动和 PostgreSQL 方言
type HighGoWriter struct {
	db        *sql.DB
	schema    string
	srcType   string
	valueConv valueconv.ValueConverter
	dia       dialect.Dialect
}

func (w *HighGoWriter) SetSourceType(srcType string) { w.srcType = srcType }
func (w *HighGoWriter) Dialect() dialect.Dialect     { return w.dia }

// NewHighGo 创建并连接 HighGo Writer
func NewHighGo(dsn, schema string, pool ConnPoolConfig) (*HighGoWriter, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &HighGoWriter{db: db, schema: schema, valueConv: valueconv.NewPostgres(), dia: dialect.NewPostgres("highgo")}, nil
}

func (w *HighGoWriter) qualifiedTable(table string) string {
	if w.schema == "" {
		return fmt.Sprintf(`"%s"`, table)
	}
	return fmt.Sprintf(`"%s"."%s"`, w.schema, table)
}

func (w *HighGoWriter) Close() error   { return w.db.Close() }
func (w *HighGoWriter) DBType() string { return "highgo" }

func (w *HighGoWriter) CreateTable(ctx context.Context, ddl string) error {
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

func (w *HighGoWriter) CopyData(ctx context.Context, table string, cols []string, colTypes []string, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

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

func (w *HighGoWriter) convertRow(row []interface{}, colTypes []string) []interface{} {
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

func (w *HighGoWriter) execStatements(ctx context.Context, stmts []dialect.Statement) error {
	for _, s := range stmts {
		if _, err := w.db.ExecContext(ctx, s.SQL); err != nil {
			return err
		}
	}
	return nil
}

func (w *HighGoWriter) CreateSequence(ctx context.Context, seq source.SequenceInfo) error {
	return w.execStatements(ctx, w.dia.SequenceStatements(w.schema, seq))
}

func (w *HighGoWriter) CreateIndex(ctx context.Context, idx source.IndexInfo) error {
	return w.execStatements(ctx, w.dia.IndexStatements(w.schema, idx))
}

func (w *HighGoWriter) CreateForeignKey(ctx context.Context, fk source.FKInfo) error {
	return w.execStatements(ctx, w.dia.ForeignKeyStatements(w.schema, fk))
}

func (w *HighGoWriter) CreateView(ctx context.Context, view source.ViewInfo) error {
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
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s.SQL); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (w *HighGoWriter) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := w.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, w.qualifiedTable(table))).Scan(&count)
	return count, err
}

func (w *HighGoWriter) AlterDistribute(ctx context.Context, table string, cols []string) error {
	ddl := fmt.Sprintf("ALTER TABLE %s DISTRIBUTE BY hash (%s);", w.qualifiedTable(table), strings.Join(cols, ", "))
	_, err := w.db.ExecContext(ctx, ddl)
	return err
}

func (w *HighGoWriter) SchemaExists(ctx context.Context, schema string) (bool, error) {
	var exists bool
	err := w.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname = $1)`,
		schema,
	).Scan(&exists)
	return exists, err
}

func (w *HighGoWriter) ChangeOwner(ctx context.Context, objType, name, owner string) error {
	_, err := w.db.ExecContext(ctx,
		fmt.Sprintf(`ALTER %s %s OWNER TO "%s"`, objType, w.qualifiedTable(name), owner))
	return err
}
