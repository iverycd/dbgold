package target

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/failure"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"gitee.com/chunanyong/dm"
)

// OracleDamengProfile describes observed capabilities of this connection. An
// empty Mode is allowed: catalog access is not needed for ordinary columns.
type OracleDamengProfile struct {
	Mode       string
	UseDate    bool
	FloatKinds map[int64]string
}

type oracleDMCapabilityCache struct {
	profile      OracleDamengProfile
	dateChecked  bool
	features     map[string]error
	floatColumns map[string]string
	floatActual  map[string]bool
	load         *oracleDMLoadCache
}

// OracleDamengCapabilityError distinguishes a column-specific unsupported path
// from a failed control that affects every string using this connection.
// Callers may inspect OracleDamengCapabilityScope through errors.As.
type OracleDamengCapabilityError struct {
	Scope string
	Stage string
	Err   error
}

func (e *OracleDamengCapabilityError) Error() string                       { return e.Stage + ": " + e.Err.Error() }
func (e *OracleDamengCapabilityError) Unwrap() error                       { return e.Err }
func (e *OracleDamengCapabilityError) OracleDamengCapabilityScope() string { return e.Scope }

func oracleDMTaskFailure(err error) bool {
	return failure.IsTaskError(err)
}

// PrepareOracleDamengLoad remains for older callers. A SELECT CAST is not an
// INSERT assignment: mode 4 can convert invalid text to zero in the former,
// regardless of the statement hint. CopyData validates values and actual target
// column capacities instead of treating that expression as an admission gate.
func (w *DaMengWriter) PrepareOracleDamengLoad(ctx context.Context) error {
	return ctx.Err()
}

func oracleDMColumnKey(table, column string) string {
	return strings.ToUpper(table) + "\x00" + strings.ToUpper(column)
}

func (w *DaMengWriter) OracleDamengProfile() OracleDamengProfile {
	w.oracleDMMu.RLock()
	defer w.oracleDMMu.RUnlock()
	p := w.oracleDM.profile
	p.FloatKinds = make(map[int64]string, len(w.oracleDM.profile.FloatKinds))
	for key, kind := range w.oracleDM.profile.FloatKinds {
		p.FloatKinds[key] = kind
	}
	return p
}

func (w *DaMengWriter) OracleDamengUsesDate() bool {
	w.oracleDMMu.RLock()
	defer w.oracleDMMu.RUnlock()
	return w.oracleDM.profile.UseDate
}

