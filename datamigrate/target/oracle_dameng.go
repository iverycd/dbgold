package target

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
)

// ValidateOracleDamengCapabilities observes the required target compatibility
// and timestamp precision before destructive table operations. It never changes INI.
func (w *DaMengWriter) ValidateOracleDamengCapabilities(ctx context.Context, infos []*source.TableDDLInfo) error {
	rows, err := w.db.QueryContext(ctx, `SELECT PARA_NAME,PARA_VALUE FROM V$DM_INI WHERE PARA_NAME IN ('COMPATIBLE_MODE','IFUN_DATETIME_MODE')`)
	if err != nil {
		return fmt.Errorf("读取达梦兼容能力: %w", err)
	}
	defer rows.Close()
	parameters := map[string]string{}
	for rows.Next() {
		var key, val string
		if err := rows.Scan(&key, &val); err != nil {
			return err
		}
		parameters[strings.ToUpper(key)] = strings.TrimSpace(val)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if parameters["COMPATIBLE_MODE"] != "2" {
		return fmt.Errorf("Oracle 迁移要求达梦 COMPATIBLE_MODE=2；不会自动修改数据库配置")
	}
	for _, info := range infos {
		for _, c := range info.Columns {
			if strings.HasPrefix(typemap.OracleDamengBaseType(c.DataType), "TIMESTAMP") && typemap.OracleDamengTimestampPrecision(c) > 6 && parameters["IFUN_DATETIME_MODE"] != "1" {
				return fmt.Errorf("列 %s.%s 需要 IFUN_DATETIME_MODE=1 才能保留超过 6 位的小数秒；当前配置会截断", info.TableName, c.Name)
			}
		}
	}
	return nil
}

// ValidateOracleDamengExisting checks capacity as well as names in data/object
// modes; an Oracle DATE requires TIMESTAMP(0), never DM DATE.
func (w *DaMengWriter) ValidateOracleDamengExisting(ctx context.Context, info *source.TableDDLInfo, opt dialect.TypeOpt) error {
	rows, err := w.db.QueryContext(ctx, `SELECT c.COLUMN_NAME,c.DATA_TYPE,NVL(c.CHAR_LENGTH,0),NVL(c.DATA_LENGTH,0),NVL(c.DATA_PRECISION,0),NVL(c.DATA_SCALE,0),c.NULLABLE,NVL(c.CHAR_USED,'B'),CASE WHEN sc.INFO2 & 0x01=0x01 THEN 1 ELSE 0 END FROM ALL_TAB_COLUMNS c JOIN ALL_OBJECTS o ON o.object_name=c.TABLE_NAME AND o.owner=c.OWNER JOIN SYS.SYSCOLUMNS sc ON sc.id=o.object_id AND sc.name=c.COLUMN_NAME WHERE c.OWNER=? AND c.TABLE_NAME=? ORDER BY c.COLUMN_ID`, w.schema, strings.ToUpper(info.TableName))
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
		base := dmBaseType(want)
		if dmBaseType(c.kind) != base {
			return fail(fmt.Sprintf("类型 %s 与所需 %s 不匹配", c.kind, want))
		}
		if src.IsNullable && c.nullable != "Y" {
			return fail("不允许 NULL")
		}
		if c.identity != 0 {
			return fail("含 IDENTITY 属性，不能保留 Oracle 显式值写入语义")
		}
		switch base {
		case "CHAR", "VARCHAR", "NVARCHAR2":
			required, capacity := src.DataLength, c.bytes
			charUnits := opt.CharInLength || opt.UseNvarchar2 || src.CharUsed == "C" || strings.HasPrefix(typemap.OracleDamengBaseType(src.DataType), "N")
			if charUnits {
				required, capacity = src.Length, c.chars
				if c.units != "C" && base != "NVARCHAR2" {
					return fail("字符长度单位不足")
				}
			} else if required == 0 {
				required = src.Length
			}
			if capacity < required {
				return fail("字符容量不足")
			}
		case "NUMBER":
			p, s := src.Precision, src.Scale
			srcBase := typemap.OracleDamengBaseType(src.DataType)
			if srcBase == "FLOAT" || srcBase == "REAL" || srcBase == "DOUBLE PRECISION" || p == 0 {
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
		case "VARBINARY":
			required := src.DataLength
			if required == 0 {
				required = src.Length
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
