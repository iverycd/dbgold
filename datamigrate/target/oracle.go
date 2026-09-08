package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	goora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/converters"
	"github.com/sijms/go-ora/v2/network"
)

type OracleWriter struct {
	db               *sql.DB
	schema, username string
	dia              *dialect.OracleDialect
	codec, ncodec    converters.IStringConverter
	mu               sync.Mutex
	columns          map[string]map[string]valueconv.OracleColumn
	warnings         map[string]valueconv.MigrationWarning
}

var _ Writer = (*OracleWriter)(nil)

func NewOracle(dsn, schema string, pool ConnPoolConfig) (*OracleWriter, error) {
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	w := newOracleWriter(db, schema)
	if err = w.initialize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return w, nil
}
func newOracleWriter(db *sql.DB, schema string) *OracleWriter {
	return &OracleWriter{db: db, schema: schema, dia: dialect.NewOracle(), columns: map[string]map[string]valueconv.OracleColumn{}, warnings: map[string]valueconv.MigrationWarning{}}
}
func (w *OracleWriter) initialize(ctx context.Context) error {
	if err := w.db.QueryRowContext(ctx, "SELECT USER FROM DUAL").Scan(&w.username); err != nil {
		return err
	}
	if w.schema == "" {
		w.schema = w.username
	}
	exists, err := w.SchemaExists(ctx, w.schema)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("Oracle Schema %q 不存在，请使用已有数据库用户", w.schema)
	}
	rows, err := w.db.QueryContext(ctx, `SELECT PARAMETER, NLS_CHARSET_ID(VALUE) FROM NLS_DATABASE_PARAMETERS WHERE PARAMETER IN ('NLS_CHARACTERSET','NLS_NCHAR_CHARACTERSET')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var id int
		if err := rows.Scan(&name, &id); err != nil {
			return err
		}
		codec := converters.NewStringConverter(id)
		if codec == nil {
			return fmt.Errorf("Oracle 字符集 %d 不受当前驱动支持", id)
		}
		width := converters.MaxBytePerChar(id)
		// UTF8 is CESU-8; supplementary characters use two surrogate encodings.
		if id == 871 || id == 870 {
			width = 6
		}
		if id == 2000 || id == 2002 {
			width = 4
		}
		if name == "NLS_CHARACTERSET" {
			w.codec, w.dia.CharBytes = codec, width
		} else {
			w.ncodec, w.dia.NCharBytes = codec, width
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if w.codec == nil || w.ncodec == nil {
		return fmt.Errorf("无法读取 Oracle 字符集信息")
	}
	return nil
}

func (w *OracleWriter) DBType() string           { return "oracle" }
func (w *OracleWriter) Dialect() dialect.Dialect { return w.dia }
func (w *OracleWriter) EffectiveSchema() string  { return w.schema }
func (w *OracleWriter) Close() error             { return w.db.Close() }
func (w *OracleWriter) SchemaExists(ctx context.Context, schema string) (bool, error) {
	var n int
	err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_USERS WHERE USERNAME = :1`, schema).Scan(&n)
	return n > 0, err
}
func (w *OracleWriter) CountRows(ctx context.Context, table string) (int64, error) {
	var n int64
	err := w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+w.dia.QualifyTable(w.schema, table)).Scan(&n)
	return n, err
}
func (w *OracleWriter) AlterDistribute(context.Context, string, []string) error {
	return fmt.Errorf("Oracle 不支持分布列选项")
}
func (w *OracleWriter) ChangeOwner(context.Context, string, string, string) error {
	return fmt.Errorf("Oracle 不支持修改对象 owner，请选择目标 Schema")
}

func oracleErrorCode(err error, code int) bool {
	var oe *network.OracleError
	return errors.As(err, &oe) && oe.ErrCode == code
}
func (w *OracleWriter) execStatements(ctx context.Context, stmts []dialect.Statement) error {
	for _, s := range stmts {
		if err := w.validateSQLText(s.SQL); err != nil {
			return err
		}
		if _, err := w.db.ExecContext(ctx, s.SQL); err != nil {
			return err
		}
	}
	return nil
}
func (w *OracleWriter) CreateTable(ctx context.Context, stmts []dialect.Statement) error {
	for _, s := range stmts {
		if err := w.validateSQLText(s.SQL); err != nil {
			return err
		}
	}
	for _, s := range stmts {
		_, err := w.db.ExecContext(ctx, s.SQL)
		if err != nil && !(strings.HasPrefix(s.SQL, "DROP TABLE ") && oracleErrorCode(err, 942)) {
			return err
		}
	}
	w.mu.Lock()
	w.columns = map[string]map[string]valueconv.OracleColumn{}
	w.mu.Unlock()
	return nil
}