// PrepareOracleDameng performs read-only, parameterized checks before any table
// is changed. COMPATIBLE_MODE is neither a prerequisite nor modified here.
func (w *DaMengWriter) PrepareOracleDameng(ctx context.Context, infos []*source.TableDDLInfo, opt dialect.TypeOpt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.oracleDMMu.Lock()
	defer w.oracleDMMu.Unlock()
	c := &w.oracleDM
	if c.features == nil {
		c.features = map[string]error{}
	}
	if c.floatColumns == nil {
		c.floatColumns = map[string]string{}
	}
	if c.profile.FloatKinds == nil {
		c.profile.FloatKinds = map[int64]string{}
	}
	check := func(key string, probe func() error) error {
		if err, ok := c.features[key]; ok {
			return err
		}
		err := probe()
		// A connection may recover on a later attempt. Never cache a transport,
		// cancellation, or shared-control failure as a missing column capability.
		if !oracleDMTaskFailure(err) {
			c.features[key] = err
		}
		return err
	}
	for _, info := range infos {
		if info == nil {
			continue
		}
		for _, col := range info.Columns {
			fail := func(err error) error {
				return fmt.Errorf("达梦目标列 %s.%s (%s) 能力不足: %w", info.TableName, col.Name, col.DataType, err)
			}
			base := typemap.OracleDamengBaseType(source.OracleDamengSourceType(col))
			if base == "DATE" && !c.dateChecked {
				// Failure to read INI merely selects the portable TIMESTAMP(0).
				c.profile.Mode, c.profile.UseDate = "", false
				modeErr := w.db.QueryRowContext(ctx, `SELECT PARA_VALUE FROM V$DM_INI WHERE PARA_NAME='COMPATIBLE_MODE'`).Scan(&c.profile.Mode)
				if oracleDMTaskFailure(modeErr) {
					return modeErr
				}
				c.profile.Mode = strings.TrimSpace(c.profile.Mode)
				if c.profile.Mode == "2" {
					const date = "2024-01-02 03:04:05"
					var got string
					err := w.db.QueryRowContext(ctx, `SELECT TO_CHAR(CAST(TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS') AS DATE), 'YYYY-MM-DD HH24:MI:SS') FROM DUAL`, date).Scan(&got)
					if oracleDMTaskFailure(err) {
						return err
					}
					c.profile.UseDate = err == nil && got == date
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				c.dateChecked = true
			}
			if strings.HasPrefix(base, "TIMESTAMP") && typemap.OracleDamengTimestampPrecision(col) > 6 {
				p := typemap.OracleDamengTimestampPrecision(col)
				zoned := strings.Contains(base, "TIME ZONE")
				key := fmt.Sprintf("timestamp:%t:%d", zoned, p)
				if err := check(key, func() error { return w.probeOracleDMTimestamp(ctx, p, zoned) }); err != nil {
					return fail(err)
				}
			}
			mapped := typemap.OracleToDameng(col, opt.CharInLength, opt.UseNvarchar2)
			mappedBase := dmBaseType(mapped)
			if mappedBase == "NCHAR" || mappedBase == "NVARCHAR" || mappedBase == "NVARCHAR2" {
				if err := check("national:control", func() error { return w.probeOracleDMStringControl(ctx) }); err != nil {
					return fail(err)
				}
				if err := check("national:"+mappedBase, func() error { return w.probeOracleDMNational(ctx, mappedBase) }); err != nil {
					return fail(err)
				}
			}
			if oracleDMFloatingSource(base) {
				p := OracleDamengFloatPrecision(mapped, 0)
				// BINARY_DOUBLE maps to the native DOUBLE name, unaffected by NUMBER_MODE.
				if base == "BINARY_DOUBLE" {
					key := oracleDMColumnKey(info.TableName, col.Name)
					if !c.floatActual[key] {
						c.floatColumns[key] = "double"
					}
					continue
				}
				if err := check(fmt.Sprintf("float:%d", p), func() error {
					kind, err := w.probeOracleDMFloat(ctx, p)
					if err == nil {
						c.profile.FloatKinds[p] = kind
					}
					return err
				}); err != nil {
					return fail(err)
				}
				key := oracleDMColumnKey(info.TableName, col.Name)
				if !c.floatActual[key] {
					c.floatColumns[key] = c.profile.FloatKinds[p]
				}
			}
			switch {
			case base == "XMLTYPE" || base == "SYS.XMLTYPE":
				if err := check("xmltype", func() error { return w.probeOracleDMXML(ctx) }); err != nil {
					return fail(err)
				}
			case base == "BFILE":
				if err := check("bfile", func() error {
					rows, err := w.db.QueryContext(ctx, `SELECT BFILENAME(?,?) FROM DUAL WHERE 1=0`, nil, nil)
					if err != nil {
						return fmt.Errorf("BFILE/BFILENAME 构造器不可用: %w", err)
					}
					return rows.Close()
				}); err != nil {
					return fail(err)
				}
			case strings.HasPrefix(mappedBase, "SYSGEO2.ST_"):
				if mappedBase != "SYSGEO2.ST_GEOMETRY" {
					return fail(fmt.Errorf("尚无 %s 的已验证原生写入编解码器", mappedBase))
				}
				if err := check("spatial:geometry", func() error { return w.probeOracleDMSpatial(ctx) }); err != nil {
					return fail(err)
				}
			}
		}
	}
	return nil
}

func (w *DaMengWriter) probeOracleDMTimestamp(ctx context.Context, p int64, zoned bool) error {
	if p < 7 || p > 9 {
		return fmt.Errorf("时间精度 %d 不在支持范围", p)
	}
	value := "2024-01-02 03:04:05." + "123456789"[:p] + strings.Repeat("0", 9-int(p))
	mask, parse, suffix := "YYYY-MM-DD HH24:MI:SS.FF9", "TO_TIMESTAMP", ""
	if zoned {
		value += " +05:45"
		mask += " TZH:TZM"
		parse, suffix = "TO_TIMESTAMP_TZ", " WITH TIME ZONE"
	}
	query := fmt.Sprintf("SELECT TO_CHAR(CAST(%s(?, '%s') AS TIMESTAMP(%d)%s), '%s') FROM DUAL", parse, mask, p, suffix, mask)
	var got string
	if err := w.db.QueryRowContext(ctx, query, value).Scan(&got); err != nil {
		return fmt.Errorf("TIMESTAMP(%d)%s 精确写入探针失败: %w", p, suffix, err)
	}
	if got != value {
		return fmt.Errorf("TIMESTAMP(%d)%s 会改变小数秒或时区（探针得到 %q）", p, suffix, got)
	}
	return nil
}

func (w *DaMengWriter) probeOracleDMLocalTimestamp(ctx context.Context, p int64) error {
	if p < 0 || p > 9 {
		return fmt.Errorf("本地时区时间精度 %d 不在支持范围", p)
	}
	fraction := "123456789"[:p] + strings.Repeat("0", 9-int(p))
	value := "2024-01-02 03:04:05." + fraction + " +05:45"
	expect := "2024-01-01 21:19:05." + fraction + " +00:00"
	const mask = "YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM"
	query := fmt.Sprintf("SELECT TO_CHAR((CAST(TO_TIMESTAMP_TZ(?, '%s') AS TIMESTAMP(%d) WITH LOCAL TIME ZONE) AT TIME ZONE '+00:00'), '%s') FROM DUAL", mask, p, mask)
	var got string
	if err := w.db.QueryRowContext(ctx, query, value).Scan(&got); err != nil {
		return fmt.Errorf("已有 LOCAL TIME ZONE 列的 UTC 写入探针失败: %w", err)
	}
	if got != expect {
		return fmt.Errorf("已有 LOCAL TIME ZONE 列无法保留 UTC 时间或精度")
	}
	return nil
}

const oracleDMNationalSentinel = "汉A"

// Canonicalizing both operands to the same VARCHAR type makes DUMP's type and
// encoding prefix identical. Its byte list then catches case changes, padding,
// and replacement characters even when the database collation ignores them.
const oracleDMStringExact = `LENGTH(P.V)=2 AND DUMP(CAST(P.V AS VARCHAR(32 CHAR)),16)=DUMP(CAST('汉A' AS VARCHAR(32 CHAR)),16)`

const oracleDMStringControlSQL = `SELECT CASE WHEN ` + oracleDMStringExact + ` THEN 1 ELSE 0 END FROM (SELECT CAST(? AS VARCHAR(32 CHAR)) AS V FROM DUAL) P`
const oracleDMStringReadControlSQL = `SELECT CAST('汉A' AS VARCHAR(32 CHAR)) FROM DUAL`

func (w *DaMengWriter) probeOracleDMStringControl(ctx context.Context) error {
	var exact int
	if err := w.db.QueryRowContext(ctx, oracleDMStringControlSQL, oracleDMNationalSentinel).Scan(&exact); err != nil {
		if oracleDMTaskFailure(err) {
			return err
		}
		return &OracleDamengCapabilityError{Scope: "column", Stage: "普通字符串控制探针不可用", Err: err}
	}
	if exact != 1 {
		return &OracleDamengCapabilityError{Scope: "task", Stage: "普通字符串绑定控制失败", Err: fmt.Errorf("数据库侧长度/字节校验未通过，不能安全验证国家字符写入")}
	}
	// Read a fixed VARCHAR literal separately. A parameterized string result
	// descriptor may be the faulty path itself, whereas the server has already
	// established that the bound value has exactly the same bytes as this literal.
	var got sql.NullString
	if err := w.db.QueryRowContext(ctx, oracleDMStringReadControlSQL).Scan(&got); err != nil {
		if oracleDMTaskFailure(err) {
			return err
		}
		return &OracleDamengCapabilityError{Scope: "column", Stage: "普通字符串回读控制探针不可用", Err: err}
	}
	if !got.Valid || got.String != oracleDMNationalSentinel {
		return &OracleDamengCapabilityError{Scope: "task", Stage: "普通字符串结果读取控制失败", Err: fmt.Errorf("数据库侧字节校验通过，但驱动回读得到 %q（NULL=%t）", got.String, !got.Valid)}
	}
	return nil
}

func oracleDMNationalProbeSQL(kind string, literal bool) string {
	value := "?"
	if literal {
		value = "'汉A'"
	}
	exact := oracleDMStringExact
	if kind == "NCHAR" {
		// Fixed-width expressions pad to their byte buffer in mode 4. The
		// sentinel has no trailing spaces; remove only this expected padding.
		exact = strings.ReplaceAll(exact, "P.V", "RTRIM(P.V)")
	}
	// On DM8 mode 4 with LENGTH_IN_CHAR=0, even CAST(... AS NVARCHAR(2
	// CHAR)) limits this expression to two bytes, yielding an empty value for
	// the four-byte UTF-8 sentinel. A wide buffer checks syntax and binding;
	// it deliberately makes no claim about a real column's CHAR capacity.
	return fmt.Sprintf("SELECT CASE WHEN %s THEN 1 ELSE 0 END FROM (SELECT CAST(%s AS %s(32 CHAR)) AS V FROM DUAL) P", exact, value, kind)
}

func (w *DaMengWriter) probeOracleDMNational(ctx context.Context, kind string) error {
	switch kind {
	case "NCHAR", "NVARCHAR", "NVARCHAR2":
	default:
		return &OracleDamengCapabilityError{Scope: "column", Stage: "国家字符能力检查", Err: fmt.Errorf("未知类型 %q", kind)}
	}
	// Keep the direct parameter-to-national-type path used by the old probe.
	// Read a server-computed integer instead of the national string descriptor:
	// a client-side empty result can no longer masquerade as server data loss.
	var exact int
	if err := w.db.QueryRowContext(ctx, oracleDMNationalProbeSQL(kind, false), oracleDMNationalSentinel).Scan(&exact); err != nil {
		if oracleDMTaskFailure(err) {
			return err
		}
		return &OracleDamengCapabilityError{Scope: "column", Stage: "国家字符参数化探针不可用", Err: fmt.Errorf("无法验证 %s(n CHAR) 的直接绑定路径；不代表实际列无法建表: %w", kind, err)}
	}
	if exact == 1 {
		return nil
	}
	// A literal is diagnostic only. Its success must never substitute for a
	// failed bound value, because the migration writes parameters, not literals.
	var literalExact int
	if err := w.db.QueryRowContext(ctx, oracleDMNationalProbeSQL(kind, true)).Scan(&literalExact); err != nil {
		if oracleDMTaskFailure(err) {
			return err
		}
		return &OracleDamengCapabilityError{Scope: "column", Stage: "国家字符绑定校验失败", Err: fmt.Errorf("%s(n CHAR) 参数化转换未保留完整字节；补充字面量诊断不可用: %w", kind, err)}
	}
	if literalExact == 1 {
		return &OracleDamengCapabilityError{Scope: "column", Stage: "国家字符绑定校验失败", Err: fmt.Errorf("%s(n CHAR) 字面量转换正常，但直接参数绑定的长度/字节校验失败；尚不能确认该列的安全写入路径", kind)}
	}
	return &OracleDamengCapabilityError{Scope: "column", Stage: "国家字符转换校验失败", Err: fmt.Errorf("%s(n CHAR) 的参数化及字面量转换均未保留完整字节；尚不能确认该列的安全写入路径", kind)}
}

// OracleDamengFloatPrecision converts Oracle's synonyms to the FLOAT precision
// rendered by the target mapper. It does not infer an IEEE precision from the name.
func OracleDamengFloatPrecision(dataType string, precision int64) int64 {
	if precision > 0 {
		return precision
	}
	if m := oracleDMFloatSize.FindStringSubmatch(dataType); len(m) > 1 {
		if p, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return p
		}
	}
	if typemap.OracleDamengBaseType(dataType) == "REAL" {
		return 63
	}
	return 126
}

