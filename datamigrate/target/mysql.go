// datamigrate/target/mysql.go
package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/valueconv"
	_ "github.com/go-sql-driver/mysql"
)

// MySQLWriter 实现 Writer 接口,写入到 MySQL 数据库。
type MySQLWriter struct {
	db        *sql.DB
	schema    string // 目标 database(MySQL 的 schema 即 database)
	srcType   string
	valueConv valueconv.ValueConverter
	dia       dialect.Dialect
}

// NewMySQL 创建并连接 MySQL Writer。
// dsn 格式:user:password@tcp(host:port)/database?parseTime=true&charset=utf8mb4
// schema 为目标 database 名;为空时使用 DSN 中的默认库。
func NewMySQL(dsn, schema string, pool ConnPoolConfig) (*MySQLWriter, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &MySQLWriter{
		db:        db,
		schema:    schema,
		valueConv: valueconv.NewMySQL(),
		dia:       dialect.NewMySQL(),
	}, nil
}

// SetSourceType 由 Migrator 注入源库类型(reader.DBType())。
func (w *MySQLWriter) SetSourceType(srcType string) { w.srcType = srcType }

// Dialect 返回 MySQL 方言。
func (w *MySQLWriter) Dialect() dialect.Dialect { return w.dia }

func (w *MySQLWriter) Close() error   { return w.db.Close() }
func (w *MySQLWriter) DBType() string { return "mysql" }

// quoteIdent 反引号转义。
func (w *MySQLWriter) quoteIdent(name string) string {
	return fmt.Sprintf("`%s`", strings.ReplaceAll(name, "`", "``"))
}

// qualifiedTable 返回带 database 前缀的表名,schema 为空时只返回表名。
func (w *MySQLWriter) qualifiedTable(table string) string {
	if w.schema == "" {
		return w.quoteIdent(table)
	}
	return w.quoteIdent(w.schema) + "." + w.quoteIdent(table)
}

// CreateTable 执行建表 DDL(dialect 用 ";\n" 连接 DROP + CREATE 两句)。
// DROP TABLE IF EXISTS 不会因表不存在报错。
func (w *MySQLWriter) CreateTable(ctx context.Context, ddl string) error {
	for _, stmt := range splitDDL(ddl) {
		if _, err := w.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (w *MySQLWriter) execStatements(ctx context.Context, stmts []dialect.Statement) error {
	for _, s := range stmts {
		if _, err := w.db.ExecContext(ctx, s.SQL); err != nil {
			return err
		}
	}
	return nil
}