func (w *OracleWriter) validateSQLText(text string) error {
	if w.codec != nil {
		codec := w.codec.Clone()
		if codec.Decode(codec.Encode(text)) != text {
			return fmt.Errorf("SQL 文本含 Oracle 目标字符集无法表示的字符")
		}
	}
	return nil
}
func (w *OracleWriter) CreateSequence(ctx context.Context, s source.SequenceInfo, owner string) error {
	if _, err := dialect.OracleSequenceStart(s); err != nil {
		return err
	}
	if err := w.execStatements(ctx, w.dia.SequenceStatements(w.schema, s, owner)); err != nil {
		return err
	}
	return w.validateObject(ctx, w.dia.TriggerName(s), "TRIGGER")
}
func (w *OracleWriter) CreateIndex(ctx context.Context, i source.IndexInfo) error {
	if i.Unsupported != "" {
		return fmt.Errorf("%s", i.Unsupported)
	}
	if len(i.Columns) == 0 {
		return fmt.Errorf("索引缺少列")
	}
	return w.execStatements(ctx, w.dia.IndexStatements(w.schema, i))
}
func (w *OracleWriter) CreateForeignKey(ctx context.Context, f source.FKInfo) error {
	if err := dialect.ValidateOracleForeignKey(f); err != nil {
		return err
	}
	return w.execStatements(ctx, w.dia.ForeignKeyStatements(w.schema, f))
}
func (w *OracleWriter) CreateView(ctx context.Context, v source.ViewInfo) error {
	if err := w.execStatements(ctx, w.dia.ViewStatements(w.schema, v)); err != nil {
		return err
	}
	return w.validateObject(ctx, v.ViewName, "VIEW")
}
func (w *OracleWriter) CreateComment(ctx context.Context, c source.CommentInfo) error {
	return w.execStatements(ctx, w.dia.CommentStatements(w.schema, c))
}
func (w *OracleWriter) validateObject(ctx context.Context, name, kind string) error {
	var status string
	if err := w.db.QueryRowContext(ctx, `SELECT STATUS FROM ALL_OBJECTS WHERE OWNER=:1 AND OBJECT_NAME=:2 AND OBJECT_TYPE=:3`, w.schema, name, kind).Scan(&status); err != nil {
		return err
	}
	if status == "VALID" {
		return nil
	}
	rows, err := w.db.QueryContext(ctx, `SELECT LINE, POSITION, TEXT FROM ALL_ERRORS WHERE OWNER=:1 AND NAME=:2 AND TYPE=:3 ORDER BY SEQUENCE`, w.schema, name, kind)
	if err != nil {
		return err
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var line, pos int
		var text string
		if err = rows.Scan(&line, &pos, &text); err != nil {
			return err
		}
		details = append(details, fmt.Sprintf("%d:%d %s", line, pos, text))
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%s %s 无效: %s", kind, name, strings.Join(details, "; "))
}

func (w *OracleWriter) loadColumns(ctx context.Context, table string) (map[string]valueconv.OracleColumn, error) {
	w.mu.Lock()
	cols, found := w.columns[table]
	w.mu.Unlock()
	if found {
		return cols, nil
	}
	rows, err := w.db.QueryContext(ctx, `SELECT COLUMN_NAME, DATA_TYPE, NULLABLE, DATA_PRECISION, DATA_SCALE, DATA_LENGTH, CHAR_LENGTH, CHAR_USED FROM ALL_TAB_COLUMNS WHERE OWNER=:1 AND TABLE_NAME=:2 ORDER BY COLUMN_ID`, w.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols = map[string]valueconv.OracleColumn{}
	for rows.Next() {
		var c valueconv.OracleColumn
		var nullable string
		var p, s, clen sql.NullInt64
		var used sql.NullString
		if err = rows.Scan(&c.Name, &c.DataType, &nullable, &p, &s, &c.DataLength, &clen, &used); err != nil {
			return nil, err
		}
		c.Nullable, c.Precision, c.Scale, c.CharLength, c.CharUsed = nullable == "Y", p.Int64, s.Int64, clen.Int64, used.String
		cols[c.Name] = c
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("Oracle 表 %s 不存在或列不可见", table)
	}
	w.mu.Lock()
	w.columns[table] = cols
	w.mu.Unlock()
	return cols, nil
}

func (w *OracleWriter) MigrationWarnings() []valueconv.MigrationWarning {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]valueconv.MigrationWarning, 0, len(w.warnings))
	for _, warn := range w.warnings {
		out = append(out, warn)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Table+"\x00"+a.Column+"\x00"+a.Reason < b.Table+"\x00"+b.Column+"\x00"+b.Reason
	})
	return out
}

