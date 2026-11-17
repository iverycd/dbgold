package source

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbgold/internal/jdbcbridge"
)

const (
	oscarCursorBytes = 8 << 20
	oscarCursorRows  = 256
	oscarCleanupTime = 3 * time.Second
)

type oscarReadBridge interface {
	OpenSession(context.Context, jdbcbridge.SessionOptions) (uint64, error)
	CloseSession(context.Context, uint64) error
	ListSchemas(context.Context, uint64) ([]string, error)
	MigrationColumns(context.Context, uint64, string, string) ([]jdbcbridge.MigrationColumn, error)
	QueryInt64(context.Context, uint64, string) (int64, error)
	OpenCursor(context.Context, uint64, string) (uint64, error)
	OpenBoundCursor(context.Context, uint64, string, ...string) (uint64, error)
	FetchCursor(context.Context, uint64, int, int) ([][]any, bool, error)
	FetchMigrationCursor(context.Context, uint64, int, int) ([][]any, bool, error)
	CloseCursor(context.Context, uint64) error
}

type OscarReader struct {
	bridge  oscarReadBridge
	session uint64
	schema  string
}

func NewOscar(dsn, user, password, schema string, pool ConnPoolConfig) (*OscarReader, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return NewOscarContext(ctx, dsn, user, password, schema, pool)
}

func NewOscarContext(ctx context.Context, dsn, user, password, schema string, pool ConnPoolConfig) (*OscarReader, error) {
	return newOscarReader(ctx, jdbcbridge.Default(), dsn, user, password, schema, pool)
}

func newOscarReader(ctx context.Context, bridge oscarReadBridge, dsn, user, password, schema string, pool ConnPoolConfig) (*OscarReader, error) {
	// Qualify every query explicitly. Opening the session in its default schema
	// keeps native catalog views visible even when migrating another schema.
	session, err := bridge.OpenSession(ctx, jdbcbridge.SessionOptions{
		URL: dsn, Username: user, Password: password,
		MaxOpenConns: pool.MaxOpenConns, MaxIdleConns: pool.MaxIdleConns, ConnMaxLifetime: pool.ConnMaxLifetime,
	})
	if err != nil {
		return nil, err
	}
	r := &OscarReader{bridge: bridge, session: session, schema: schema}
	if schema == "" {
		rows, queryErr := r.query(ctx, `SELECT CURRENT_SCHEMA()`)
		if queryErr == nil {
			r.schema, queryErr = oscarSingleText(rows)
		}
		if queryErr != nil {
			_ = r.Close()
			return nil, fmt.Errorf("读取神通当前 Schema: %w", queryErr)
		}
	} else {
		schemas, schemaErr := r.ListDatabases(ctx)
		if schemaErr != nil {
			_ = r.Close()
			return nil, fmt.Errorf("检查神通源 Schema %q: %w", schema, schemaErr)
		}
		found := false
		for _, visible := range schemas {
			if visible == schema {
				found = true
				break
			}
		}
		if !found {
			_ = r.Close()
			return nil, fmt.Errorf("神通源 Schema %q 不存在或当前用户不可见", schema)
		}
	}
	return r, nil
}

func (r *OscarReader) DBType() string          { return "oscar" }
func (r *OscarReader) MigrationSchema() string { return r.schema }

func (r *OscarReader) Close() error {
	if r.session == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), oscarCleanupTime)
	defer cancel()
	err := r.bridge.CloseSession(ctx, r.session)
	r.session = 0
	return err
}

func (r *OscarReader) ListDatabases(ctx context.Context) ([]string, error) {
	schemas, err := r.bridge.ListSchemas(ctx, r.session)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		switch strings.ToUpper(schema) {
		case "SYS", "SYS_CATALOG", "INFO_SCHEM", "INFORMATION_SCHEMA", "PG_CATALOG", "SYS_TOAST":
			continue
		}
		if schema != "" {
			out = append(out, schema)
		}
	}
	return out, nil
}

func oscarQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func (r *OscarReader) table(table string) string {
	return oscarQuoteIdent(r.schema) + "." + oscarQuoteIdent(table)
}

func (r *OscarReader) closeCursor(id uint64) error {
	ctx, cancel := context.WithTimeout(context.Background(), oscarCleanupTime)
	defer cancel()
	return r.bridge.CloseCursor(ctx, id)
}

func (r *OscarReader) scan(ctx context.Context, query string, args []string, fetchRows int, data bool, consume func([][]any) error) (retErr error) {
	var id uint64
	var err error
	if len(args) == 0 {
		id, err = r.bridge.OpenCursor(ctx, r.session, query)
	} else {
		id, err = r.bridge.OpenBoundCursor(ctx, r.session, query, args...)
	}
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, r.closeCursor(id)) }()
	fetch := r.bridge.FetchCursor
	if data {
		fetch = r.bridge.FetchMigrationCursor
	}
	for {
		rows, done, err := fetch(ctx, id, fetchRows, oscarCursorBytes)
		if err != nil {
			return err
		}
		if len(rows) == 0 && !done {
			return fmt.Errorf("神通 JDBC 游标未返回数据或完成状态")
		}
		if len(rows) > 0 {
			if err := consume(rows); err != nil {
				return err
			}
		}
		if done {
			return ctx.Err()
		}
	}
}

func (r *OscarReader) query(ctx context.Context, query string, args ...string) ([][]any, error) {
	var result [][]any
	err := r.scan(ctx, query, args, oscarCursorRows, false, func(rows [][]any) error {
		result = append(result, rows...)
		return nil
	})
	return result, err
}

