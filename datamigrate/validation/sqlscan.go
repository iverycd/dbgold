package validation

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// SQLScanner owns read-only, explicitly projected queries. It deliberately
// bypasses migration ReadPage and ValueConverter, both of which may clean data.
// MaxScanRowBytes bounds retained row payloads. database/sql drivers may still
// allocate one current row internally before Scan; larger values fail closed.
const MaxScanRowBytes = 8 << 20

type SQLScanner struct {
	db             *sql.DB
	family, schema string
	catalog        string
}

func NewSQLScanner(db *sql.DB, family, schema string) *SQLScanner {
	return &SQLScanner{db: db, family: family, schema: schema}
}

// WithCatalog reuses a PostgreSQL-family catalog already detected by the
// connection owner. Empty leaves detection to this scanner. Catalog identifiers
// are validated against the same fixed candidates used by source.PGProfile.
func (s *SQLScanner) WithCatalog(catalog string) *SQLScanner {
	s.catalog = catalog
	return s
}

func (s *SQLScanner) catalogFor(ctx context.Context) (string, error) {
	if s.catalog != "" {
		if s.catalog != "pg_catalog" && s.catalog != "sys_catalog" {
			return "", unsupported("不支持的 PG 校验系统目录 %q", s.catalog)
		}
		return s.catalog, nil
	}
	var last error
	for _, candidate := range []string{"pg_catalog", "sys_catalog"} {
		cat := QuoteValidationIdent("postgres", candidate)
		query := `SELECT c.relname,a.attname FROM ` + cat + `.pg_attribute a JOIN ` + cat + `.pg_class c ON c.oid=a.attrelid JOIN ` + cat + `.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND a.attnum>0 AND NOT a.attisdropped AND c.relname IN ('pg_class','pg_namespace','pg_constraint')`
		rows, err := s.db.QueryContext(ctx, query, candidate)
		if err != nil {
			last = err
			continue
		}
		fields := map[string]bool{}
		for rows.Next() {
			var table, column string
			if err = rows.Scan(&table, &column); err != nil {
				break
			}
			fields[table+"."+column] = true
		}
		if err == nil {
			err = rows.Err()
		}
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			last = err
			continue
		}
		for _, field := range []string{"pg_class.oid", "pg_class.relname", "pg_class.relnamespace", "pg_namespace.oid", "pg_namespace.nspname", "pg_constraint.conrelid", "pg_constraint.contype", "pg_constraint.conkey"} {
			if !fields[field] {
				err = unsupported("目录 %s 缺少必要元数据 %s", candidate, field)
				break
			}
		}
		if err != nil {
			last = err
			continue
		}
		s.catalog = candidate
		return candidate, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("无法识别可用于内容校验的 PG 系统目录: %w", last)
}

