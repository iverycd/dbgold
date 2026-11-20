package partition

import (
	"fmt"
	"strconv"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
)

func (c *compiler) pgRoot(base string, s *source.PartitionSpec) (string, error) {
	return strings.TrimSuffix(strings.TrimSpace(base), ";") + " PARTITION BY " + baseMethod(s.Method) + " (" + c.keys(s) + ")", nil
}

func (c *compiler) pgChildren(schema, parent string, s *source.PartitionSpec) ([]dialect.Statement, error) {
	var out []dialect.Statement
	for _, p := range s.Partitions {
		ddl := "CREATE TABLE " + c.qualified(p.Schema, p.Name) + " PARTITION OF " + c.qualified(schema, parent) + " "
		if p.Default {
			ddl += "DEFAULT"
		} else {
			switch baseMethod(s.Method) {
			case "RANGE":
				if !p.LowerInclusive || p.UpperInclusive {
					return nil, fmt.Errorf("PostgreSQL 原生 RANGE 要求下界包含、上界不包含，闭开转换尚未实现/验证")
				}
				lo, e := c.tuple(p.Lower)
				if e != nil {
					return nil, e
				}
				hi, e := c.tuple(p.Upper)
				if e != nil {
					return nil, e
				}
				ddl += "FOR VALUES FROM (" + lo + ") TO (" + hi + ")"
			case "LIST":
				var values []string
				for _, v := range p.Values {
					tuple, e := c.tuple(v)
					if e != nil {
						return nil, e
					}
					values = append(values, tuple)
				}
				ddl += "FOR VALUES IN (" + strings.Join(values, ",") + ")"
			case "HASH":
				ddl += "FOR VALUES WITH (MODULUS " + strconv.Itoa(p.HashModulus) + ", REMAINDER " + strconv.Itoa(p.HashRemainder) + ")"
			}
		}
		if p.Subpartition != nil {
			ddl += " PARTITION BY " + baseMethod(p.Subpartition.Method) + " (" + c.keys(p.Subpartition) + ")"
		}
		out = append(out, dialect.Statement{SQL: ddl})
		if p.Subpartition != nil {
			children, e := c.pgChildren(p.Schema, p.Name, p.Subpartition)
			if e != nil {
				return nil, e
			}
			out = append(out, children...)
		}
	}
	return out, nil
}

func (c *compiler) inlineFlat(s *source.PartitionSpec) (string, error) {
	method := method(s.Method)
	nativeMethod, methodErr := mysqlKeyMethodSQL(s)
	if methodErr != nil {
		return "", methodErr
	}
	if baseMethod(method) == "HASH" && c.opt.TargetType != "mysql" {
		for i, p := range s.Partitions {
			if p.HashModulus != len(s.Partitions) || p.HashRemainder != i {
				return "", fmt.Errorf("目标原生 HASH 不能表达稀疏或混合 modulus 的精确分区布局")
			}
		}
	}
	var defs []string
	for i, p := range s.Partitions {
		if p.Subpartition != nil {
			return "", fmt.Errorf("原生多级分区完整声明适配尚未实现/验证")
		}
		def := "PARTITION " + c.quote(p.Name)
		switch baseMethod(method) {
		case "RANGE":
			if p.Default {
				return "", fmt.Errorf("目标原生 RANGE DEFAULT 不可等价表达")
			}
			values, e := c.tuple(p.Upper)
			if e != nil {
				return "", e
			}
			def += " VALUES LESS THAN (" + values + ")"
		case "LIST":
			if p.Default {
				if c.profile.Features["inline_mysql"] {
					return "", fmt.Errorf("MySQL LIST 不支持兜底分区，不能等价表达 DEFAULT")
				}
				def += " VALUES (DEFAULT)"
			} else {
				var values []string
				for _, v := range p.Values {
					tuple, e := c.tuple(v)
					if e != nil {
						return "", e
					}
					if len(v) > 1 {
						tuple = "(" + tuple + ")"
					}
					values = append(values, tuple)
				}
				if c.profile.Features["inline_mysql"] {
					def += " VALUES IN (" + strings.Join(values, ",") + ")"
				} else {
					def += " VALUES (" + strings.Join(values, ",") + ")"
				}
			}
		case "HASH", "KEY":
			if p.HashModulus != len(s.Partitions) || p.HashRemainder != i {
				return "", fmt.Errorf("目标原生 HASH 不能保留源 modulus/remainder 布局")
			}
		}
		defs = append(defs, def)
	}
	clause := "PARTITION BY " + nativeMethod + " (" + c.keys(s) + ") (\n  " + strings.Join(defs, ",\n  ") + "\n)"
	return clause, nil
}

// SQLServerHelperNames are separate database-scoped objects. Their conflicts
// must be checked before any table drop, even though all partitions use the
// target's default filegroup and individual partition names are ordinal only.
func SQLServerHelperNames(schema, root string) (string, string) {
	prefix := root
	if schema != "" {
		prefix = schema + "_" + root
	}
	return prefix + "__partition_function", prefix + "__partition_scheme"
}

