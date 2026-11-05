package cdc

import (
	"database/sql"
	"fmt"
	"math"
	"math/big"
	"strings"
)

func validateDamengIdentityNext(table string, column targetColumnMetadata, seedText, currentText, stepText string, maximum, sourceNext sql.NullString) error {
	seed, ok := new(big.Int).SetString(seedText, 10)
	if !ok {
		return fmt.Errorf("目标表 %s 自增种子无效: %s", table, seedText)
	}
	current, ok := new(big.Int).SetString(currentText, 10)
	if !ok {
		return fmt.Errorf("目标表 %s 自增当前值无效: %s", table, currentText)
	}
	step, ok := new(big.Int).SetString(stepText, 10)
	if !ok || step.Sign() <= 0 {
		return fmt.Errorf("目标表 %s 自增步长必须为正整数: %s", table, stepText)
	}
	var maximumValue *big.Int
	if maximum.Valid {
		maximumValue, ok = new(big.Int).SetString(maximum.String, 10)
		if !ok {
			return fmt.Errorf("表 %s 自增数据边界无效: %s", table, maximum.String)
		}
	}
	upper := new(big.Int).Add(current, step)
	lower := new(big.Int).Set(upper)
	ambiguous := current.Cmp(seed) == 0 && (maximumValue == nil || maximumValue.Cmp(seed) < 0)
	if ambiguous {
		// IDENT_CURRENT is the seed both before allocation and after the first
		// generated value was deleted or rolled back. Explicit values below the
		// seed can also leave a nonempty table whose next generated ID is seed.
		// These public read-only values cannot distinguish seed from seed+step.
		lower.Set(seed)
	}
	var typeMaximum *big.Int
	switch strings.ToUpper(strings.TrimSpace(column.DataType)) {
	case "INT", "INTEGER":
		typeMaximum = big.NewInt(math.MaxInt32)
	case "BIGINT":
		// Verified DM8 releases (20240920/20261008) reserve MaxInt64 for the identity allocator: a seed
		// at that value is rejected, and allocating it reports -7070 even
		// though an explicit BIGINT data value can still equal MaxInt64.
		typeMaximum = big.NewInt(math.MaxInt64 - 1)
	}
	if typeMaximum != nil && upper.Cmp(typeMaximum) > 0 {
		return fmt.Errorf("达梦表 %s 自增列 %s 可能的下一值 %s 超过 %s 自增安全上限 %s，不能确认续号安全；请修复后重试切换", table, column.Name, upper, column.DataType, typeMaximum)
	}
	if maximumValue != nil && lower.Cmp(maximumValue) <= 0 {
		return fmt.Errorf("达梦表 %s 可确认的自增下界 %s 未越过数据最大值 %s；请修复后重试切换", table, lower, maximumValue)
	}
	if sourceNext.Valid {
		minimum, valid := new(big.Int).SetString(sourceNext.String, 10)
		if !valid {
			return fmt.Errorf("源表 %s 自增水位无效: %s", table, sourceNext.String)
		}
		if lower.Cmp(minimum) < 0 {
			if ambiguous {
				return fmt.Errorf("达梦表 %s 无法确认自增首值已消费，保守下界 %s 低于源库下一值 %s；请核实并修复后重试切换", table, lower, minimum)
			}
			return fmt.Errorf("达梦表 %s 自增下一值 %s 低于源库下一值 %s；请修复后重试切换", table, lower, minimum)
		}
	}
	return nil
}
