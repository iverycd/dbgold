// datamigrate/target/dameng.go
package target

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/valueconv"
	"gitee.com/chunanyong/dm"
)

// DaMengWriter 实现 Writer 接口,写入到达梦(Oracle 兼容模式)数据库。
type DaMengWriter struct {
	db        *sql.DB
	schema    string // 目标 OWNER/SCHEMA
	srcType   string
	valueConv valueconv.ValueConverter
	dia       dialect.Dialect

	identMu    sync.Mutex      // 保护 identCache
	identCache map[string]bool // table → 是否含自增(IDENTITY)列

	oracleDMMu sync.RWMutex
	oracleDM   oracleDMCapabilityCache
}

// NewDaMeng 创建并连接达梦 Writer。
// dsn 格式:dm://username:password@host:port
// schema 为目标 OWNER(达梦用 schema 隔离数据,等同用户名)。
func NewDaMeng(dsn, schema string, pool ConnPoolConfig) (*DaMengWriter, error) {
	db, err := sql.Open("dm", dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if schema == "" {
		if err := db.QueryRow("SELECT CURRENT_SCHEMA").Scan(&schema); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &DaMengWriter{
		db:         db,
		schema:     strings.ToUpper(schema),
		valueConv:  valueconv.NewDaMeng(),
		dia:        dialect.NewDaMeng(),
		identCache: map[string]bool{},
	}, nil
}

func (w *DaMengWriter) EffectiveSchema() string { return w.schema }

// SetSourceType 由 Migrator 注入源库类型(reader.DBType())。
func (w *DaMengWriter) SetSourceType(srcType string) { w.srcType = srcType }

// Dialect 返回达梦方言。
func (w *DaMengWriter) Dialect() dialect.Dialect { return w.dia }

func (w *DaMengWriter) Close() error   { return w.db.Close() }
func (w *DaMengWriter) DBType() string { return "dameng" }

func (w *DaMengWriter) qualifiedTable(table string) string {
	return w.dia.QualifyTable(w.schema, table)
}

// CreateTable 执行建表 DDL。达梦不支持 DROP TABLE IF EXISTS,
// 故 DDL 拆成 DROP + CREATE 两句,DROP 失败(表不存在)忽略。
func (w *DaMengWriter) CreateTable(ctx context.Context, stmts []dialect.Statement) error {
	w.identMu.Lock()
	w.identCache = map[string]bool{}
	w.identMu.Unlock()
	for _, stmt := range stmts {
		_, err := execObservedAllow(ctx, w.db, stmt.SQL, func(err error) bool {
			if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(stmt.SQL)), "DROP") {
				return false
			}
			if w.srcType != "sqlserver" && w.srcType != "oracle" {
				return true
			}
			var dmErr *dm.DmError
			return errors.As(err, &dmErr) && dmErr.ErrCode == -2106
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// splitDDL 按分号拆分 DDL(dialect 用 ";\n" 连接多句),过滤空句。
func splitDDL(ddl string) []string {
	parts := strings.Split(ddl, ";\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (w *DaMengWriter) execStatements(ctx context.Context, stmts []dialect.Statement) error {
	for _, s := range stmts {
		if _, err := execObserved(ctx, w.db, s.SQL); err != nil {
			return err
		}
	}
	return nil
}