func (w *OracleWriter) CopyData(ctx context.Context, table string, cols, srcTypes []string, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}
	metadata, err := w.loadColumns(ctx, table)
	if err != nil {
		return err
	}
	codecs := make([]converters.IStringConverter, len(cols))
	quoted, expressions := make([]string, len(cols)), make([]string, len(cols))
	binds, emptyBinds, bindCount := make([]int, len(cols)), make([]int, len(cols)), 0
	for i, name := range cols {
		c, ok := metadata[name]
		if !ok {
			return fmt.Errorf("Oracle 表 %s 缺少列 %s", table, name)
		}
		codec := w.codec
		if c.DataType == "NVARCHAR2" || c.DataType == "NCHAR" || c.DataType == "NCLOB" {
			codec = w.ncodec
		}
		if codec != nil {
			codecs[i] = codec.Clone()
		}
		quoted[i] = w.dia.QuoteIdent(name)
		binds[i] = bindCount
		bindCount++
		expressions[i] = fmt.Sprintf(":p%d", bindCount)
		emptyBinds[i] = -1
		if c.DataType == "BINARY_FLOAT" || c.DataType == "BINARY_DOUBLE" {
			model := "9.9999999999999999EEEE"
			if c.DataType == "BINARY_FLOAT" {
				model = "9.99999999EEEE"
			}
			expressions[i] = "TO_" + c.DataType + "(" + expressions[i] + ", '" + model + "', 'NLS_NUMERIC_CHARACTERS=''.,''')"
		}
		if c.DataType == "CLOB" || c.DataType == "NCLOB" || c.DataType == "BLOB" {
			emptyBinds[i] = bindCount
			bindCount++
			empty := "EMPTY_CLOB()"
			if c.DataType == "BLOB" {
				empty = "EMPTY_BLOB()"
			}
			if c.DataType == "NCLOB" {
				empty = "TO_NCLOB(EMPTY_CLOB())"
			}
			expressions[i] = fmt.Sprintf("CASE WHEN :p%d = 1 THEN %s ELSE %s END", bindCount, empty, expressions[i])
		}
	}
	query := "INSERT INTO " + w.dia.QualifyTable(w.schema, table) + " (" + strings.Join(quoted, ", ") + ") VALUES (" + strings.Join(expressions, ", ") + ")"
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	warnings := map[string]valueconv.MigrationWarning{}
	for offset := 0; offset < len(rows); offset += 500 {
		end := min(offset+500, len(rows))
		arrays := make([][]interface{}, bindCount)
		for b := range arrays {
			arrays[b] = make([]interface{}, end-offset)
		}
		for ri := offset; ri < end; ri++ {
			if err = ctx.Err(); err != nil {
				return err
			}
			if len(rows[ri]) != len(cols) {
				return fmt.Errorf("表 %s 页内第 %d 行列数不匹配", table, ri+1)
			}
			for ci, raw := range rows[ri] {
				src := ""
				if ci < len(srcTypes) {
					src = srcTypes[ci]
				}
				v, warning, e := valueconv.ConvertOracle(raw, src, metadata[cols[ci]], codecs[ci])
				if e != nil {
					return fmt.Errorf("表 %s 列 %s 页内第 %d 行: %w", table, cols[ci], ri+1, e)
				}
				bound := v
				if bound == nil {
					bound = oracleNullBinding(metadata[cols[ci]].DataType)
				}
				arrays[binds[ci]][ri-offset] = bound
				if eb := emptyBinds[ci]; eb >= 0 {
					empty := 0
					if v != nil && raw != nil && valueconv.OracleText(raw) == "" {
						empty = 1
					}
					arrays[eb][ri-offset] = empty
				}
				if warning != "" {
					key := table + "\x00" + cols[ci] + "\x00" + warning
					a := warnings[key]
					a.Table, a.Column, a.Reason = table, cols[ci], warning
					a.Count++
					warnings[key] = a
				}
			}
		}
		args := make([]interface{}, bindCount)
		for i := range arrays {
			// LOB CASE expressions mention the empty flag before the value.
			// Bind by name so textual placeholder order cannot swap the arrays.
			args[i] = sql.Named(fmt.Sprintf("p%d", i+1), arrays[i])
		}
		if _, err = stmt.ExecContext(ctx, args...); err != nil {
			return fmt.Errorf("表 %s 页内第 %d–%d 行: %w", table, offset+1, end, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, a := range warnings {
		b := w.warnings[key]
		a.Count += b.Count
		w.warnings[key] = a
	}
	return nil
}

// Untyped nil makes go-ora infer NCHAR for an all-NULL array. Preserve the
// target bind type, including LOB CASE expressions and national character sets.
func oracleNullBinding(dataType string) interface{} {
	switch {
	case dataType == "NUMBER":
		return (*goora.Number)(nil)
	case dataType == "DATE" || strings.HasPrefix(dataType, "TIMESTAMP"):
		return (*goora.TimeStamp)(nil)
	case dataType == "CLOB":
		return (*goora.Clob)(nil)
	case dataType == "NCLOB":
		return (*goora.NClob)(nil)
	case dataType == "BLOB":
		return (*goora.Blob)(nil)
	case dataType == "NVARCHAR2" || dataType == "NCHAR":
		return (*goora.NVarChar)(nil)
	default:
		return (*string)(nil)
	}
}
