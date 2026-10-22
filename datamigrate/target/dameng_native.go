package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	"gitee.com/chunanyong/dm"
)

// DaMengNativeWriter never routes native definitions through a cross-vendor
// dialect. The embedded writer supplies connection lifetime and validation only.
type DaMengNativeWriter struct{ *DaMengWriter }

var _ Writer = (*DaMengNativeWriter)(nil)
var _ damengnative.Writer = (*DaMengNativeWriter)(nil)

func NewDaMengNative(dsn, schema string, pool ConnPoolConfig) (*DaMengNativeWriter, error) {
	db, err := sql.Open("dm", dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err = db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	schema, err = damengnative.ResolveSchema(context.Background(), db, schema)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	dia := &damengNativeDialect{DaMengDialect: dialect.NewDaMeng()}
	return &DaMengNativeWriter{&DaMengWriter{db: db, schema: schema, srcType: "dameng", dia: dia, identCache: map[string]bool{}}}, nil
}

// This adapter deliberately rejects regeneration: original native SQL is the
// only executable definition, including during recovery.
type damengNativeDialect struct{ *dialect.DaMengDialect }

func (d *damengNativeDialect) QuoteIdent(name string) string { return damengnative.Quote(name) }
func (d *damengNativeDialect) QualifyTable(schema, table string) string {
	if schema == "" {
		return damengnative.Quote(table)
	}
	return damengnative.Qualified(schema, table)
}
func (d *damengNativeDialect) MapType(col source.ColumnInfo, _ string, _ dialect.TypeOpt) string {
	if col.FormattedType != "" {
		return col.FormattedType
	}
	return col.DataType
}
func (d *damengNativeDialect) CreateTableStatements(string, *source.TableDDLInfo, string, dialect.TypeOpt, dialect.NameFunc) ([]dialect.Statement, error) {
	return nil, fmt.Errorf("达梦同库迁移必须使用冻结的原始表 DDL")
}
func (d *damengNativeDialect) SequenceStatements(string, source.SequenceInfo, string) []dialect.Statement {
	return nil
}
func (d *damengNativeDialect) IndexStatements(string, source.IndexInfo) []dialect.Statement {
	return nil
}
func (d *damengNativeDialect) ForeignKeyStatements(string, source.FKInfo) []dialect.Statement {
	return nil
}
func (d *damengNativeDialect) ViewStatements(string, source.ViewInfo) []dialect.Statement { return nil }
func (d *damengNativeDialect) CommentStatements(string, source.CommentInfo) []dialect.Statement {
	return nil
}
func (d *damengNativeDialect) AdjustViewDefinition(def string) string { return def }

func (w *DaMengNativeWriter) InspectDamengNative(ctx context.Context) (*damengnative.Snapshot, error) {
	return damengnative.ReadCatalog(ctx, w.db, w.schema, false)
}

func (w *DaMengNativeWriter) SchemaExists(ctx context.Context, schema string) (bool, error) {
	resolved, err := damengnative.ResolveSchema(ctx, w.db, schema)
	if err != nil {
		return false, err
	}
	return resolved != "", nil
}

func (w *DaMengNativeWriter) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+damengnative.Qualified(w.schema, table)).Scan(&count)
	return count, err
}

func (w *DaMengNativeWriter) CopyData(context.Context, string, []string, []string, [][]interface{}) error {
	return fmt.Errorf("达梦同库数据必须使用冻结列定义的原生写入器")
}

func (w *DaMengNativeWriter) ExecuteDamengNative(ctx context.Context, statement string) error {
	if err := validateDamengNativeSQL(w.schema, statement); err != nil {
		return err
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	// SET SCHEMA is session-scoped. Never return this modified session to the
	// pool, even after an execution or observer failure.
	defer func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }); _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "SET SCHEMA "+damengnative.Quote(w.schema)); err != nil {
		return err
	}
	_, err = execObserved(ctx, conn, statement)
	if err == nil {
		w.identMu.Lock()
		w.identCache = map[string]bool{}
		w.identMu.Unlock()
	}
	return err
}

// NativeBatchBytes bounds retained converted data. The orchestrator flushes
// earlier; accepting one larger row avoids turning large LOBs into a row limit.
const NativeBatchBytes = 8 << 20

