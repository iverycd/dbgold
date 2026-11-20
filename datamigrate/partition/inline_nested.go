package partition

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
)

func (c *compiler) inlineTable(base string, s *source.PartitionSpec) (string, error) {
	clause, err := c.inline(s)
	if err != nil {
		return "", err
	}
	base = strings.TrimSuffix(strings.TrimSpace(base), ";")
	if c.opt.SourceType == "mysql" && c.opt.TargetType == "dameng" {
		// The root STORAGE attribute follows the partition definitions and
		// precedes ROW MOVEMENT. The dialect emits it as an exact suffix.
		if columns, found := strings.CutSuffix(base, " "+dialect.DamengLongRowStorage); found {
			base = columns
			clause += " " + dialect.DamengLongRowStorage
		}
	}
	if (c.profile.Features["row_movement_clause"] || s.Interval != nil) && s.RowMovement != "" {
		if s.RowMovement == "enabled" {
			clause += " ENABLE ROW MOVEMENT"
		} else {
			clause += " DISABLE ROW MOVEMENT"
		}
	}
	return base + "\n" + clause, nil
}

func (c *compiler) inline(s *source.PartitionSpec) (string, error) {
	if s.Interval != nil {
		return c.inlineInterval(s)
	}
	var child *source.PartitionSpec
	for _, p := range s.Partitions {
		if p.Subpartition != nil {
			child = p.Subpartition
			break
		}
	}
	if child == nil {
		return c.inlineFlat(s)
	}
	if c.profile.Features["inline_mysql"] && (baseMethod(s.Method) != "RANGE" && baseMethod(s.Method) != "LIST" || baseMethod(child.Method) != "HASH" && baseMethod(child.Method) != "KEY") {
		return "", fmt.Errorf("MySQL 原生子分区只能在 RANGE/LIST 下使用 HASH/KEY，不能改变源子分区路由")
	}
	parentMethod, err := mysqlKeyMethodSQL(s)
	if err != nil {
		return "", err
	}
	childMethod, err := mysqlKeyMethodSQL(child)
	if err != nil {
		return "", err
	}
	var definitions []string
	for _, p := range s.Partitions {
		if p.Subpartition == nil || method(p.Subpartition.Method) != method(child.Method) || len(p.Subpartition.Keys) != len(child.Keys) || p.Subpartition.Algorithm != child.Algorithm {
			return "", fmt.Errorf("目标要求所有父分区使用一致的子分区方法与键")
		}
		for i, k := range child.Keys {
			if CanonicalKey(k) != CanonicalKey(p.Subpartition.Keys[i]) {
				return "", fmt.Errorf("目标不能原生表达每个父分区不同的子分区键")
			}
		}
		for _, leaf := range p.Subpartition.Partitions {
			if leaf.Subpartition != nil {
				return "", fmt.Errorf("原生目标不能压平源的多级子分区")
			}
		}
		parent := source.ClonePartitionSpec(s)
		parent.Partitions = []source.Partition{p}
		parent.Partitions[0].Subpartition = nil
		flat, e := c.inlineFlat(parent)
		if e != nil {
			return "", e
		}
		prefix := "PARTITION BY " + parentMethod + " (" + c.keys(parent) + ") (\n  "
		if !strings.HasPrefix(flat, prefix) || !strings.HasSuffix(flat, "\n)") {
			return "", fmt.Errorf("父分区声明边界不完整")
		}
		definition := strings.TrimSuffix(strings.TrimPrefix(flat, prefix), "\n)")
		sub, e := c.inlineFlat(p.Subpartition)
		if e != nil {
			return "", e
		}
		subPrefix := "PARTITION BY " + childMethod + " (" + c.keys(p.Subpartition) + ") (\n  "
		if !strings.HasPrefix(sub, subPrefix) || !strings.HasSuffix(sub, "\n)") {
			return "", fmt.Errorf("子分区声明边界不完整")
		}
		body := strings.TrimSuffix(strings.TrimPrefix(sub, subPrefix), "\n)")
		body = "SUBPARTITION " + strings.TrimPrefix(body, "PARTITION ")
		body = strings.ReplaceAll(body, ",\n  PARTITION ", ",\n    SUBPARTITION ")
		definitions = append(definitions, definition+" (\n    "+body+"\n  )")
	}
	clause := "PARTITION BY " + parentMethod + " (" + c.keys(s) + ") SUBPARTITION BY " + childMethod + " (" + c.keys(child) + ") (\n  " + strings.Join(definitions, ",\n  ") + "\n)"
	return clause, nil
}
