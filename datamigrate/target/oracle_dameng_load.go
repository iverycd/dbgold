package target

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// These are the effective target column limits, rather than the Oracle source
// byte length or the behavior of a SELECT CAST expression.
type oracleDMLoadColumn struct {
	kind, units  string
	chars, bytes int64
	charset      int
}

type oracleDMLoadCache struct {
	tables       map[string]map[string]oracleDMLoadColumn
	charset      int
	charsetKnown bool
}

const oracleDMLoadColumnsSQL = `SELECT c.COLUMN_NAME,c.DATA_TYPE,NVL(c.CHAR_LENGTH,0),NVL(c.DATA_LENGTH,0),CASE WHEN c.CHAR_USED='C' OR sc.SCALE IN (7,8) THEN 'C' ELSE NVL(c.CHAR_USED,'B') END FROM ALL_TAB_COLUMNS c JOIN ALL_OBJECTS o ON o.object_name=c.TABLE_NAME AND o.owner=c.OWNER JOIN SYS.SYSCOLUMNS sc ON sc.id=o.object_id AND sc.name=c.COLUMN_NAME WHERE c.OWNER=? AND c.TABLE_NAME=? ORDER BY c.COLUMN_ID`
const oracleDMLoadCharsetSQL = `SELECT SF_GET_UNICODE_FLAG() FROM DUAL`

func (w *DaMengWriter) resetOracleDMLoadColumns() {
	w.oracleDMMu.Lock()
	defer w.oracleDMMu.Unlock()
	if w.oracleDM.load != nil {
		w.oracleDM.load.tables = nil
	}
}

func oracleDMLimitedSource(dt string) bool {
	switch typemap.OracleDamengBaseType(dt) {
	case "CHAR", "CHARACTER", "VARCHAR", "VARCHAR2", "CHAR VARYING", "CHARACTER VARYING", "NCHAR", "NVARCHAR", "NVARCHAR2", "NATIONAL CHAR", "NATIONAL CHARACTER", "NATIONAL CHAR VARYING", "NATIONAL CHARACTER VARYING", "NCHAR VARYING", "RAW", "ROWID", "UROWID":
		return true
	}
	return false
}

func oracleDMLoadTextSource(dt string) bool {
	base := typemap.OracleDamengBaseType(dt)
	switch base {
	case "RAW", "ROWID", "UROWID":
		return false
	case "CLOB", "NCLOB", "LONG", "BFILE", "XMLTYPE", "SYS.XMLTYPE", "SDE.ST_GEOMETRY", "MDSYS.SDO_GEOMETRY", "SDO_GEOMETRY":
		return true
	}
	return oracleDMLimitedSource(dt) || strings.HasPrefix(base, "SYSGEO.ST_")
}

func (w *DaMengWriter) prepareOracleDMLoadColumns(ctx context.Context, table string, cols, colTypes []string) ([]oracleDMLoadColumn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limited, text := false, false
	for _, dt := range colTypes {
		limited = limited || oracleDMLimitedSource(dt)
		text = text || oracleDMLoadTextSource(dt)
	}
	result := make([]oracleDMLoadColumn, len(cols))
	if !limited && !text {
		return result, nil
	}
	w.oracleDMMu.Lock()
	defer w.oracleDMMu.Unlock()
	if w.oracleDM.load == nil {
		w.oracleDM.load = &oracleDMLoadCache{}
	}
	cache := w.oracleDM.load
	tableKey := strings.ToUpper(table)
	actual := cache.tables[tableKey]
	if limited && actual == nil {
		rows, err := w.db.QueryContext(ctx, oracleDMLoadColumnsSQL, w.schema, tableKey)
		if err != nil {
			return nil, fmt.Errorf("读取目标表 %s 写入容量: %w", table, err)
		}
		defer rows.Close()
		actual = map[string]oracleDMLoadColumn{}
		for rows.Next() {
			var name string
			var col oracleDMLoadColumn
			if err := rows.Scan(&name, &col.kind, &col.chars, &col.bytes, &col.units); err != nil {
				return nil, fmt.Errorf("读取目标表 %s 写入容量: %w", table, err)
			}
			actual[strings.ToUpper(name)] = col
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("读取目标表 %s 写入容量: %w", table, err)
		}
		// A migration pool may have a single connection. Close the catalog cursor
		// before asking for the database character set.
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if cache.tables == nil {
			cache.tables = map[string]map[string]oracleDMLoadColumn{}
		}
		cache.tables[tableKey] = actual
	}
	if text && !cache.charsetKnown {
		var charset int
		if err := w.db.QueryRowContext(ctx, oracleDMLoadCharsetSQL).Scan(&charset); err != nil {
			return nil, fmt.Errorf("读取达梦写入字符集: %w", err)
		}
		if charset < 0 || charset > 2 {
			return nil, fmt.Errorf("不支持的达梦写入字符集 %d", charset)
		}
		cache.charset, cache.charsetKnown = charset, true
	}
	for i, name := range cols {
		col := actual[strings.ToUpper(name)]
		if oracleDMLimitedSource(colTypes[i]) && col.kind == "" {
			return nil, fmt.Errorf("目标列 %s.%s 缺少写入容量元数据", table, name)
		}
		col.charset = cache.charset
		result[i] = col
	}
	return result, nil
}

