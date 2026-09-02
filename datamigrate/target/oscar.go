package target

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	"dbgold/internal/jdbcbridge"
)

const (
	oscarBatchRows  = 1000
	oscarBatchBytes = 8 << 20
)

type OscarWriter struct {
	bridge    oscarBridge
	session   uint64
	schema    string
	srcType   string
	valueConv valueconv.ValueConverter
	dia       dialect.Dialect
}

type oscarBridge interface {
	OpenSession(context.Context, jdbcbridge.SessionOptions) (uint64, error)
	Exec(context.Context, uint64, string) error
	QueryInt64(context.Context, uint64, string) (int64, error)
	SchemaExists(context.Context, uint64, string) (bool, error)
	BeginBatch(context.Context, uint64, string) (uint64, error)
	AddBatch(context.Context, uint64, [][]any) error
	CommitBatch(context.Context, uint64) error
	AbortBatch(context.Context, uint64) error
	CloseSession(context.Context, uint64) error
}

func NewOscar(url, username, password, schema string, pool ConnPoolConfig) (*OscarWriter, error) {
	return newOscarWithBridge(jdbcbridge.Default(), url, username, password, schema, pool)
}

func newOscarWithBridge(manager oscarBridge, url, username, password, schema string, pool ConnPoolConfig) (*OscarWriter, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := manager.OpenSession(ctx, jdbcbridge.SessionOptions{
		URL: url, Username: username, Password: password, Schema: schema,
		MaxOpenConns: pool.MaxOpenConns, MaxIdleConns: pool.MaxIdleConns,
		ConnMaxLifetime: pool.ConnMaxLifetime,
	})
	if err != nil {
		return nil, err
	}
	return &OscarWriter{bridge: manager, session: session, schema: schema, valueConv: valueconv.NewOscar(), dia: dialect.NewOscar()}, nil
}

func (w *OscarWriter) SetSourceType(srcType string) { w.srcType = srcType }
func (w *OscarWriter) DBType() string               { return "oscar" }
func (w *OscarWriter) Dialect() dialect.Dialect     { return w.dia }

func (w *OscarWriter) Close() error {
	if w.session == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := w.bridge.CloseSession(ctx, w.session)
	w.session = 0
	return err
}

func (w *OscarWriter) execStatements(ctx context.Context, statements []dialect.Statement) error {
	for _, statement := range statements {
		if err := w.bridge.Exec(ctx, w.session, statement.SQL); err != nil {
			return err
		}
	}
	return nil
}

func (w *OscarWriter) CreateTable(ctx context.Context, statements []dialect.Statement) error {
	return w.execStatements(ctx, statements)
}

func (w *OscarWriter) CopyData(ctx context.Context, table string, columns []string, columnTypes []string, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}
	quoted := make([]string, len(columns))
	params := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = w.dia.QuoteIdent(column)
		params[i] = "?"
	}
	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", w.dia.QualifyTable(w.schema, table), strings.Join(quoted, ", "), strings.Join(params, ", "))
	batch, err := w.bridge.BeginBatch(ctx, w.session, sql)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			abortCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = w.bridge.AbortBatch(abortCtx, batch)
		}
	}()

	chunk := make([][]any, 0, oscarBatchRows)
	chunkBytes := 0
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		if err := w.bridge.AddBatch(ctx, batch, chunk); err != nil {
			return err
		}
		chunk = chunk[:0]
		chunkBytes = 0
		return nil
	}
	for _, row := range rows {
		converted := make([]any, len(row))
		rowBytes := 0
		for i, value := range row {
			dbType := ""
			if i < len(columnTypes) {
				dbType = columnTypes[i]
			}
			converted[i] = w.valueConv.Convert(value, w.srcType, dbType)
			rowBytes += estimateOscarValueSize(converted[i])
		}
		if len(chunk) > 0 && (len(chunk) >= oscarBatchRows || chunkBytes+rowBytes > oscarBatchBytes) {
			if err := flush(); err != nil {
				return err
			}
		}
		chunk = append(chunk, converted)
		chunkBytes += rowBytes
	}
	if err := flush(); err != nil {
		return err
	}
	if err := w.bridge.CommitBatch(ctx, batch); err != nil {
		return err
	}
	committed = true
	return nil
}

func estimateOscarValueSize(value any) int {
	switch v := value.(type) {
	case nil:
		return 1
	case string:
		return len(v) + 8
	case []byte:
		return len(v) + 8
	case jdbcbridge.Decimal:
		return len(v) + 8
	case jdbcbridge.Date:
		return len(v) + 8
	case jdbcbridge.Time:
		return len(v) + 8
	case jdbcbridge.Timestamp:
		return len(v) + 8
	default:
		return 16
	}
}

func (w *OscarWriter) CreateSequence(ctx context.Context, seq source.SequenceInfo, owner string) error {
	return w.execStatements(ctx, w.dia.SequenceStatements(w.schema, seq, owner))
}
func (w *OscarWriter) CreateIndex(ctx context.Context, idx source.IndexInfo) error {
	return w.execStatements(ctx, w.dia.IndexStatements(w.schema, idx))
}
func (w *OscarWriter) CreateForeignKey(ctx context.Context, fk source.FKInfo) error {
	return w.execStatements(ctx, w.dia.ForeignKeyStatements(w.schema, fk))
}
func (w *OscarWriter) CreateView(ctx context.Context, view source.ViewInfo) error {
	return w.execStatements(ctx, w.dia.ViewStatements(w.schema, view))
}
func (w *OscarWriter) CreateComment(ctx context.Context, comment source.CommentInfo) error {
	return w.execStatements(ctx, w.dia.CommentStatements(w.schema, comment))
}
func (w *OscarWriter) CountRows(ctx context.Context, table string) (int64, error) {
	return w.bridge.QueryInt64(ctx, w.session, "SELECT COUNT(*) FROM "+w.dia.QualifyTable(w.schema, table))
}
func (w *OscarWriter) AlterDistribute(context.Context, string, []string) error { return nil }
func (w *OscarWriter) SchemaExists(ctx context.Context, schema string) (bool, error) {
	return w.bridge.SchemaExists(ctx, w.session, schema)
}
func (w *OscarWriter) ChangeOwner(ctx context.Context, objectType, name, owner string) error {
	sql := fmt.Sprintf("ALTER %s %s OWNER TO %s", objectType, w.dia.QualifyTable(w.schema, name), w.dia.QuoteIdent(owner))
	return w.bridge.Exec(ctx, w.session, sql)
}