func (s *SQLScanner) schemaFor(ctx context.Context, table string) (string, error) {
	if s.schema != "" {
		return s.schema, nil
	}
	if s.family == "sqlserver" {
		rows, e := s.db.QueryContext(ctx, `SELECT s.name FROM sys.tables t JOIN sys.schemas s ON s.schema_id=t.schema_id WHERE t.name=@p1`, table)
		if e != nil {
			return "", e
		}
		defer rows.Close()
		var name string
		n := 0
		for rows.Next() {
			if e = rows.Scan(&name); e != nil {
				return "", e
			}
			n++
		}
		if e = rows.Err(); e != nil {
			return "", e
		}
		if n != 1 {
			return "", fmt.Errorf("table %s resolves to %d SQL Server schemas", table, n)
		}
		return name, nil
	}
	q := "SELECT current_schema()"
	switch s.family {
	case "mysql":
		q = "SELECT DATABASE()"
	case "oracle":
		q = "SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM DUAL"
	case "dameng":
		q = "SELECT CURRENT_SCHEMA"
	}
	var schema string
	e := s.db.QueryRowContext(ctx, q).Scan(&schema)
	if e == nil && schema == "" {
		e = fmt.Errorf("cannot resolve validation schema")
	}
	return schema, e
}
func (s *SQLScanner) DescribeValidationTable(ctx context.Context, table string) (Table, error) {
	schema, e := s.schemaFor(ctx, table)
	if e != nil {
		return Table{}, e
	}
	var query, pk string
	var args []any
	switch s.family {
	case "mysql":
		query = `SELECT COLUMN_NAME, DATA_TYPE, COALESCE(NUMERIC_PRECISION,0), COALESCE(NUMERIC_SCALE,0), COALESCE(DATETIME_PRECISION,0), COALESCE(CHARACTER_MAXIMUM_LENGTH,0) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION`
		pk = `SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME='PRIMARY' ORDER BY ORDINAL_POSITION`
		args = []any{schema, table}
	case "postgres":
		catalog, err := s.catalogFor(ctx)
		if err != nil {
			return Table{}, err
		}
		cat := QuoteValidationIdent("postgres", catalog)
		query = `SELECT column_name, CASE WHEN data_type IN ('USER-DEFINED','ARRAY') THEN udt_name ELSE data_type END, COALESCE(numeric_precision,0), COALESCE(numeric_scale,0), COALESCE(datetime_precision,0), COALESCE(character_maximum_length,0), ordinal_position FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2 ORDER BY ordinal_position`
		// conkey contains primary-key columns only, unlike indkey on newer
		// servers which can additionally contain INCLUDE columns. Decode the
		// ordered vector in Go so old compatible kernels need no LATERAL,
		// unnest WITH ORDINALITY, or PostgreSQL 11 indnkeyatts field.
		pk = `SELECT c.conkey::text FROM ` + cat + `.pg_constraint c JOIN ` + cat + `.pg_class t ON t.oid=c.conrelid JOIN ` + cat + `.pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND t.relname=$2 AND c.contype='p'`
		args = []any{schema, table}
	case "oracle", "dameng":
		p1, p2 := ":1", ":2"
		if s.family == "dameng" {
			p1, p2 = "?", "?"
		}
		query = `SELECT COLUMN_NAME, DATA_TYPE, NVL(DATA_PRECISION,0), NVL(DATA_SCALE,0), NVL(DATA_SCALE,0), NVL(CHAR_LENGTH,0) FROM ALL_TAB_COLUMNS WHERE OWNER=` + p1 + ` AND TABLE_NAME=` + p2 + ` ORDER BY COLUMN_ID`
		pk = `SELECT c.COLUMN_NAME FROM ALL_CONS_COLUMNS c JOIN ALL_CONSTRAINTS k ON k.OWNER=c.OWNER AND k.CONSTRAINT_NAME=c.CONSTRAINT_NAME WHERE c.OWNER=` + p1 + ` AND c.TABLE_NAME=` + p2 + ` AND k.CONSTRAINT_TYPE='P' ORDER BY c.POSITION`
		args = []any{schema, table}
	case "sqlserver":
		query = `SELECT c.name, t.name, CAST(c.precision AS bigint), CAST(c.scale AS bigint), CAST(c.scale AS bigint), CAST(c.max_length AS bigint) FROM sys.columns c JOIN sys.tables o ON o.object_id=c.object_id JOIN sys.schemas s ON s.schema_id=o.schema_id JOIN sys.types t ON t.user_type_id=c.user_type_id WHERE s.name=@p1 AND o.name=@p2 ORDER BY c.column_id`
		pk = `SELECT c.name FROM sys.indexes i JOIN sys.tables o ON o.object_id=i.object_id JOIN sys.schemas s ON s.schema_id=o.schema_id JOIN sys.index_columns ic ON ic.object_id=i.object_id AND ic.index_id=i.index_id JOIN sys.columns c ON c.object_id=ic.object_id AND c.column_id=ic.column_id WHERE s.name=@p1 AND o.name=@p2 AND i.is_primary_key=1 ORDER BY ic.key_ordinal`
		args = []any{schema, table}
	default:
		return Table{}, fmt.Errorf("unsupported validation database family %s", s.family)
	}
	rows, e := s.db.QueryContext(ctx, query, args...)
	if e != nil {
		return Table{}, e
	}
	out := Table{}
	attributeNames := map[int]string{}
	for rows.Next() {
		var c Column
		var ordinal int
		dest := []any{&c.Name, &c.DatabaseType, &c.Precision, &c.Scale, &c.TimePrecision, &c.Length}
		if s.family == "postgres" {
			dest = append(dest, &ordinal)
		}
		if e = rows.Scan(dest...); e != nil {
			rows.Close()
			return Table{}, e
		}
		if s.family == "postgres" {
			if ordinal <= 0 || attributeNames[ordinal] != "" {
				rows.Close()
				return Table{}, unsupported("PG 列序号缺失或重复，无法恢复主键列顺序")
			}
			attributeNames[ordinal] = c.Name
		}
		ClassifyColumn(s.family, &c)
		out.Columns = append(out.Columns, c)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return Table{}, e
	}
	if len(out.Columns) == 0 {
		return Table{}, fmt.Errorf("no visible columns for %s.%s", schema, table)
	}
	rows, e = s.db.QueryContext(ctx, pk, args...)
	if e != nil {
		return Table{}, e
	}
	defer rows.Close()
	primaryRows := 0
	for rows.Next() {
		var name string
		if e = rows.Scan(&name); e != nil {
			return Table{}, e
		}
		if s.family == "postgres" {
			primaryRows++
			if primaryRows > 1 {
				return Table{}, unsupported("PG 表存在多条主键定义，无法确定校验键")
			}
			out.PrimaryKey, e = decodePGPrimaryKey(name, attributeNames)
			if e != nil {
				return Table{}, e
			}
		} else {
			out.PrimaryKey = append(out.PrimaryKey, name)
		}
	}
	return out, rows.Err()
}