var oracleDMFloatSize = regexp.MustCompile(`(?i)FLOAT\s*\(\s*([0-9]+)\s*\)`)

func oracleDMFloatingSource(base string) bool {
	switch base {
	case "FLOAT", "REAL", "DOUBLE PRECISION", "BINARY_FLOAT", "BINARY_DOUBLE":
		return true
	}
	return false
}

func oracleDMDecimalMetadata(c *sql.ColumnType) bool {
	_, _, ok := c.DecimalSize()
	return ok || dmBaseType(c.DatabaseTypeName()) == "NUMBER"
}

func (w *DaMengWriter) probeOracleDMFloat(ctx context.Context, p int64) (string, error) {
	if p < 1 || p > 126 {
		return "", fmt.Errorf("FLOAT 精度 %d 不在 1 到 126 之间", p)
	}
	rows, err := w.db.QueryContext(ctx, fmt.Sprintf("SELECT CAST(NULL AS FLOAT(%d)) FROM DUAL", p))
	if err != nil {
		return "", fmt.Errorf("无法探测 FLOAT(%d) 实际存储类型: %w", p, err)
	}
	defer rows.Close()
	cols, err := rows.ColumnTypes()
	if err != nil {
		return "", fmt.Errorf("无法描述 FLOAT(%d) 实际存储类型: %w", p, err)
	}
	if len(cols) != 1 {
		return "", fmt.Errorf("无法描述 FLOAT(%d) 实际存储类型：返回列数为 %d", p, len(cols))
	}
	if oracleDMDecimalMetadata(cols[0]) {
		return "decimal", nil
	}
	switch dmBaseType(cols[0].DatabaseTypeName()) {
	case "REAL":
		return "real", nil
	case "DOUBLE":
		return "double", nil
	case "FLOAT":
		// The DM driver misreports FLOAT-backed DOUBLE ScanType as float32.
		// Native FLOAT's declared binary precision is the reliable discriminator.
		if p <= 24 {
			return "real", nil
		}
		return "double", nil
	}
	return "", fmt.Errorf("FLOAT(%d) 返回未知实际类型 %q", p, cols[0].DatabaseTypeName())
}

