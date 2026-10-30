package partition

import (
	"fmt"
	"math/big"
	"strings"

	"dbgold/datamigrate/source"
)

// SQL Server RANGE LEFT owns finite boundary values on the left. For one
// discrete integer key, (a,b] is exactly [a+1,b+1). No such rewrite is valid for
// arbitrary numeric, temporal or textual keys. Infinite sentinels stay infinite.
func (c *compiler) sqlServerRangeLeft(src *source.PartitionSpec) (*source.PartitionSpec, error) {
	needsConversion := false
	for _, p := range src.Partitions {
		needsConversion = needsConversion || !p.LowerInclusive || p.UpperInclusive
	}
	if !needsConversion {
		return nil, nil
	}
	if len(src.Keys) != 1 || src.Keys[0].Column == "" {
		return nil, fmt.Errorf("SQL Server RANGE LEFT 仅支持单个直接整数列的精确闭开转换")
	}
	column, ok := c.column(src.Keys[0].Column)
	if !ok {
		return nil, fmt.Errorf("SQL Server RANGE LEFT 分区键列不在冻结结构中")
	}
	minimum, maximum, ok := sqlServerIntegerDomain(column.DataType)
	if !ok || (src.Keys[0].DataType != "" && !strings.EqualFold(src.Keys[0].DataType, column.DataType)) {
		return nil, fmt.Errorf("SQL Server RANGE LEFT 键类型 %s 尚无可证明的离散整数闭开转换", column.DataType)
	}
	normal := source.ClonePartitionSpec(src)
	for i, p := range src.Partitions {
		if p.Default || p.Subpartition != nil || p.LowerInclusive || !p.UpperInclusive || len(p.Lower) != 1 || len(p.Upper) != 1 {
			return nil, fmt.Errorf("SQL Server RANGE LEFT 目录含混合开闭边界、子分区或不完整单列范围")
		}
		var err error
		normal.Partitions[i].Lower[0], err = sqlServerIntegerSuccessor(p.Lower[0], "minvalue", minimum, maximum)
		if err != nil {
			return nil, fmt.Errorf("分区 %s 下界: %w", p.Name, err)
		}
		normal.Partitions[i].Upper[0], err = sqlServerIntegerSuccessor(p.Upper[0], "maxvalue", minimum, maximum)
		if err != nil {
			return nil, fmt.Errorf("分区 %s 上界: %w", p.Name, err)
		}
		normal.Partitions[i].LowerInclusive, normal.Partitions[i].UpperInclusive = true, false
	}
	return normal, nil
}

func sqlServerIntegerDomain(dataType string) (*big.Int, *big.Int, bool) {
	var low, high string
	switch strings.ToLower(dataType) {
	case "tinyint":
		low, high = "0", "255"
	case "smallint":
		low, high = "-32768", "32767"
	case "int":
		low, high = "-2147483648", "2147483647"
	case "bigint":
		low, high = "-9223372036854775808", "9223372036854775807"
	default:
		return nil, nil, false
	}
	minimum, _ := new(big.Int).SetString(low, 10)
	maximum, _ := new(big.Int).SetString(high, 10)
	return minimum, maximum, true
}

func sqlServerIntegerSuccessor(value source.PartitionValue, sentinel string, minimum, maximum *big.Int) (source.PartitionValue, error) {
	if strings.EqualFold(value.Kind, sentinel) {
		return source.PartitionValue{Kind: sentinel}, nil
	}
	normalized, err := canonicalValue(value)
	if err != nil || normalized.Kind != "number" {
		return source.PartitionValue{}, fmt.Errorf("闭开转换需要精确整数常量或 %s，不能转换 NULL/表达式/其它边界", sentinel)
	}
	number, ok := new(big.Int).SetString(normalized.Text, 10)
	if !ok || number.Cmp(minimum) < 0 || number.Cmp(maximum) > 0 {
		return source.PartitionValue{}, fmt.Errorf("闭边界不是源整数类型值域内的精确整数")
	}
	if number.Cmp(maximum) == 0 {
		return source.PartitionValue{}, fmt.Errorf("边界等于源整数最大值，不能原生保留超出值域的空叶分区；禁止溢出、删除分区或改用 MAXVALUE 扩大范围")
	}
	number.Add(number, big.NewInt(1))
	return source.PartitionValue{Kind: "number", Text: number.String()}, nil
}