func oracleDMEncodedBytes(value string, charset int) (int64, error) {
	if !utf8.ValidString(value) {
		return 0, fmt.Errorf("字符串包含无效 UTF-8，禁止静默替换")
	}
	if charset == 1 {
		return int64(len(value)), nil
	}
	var codec encoding.Encoding
	switch charset {
	case 0:
		codec = simplifiedchinese.GB18030
	case 2:
		codec = korean.EUCKR
	default:
		return 0, fmt.Errorf("无法确认达梦写入字符集 %d", charset)
	}
	encoded, err := codec.NewEncoder().Bytes([]byte(value))
	if err != nil {
		return 0, fmt.Errorf("字符串无法按目标字符集无损编码: %w", err)
	}
	decoded, err := codec.NewDecoder().Bytes(encoded)
	if err != nil || string(decoded) != value {
		return 0, fmt.Errorf("字符串无法按目标字符集无损往返")
	}
	return int64(len(encoded)), nil
}

func validateOracleDMLoadValue(col oracleDMLoadColumn, dt string, original, converted any) error {
	if converted == nil {
		return nil
	}
	if oracleDMLoadTextSource(dt) {
		value, ok := converted.(string)
		if !ok {
			// CLOB/XML values are native driver LOBs after conversion. Validate
			// their original text without imposing a finite VARCHAR limit.
			switch raw := original.(type) {
			case string:
				value = raw
			case []byte:
				value = string(raw)
			default:
				return fmt.Errorf("缺少可验证的字符写入值")
			}
		}
		bytes, err := oracleDMEncodedBytes(value, col.charset)
		if err != nil {
			return err
		}
		if !oracleDMLimitedSource(dt) {
			return nil
		}
		family := oracleDMTypeFamily(col.kind)
		if family != "CHAR" && family != "VARCHAR" {
			return fmt.Errorf("目标类型 %s 不是可验证的字符列", col.kind)
		}
		if col.bytes <= 0 {
			return fmt.Errorf("无法确认目标列字节容量")
		}
		if bytes > col.bytes {
			return fmt.Errorf("编码后 %d 字节超过目标列 %d 字节容量，禁止截断", bytes, col.bytes)
		}
		if strings.EqualFold(col.units, "C") {
			if col.chars <= 0 {
				return fmt.Errorf("无法确认目标列字符容量")
			}
			if chars := int64(utf8.RuneCountInString(value)); chars > col.chars {
				return fmt.Errorf("%d 个字符超过目标列 %d 字符容量，禁止截断", chars, col.chars)
			}
		}
		return nil
	}
	if oracleDMLimitedSource(dt) {
		value, ok := converted.([]byte)
		if !ok {
			return fmt.Errorf("缺少可验证的二进制写入值")
		}
		family := oracleDMTypeFamily(col.kind)
		if family != "BINARY" && family != "VARBINARY" || col.bytes <= 0 {
			return fmt.Errorf("无法确认目标二进制列容量（%s）", col.kind)
		}
		if int64(len(value)) > col.bytes {
			return fmt.Errorf("%d 字节超过目标列 %d 字节容量，禁止截断", len(value), col.bytes)
		}
	}
	return nil
}

func (w *DaMengWriter) prepareOracleDMLoadPage(ctx context.Context, table string, cols, colTypes []string, rows [][]any, floatKinds []string) ([][]any, error) {
	limits, err := w.prepareOracleDMLoadColumns(ctx, table, cols, colTypes)
	if err != nil {
		return nil, err
	}
	converted := make([][]any, len(rows))
	for rowIndex, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(row) != len(cols) {
			return nil, fmt.Errorf("表 %s 行列数不匹配", table)
		}
		converted[rowIndex] = make([]any, len(row))
		for i, value := range row {
			var result any
			if kind := floatKinds[i]; kind != "" {
				bits := 64
				if kind == "real" {
					bits = 32
				}
				result, err = valueconv.OracleDamengFloat(value, colTypes[i], kind == "decimal", bits)
			} else {
				result, err = valueconv.OracleDameng(value, colTypes[i])
			}
			if err == nil {
				err = validateOracleDMLoadValue(limits[i], colTypes[i], value, result)
			}
			if err != nil {
				return nil, fmt.Errorf("%s.%s 第 %d 行: %w", table, cols[i], rowIndex+1, err)
			}
			converted[rowIndex][i] = result
		}
	}
	return converted, nil
}