func (w *DaMengWriter) probeOracleDMXML(ctx context.Context) error {
	const value = `<r xmlns="urn:dbgold"><v>汉A</v></r>`
	var got string
	err := w.db.QueryRowContext(ctx, `SELECT CAST(XMLSERIALIZE(CONTENT XMLTYPE(CAST(? AS CLOB)) AS CLOB) AS VARCHAR(2048 CHAR)) FROM DUAL`, dm.NewClob(value)).Scan(&got)
	if err != nil {
		return fmt.Errorf("XMLTYPE 原生 CLOB 构造/序列化不可用: %w", err)
	}
	// Serialization may add harmless indentation. Compare structure and text.
	if oracleDMCompactXML(got) != oracleDMCompactXML(value) {
		return fmt.Errorf("XMLTYPE 构造/序列化改变了 XML 内容")
	}
	return nil
}

var oracleDMXMLWhitespace = regexp.MustCompile(`>\s+<`)

func oracleDMCompactXML(value string) string {
	return oracleDMXMLWhitespace.ReplaceAllString(strings.TrimSpace(value), "><")
}

var oracleDMPoint = regexp.MustCompile(`(?i)^\s*POINT\s*\(\s*([^\s]+)\s+([^\s)]+)\s*\)\s*$`)

func (w *DaMengWriter) probeOracleDMSpatial(ctx context.Context) error {
	const value = "SRID=4326;POINT (1.25 -2.5)"
	var count int
	if err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TYPES WHERE OWNER=? AND TYPE_NAME=?`, "SYSGEO2", "ST_GEOMETRY").Scan(&count); err != nil {
		return fmt.Errorf("无法验证原生 SYSGEO2.ST_GEOMETRY 类型: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("原生 SYSGEO2.ST_GEOMETRY 类型不可用；不会自动初始化空间扩展")
	}
	var text string
	var srid int64
	err := w.db.QueryRowContext(ctx, `SELECT DMGEO2.ST_ASTEXT(DMGEO2.ST_GEOMFROMEWKT(?)),DMGEO2.ST_SRID(DMGEO2.ST_GEOMFROMEWKT(?)) FROM DUAL`, value, value).Scan(&text, &srid)
	if err != nil {
		return fmt.Errorf("SYSGEO2/DMGEO2 原生 EWKT 构造或 SRID 读取不可用: %w", err)
	}
	m := oracleDMPoint.FindStringSubmatch(text)
	if len(m) != 3 || srid != 4326 {
		return fmt.Errorf("DMGEO2 EWKT 往返改变了几何类型或 SRID")
	}
	x, e1 := strconv.ParseFloat(m[1], 64)
	y, e2 := strconv.ParseFloat(m[2], 64)
	if e1 != nil || e2 != nil || x != 1.25 || y != -2.5 {
		return fmt.Errorf("DMGEO2 EWKT 往返改变了二维坐标")
	}
	return nil
}

// OracleDamengColumnFloatKind returns the actual target binding family. Existing
// tables override the planned family, including manually created data-only tables.
func (w *DaMengWriter) OracleDamengColumnFloatKind(ctx context.Context, table, column string) (string, error) {
	key := oracleDMColumnKey(table, column)
	w.oracleDMMu.RLock()
	kind := w.oracleDM.floatColumns[key]
	w.oracleDMMu.RUnlock()
	if kind != "" {
		return kind, nil
	}
	kind, err := w.inspectOracleDMColumnFloat(ctx, table, column)
	if err != nil {
		return "", err
	}
	w.cacheOracleDMFloatColumn(table, column, kind)
	return kind, nil
}

func (w *DaMengWriter) cacheOracleDMFloatColumn(table, column, kind string) {
	w.oracleDMMu.Lock()
	defer w.oracleDMMu.Unlock()
	if w.oracleDM.floatColumns == nil {
		w.oracleDM.floatColumns = map[string]string{}
	}
	if w.oracleDM.floatActual == nil {
		w.oracleDM.floatActual = map[string]bool{}
	}
	w.oracleDM.floatColumns[oracleDMColumnKey(table, column)] = kind
	w.oracleDM.floatActual[oracleDMColumnKey(table, column)] = true
}

func (w *DaMengWriter) inspectOracleDMColumnFloat(ctx context.Context, table, column string) (string, error) {
	rows, err := w.db.QueryContext(ctx, "SELECT "+w.dia.QuoteIdent(column)+" FROM "+w.qualifiedTable(table)+" WHERE 1=0")
	if err != nil {
		return "", fmt.Errorf("读取目标列 %s.%s 实际浮点类型: %w", table, column, err)
	}
	cols, err := rows.ColumnTypes()
	closeErr := rows.Close()
	if err != nil {
		return "", fmt.Errorf("无法描述目标列 %s.%s 浮点类型: %w", table, column, err)
	}
	if len(cols) != 1 {
		return "", fmt.Errorf("无法描述目标列 %s.%s 浮点类型：返回列数为 %d", table, column, len(cols))
	}
	if closeErr != nil {
		return "", closeErr
	}
	if oracleDMDecimalMetadata(cols[0]) {
		return "decimal", nil
	}
	switch dmBaseType(cols[0].DatabaseTypeName()) {
	case "REAL":
		return "real", nil
	case "DOUBLE":
		return "double", nil
	case "FLOAT":
		var precision int64
		if err := w.db.QueryRowContext(ctx, `SELECT NVL(DATA_PRECISION,0) FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? AND COLUMN_NAME=?`, w.schema, strings.ToUpper(table), strings.ToUpper(column)).Scan(&precision); err != nil {
			return "", err
		}
		if precision > 0 && precision <= 24 {
			return "real", nil
		}
		return "double", nil
	}
	return "", fmt.Errorf("目标列 %s.%s 实际类型 %s 不是可支持的数值类型", table, column, cols[0].DatabaseTypeName())
}
