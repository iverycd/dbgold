package partition

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbgold/datamigrate/source"
)

func (c *compiler) prepareOscarMySQLDomains(dst *source.PartitionSpec) error {
	if baseMethod(dst.Method) != "RANGE" && baseMethod(dst.Method) != "LIST" {
		return fmt.Errorf("神通到 MySQL 仅支持单层 RANGE/LIST")
	}
	if dst.RowMovement != "enabled" || dst.Automatic != "" || dst.Interval != nil {
		return fmt.Errorf("神通分区缺少等价的行移动证据或包含自动扩展")
	}
	if len(dst.Partitions) > 8192 || (baseMethod(dst.Method) == "LIST" && len(dst.Keys) != 1) {
		return fmt.Errorf("超过 MySQL 的 8192 个分区上限，或 LIST 使用了多列键")
	}
	names := make(map[string]bool, len(dst.Partitions))
	for _, p := range dst.Partitions {
		if p.Subpartition != nil || p.Default {
			return fmt.Errorf("分区 %s 含 MySQL 无法等价保留的神通子分区或 DEFAULT", p.Name)
		}
		name := strings.ToLower(p.Name)
		if names[name] {
			return fmt.Errorf("MySQL 分区名称大小写冲突: %s", p.Name)
		}
		names[name] = true
	}
	for i, key := range c.info.Partition.Keys {
		col, ok := c.column(key.Column)
		if !ok || key.Expression != "" || col.IsNullable {
			return fmt.Errorf("神通分区键必须是已确认的非空直接列: %q", key.Column)
		}
		typ, bits := oscarMySQLSignedKeyType(col.DataType)
		if bits == 0 {
			typ = c.opt.OscarTemporalDomains[col.Name]
			if !oscarMySQLTemporalType(typ) {
				return fmt.Errorf("分区键 %s 的类型 %s 缺少等价的 MySQL 键值域证据", col.Name, col.DataType)
			}
		}
		c.keyColumns = append(c.keyColumns, KeyColumnDomain{Column: dst.Keys[i].Column, DataType: typ})
		for j := range dst.Partitions {
			part := &dst.Partitions[j]
			tuples := append([][]source.PartitionValue{part.Lower, part.Upper}, part.Values...)
			for _, tuple := range tuples {
				if len(tuple) == 0 {
					continue
				}
				if len(tuple) != len(dst.Keys) {
					return fmt.Errorf("分区 %s 的边界元组与键列数量不一致", part.Name)
				}
				if err := normalizeOscarMySQLBoundary(&tuple[i], typ, bits); err != nil {
					return fmt.Errorf("分区 %s 键 %s: %w", part.Name, col.Name, err)
				}
			}
		}
	}
	c.rules = append(c.rules, "oscar-mysql-exact-key-domain")
	return nil
}

func oscarMySQLSignedKeyType(typ string) (string, int) {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "smallint", "int2":
		return "smallint", 16
	case "int", "integer", "int4":
		return "int", 32
	case "bigint", "int8":
		return "bigint", 64
	}
	return "", 0
}

func oscarMySQLTemporalType(typ string) bool {
	return typ == "date" || typ == "datetime" || len(typ) == len("datetime(0)") &&
		strings.HasPrefix(typ, "datetime(") && typ[9] >= '0' && typ[9] <= '6' && typ[10] == ')'
}

func normalizeOscarMySQLBoundary(v *source.PartitionValue, typ string, bits int) error {
	if v.Kind == "minvalue" || v.Kind == "maxvalue" {
		return nil
	}
	if bits > 0 {
		n, err := strconv.ParseInt(v.Text, 10, bits)
		if v.Kind != "number" || err != nil {
			return fmt.Errorf("边界 %q 超出 %s 或不是精确整数", v.Text, typ)
		}
		v.Text = strconv.FormatInt(n, 10)
		return nil
	}
	if v.Kind != "string" && v.Kind != "date" && v.Kind != "timestamp" {
		return fmt.Errorf("时间边界类型 %s 无法验证", v.Kind)
	}
	layout := "2006-01-02"
	if typ != "date" {
		layout += " 15:04:05.999999999"
	}
	t, err := time.Parse(layout, strings.Replace(v.Text, "T", " ", 1))
	precision := 0
	if strings.HasPrefix(typ, "datetime(") {
		precision = int(typ[9] - '0')
	}
	quantum := 1_000_000_000
	for i := 0; i < precision; i++ {
		quantum /= 10
	}
	if err != nil || t.Year() < 1000 || t.Nanosecond()%quantum != 0 {
		return fmt.Errorf("时间边界 %q 超出 %s 的值域或精度", v.Text, typ)
	}
	// MySQL's partition catalog reads native temporal bounds as SQL strings.
	v.Kind, v.Text = "string", t.Format(layout)
	return nil
}
