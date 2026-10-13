package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
	"encoding/xml"
	"fmt"
	_ "github.com/microsoft/go-mssqldb"
	"math/big"
	"strings"
	"time"
)

// Filtered indexes require NUMERIC_ROUNDABORT OFF. Values are already validated
// by ConvertSQLServer; INSERT parameters are explicitly cast to target types.
const sqlServerDDLSettings = `SET ANSI_NULLS ON; SET ANSI_PADDING ON; SET ANSI_WARNINGS ON; SET ARITHABORT ON; SET CONCAT_NULL_YIELDS_NULL ON; SET QUOTED_IDENTIFIER ON; SET NUMERIC_ROUNDABORT OFF; SET XACT_ABORT ON;`

type SQLServerWriter struct {
	db     *sql.DB
	schema string
	dia    *dialect.SQLServerDialect
	// Set only on a dedicated recovery Writer, after inspecting the repaired table.
	recoveryCasts map[string]map[string]string
}

var _ Writer = (*SQLServerWriter)(nil)

func NewSQLServer(dsn, schema string, pool ConnPoolConfig) (*SQLServerWriter, error) {
	if schema == "" {
		schema = "dbo"
	}
	if e := dialect.ValidateSQLServerName(schema); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlserver", dsn)
	if e != nil {
		return nil, e
	}
	pool.applyTo(db)
	if e = db.Ping(); e != nil {
		db.Close()
		return nil, e
	}
	w := &SQLServerWriter{db: db, schema: schema, dia: dialect.NewSQLServer()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exists, e := w.SchemaExists(ctx, schema)
	if e != nil || !exists {
		db.Close()
		if e == nil {
			e = fmt.Errorf("目标 Schema %s 不存在", schema)
		}
		return nil, e
	}
	return w, nil
}
func (w *SQLServerWriter) DBType() string           { return "sqlserver" }
func (w *SQLServerWriter) Dialect() dialect.Dialect { return w.dia }
func (w *SQLServerWriter) EffectiveSchema() string  { return w.schema }
func (w *SQLServerWriter) Close() error             { return w.db.Close() }
func (w *SQLServerWriter) SchemaExists(ctx context.Context, s string) (bool, error) {
	var n int
	e := w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sys.schemas WHERE name=@p1", s).Scan(&n)
	return n == 1, e
}
func (w *SQLServerWriter) CountRows(ctx context.Context, t string) (int64, error) {
	var n int64
	e := w.db.QueryRowContext(ctx, "SELECT COUNT_BIG(*) FROM "+w.dia.QualifyTable(w.schema, t)).Scan(&n)
	return n, e
}
func (w *SQLServerWriter) AlterDistribute(context.Context, string, []string) error {
	return fmt.Errorf("SQL Server 不支持分布式选项")
}
func (w *SQLServerWriter) ChangeOwner(context.Context, string, string, string) error {
	return fmt.Errorf("SQL Server 不支持迁移 owner 选项")
}

// Dedicated connections are discarded on close: neither failures nor pooled
// reuse can leak identity or SET state to another migration batch.
func (w *SQLServerWriter) session(ctx context.Context) (*sql.Conn, func(), error) {
	c, e := w.db.Conn(ctx)
	if e != nil {
		return nil, nil, e
	}
	close := func() { _ = c.Raw(func(any) error { return driver.ErrBadConn }); _ = c.Close() }
	if _, e = c.ExecContext(ctx, sqlServerDDLSettings); e != nil {
		close()
		return nil, nil, e
	}
	return c, close, nil
}
func (w *SQLServerWriter) execStatements(ctx context.Context, ss []dialect.Statement) error {
	c, close, e := w.session(ctx)
	if e != nil {
		return e
	}
	defer close()
	tx, e := c.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	return execObservedTransaction(ctx, tx, ss)
}
func (w *SQLServerWriter) CreateTable(ctx context.Context, s []dialect.Statement) error {
	return w.execStatements(ctx, s)
}
func (w *SQLServerWriter) CreateIndex(ctx context.Context, i source.IndexInfo) error {
	if e := w.dia.ValidateIndex(i); e != nil {
		return e
	}
	return w.execStatements(ctx, w.dia.IndexStatements(w.schema, i))
}
func (w *SQLServerWriter) CreateForeignKey(ctx context.Context, f source.FKInfo) error {
	if e := w.dia.ValidateForeignKey(f); e != nil {
		return e
	}
	return w.execStatements(ctx, w.dia.ForeignKeyStatements(w.schema, f))
}
func (w *SQLServerWriter) CreateView(ctx context.Context, v source.ViewInfo) error {
	return w.execStatements(ctx, w.dia.ViewStatements(w.schema, v))
}
func (w *SQLServerWriter) CreateComment(ctx context.Context, c source.CommentInfo) error {
	return w.execStatements(ctx, w.dia.CommentStatements(w.schema, c))
}
func (w *SQLServerWriter) CreateSequence(ctx context.Context, s source.SequenceInfo, owner string) error {
	col, ok := w.dia.Column(s.TableName, s.ColumnName)
	if !ok || !strings.Contains(col.Extra, "auto_increment") {
		return fmt.Errorf("自增列不存在: %s.%s", s.TableName, s.ColumnName)
	}
	next := s.StartValueText
	if next == "" {
		next = fmt.Sprint(s.StartValue)
	}
	n, ok := new(big.Int).SetString(next, 10)
	if !ok || n.Sign() < 1 {
		return fmt.Errorf("自增起点无效: %s", next)
	}
	if _, e := valueconv.ConvertSQLServer(next, col); e != nil {
		return e
	}
	ss := w.dia.SequenceStatements(w.schema, s, owner)
	if len(ss) == 0 {
		return fmt.Errorf("自增重置 SQL 为空")
	}
	return w.execStatements(ctx, ss)
}

func (w *SQLServerWriter) CopyData(ctx context.Context, table string, cols, types []string, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}
	if len(cols) == 0 || len(cols) > 1024 {
		return fmt.Errorf("无效列数")
	}
	info := make([]source.ColumnInfo, len(cols))
	identity := false
	for i, n := range cols {
		c, ok := w.dia.Column(table, n)
		if !ok {
			return fmt.Errorf("缺少写入列元数据: %s.%s", table, n)
		}
		info[i] = c
		identity = identity || strings.Contains(c.Extra, "auto_increment")
	}
	conn, close, e := w.session(ctx)
	if e != nil {
		return e
	}
	defer close()
	tx, e := conn.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	q := w.dia.QualifyTable(w.schema, table)
	if identity {
		if _, e = tx.ExecContext(ctx, "SET IDENTITY_INSERT "+q+" ON"); e != nil {
			return e
		}
		defer func() {
			_ = tx.Rollback()
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = conn.ExecContext(cleanup, "SET IDENTITY_INSERT "+q+" OFF")
		}()
	}
	names := make([]string, len(cols))
	for i, n := range cols {
		names[i] = w.dia.QuoteIdent(n)
	}
	batch := min(1000, 2000/len(cols))
	for off := 0; off < len(rows); off += batch {
		stop := min(off+batch, len(rows))
		values := []string{}
		args := []interface{}{}
		for ri := off; ri < stop; ri++ {
			if len(rows[ri]) != len(cols) {
				return fmt.Errorf("表 %s 行 %d 列数不匹配", table, ri+1)
			}
			ps := make([]string, len(cols))
			for ci, v := range rows[ri] {
				cv, err := valueconv.ConvertSQLServer(v, info[ci])
				if err != nil {
					return fmt.Errorf("表 %s 批次行 %d: %w", table, ri+1, err)
				}
				args = append(args, cv)
				t, _ := typemap.SQLServerType(info[ci])
				if actual := w.recoveryCasts[table][cols[ci]]; actual != "" {
					t = actual
				}
				ps[ci] = fmt.Sprintf("CAST(@p%d AS %s)", len(args), t)
			}
			values = append(values, "("+strings.Join(ps, ",")+")")
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO "+q+" ("+strings.Join(names, ",")+") VALUES "+strings.Join(values, ","), args...); e != nil {
			return e
		}
	}
	if identity {
		if _, e = tx.ExecContext(ctx, "SET IDENTITY_INSERT "+q+" OFF"); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// CheckNames compares with the actual target collation (including accents and
// case), rather than guessing equivalence using Go strings.ToLower.
func (w *SQLServerWriter) CheckNames(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}
	var xmlText strings.Builder
	xmlText.WriteString("<names>")
	for _, n := range names {
		if e := dialect.ValidateSQLServerName(n); e != nil {
			return e
		}
		xmlText.WriteString("<n>")
		if e := xml.EscapeText(&xmlText, []byte(n)); e != nil {
			return e
		}
		xmlText.WriteString("</n>")
	}
	xmlText.WriteString("</names>")
	var duplicate string
	e := w.db.QueryRowContext(ctx, `DECLARE @x xml=CAST(@p1 AS xml); SELECT TOP 1 n FROM (SELECT v.value('.', 'nvarchar(128)') COLLATE DATABASE_DEFAULT AS n FROM @x.nodes('/names/n') AS t(v)) q GROUP BY n HAVING COUNT(*)>1`, xmlText.String()).Scan(&duplicate)
	if e == sql.ErrNoRows {
		return nil
	}
	if e != nil {
		return e
	}
	return fmt.Errorf("目标排序规则下对象名冲突: %s", duplicate)
}

// ValidateExisting is required for append/object modes. Exact types and
// capacities avoid implicit narrowing casts against user-created targets.
func (w *SQLServerWriter) ValidateExisting(ctx context.Context, info *source.TableDDLInfo) error {
	rows, e := w.db.QueryContext(ctx, `SELECT c.name,t.name,c.max_length,c.precision,c.scale,c.is_nullable,c.is_identity,c.is_computed FROM sys.columns c JOIN sys.types t ON c.user_type_id=t.user_type_id WHERE c.object_id=OBJECT_ID(@p1) ORDER BY c.column_id`, w.dia.QualifyTable(w.schema, info.TableName))
	if e != nil {
		return e
	}
	defer rows.Close()
	type column struct {
		name, typ                    string
		length, precision, scale     int64
		nullable, identity, computed bool
	}
	found := map[string]column{}
	for rows.Next() {
		var c column
		if e = rows.Scan(&c.name, &c.typ, &c.length, &c.precision, &c.scale, &c.nullable, &c.identity, &c.computed); e != nil {
			return e
		}
		found[c.name] = c
	}
	if e = rows.Err(); e != nil {
		return e
	}
	if len(found) != len(info.Columns) {
		return fmt.Errorf("目标表 %s 不存在或列数不一致", info.TableName)
	}
	for _, c := range info.Columns {
		actual, ok := found[c.Name]
		if !ok {
			return fmt.Errorf("目标缺少列 %s.%s", info.TableName, c.Name)
		}
		want, e := typemap.SQLServerType(c)
		if e != nil {
			return e
		}
		got := strings.ToUpper(actual.typ)
		switch got {
		case "NVARCHAR":
			if actual.length < 0 {
				got += "(MAX)"
			} else {
				got += fmt.Sprintf("(%d)", actual.length/2)
			}
		case "VARBINARY":
			if actual.length < 0 {
				got += "(MAX)"
			} else {
				got += fmt.Sprintf("(%d)", actual.length)
			}
		case "DECIMAL", "NUMERIC":
			got = fmt.Sprintf("DECIMAL(%d,%d)", actual.precision, actual.scale)
		case "DATETIME2", "TIME":
			got += fmt.Sprintf("(%d)", actual.scale)
		case "FLOAT":
			got = fmt.Sprintf("FLOAT(%d)", actual.precision)
		}
		if got != want || actual.computed || actual.nullable != c.IsNullable || actual.identity != strings.Contains(c.Extra, "auto_increment") {
			return fmt.Errorf("目标列 %s.%s 类型/可空/自增不匹配: %s，预期 %s", info.TableName, c.Name, got, want)
		}
	}
	return nil
}

// RebuildDependencies observes all foreign keys before returning any DROP.
// An out-of-scope referencing table prevents rebuilding its referenced table.
func (w *SQLServerWriter) RebuildDependencies(ctx context.Context, tables map[string]bool) ([]dialect.Statement, error) {
	rows, e := w.db.QueryContext(ctx, `SELECT ps.name,p.name,f.name,rs.name,r.name FROM sys.foreign_keys f JOIN sys.tables p ON p.object_id=f.parent_object_id JOIN sys.schemas ps ON ps.schema_id=p.schema_id JOIN sys.tables r ON r.object_id=f.referenced_object_id JOIN sys.schemas rs ON rs.schema_id=r.schema_id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	ss := []dialect.Statement{}
	for rows.Next() {
		var ps, p, n, rs, r string
		if e = rows.Scan(&ps, &p, &n, &rs, &r); e != nil {
			return nil, e
		}
		if rs == w.schema && tables[r] {
			if ps != w.schema || !tables[p] {
				return nil, &SQLServerDependencyError{Table: r, Reason: fmt.Sprintf("范围外外键 %s.%s.%s 引用 %s，未删除任何外键", ps, p, n, r)}
			}
			ss = append(ss, dialect.Statement{SQL: "ALTER TABLE " + w.dia.QualifyTable(ps, p) + " DROP CONSTRAINT " + w.dia.QuoteIdent(n)})
		}
	}
	return ss, rows.Err()
}
func (w *SQLServerWriter) DropDependencies(ctx context.Context, ss []dialect.Statement) error {
	if len(ss) == 0 {
		return nil
	}
	return w.execStatements(ctx, ss)
}

type SQLServerDependencyError struct{ Table, Reason string }

func (e *SQLServerDependencyError) Error() string { return e.Reason }
func (w *SQLServerWriter) ObjectExists(ctx context.Context, name string) (bool, error) {
	var n int
	e := w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sys.objects WHERE object_id=OBJECT_ID(@p1) AND type IN ('U','V')", w.dia.QualifyTable(w.schema, name)).Scan(&n)
	return n > 0, e
}
