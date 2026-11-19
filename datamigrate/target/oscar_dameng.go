package target

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
)

// ValidateOscarDamengExisting is read-only and runs before appending data to an
// existing table. All column names are the planned, physical target names.
func (w *DaMengWriter) ValidateOscarDamengExisting(ctx context.Context, info *source.TableDDLInfo) error {
	rows, err := w.db.QueryContext(ctx, damengExistingColumnsSQL, w.schema, strings.ToUpper(info.TableName))
	if err != nil {
		return err
	}
	defer rows.Close()
	actual := map[string]damengExistingColumn{}
	for rows.Next() {
		var c damengExistingColumn
		if err := rows.Scan(&c.name, &c.kind, &c.chars, &c.bytes, &c.precision, &c.scale, &c.nullable, &c.units, &c.identity); err != nil {
			return err
		}
		actual[c.name] = c
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Native type verification may need another query with a single-connection pool.
	if err := rows.Close(); err != nil {
		return err
	}
	if len(actual) != len(info.Columns) || len(actual) == 0 {
		return fmt.Errorf("目标表 %s 不存在或列数不一致", info.TableName)
	}
	for _, src := range info.Columns {
		c, ok := actual[strings.ToUpper(src.Name)]
		if !ok {
			return fmt.Errorf("目标表 %s 缺少列 %s", info.TableName, src.Name)
		}
		if err := w.validateOscarDamengColumn(ctx, info.TableName, src, c); err != nil {
			return fmt.Errorf("目标列 %s.%s: %w", info.TableName, src.Name, err)
		}
	}
	return nil
}

func (w *DaMengWriter) validateOscarDamengColumn(ctx context.Context, table string, src source.ColumnInfo, c damengExistingColumn) error {
	want, err := typemap.OscarToDamengChecked(src)
	if err != nil {
		return err
	}
	if src.IsNullable && !strings.EqualFold(c.nullable, "Y") {
		return fmt.Errorf("不允许源端 NULL 值")
	}
	if (src.Extra == "auto_increment") != (c.identity == 1) {
		return fmt.Errorf("IDENTITY 属性与源列不一致")
	}
	base := oracleDMTypeFamily(want)
	actualBase := oracleDMTypeFamily(c.kind)
	if actualBase == "DATETIME WITH TIME ZONE" {
		actualBase = "TIMESTAMP WITH TIME ZONE"
	}
	switch base {
	case "XMLTYPE":
		return w.validateOracleDMSpecialColumn(ctx, table, src.Name, base)
	case "FLOAT":
		storage, err := w.inspectOracleDMColumnFloat(ctx, table, src.Name)
		if err != nil {
			return err
		}
		if storage != "double" && (storage != "real" || want != "FLOAT(24)") {
			return fmt.Errorf("浮点存储 %s 与 %s 不兼容", storage, want)
		}
		return nil
	}
	if actualBase != base {
		return fmt.Errorf("类型 %s 与所需 %s 不兼容", c.kind, want)
	}
	baseSrc := source.OscarDamengColumnType(src.DataType)
	switch base {
	case "CHAR", "VARCHAR":
		required := max(src.Length, 1)
		if baseSrc == `"CHAR"` {
			required = 1
		} else if baseSrc == "NAME" {
			required = 127
		}
		if c.units != "C" || c.chars < required || base == "CHAR" && c.chars != required {
			return fmt.Errorf("字符长度或单位不满足 %s", want)
		}
	case "NUMBER":
		if c.precision > 0 && (src.Precision == 0 || c.scale < src.Scale || c.precision-c.scale < src.Precision-src.Scale) {
			return fmt.Errorf("精度或标度不满足 %s", want)
		}
	case "BINARY", "VARBINARY":
		required := max(src.Length, 1)
		if c.bytes < required || base == "BINARY" && c.bytes != required {
			return fmt.Errorf("二进制长度不满足 %s", want)
		}
	case "TIME", "TIME WITH TIME ZONE", "TIMESTAMP", "TIMESTAMP WITH TIME ZONE":
		if c.scale < typemap.OscarDamengTimePrecision(src) {
			return fmt.Errorf("小数秒精度不满足 %s", want)
		}
	case "INTERVAL YEAR TO MONTH", "INTERVAL DAY TO SECOND":
		p, s := typemap.OscarDamengIntervalPrecisions(src)
		tp, ts := typemap.OracleDamengIntervalPrecisions(source.ColumnInfo{DataType: c.kind, Precision: c.precision, Scale: c.scale})
		if tp < p || base == "INTERVAL DAY TO SECOND" && ts < s {
			return fmt.Errorf("区间精度不满足 %s", want)
		}
	}
	return nil
}

// VerifyOscarDamengIdentity verifies that the existing generator remains on
// the source progression beyond all copied IDs, without consuming a value.
func (w *DaMengWriter) VerifyOscarDamengIdentity(ctx context.Context, seq source.SequenceInfo) error {
	if seq.Unsupported != "" {
		return fmt.Errorf("%s", seq.Unsupported)
	}
	if seq.IdentityInfo == nil || seq.IdentityInfo.Increment <= 0 {
		return fmt.Errorf("缺少可核验的神通自增步长")
	}
	seq.TableName, seq.ColumnName = strings.ToUpper(seq.TableName), strings.ToUpper(seq.ColumnName)
	var count int
	if err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSCOLUMNS c JOIN ALL_OBJECTS o ON o.object_id=c.id WHERE o.owner=? AND o.object_name=? AND c.name=? AND c.INFO2 & 0x01=0x01`, w.schema, seq.TableName, seq.ColumnName).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("目标列 %s.%s 缺少唯一 IDENTITY 绑定", seq.TableName, seq.ColumnName)
	}
	qualified := w.qualifiedTable(seq.TableName)
	var stepValue, currentValue string
	if err := w.db.QueryRowContext(ctx, `SELECT IDENT_INCR(?),IDENT_CURRENT(?)`, qualified, qualified).Scan(&stepValue, &currentValue); err != nil {
		return err
	}
	step, err := repairInteger(stepValue)
	if err != nil {
		return err
	}
	if step.Cmp(big.NewInt(seq.IdentityInfo.Increment)) != 0 {
		return fmt.Errorf("目标 IDENTITY 步长 %s 与源端 %d 不一致", step, seq.IdentityInfo.Increment)
	}
	current, err := repairInteger(currentValue)
	if err != nil {
		return err
	}
	next := new(big.Int).Add(current, step)
	start, err := recoveryStart(seq)
	if err != nil {
		return err
	}
	extreme, err := recoveryExtreme(ctx, w, w.db, w.schema, seq, step)
	if err != nil {
		return err
	}
	delta := new(big.Int).Sub(next, start)
	if !next.IsInt64() || delta.Sign() < 0 || new(big.Int).Mod(delta, step).Sign() != 0 || extreme != nil && next.Cmp(extreme) <= 0 {
		return fmt.Errorf("目标 IDENTITY 下一值 %s 未覆盖源端续号 %s 和目标已写入的标识值", next, start)
	}
	return nil
}