var nativeNumeric = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)
var nativeTypeSize = regexp.MustCompile(`\([^)]*\)`)
var nativeSQLType = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_ ]*(?:\([0-9]+(?:\s*,\s*-?[0-9]+)?(?:\s+(?:BYTE|CHAR))?\))?(?:\s+(?:WITH(?: LOCAL)? TIME ZONE|TO (?:MONTH|SECOND)(?:\([0-9]+\))?))?$`)
var nativeYMInterval = regexp.MustCompile(`^INTERVAL (?:YEAR(?: TO MONTH)?|MONTH)$`)
var nativeDTInterval = regexp.MustCompile(`^INTERVAL (?:DAY(?: TO (?:HOUR|MINUTE|SECOND))?|HOUR(?: TO (?:MINUTE|SECOND))?|MINUTE(?: TO SECOND)?|SECOND)$`)

func nativeColumnKind(c damengnative.Column) string {
	t := strings.ToUpper(strings.Join(strings.Fields(c.DataType), " "))
	t = strings.TrimSpace(nativeTypeSize.ReplaceAllString(t, ""))
	return strings.Join(strings.Fields(t), " ")
}

// ValidateDamengNativeColumn is shared with preflight so an unknown codec can
// never be discovered only after the destination has been cleared.
func ValidateDamengNativeColumn(c damengnative.Column) error {
	if !c.Writable {
		return nil
	}
	if c.Name == "" || strings.ContainsRune(c.Name, 0) {
		return fmt.Errorf("无效的达梦列名称")
	}
	if c.TypeSchema != "" && c.TypeName != "" {
		return nil
	}
	if kind := nativeColumnKind(c); nativeYMInterval.MatchString(kind) || nativeDTInterval.MatchString(kind) {
		return nil
	}
	switch nativeColumnKind(c) {
	case "DECIMAL", "DEC", "NUMERIC", "NUMBER", "FLOAT", "DATE", "DATETIME", "TIMESTAMP", "TIME", "TIME WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE", "DATETIME WITH TIME ZONE":
		if _, err := nativeTypeSQL(c); err != nil {
			return err
		}
	}
	switch nativeColumnKind(c) {
	case "TINYINT", "SMALLINT", "INT", "INTEGER", "BIGINT", "BYTE", "BIT", "BOOLEAN", "BOOL",
		"DECIMAL", "DEC", "NUMERIC", "NUMBER", "REAL", "DOUBLE", "DOUBLE PRECISION", "FLOAT",
		"CHAR", "CHARACTER", "VARCHAR", "VARCHAR2", "CHAR VARYING", "CHARACTER VARYING", "NCHAR", "NVARCHAR", "NVARCHAR2",
		"CLOB", "NCLOB", "TEXT", "LONG", "LONGVARCHAR", "BINARY", "VARBINARY", "RAW", "LONG RAW", "BLOB", "IMAGE", "LONGVARBINARY",
		"DATE", "DATETIME", "TIMESTAMP", "TIME", "TIME WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE", "DATETIME WITH TIME ZONE",
		"INTERVAL YEAR TO MONTH", "INTERVAL DAY TO SECOND", "ROWID", "UROWID":
		return nil
	}
	return fmt.Errorf("列 %s 的达梦类型 %s 尚无无损写入器", c.Name, c.DataType)
}

func nativeTypeSQL(c damengnative.Column) (string, error) {
	t := strings.ToUpper(strings.Join(strings.Fields(c.SQLType), " "))
	if t == "" {
		t = strings.ToUpper(strings.Join(strings.Fields(c.DataType), " "))
	}
	if !nativeSQLType.MatchString(t) {
		return "", fmt.Errorf("列 %s 类型定义无法安全绑定: %s", c.Name, c.SQLType)
	}
	return t, nil
}

func nativePlaceholder(c damengnative.Column) (string, error) {
	if err := ValidateDamengNativeColumn(c); err != nil {
		return "", err
	}
	kind := nativeColumnKind(c)
	switch kind {
	case "DECIMAL", "DEC", "NUMERIC", "NUMBER":
		t, err := nativeTypeSQL(c)
		if err != nil {
			return "", err
		}
		// The inner exact VARCHAR parameter prevents dm.fromString from
		// converting text to a 64-bit big.Float or encoding a DmDecimal.
		return "CAST(CAST(? AS VARCHAR(256)) AS " + t + ")", nil
	case "FLOAT":
		t, err := nativeTypeSQL(c)
		if err != nil {
			return "", err
		}
		return "CAST(CAST(? AS VARCHAR(256)) AS " + t + ")", nil
	case "DATE", "DATETIME", "TIMESTAMP", "TIME", "TIME WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE", "DATETIME WITH TIME ZONE":
		t, err := nativeTypeSQL(c)
		if err != nil {
			return "", err
		}
		return "CAST(? AS " + t + ")", nil
	}
	return "?", nil
}

func nativeDecimalText(v any) (string, error) {
	var raw string
	switch x := v.(type) {
	case string:
		raw = x
	case []byte:
		raw = string(x)
	case *dm.DmDecimal:
		if x == nil {
			return "", fmt.Errorf("nil decimal")
		}
		raw = x.String()
	case dm.DmDecimal:
		raw = x.String()
	case int64:
		raw = fmt.Sprint(x)
	case int32:
		raw = fmt.Sprint(x)
	case int16:
		raw = fmt.Sprint(x)
	case int8:
		raw = fmt.Sprint(x)
	case int:
		raw = fmt.Sprint(x)
	default:
		return "", fmt.Errorf("精确十进制值不允许 %T", v)
	}
	if len(raw) > 250 || !nativeNumeric.MatchString(raw) {
		return "", fmt.Errorf("无效的精确十进制值")
	}
	if at := strings.IndexAny(raw, "eE"); at >= 0 {
		exponent, err := strconv.ParseInt(raw[at+1:], 10, 16)
		if err != nil || exponent < -256 || exponent > 256 {
			return "", fmt.Errorf("十进制指数超出达梦可表示范围")
		}
	}
	if _, ok := new(big.Rat).SetString(raw); !ok {
		return "", fmt.Errorf("无效的精确十进制值")
	}
	return raw, nil
}

func nativeCopyValue(c damengnative.Column, value any, schema string) (any, int, error) {
	if value == nil {
		return nil, 0, nil
	}
	if c.TypeSchema != "" && c.TypeName != "" {
		switch v := value.(type) {
		case *dm.DmStruct:
			if v == nil || !v.Valid {
				return nil, 0, nil
			}
		case *dm.DmArray:
			if v == nil || !v.Valid {
				return nil, 0, nil
			}
		}
		sourceOwner := ""
		if c.TypeSchema == schema {
			typeName, err := nativeComplexSQLName(value)
			if err != nil {
				return nil, 0, err
			}
			parts, err := nativeSQLTokens(typeName)
			if err != nil {
				return nil, 0, err
			}
			if len(parts) == 3 && parts[1] == "." {
				sourceOwner = nativeTokenName(parts[0])
			}
		}
		size, err := DamengNativeValueSize(value)
		if err != nil {
			return nil, 0, err
		}
		v, err := nativeComplexValue(value, sourceOwner, c.TypeSchema)
		return v, size, err
	}
	kind := nativeColumnKind(c)
	if nativeYMInterval.MatchString(kind) || nativeDTInterval.MatchString(kind) {
		text, ok := value.(string)
		if !ok {
			return nil, 0, fmt.Errorf("时间间隔收到 %T", value)
		}
		if nativeYMInterval.MatchString(kind) {
			v, err := dm.NewDmIntervalYMByString(text)
			return v, len(text), err
		}
		v, err := dm.NewDmIntervalDTByString(text)
		return v, len(text), err
	}
	switch kind {
	case "DECIMAL", "DEC", "NUMERIC", "NUMBER":
		v, err := nativeDecimalText(value)
		return v, len(v), err
	case "CHAR", "CHARACTER", "VARCHAR", "VARCHAR2", "CHAR VARYING", "CHARACTER VARYING", "NCHAR", "NVARCHAR", "NVARCHAR2", "CLOB", "NCLOB", "TEXT", "LONG", "LONGVARCHAR", "ROWID", "UROWID":
		var text string
		switch v := value.(type) {
		case string:
			text = v
		case []byte:
			text = string(v)
		default:
			return nil, 0, fmt.Errorf("文本列收到 %T", value)
		}
		if !utf8.ValidString(text) {
			return nil, 0, fmt.Errorf("文本不是有效 UTF-8")
		}
		if kind == "CLOB" || kind == "NCLOB" || kind == "TEXT" || kind == "LONG" || kind == "LONGVARCHAR" {
			return dm.NewClob(text), len(text), nil
		}
		return text, len(text), nil
	case "BINARY", "VARBINARY", "RAW", "LONG RAW", "BLOB", "IMAGE", "LONGVARBINARY":
		v, ok := value.([]byte)
		if !ok {
			return nil, 0, fmt.Errorf("二进制列收到 %T", value)
		}
		if kind == "BLOB" || kind == "IMAGE" || kind == "LONG RAW" || kind == "LONGVARBINARY" {
			return dm.NewBlob(v), len(v), nil
		}
		return v, len(v), nil
	case "INTERVAL YEAR TO MONTH":
		text, ok := value.(string)
		if !ok {
			return nil, 0, fmt.Errorf("年月间隔收到 %T", value)
		}
		v, err := dm.NewDmIntervalYMByString(text)
		return v, len(text), err
	case "INTERVAL DAY TO SECOND":
		text, ok := value.(string)
		if !ok {
			return nil, 0, fmt.Errorf("日秒间隔收到 %T", value)
		}
		v, err := dm.NewDmIntervalDTByString(text)
		return v, len(text), err
	case "DATE", "DATETIME", "TIMESTAMP", "TIME", "TIME WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE", "DATETIME WITH TIME ZONE":
		v, ok := value.(time.Time)
		if !ok {
			return nil, 0, fmt.Errorf("时间列收到 %T", value)
		}
		return v, 16, nil
	case "REAL", "FLOAT", "DOUBLE", "DOUBLE PRECISION":
		switch v := value.(type) {
		case float32:
			if math.IsInf(float64(v), 0) || math.IsNaN(float64(v)) {
				return nil, 0, fmt.Errorf("无法无损复制非有限浮点值")
			}
			if kind == "FLOAT" {
				return strconv.FormatFloat(float64(v), 'g', -1, 32), 4, nil
			}
			return v, 4, nil
		case float64:
			if math.IsInf(v, 0) || math.IsNaN(v) {
				return nil, 0, fmt.Errorf("无法无损复制非有限浮点值")
			}
			if kind == "FLOAT" {
				return strconv.FormatFloat(v, 'g', -1, 64), 8, nil
			}
			return v, 8, nil
		case string:
			raw, err := nativeDecimalText(v)
			return raw, len(raw), err
		}
	case "TINYINT", "SMALLINT", "INT", "INTEGER", "BIGINT", "BYTE", "BIT", "BOOLEAN", "BOOL":
		switch v := value.(type) {
		case int8, int16, int32, int64, int, bool:
			return v, 8, nil
		}
	}
	return nil, 0, fmt.Errorf("列 %s 的值类型 %T 无法无损写入 %s", c.Name, value, c.DataType)
}

func nativeComplexSQLName(value any) (name string, err error) {
	defer func() {
		if recover() != nil {
			name = ""
			err = fmt.Errorf("达梦对象值缺少可读取的类型描述符")
		}
	}()
	switch v := value.(type) {
	case *dm.DmStruct:
		return v.GetSQLTypeName()
	case *dm.DmArray:
		return v.GetBaseTypeName()
	}
	return "", fmt.Errorf("对象列收到 %T", value)
}

func nativeComplexValue(value any, sourceOwner, schema string) (any, error) {
	return nativeComplexValueAt(value, sourceOwner, schema, true)
}
func nativeComplexValueAt(value any, sourceOwner, schema string, top bool) (any, error) {
	switch v := value.(type) {
	case *dm.DmBlob:
		if !top {
			return nil, fmt.Errorf("达梦 Go 驱动无法无损绑定自定义对象内部的 BLOB")
		}
		if v == nil || !v.Valid {
			return nil, nil
		}
		length, err := v.GetLength()
		if err != nil {
			return nil, err
		}
		if length < 0 || length > math.MaxInt32 {
			return nil, fmt.Errorf("对象 BLOB 超出驱动长度上限")
		}
		data := make([]byte, int(length))
		if length > 0 {
			n, err := v.ReadAt(1, data)
			if err != nil {
				return nil, err
			}
			if n != len(data) {
				return nil, fmt.Errorf("对象 BLOB 读取不完整")
			}
		}
		return dm.NewBlob(data), nil
	case *dm.DmClob:
		if !top {
			return nil, fmt.Errorf("达梦 Go 驱动无法无损绑定自定义对象内部的 CLOB")
		}
		if v == nil || !v.Valid {
			return nil, nil
		}
		length, err := v.GetLength()
		if err != nil {
			return nil, err
		}
		if length < 0 || length > math.MaxInt32 {
			return nil, fmt.Errorf("对象 CLOB 超出驱动长度上限")
		}
		text := ""
		if length > 0 {
			text, err = v.ReadString(1, int(length))
			if err != nil {
				return nil, err
			}
			if int64(utf8.RuneCountInString(text)) != length {
				return nil, fmt.Errorf("对象 CLOB 读取不完整")
			}
		}
		return dm.NewClob(text), nil
	case dm.DmBlob:
		return nativeComplexValueAt(&v, sourceOwner, schema, top)
	case dm.DmClob:
		return nativeComplexValueAt(&v, sourceOwner, schema, top)
	case *dm.DmStruct:
		if v == nil || !v.Valid {
			return nil, nil
		}
		name := ""
		var err error
		if top {
			name, err = nativeComplexSQLName(v)
			if err != nil {
				return nil, err
			}
		}
		fields, err := v.GetAttributes()
		if err != nil {
			return nil, err
		}
		for i := range fields {
			fields[i], err = nativeComplexValueAt(fields[i], sourceOwner, schema, false)
			if err != nil {
				return nil, err
			}
		}
		if !top {
			return fields, nil
		}
		return dm.NewDmStruct(nativeComplexTypeName(sourceOwner, schema, name), fields), nil
	case *dm.DmArray:
		if v == nil || !v.Valid {
			return nil, nil
		}
		name := ""
		var err error
		if top {
			name, err = nativeComplexSQLName(v)
			if err != nil {
				return nil, err
			}
		}
		values, err := v.GetArray()
		if err != nil {
			return nil, err
		}
		var elements []any
		if values != nil {
			rv := reflect.ValueOf(values)
			if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
				return nil, fmt.Errorf("数组 %s 返回不支持的元素类型 %T", name, values)
			}
			elements = make([]any, rv.Len())
			for i := range elements {
				elements[i] = rv.Index(i).Interface()
			}
		}
		for i := range elements {
			elements[i], err = nativeComplexValueAt(elements[i], sourceOwner, schema, false)
			if err != nil {
				return nil, err
			}
		}
		if !top {
			return elements, nil
		}
		return dm.NewDmArray(nativeComplexTypeName(sourceOwner, schema, name), elements), nil
	case *dm.DmDecimal:
		if v == nil {
			return nil, nil
		}
		checked, err := valueconv.OracleDameng(v.String(), "NUMBER")
		if err != nil {
			return nil, err
		}
		if !top {
			return *checked.(*dm.DmDecimal), nil
		}
		return checked, nil
	case dm.DmDecimal:
		checked, err := valueconv.OracleDameng(v.String(), "NUMBER")
		if err != nil {
			return nil, err
		}
		if !top {
			return *checked.(*dm.DmDecimal), nil
		}
		return checked, nil
	case *dm.DmIntervalDT:
		if v == nil {
			return nil, nil
		}
		if !top {
			return *v, nil
		}
		return v, nil
	case *dm.DmIntervalYM:
		if v == nil {
			return nil, nil
		}
		if !top {
			return *v, nil
		}
		return v, nil
	case nil, string, []byte, bool, int8, int16, int32, int64, int, float32, float64, time.Time, dm.DmIntervalDT, dm.DmIntervalYM:
		return value, nil
	default:
		return nil, fmt.Errorf("对象字段类型 %T 无法无损复制", value)
	}
}
func nativeComplexTypeName(sourceOwner, schema, name string) string {
	// Type names obtained from the driver are qualified. Rebind the owner while
	// preserving the last quoted identifier (including escaped quotes).
	parts, _ := nativeSQLTokens(name)
	if len(parts) == 3 && parts[1] == "." {
		owner := nativeTokenName(parts[0])
		if sourceOwner != "" && owner == sourceOwner {
			owner = schema
		}
		return damengnative.Qualified(owner, nativeTokenName(parts[2]))
	}
	if len(parts) == 1 {
		return damengnative.Qualified(schema, nativeTokenName(parts[0]))
	}
	return name
}

func (w *DaMengNativeWriter) CopyDamengNativeBatch(ctx context.Context, table damengnative.Table, rows [][]any) (retErr error) {
	if len(rows) == 0 {
		return nil
	}
	if table.Schema != "" && table.Schema != w.schema {
		return fmt.Errorf("原生数据写入 schema 与目标不一致")
	}
	var columns []damengnative.Column
	var names, placeholders []string
	hasIdentity := false
	for _, c := range table.Columns {
		if !c.Writable {
			continue
		}
		p, err := nativePlaceholder(c)
		if err != nil {
			return err
		}
		columns = append(columns, c)
		names = append(names, damengnative.Quote(c.Name))
		placeholders = append(placeholders, p)
		hasIdentity = hasIdentity || c.Identity
	}
	if len(columns) == 0 {
		return fmt.Errorf("表 %s 没有可写列", table.Name)
	}
	converted := make([][]any, len(rows))
	bytes := 0
	for i, row := range rows {
		if len(row) != len(columns) {
			return fmt.Errorf("表 %s 行列数不匹配", table.Name)
		}
		converted[i] = make([]any, len(row))
		for j, value := range row {
			v, size, err := nativeCopyValue(columns[j], value, w.schema)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", table.Name, columns[j].Name, err)
			}
			converted[i][j] = v
			bytes += size
		}
	}
	if len(rows) > 1 && bytes > NativeBatchBytes {
		return fmt.Errorf("原生写入批次超过 %d 字节，请缩小批次", NativeBatchBytes)
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qualified := damengnative.Qualified(w.schema, table.Name)
	identityEnabled := false
	if hasIdentity {
		defer func() {
			if !identityEnabled {
				return
			}
			_ = tx.Rollback()
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, e := conn.ExecContext(cleanup, "SET IDENTITY_INSERT "+qualified+" OFF"); e != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				retErr = errors.Join(retErr, fmt.Errorf("清理 IDENTITY_INSERT 失败: %w", e))
			}
		}()
		if _, err = tx.ExecContext(ctx, "SET IDENTITY_INSERT "+qualified+" ON"); err != nil {
			return err
		}
		identityEnabled = true
	}
	statement := "INSERT INTO " + qualified + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ")"
	stmt, err := tx.PrepareContext(ctx, statement)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range converted {
		if _, err = stmt.ExecContext(ctx, row...); err != nil {
			return err
		}
	}
	if hasIdentity {
		if _, err = tx.ExecContext(ctx, "SET IDENTITY_INSERT "+qualified+" OFF"); err != nil {
			return err
		}
		identityEnabled = false
	}
	return tx.Commit()
}

func (w *DaMengNativeWriter) VerifyDamengNative(ctx context.Context, expected *damengnative.Snapshot) ([]damengnative.Issue, error) {
	actual, err := damengnative.ReadCatalog(ctx, w.db, w.schema, true)
	if err != nil {
		return nil, err
	}
	issues := []damengnative.Issue{}
	objects := map[string]damengnative.Object{}
	for _, o := range actual.Objects {
		objects[o.Kind+"\x00"+o.Name] = o
	}
	expectedByID := map[string]damengnative.Object{}
	for _, o := range expected.Objects {
		expectedByID[o.ID] = o
	}
	for _, want := range expected.Objects {
		got, ok := objects[want.Kind+"\x00"+want.Name]
		if !ok {
			if want.Managed && (damengnative.NormalizeKind(want.Kind) != "constraint" || want.Generated) {
				if parent, known := expectedByID[want.ParentID]; known {
					if targetParent, present := objects[parent.Kind+"\x00"+parent.Name]; present && !strings.EqualFold(targetParent.Status, "INVALID") && !strings.EqualFold(targetParent.Status, "ERROR") {
						// Generated storage/index names belong to the physical
						// parent; that parent's independent DDL check below is
						// the semantic proof, not its regenerated child name.
						continue
					}
				}
			}
			issues = append(issues, damengnative.Issue{ObjectID: want.ID, Kind: want.Kind, Name: want.Name, Severity: "error", Reason: "目标缺少原生对象"})
			continue
		}
		if strings.EqualFold(got.Status, "INVALID") || strings.EqualFold(got.Status, "ERROR") {
			issues = append(issues, damengnative.Issue{ObjectID: want.ID, Kind: want.Kind, Name: want.Name, Severity: "error", Reason: "目标对象编译状态为 " + got.Status})
		}
		if (strings.EqualFold(want.Status, "DISABLED") || strings.EqualFold(want.Status, "ENABLED")) && got.Status != want.Status {
			issues = append(issues, damengnative.Issue{ObjectID: want.ID, Kind: want.Kind, Name: want.Name, Severity: "error", Reason: "目标对象启停状态与源端不同"})
		}
		if !want.Managed {
			for _, unit := range want.Units {
				if unit.Phase == "restore" || unit.Phase == "compile" || unit.Phase == "drop" {
					continue
				}
				mapped, _, rewriteErr := damengnative.RewriteSchema(unit.SQL, expected.Profile.Schema, w.schema)
				if rewriteErr != nil {
					return issues, rewriteErr
				}
				match := false
				for _, candidate := range got.Units {
					if nativeDefinitionsEqual(mapped, candidate.SQL) {
						match = true
						break
					}
				}
				if !match {
					issues = append(issues, damengnative.Issue{ObjectID: want.ID, Kind: want.Kind, Name: want.Name, Severity: "error", Reason: "目标原生定义与冻结源定义不同"})
					break
				}
			}
		}
	}
	tables := map[string]damengnative.Table{}
	for _, t := range actual.Tables {
		tables[t.Name] = t
	}
	for _, want := range expected.Tables {
		got, ok := tables[want.Name]
		if !ok {
			continue
		}
		if want.Rows >= 0 && got.Rows != want.Rows {
			issues = append(issues, damengnative.Issue{ObjectID: want.ObjectID, Kind: "table", Name: want.Name, Severity: "error", Reason: fmt.Sprintf("行数不同: 源=%d 目标=%d", want.Rows, got.Rows)})
		}
		expectedColumns := append([]damengnative.Column{}, want.Columns...)
		for i := range expectedColumns {
			expectedColumns[i].SQLType, _, err = damengnative.RewriteSchema(expectedColumns[i].SQLType, expected.Profile.Schema, w.schema)
			if err != nil {
				return issues, err
			}
		}
		if !nativeColumnsEqual(expectedColumns, got.Columns) {
			issues = append(issues, damengnative.Issue{ObjectID: want.ObjectID, Kind: "table", Name: want.Name, Severity: "error", Reason: "目标列定义与原生源定义不同"})
		}
	}
	return issues, nil
}

func nativeDefinitionsEqual(a, b string) bool {
	canonical := func(sql string) (string, error) {
		tokens, err := nativeSQLTokens(damengnative.StripClientDelimiter(sql))
		if err != nil {
			return "", err
		}
		for len(tokens) > 0 && tokens[len(tokens)-1] == ";" {
			tokens = tokens[:len(tokens)-1]
		}
		for i, token := range tokens {
			if !strings.HasPrefix(token, "'") && !strings.HasPrefix(token, `"`) && !strings.HasPrefix(strings.ToUpper(token), "Q'") && !strings.HasPrefix(strings.ToUpper(token), "NQ'") {
				tokens[i] = strings.ToUpper(token)
			}
		}
		// Restoring the identity's next value intentionally changes its seed,
		// while its increment and all other table semantics remain fixed.
		for i := 0; i+3 < len(tokens); i++ {
			if tokens[i] == "IDENTITY" && tokens[i+1] == "(" {
				for end := i + 2; end < len(tokens) && tokens[end] != ")"; end++ {
					if tokens[end] == "," {
						tokens = append(tokens[:i+2], append([]string{"<IDENTITY_NEXT>"}, tokens[end:]...)...)
						break
					}
				}
			}
		}
		return strings.Join(tokens, "\x00"), nil
	}
	x, err := canonical(a)
	if err != nil {
		return false
	}
	y, err := canonical(b)
	return err == nil && x == y
}

func nativeColumnsEqual(a, b []damengnative.Column) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Name != y.Name || x.Writable != y.Writable || x.Identity != y.Identity || x.Precision != y.Precision || x.Scale != y.Scale || !strings.EqualFold(x.SQLType, y.SQLType) {
			return false
		}
	}
	return true
}
