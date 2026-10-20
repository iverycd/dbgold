package target

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
)

// ValidateOracleDamengCapabilities retains the optional preflight interface used
// by older callers. Preparation probes only features used by the selected columns.
func (w *DaMengWriter) ValidateOracleDamengCapabilities(ctx context.Context, infos []*source.TableDDLInfo) error {
	return w.PrepareOracleDameng(ctx, infos, dialect.TypeOpt{})
}

// ValidateOracleDamengExisting checks capacity as well as names in data/object
// modes. Alias spellings are accepted only when the effective family and
// capacity preserve the source values.
func (w *DaMengWriter) ValidateOracleDamengExisting(ctx context.Context, info *source.TableDDLInfo, opt dialect.TypeOpt) error {
	rows, err := w.db.QueryContext(ctx, `SELECT c.COLUMN_NAME,c.DATA_TYPE,NVL(c.CHAR_LENGTH,0),NVL(c.DATA_LENGTH,0),NVL(c.DATA_PRECISION,0),NVL(c.DATA_SCALE,0),c.NULLABLE,CASE WHEN c.CHAR_USED='C' OR sc.SCALE IN (7,8) THEN 'C' ELSE NVL(c.CHAR_USED,'B') END,CASE WHEN sc.INFO2 & 0x01=0x01 THEN 1 ELSE 0 END FROM ALL_TAB_COLUMNS c JOIN ALL_OBJECTS o ON o.object_name=c.TABLE_NAME AND o.owner=c.OWNER JOIN SYS.SYSCOLUMNS sc ON sc.id=o.object_id AND sc.name=c.COLUMN_NAME WHERE c.OWNER=? AND c.TABLE_NAME=? ORDER BY c.COLUMN_ID`, w.schema, strings.ToUpper(info.TableName))
	if err != nil {
		return err
	}
	defer rows.Close()
	type targetColumn struct {
		name, kind                     string
		chars, bytes, precision, scale int64
		nullable, units                string
		identity                       int
	}
	actual := map[string]targetColumn{}
	for rows.Next() {
		var c targetColumn
		if err := rows.Scan(&c.name, &c.kind, &c.chars, &c.bytes, &c.precision, &c.scale, &c.nullable, &c.units, &c.identity); err != nil {
			return err
		}
		actual[c.name] = c
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Release the catalog cursor before feature-specific metadata queries. Some
	// migration pools deliberately use a single connection.
	if err := rows.Close(); err != nil {
		return err
	}
	if len(actual) != len(info.Columns) {
		return fmt.Errorf("目标表 %s 不存在或列数不一致", info.TableName)
	}
	for _, src := range info.Columns {
		if err := typemap.ValidateOracleDamengColumn(src); err != nil {
			return err
		}
		c, ok := actual[strings.ToUpper(src.Name)]
		if !ok {
			return fmt.Errorf("目标缺少列 %s.%s", info.TableName, src.Name)
		}
		fail := func(reason string) error { return fmt.Errorf("目标列 %s.%s %s", info.TableName, src.Name, reason) }
		want := typemap.OracleToDameng(src, opt.CharInLength, opt.UseNvarchar2)
		base, actualBase := oracleDMTypeFamily(want), oracleDMTypeFamily(c.kind)
		srcBase := typemap.OracleDamengBaseType(source.OracleDamengSourceType(src))
		isFloat := oracleDMFloatingSource(srcBase)
		if srcBase == "DATE" && actualBase == "DATE" {
			if !w.OracleDamengUsesDate() {
				return fail("DATE 无法保证完整时分秒，所需类型为 TIMESTAMP(0)")
			}
			base = "DATE"
		}
		if srcBase == "TIMESTAMP WITH LOCAL TIME ZONE" && actualBase == "TIMESTAMP WITH LOCAL TIME ZONE" {
			if err := w.validateOracleDMLocalTimestamp(ctx, typemap.OracleDamengTimestampPrecision(src)); err != nil {
				return fmt.Errorf("目标列 %s.%s: %w", info.TableName, src.Name, err)
			}
			base = actualBase
		}
		if isFloat {
			switch actualBase {
			case "NUMBER", "FLOAT", "REAL", "DOUBLE":
				// FLOAT is intentionally allowed to round using its actual family.
			default:
				return fail(fmt.Sprintf("类型 %s 不是浮点或十进制数值类型", c.kind))
			}
			if actualBase == "NUMBER" && c.precision > 0 {
				return fail("浮点值需要无固定标度的 NUMBER，禁止按整数/定点数截断")
			}
			kind, err := w.inspectOracleDMColumnFloat(ctx, info.TableName, src.Name)
			if err != nil {
				return fmt.Errorf("目标列 %s.%s: %w", info.TableName, src.Name, err)
			}
			w.cacheOracleDMFloatColumn(info.TableName, src.Name, kind)
			base = actualBase
		} else if base == "XMLTYPE" || strings.HasPrefix(base, "SYSGEO2.ST_") {
			if err := w.validateOracleDMSpecialColumn(ctx, info.TableName, src.Name, base); err != nil {
				return fmt.Errorf("目标列 %s.%s: %w", info.TableName, src.Name, err)
			}
		} else if base == "BFILE" && actualBase == "VARCHAR" && c.bytes == 512 && c.scale == 6 {
			// DM represents BFILE internally as a masked VARCHAR(512).
			actualBase = "BFILE"
		}
		if !isFloat && base != actualBase && base != "XMLTYPE" && !strings.HasPrefix(base, "SYSGEO2.ST_") {
			return fail(fmt.Sprintf("类型 %s 与所需 %s 不匹配", c.kind, want))
		}
		if src.IsNullable && c.nullable != "Y" {
			return fail("不允许 NULL")
		}
		if c.identity != 0 {
			return fail("含 IDENTITY 属性，不能保留 Oracle 显式值写入语义")
		}
		switch base {
		case "CHAR", "VARCHAR":
			required, capacity := src.DataLength, c.bytes
			charUnits := strings.Contains(strings.ToUpper(want), " CHAR)")
			if charUnits {
				required, capacity = src.Length, c.chars
				if !strings.EqualFold(c.units, "C") {
					return fail("字符长度单位不足")
				}
			} else if required == 0 {
				required = src.Length
			}
			if capacity < required {
				return fail("字符容量不足")
			}
		case "NUMBER":
			if isFloat {
				continue
			}
			p, s := src.Precision, src.Scale
			if srcBase == "INTEGER" || srcBase == "INT" || srcBase == "SMALLINT" {
				p, s = 38, 0
			}
			if p == 0 {
				if c.precision > 0 {
					return fail("需要无固定精度的 NUMBER，禁止窄化")
				}
			} else if c.precision > 0 && (c.scale < s || c.precision-c.scale < p-s) {
				return fail("数值精度不足")
			}
		case "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE":
			p := typemap.OracleDamengTimestampPrecision(src)
			if typemap.OracleDamengBaseType(src.DataType) == "DATE" {
				p = 0
			}
			if c.scale < p {
				return fail("小数秒精度不足")
			}
		case "BINARY", "VARBINARY":
			required := src.DataLength
			if required == 0 {
				required = src.Length
			}
			if srcBase == "ROWID" {
				required = 18
				if c.bytes != required {
					return fail("ROWID 需要 BINARY(18)，避免定长补零改变标识")
				}
			}
			if srcBase == "UROWID" {
				required = 4000
			}
			if c.bytes < required {
				return fail("二进制容量不足")
			}
		case "INTERVAL YEAR TO MONTH", "INTERVAL DAY TO SECOND":
			p, s := typemap.OracleDamengIntervalPrecisions(src)
			tp, ts := typemap.OracleDamengIntervalPrecisions(source.ColumnInfo{DataType: c.kind, Precision: c.precision, Scale: c.scale})
			if tp < p || base == "INTERVAL DAY TO SECOND" && ts < s {
				return fail("区间精度不足")
			}
		}
	}
	return nil
}

func (w *DaMengWriter) createOracleDamengSequence(ctx context.Context, seq source.SequenceInfo, owner string) error {
	if err := dialect.ValidateOracleDamengSequence(seq); err != nil {
		return err
	}
	names := []string{seq.Native.Name}
	for _, b := range seq.Native.Bindings {
		names = append(names, dialect.OracleDamengTriggerName(seq, b))
	}
	for _, name := range names {
		var count int
		if err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=?`, w.schema, strings.ToUpper(name)).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("达梦对象 %s.%s 已存在，禁止覆盖命名序列或触发器", w.schema, name)
		}
	}
	return w.execStatements(ctx, w.dia.SequenceStatements(w.schema, seq, owner))
}
