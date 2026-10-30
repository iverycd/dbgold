package partition

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strconv"
	"strings"
)

// MySQL routes HASH by abs(signed_value % N); NULL is LLONG_MIN, not zero.
// Proof: mysql/mysql-server sql/sql_partition.cc part_val_int/get_part_id_hash.
// Exact numeric arithmetic also handles the signed minimum without overflow.
func (c *compiler) mysqlHashAsList(dst, src *source.PartitionSpec, depth int, mapping *[]Mapping) error {
	linear := method(src.Method) == "LINEAR HASH" && src.Algorithm == "mysql-linear-hash"
	if (!linear && (method(src.Method) != "HASH" || src.Algorithm != "mysql-mod")) || len(src.Keys) != 1 {
		return fmt.Errorf("MySQL HASH 仅实现单个整数键或已证明整数表达式的精确模数转换")
	}
	keySQL := ""
	if src.Keys[0].Column == "" {
		n, err := c.integerExpression(src.Keys[0].Expression)
		if err != nil {
			return fmt.Errorf("MySQL HASH 表达式: %w", err)
		}
		keySQL = n.sql
	} else {
		col, ok := c.column(src.Keys[0].Column)
		if !ok {
			return fmt.Errorf("HASH 键列不存在")
		}
		switch strings.ToLower(col.DataType) {
		case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
		default:
			return fmt.Errorf("HASH 键 %s 不是可证明的整数域", col.Name)
		}
		if strings.Contains(strings.ToLower(col.FormattedType+" "+src.Keys[0].DataType), "unsigned") {
			return fmt.Errorf("无符号 HASH 键的目标类型全值域适配尚未实现，不能仅凭当前数据成功证明支持")
		}
		keySQL = "CAST(" + c.quote(c.opt.ColumnName(col.Name)) + " AS numeric)"
	}
	if !c.profile.Features[FeatureExpressions] || !c.profile.Features[FeatureList] {
		return fmt.Errorf("目标没有可验证的原生表达式 LIST 能力")
	}
	n := len(src.Partitions)
	if n == 0 {
		return fmt.Errorf("HASH 分区清单为空")
	}
	modulus := n
	if linear {
		// MySQL's documented partition limit also bounds the enumerated LIST
		// constants. Do not permit an untrusted frozen catalog to grow this loop.
		if n > 8192 {
			return fmt.Errorf("MySQL LINEAR HASH 分区数超过已验证的 8192 个上限")
		}
		modulus = 1
		for modulus < n {
			modulus <<= 1
		}
	}
	normal := source.ClonePartitionSpec(src)
	normal.Method = "LIST"
	normal.Algorithm = ""
	for i := range normal.Partitions {
		p := &normal.Partitions[i]
		if p.HashModulus != n || p.HashRemainder != i || p.Default {
			return fmt.Errorf("MySQL HASH 目录的分区数、顺序或桶号不完整")
		}
		p.HashModulus, p.HashRemainder = 0, 0
		p.Lower, p.Upper = nil, nil
		p.LowerInclusive, p.UpperInclusive = false, false
		p.Values = [][]source.PartitionValue{{{Kind: "number", Text: strconv.Itoa(i)}}}
		if linear {
			p.Values = nil
		}
	}
	if linear {
		// sql/sql_partition.cc get_part_id_from_linear_hash first masks by
		// V-1, then V/2-1 when the bucket is outside N. Signed numeric MOD by
		// V has the complete residue domain -(V-1)..V-1. Assign those residues
		// to the native buckets, preserving negative keys and LLONG_MIN/NULL
		// without a generated column, trigger, or a target HASH substitution.
		for residue := 1 - modulus; residue < modulus; residue++ {
			bucket := residue & (modulus - 1)
			if bucket >= n {
				bucket = residue & (modulus/2 - 1)
			}
			p := &normal.Partitions[bucket]
			p.Values = append(p.Values, []source.PartitionValue{{Kind: "number", Text: strconv.Itoa(residue)}})
		}
	}
	*dst = *source.ClonePartitionSpec(normal)
	if err := c.prepare(dst, normal, depth, mapping); err != nil {
		return err
	}
	expression := "pg_catalog.mod(COALESCE(" + keySQL + ",CAST('-9223372036854775808' AS numeric)),CAST('" + strconv.Itoa(modulus) + "' AS numeric))"
	rule := "mysql-linear-mask-to-native-list"
	if !linear {
		expression = "pg_catalog.abs(" + expression + ")"
		rule = "mysql-signed-modulo-to-native-list"
	}
	dst.Keys = []source.PartitionKey{{Expression: expression, DataType: "numeric"}}
	c.rules = append(c.rules, rule)
	return nil
}