func oscarText(value any) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("神通目录返回非文本值 %T", value)
	}
	return s, nil
}

func oscarSingleText(rows [][]any) (string, error) {
	if len(rows) != 1 || len(rows[0]) != 1 {
		return "", fmt.Errorf("神通目录未返回唯一值")
	}
	s, err := oscarText(rows[0][0])
	if err == nil && s == "" {
		err = fmt.Errorf("神通目录返回空文本")
	}
	return s, err
}

func oscarInteger(value any) (int64, error) {
	switch n := value.(type) {
	case int64:
		return n, nil
	case string:
		v, err := strconv.ParseInt(n, 10, 64)
		if err == nil {
			return v, nil
		}
	}
	return 0, fmt.Errorf("神通目录返回非整数值 %T", value)
}

func (r *OscarReader) CountRows(ctx context.Context, table string) (int64, error) {
	return r.bridge.QueryInt64(ctx, r.session, "SELECT COUNT(*) FROM "+r.table(table))
}

func (r *OscarReader) ReadPage(ctx context.Context, table string, pkCols []string, offset, limit int64) ([]string, []string, [][]any, error) {
	if offset < 0 || limit <= 0 {
		return nil, nil, nil, fmt.Errorf("神通数据分页边界无效")
	}
	cols, types, raw, selectSQL, err := r.dataProjection(ctx, table)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(pkCols) > 0 {
		selectSQL += " ORDER BY " + oscarProjection(pkCols)
	}
	selectSQL += fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)
	var rows [][]any
	err = r.scan(ctx, selectSQL, nil, oscarCursorRows, true, func(chunk [][]any) error {
		if err := checkOscarMigrationRows(table, raw, chunk); err != nil {
			return err
		}
		rows = append(rows, chunk...)
		return nil
	})
	return cols, types, rows, err
}

func oscarProjection(cols []string) string {
	quoted := make([]string, len(cols))
	for i, col := range cols {
		quoted[i] = oscarQuoteIdent(col)
	}
	return strings.Join(quoted, ",")
}

func (r *OscarReader) dataProjection(ctx context.Context, table string) ([]string, []string, []jdbcbridge.MigrationColumn, string, error) {
	raw, err := r.columns(ctx, table)
	if err != nil {
		return nil, nil, nil, "", err
	}
	names := make([]string, len(raw))
	types := make([]string, len(raw))
	for i, c := range raw {
		names[i] = c.Name
		types[i] = strings.ToUpper(oscarMigrationColumnType(c))
		if (types[i] == "BIT" && c.Size != 1) || types[i] == "VARBIT" || types[i] == "BIT VARYING" {
			return nil, nil, nil, "", fmt.Errorf("神通列 %s.%s 的 %s(%d) 缺少无损游标读取能力", table, c.Name, c.Type, c.Size)
		}
	}
	return names, types, raw, "SELECT " + oscarProjection(names) + " FROM " + r.table(table), nil
}

// StreamOscarTable uses a single JDBC result set for every table, including
// tables with no primary key and tables with identical rows.
func (r *OscarReader) StreamOscarTable(ctx context.Context, table string, pageSize int, consume func([]string, []string, [][]any) error) error {
	if pageSize <= 0 || consume == nil {
		return fmt.Errorf("神通流式读取需要正数批次大小和回调")
	}
	cols, types, raw, query, err := r.dataProjection(ctx, table)
	if err != nil {
		return err
	}
	return r.scan(ctx, query, nil, min(pageSize, oscarCursorRows), true, func(rows [][]any) error {
		if err := checkOscarMigrationRows(table, raw, rows); err != nil {
			return err
		}
		return consume(cols, types, rows)
	})
}

func checkOscarMigrationRows(table string, columns []jdbcbridge.MigrationColumn, rows [][]any) error {
	for _, row := range rows {
		if len(row) != len(columns) {
			return fmt.Errorf("神通表 %s 返回列数与固定投影不一致", table)
		}
	}
	for i, column := range columns {
		typ := strings.ToUpper(oscarMigrationColumnType(column))
		layout := ""
		switch typ {
		case "DATE":
			layout = "2006-01-02"
		case "TIMESTAMP", "TIMESTAMP WITHOUT TIME ZONE", "DATETIME":
			layout = "2006-01-02 15:04:05.999999999"
		case "TIME", "TIME WITHOUT TIME ZONE":
			layout = "15:04:05.999999999"
		default:
			continue
		}
		quantum := 1_000_000_000
		for j := int64(0); j < min(column.Scale, 6); j++ {
			quantum /= 10
		}
		for _, row := range rows {
			if row[i] == nil {
				continue
			}
			raw, ok := row[i].(string)
			t, err := time.Parse(layout, strings.Replace(raw, "T", " ", 1))
			if typ == "DATE" && err != nil {
				t, err = time.Parse("2006-01-02 15:04:05.999999999", strings.Replace(raw, "T", " ", 1))
				if err == nil && (t.Hour() != 0 || t.Minute() != 0 || t.Second() != 0 || t.Nanosecond() != 0) {
					return fmt.Errorf("神通表 %s 列 %s 的 DATE 含日内时间，无法无损写入 MySQL DATE", table, column.Name)
				}
			}
			if !ok || err != nil || strings.HasPrefix(layout, "2006") && t.Year() < 1000 || t.Nanosecond()%quantum != 0 {
				return fmt.Errorf("神通表 %s 列 %s 的时间值超出目标值域或列精度", table, column.Name)
			}
			row[i] = t.Format(layout)
		}
	}
	return nil
}

var _ Reader = (*OscarReader)(nil)