func (c *compiler) sqlServer(base []dialect.Statement, create int, s *source.PartitionSpec, root string) ([]dialect.Statement, error) {
	if baseMethod(s.Method) != "RANGE" || len(s.Keys) != 1 || s.Keys[0].Column == "" {
		return nil, fmt.Errorf("SQL Server 原生分区只支持单列 RANGE；该路由不能原生等价表达")
	}
	if !allKind(s.Partitions[0].Lower, "minvalue") || !allKind(s.Partitions[len(s.Partitions)-1].Upper, "maxvalue") {
		return nil, fmt.Errorf("SQL Server 分区函数覆盖整个类型域，无法原生保留源 RANGE 两端空洞")
	}
	left := s.Partitions[0].UpperInclusive
	for i, p := range s.Partitions {
		if p.Default || p.Subpartition != nil {
			return nil, fmt.Errorf("SQL Server 原生 RANGE 不支持 DEFAULT/子分区")
		}
		if p.UpperInclusive != left || p.LowerInclusive == left {
			return nil, fmt.Errorf("SQL Server 不能原生表达混合开闭边界")
		}
		if i > 0 && !sameValues(s.Partitions[i-1].Upper, p.Lower) {
			return nil, fmt.Errorf("SQL Server RANGE 不能保留源分区空洞")
		}
	}
	fn, scheme := SQLServerHelperNames(c.opt.TargetSchema, root)
	if err := c.validateName(fn); err != nil {
		return nil, err
	}
	if err := c.validateName(scheme); err != nil {
		return nil, err
	}
	column, ok := c.column(c.info.Partition.Keys[0].Column)
	if !ok {
		return nil, fmt.Errorf("分区列元数据缺失")
	}
	keyType := dialect.NewSQLServer().MapType(column, c.opt.SourceType, dialect.TypeOpt{})
	if keyType == "" || strings.Contains(strings.ToLower(keyType), "max") || strings.Contains(strings.ToLower(keyType), "text") {
		return nil, fmt.Errorf("SQL Server 分区列类型 %q 无法原生等价表达", keyType)
	}
	crossSource := c.opt.SourceType == "oracle" || source.IsPGFamily(c.opt.SourceType)
	if crossSource {
		kind, _, _ := strings.Cut(keyType, "(")
		switch kind {
		case "SMALLINT", "INT", "BIGINT", "DECIMAL", "DATE", "DATETIME2":
		default:
			return nil, fmt.Errorf("SQL Server 分区键类型 %q 的边界顺序尚未验证", keyType)
		}
		if typemap.IsSQLServerUnboundedDecimal(column, c.opt.SourceType) {
			return nil, fmt.Errorf("未声明精度的数值分区键无法用依据现有数据定型的 DECIMAL 保留后续路由")
		}
	}
	var bounds []string
	for i := 0; i < len(s.Partitions)-1; i++ {
		bound := s.Partitions[i].Upper[0]
		v, e := c.value(bound)
		if e != nil {
			return nil, e
		}
		if crossSource {
			bound, e = canonicalValue(bound)
			if e != nil {
				return nil, e
			}
			converted, err := valueconv.ConvertSQLServerForSource(bound.Text, column, c.opt.SourceType)
			if err != nil {
				return nil, fmt.Errorf("分区边界 %s: %w", bound.Text, err)
			}
			if bound.Kind == "date" || bound.Kind == "timestamp" {
				v = dialect.SQLServerLiteral(fmt.Sprint(converted))
			}
		}
		bounds = append(bounds, v)
	}
	side := "RIGHT"
	if left {
		side = "LEFT"
	}
	fg := strings.TrimPrefix(c.profile.Mode, "filegroup:")
	if fg == "" || fg == c.profile.Mode {
		return nil, fmt.Errorf("目标默认文件组尚未读取，不能猜测分区物理放置")
	}
	functionSQL := "CREATE PARTITION FUNCTION " + c.quote(fn) + " (" + keyType + ") AS RANGE " + side + " FOR VALUES (" + strings.Join(bounds, ",") + ")"
	schemeSQL := "CREATE PARTITION SCHEME " + c.quote(scheme) + " AS PARTITION " + c.quote(fn) + " ALL TO (" + c.quote(fg) + ")"
	result := append([]dialect.Statement(nil), base[:create]...)
	result = append(result, dialect.Statement{SQL: functionSQL}, dialect.Statement{SQL: schemeSQL})
	rootSQL := strings.TrimSuffix(strings.TrimSpace(base[create].SQL), ";") + " ON " + c.quote(scheme) + " (" + c.keys(s) + ")"
	result = append(result, dialect.Statement{SQL: rootSQL})
	result = append(result, base[create+1:]...)
	s.Algorithm = "sqlserver-range:" + fn
	return result, nil
}
