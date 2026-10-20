package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/dialect"
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
}

const oracleDMStrictLoadProbeSQL = `SELECT /*+ MY_STRICT_TABLES(7) */ CAST(CAST(? AS VARCHAR(32)) AS INT) FROM DUAL`

// PrepareOracleDamengLoad checks that invalid text cannot become a numeric zero
// or NULL under a permissive compatibility mode. The INSERT uses the same
// statement-scoped strictness hint; no session or instance setting is changed.
func (w *DaMengWriter) PrepareOracleDamengLoad(ctx context.Context) error {
	w.oracleDMMu.Lock()
	defer w.oracleDMMu.Unlock()
	if w.oracleDM.features == nil {
		w.oracleDM.features = map[string]error{}
	}
	const key = "load:strict"
	if err, ok := w.oracleDM.features[key]; ok {
		return err
	}
	var result any
	err := w.db.QueryRowContext(ctx, oracleDMStrictLoadProbeSQL, "dbgold_invalid_number").Scan(&result)
	var dmErr *dm.DmError
	if errors.As(err, &dmErr) && dmErr.ErrCode == -6111 {
		w.oracleDM.features[key] = nil
		return nil
	}
	if err == nil {
		err = fmt.Errorf("非法数值被转换为 %v", result)
	}
	err = fmt.Errorf("达梦严格写入能力探针未返回预期的数据库转换错误，禁止继续写入: %w", err)
	w.oracleDM.features[key] = err
	return err
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
		c.features[key] = err
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
				c.dateChecked = true
				// Failure to read INI merely selects the portable TIMESTAMP(0).
				_ = w.db.QueryRowContext(ctx, `SELECT PARA_VALUE FROM V$DM_INI WHERE PARA_NAME='COMPATIBLE_MODE'`).Scan(&c.profile.Mode)
				c.profile.Mode = strings.TrimSpace(c.profile.Mode)
				if c.profile.Mode == "2" {
					const date = "2024-01-02 03:04:05"
					var got string
					err := w.db.QueryRowContext(ctx, `SELECT TO_CHAR(CAST(TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS') AS DATE), 'YYYY-MM-DD HH24:MI:SS') FROM DUAL`, date).Scan(&got)
					c.profile.UseDate = err == nil && got == date
				}
				if err := ctx.Err(); err != nil {
					return err
				}
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

func (w *DaMengWriter) probeOracleDMNational(ctx context.Context, kind string) error {
	const value = "汉A"
	var got string
	if err := w.db.QueryRowContext(ctx, fmt.Sprintf("SELECT CAST(? AS %s(2 CHAR)) FROM DUAL", kind), value).Scan(&got); err != nil {
		return fmt.Errorf("%s(n CHAR) 语法或多字节字符容量不可用: %w", kind, err)
	}
	if got != value {
		return fmt.Errorf("%s(n CHAR) 无法保留多字节字符（探针得到 %q）", kind, got)
	}
	return nil
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
	if err != nil || len(cols) != 1 {
		return "", fmt.Errorf("无法描述 FLOAT(%d) 实际存储类型: %v", p, err)
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
	if err != nil || len(cols) != 1 {
		return "", fmt.Errorf("无法描述目标列 %s.%s 浮点类型: %v", table, column, err)
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