func decodePGPrimaryKey(raw string, names map[int]string) ([]string, error) {
	// Handles int2vector, ordinary arrays, and explicit array-bound output.
	// The numeric order belongs to the key; sorting it would change composite PKs.
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, '='); i >= 0 {
		raw = raw[i+1:]
	}
	raw = strings.Trim(raw, "{}")
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
	if len(parts) == 0 {
		return nil, unsupported("PG 主键目录向量为空")
	}
	key := make([]string, 0, len(parts))
	seen := map[int]bool{}
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 || seen[n] || names[n] == "" {
			return nil, unsupported("PG 主键目录向量包含无效、重复或不可见列序号")
		}
		seen[n] = true
		key = append(key, names[n])
	}
	return key, nil
}

// ClassifyColumn is an explicit whitelist, shared with JDBC metadata. Unknown
// types (including user-defined and spatial types) are never stringified.
func ClassifyColumn(family string, c *Column) {
	t := strings.ToLower(strings.TrimSpace(c.DatabaseType))
	if i := strings.Index(t, "("); i >= 0 && !strings.Contains(t, "timestamp") {
		t = strings.TrimSpace(t[:i])
	}
	c.Kind = Unsupported
	switch t {
	case "char", "character", "nchar", "varchar", "nvarchar", "varchar2", "nvarchar2", "character varying", "national character varying", "national character", "text", "ntext", "tinytext", "mediumtext", "longtext", "clob", "nclob", "bpchar", "enum", "set":
		c.Kind = Text
	case "binary", "varbinary", "raw", "long raw", "bytea", "blob", "tinyblob", "mediumblob", "longblob", "image", "rowversion":
		c.Kind = Binary
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "int2", "int4", "int8", "number", "numeric", "decimal", "dec", "money", "smallmoney", "year":
		c.Kind = Number
	case "real", "float", "double", "double precision", "binary_float", "binary_double", "float4", "float8":
		c.Kind = Float
		c.FloatBits = 64
		if t == "float4" || t == "binary_float" || t == "real" && family != "mysql" || t == "float" && (family == "mysql" || family == "sqlserver" && c.Precision <= 24) {
			c.FloatBits = 32
		}
		if family == "oracle" && (t == "float" || t == "real" || t == "double precision") {
			c.Kind = Number
			c.FloatBits = 0
		}
		if family == "dameng" && (t == "float" || t == "double" || t == "double precision") {
			c.Kind = Unsupported
			c.FloatBits = 0
		}
	case "boolean", "bool":
		c.Kind = Bool
	case "bit", "bit varying", "varbit":
		c.Kind = Bits
		if family == "sqlserver" {
			c.Kind = Bool
		}
		if family == "mysql" {
			c.Length = c.Precision
		}
	case "uuid", "uniqueidentifier":
		c.Kind = UUID
	case "json", "jsonb":
		c.Kind = JSON
	case "date":
		c.Kind = Date
		if family == "oracle" {
			c.Kind = Timestamp
		}
	case "time", "time without time zone":
		c.Kind = Time
	case "datetime", "datetime2", "smalldatetime", "timestamp", "timestamp without time zone":
		c.Kind = Timestamp
	case "datetimeoffset", "timestamp with time zone", "timestamptz":
		c.Kind = TimestampTZ
	}
	if strings.HasPrefix(t, "timestamp(") {
		c.Kind = Timestamp
		if strings.Contains(t, "with time zone") {
			c.Kind = TimestampTZ
		}
		if strings.Contains(t, "local time zone") {
			c.Kind = Unsupported
		}
	}
	if family == "sqlserver" && t == "timestamp" {
		c.Kind = Binary
	}
}
func QuoteValidationIdent(family, name string) string {
	if family == "mysql" {
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	}
	if family == "sqlserver" {
		return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Projection avoids lossy driver decoding (notably MySQL parseTime) and NLS
// dependent decimal conversions. Native binary floats and bytes remain native.
func ValidationProjection(family string, c Column) (string, error) {
	q := QuoteValidationIdent(family, c.Name)
	if c.Kind == Unsupported {
		return "", fmt.Errorf("column %s type %s has no lossless validation serializer", c.Name, c.DatabaseType)
	}
	switch family {
	case "mysql":
		switch c.Kind {
		case Number, Date, Time, Timestamp:
			return "CAST(" + q + " AS CHAR)", nil
		case Bits:
			if c.Length <= 0 || c.Length > 64 {
				return "", fmt.Errorf("unsupported BIT width %d", c.Length)
			}
			return fmt.Sprintf("LPAD(BIN(CAST(%s AS UNSIGNED)),%d,'0')", q, c.Length), nil
		}
	case "postgres":
		switch c.Kind {
		case Number, Date, Time, Timestamp, TimestampTZ, Bits, UUID, JSON:
			return q + "::text", nil
		}
	case "oracle":
		switch c.Kind {
		case Number:
			return "TO_CHAR(" + q + ",'TM9','NLS_NUMERIC_CHARACTERS=''.,''')", nil
		case Timestamp:
			if strings.EqualFold(c.DatabaseType, "DATE") {
				return "TO_CHAR(" + q + ",'SYYYY-MM-DD\"T\"HH24:MI:SS')", nil
			}
			return "TO_CHAR(" + q + ",'SYYYY-MM-DD\"T\"HH24:MI:SS.FF9')", nil
		case TimestampTZ:
			return "TO_CHAR(" + q + ",'SYYYY-MM-DD\"T\"HH24:MI:SS.FF9TZH:TZM')", nil
		}
	case "dameng":
		switch c.Kind {
		case Number:
			return "CAST(" + q + " AS VARCHAR(256))", nil
		case Date:
			return "TO_CHAR(" + q + ",'YYYY-MM-DD')", nil
		case Time:
			return "CAST(" + q + " AS VARCHAR(128))", nil
		case Timestamp:
			return "TO_CHAR(" + q + ",'SYYYY-MM-DD\"T\"HH24:MI:SS.FF9')", nil
		case TimestampTZ:
			return "TO_CHAR(" + q + ",'SYYYY-MM-DD\"T\"HH24:MI:SS.FF9TZH:TZM')", nil
		}
	case "sqlserver":
		switch c.Kind {
		case Number:
			return "CONVERT(varchar(100)," + q + ",2)", nil
		case Date, Time, Timestamp:
			return "CONVERT(varchar(64)," + q + ",126)", nil
		case TimestampTZ:
			return "CONVERT(varchar(64)," + q + ",127)", nil
		case UUID:
			return "CONVERT(varchar(36)," + q + ")", nil
		case Text:
			if isSQLServerUnicode(c) {
				if strings.EqualFold(c.DatabaseType, "ntext") {
					q = "CAST(" + q + " AS nvarchar(max))"
				}
				return "CONVERT(varbinary(max)," + q + ")", nil
			}
			if strings.EqualFold(c.DatabaseType, "text") {
				return "CAST(" + q + " AS varchar(max))", nil
			}
		}
	}
	return q, nil
}
func (s *SQLScanner) OpenValidationScan(ctx context.Context, table string, columns []Column) (RowStream, error) {
	actual, e := s.DescribeValidationTable(ctx, table)
	if e != nil {
		return nil, e
	}
	for _, want := range columns {
		found := false
		for _, got := range actual.Columns {
			if got.Name == want.Name {
				found = true
				if !reflect.DeepEqual(got, want) {
					return nil, fmt.Errorf("column %s metadata changed", want.Name)
				}
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("column %s disappeared", want.Name)
		}
	}
	schema, e := s.schemaFor(ctx, table)
	if e != nil {
		return nil, e
	}
	expr := make([]string, len(columns))
	for i, c := range columns {
		expr[i], e = ValidationProjection(s.family, c)
		if e != nil {
			return nil, e
		}
	}
	conn, e := s.db.Conn(ctx)
	if e != nil {
		return nil, e
	}
	cleanup := func() {}
	// Use a dedicated session and restore changed settings before returning it to
	// the pool, including cancellation paths.
	if s.family == "postgres" {
		var oldZone, oldDate string
		if e = conn.QueryRowContext(ctx, `SELECT current_setting('TimeZone'), current_setting('DateStyle')`).Scan(&oldZone, &oldDate); e == nil {
			_, e = conn.ExecContext(ctx, `SELECT set_config('TimeZone','UTC',false),set_config('DateStyle','ISO, YMD',false)`)
		}
		if e != nil {
			conn.Close()
			return nil, e
		}
		cleanup = func() {
			c, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			if _, err := conn.ExecContext(c, `SELECT set_config('TimeZone',$1,false),set_config('DateStyle',$2,false)`, oldZone, oldDate); err != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
	}
	query := "SELECT " + strings.Join(expr, ",") + " FROM " + QuoteValidationIdent(s.family, schema) + "." + QuoteValidationIdent(s.family, table)
	rows, e := conn.QueryContext(ctx, query)
	if e != nil {
		cleanup()
		conn.Close()
		return nil, e
	}
	return &sqlValidationStream{rows: rows, conn: conn, cleanup: cleanup, columns: len(columns), nativeColumns: columns, family: s.family}, nil
}

type sqlValidationStream struct {
	rows          *sql.Rows
	conn          *sql.Conn
	cleanup       func()
	columns       int
	nativeColumns []Column
	family        string
	closed        bool
}

func (s *sqlValidationStream) Next(ctx context.Context) ([]any, error) {
	if s.closed {
		return nil, io.EOF
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if !s.rows.Next() {
		if e := s.rows.Err(); e != nil {
			return nil, e
		}
		return nil, io.EOF
	}
	values := make([]any, s.columns)
	p := make([]any, s.columns)
	for i := range values {
		p[i] = &values[i]
	}
	if e := s.rows.Scan(p...); e != nil {
		return nil, e
	}
	if s.family == "sqlserver" {
		for i, c := range s.nativeColumns {
			if values[i] == nil {
				continue
			}
			if isSQLServerUnicode(c) {
				raw, ok := values[i].([]byte)
				if !ok {
					return nil, unsupported("SQL Server Unicode 列 %s 未返回原始 UTF-16LE 字节", c.Name)
				}
				if len(raw) > MaxScanRowBytes {
					return nil, unsupported("SQL Server Unicode 列超过原值读取字节上限")
				}
				text, err := decodeValidationUTF16LE(raw)
				if err != nil {
					return nil, err
				}
				values[i] = text
			} else if c.Kind == Text {
				var text string
				switch raw := values[i].(type) {
				case string:
					text = raw
				case []byte:
					text = string(raw)
				}
				if strings.ContainsRune(text, '\uFFFD') {
					return nil, unsupported("SQL Server 非 Unicode 字符结果含替换字符，驱动可能已替换原始字节，无法证明无损")
				}
			}
		}
	}
	total := 0
	for _, v := range values {
		switch x := v.(type) {
		case []byte:
			total += len(x)
		case string:
			total += len(x)
		default:
			total += 32
		}
		if total > MaxScanRowBytes {
			return nil, fmt.Errorf("validation row exceeds %d byte read limit", MaxScanRowBytes)
		}
	}
	return values, nil
}
func (s *sqlValidationStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	e := s.rows.Close()
	s.cleanup()
	if err := s.conn.Close(); e == nil {
		e = err
	}
	return e
}

func isSQLServerUnicode(c Column) bool {
	if c.Kind != Text {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(c.DatabaseType)) {
	case "nchar", "nvarchar", "ntext":
		return true
	}
	return false
}

// The SQL Server driver uses utf16.Decode, which silently substitutes U+FFFD
// for unpaired surrogates. Raw varbinary projections let validation reject that
// loss instead of comparing the same replacement on both sides.
func decodeValidationUTF16LE(raw []byte) (string, error) {
	if len(raw)%2 != 0 {
		return "", unsupported("SQL Server Unicode 原值含奇数字节，无法无损解码 UTF-16LE")
	}
	var out strings.Builder
	out.Grow(len(raw))
	for i := 0; i < len(raw); i += 2 {
		unit := binary.LittleEndian.Uint16(raw[i : i+2])
		if unit >= 0xD800 && unit <= 0xDBFF {
			if i+4 > len(raw) {
				return "", unsupported("SQL Server Unicode 原值含未配对高代理项，无法无损解码 UTF-16LE")
			}
			low := binary.LittleEndian.Uint16(raw[i+2 : i+4])
			if low < 0xDC00 || low > 0xDFFF {
				return "", unsupported("SQL Server Unicode 原值含未配对高代理项，无法无损解码 UTF-16LE")
			}
			out.WriteRune(utf16.DecodeRune(rune(unit), rune(low)))
			i += 2
		} else if unit >= 0xDC00 && unit <= 0xDFFF {
			return "", unsupported("SQL Server Unicode 原值含未配对低代理项，无法无损解码 UTF-16LE")
		} else {
			out.WriteRune(rune(unit))
		}
	}
	return out.String(), nil
}
