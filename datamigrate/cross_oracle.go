package datamigrate

import (
	"context"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"fmt"
	"slices"
	"strings"
)

func (m *Migrator) isCrossOracle() bool {
	return m.isOracle() && (m.reader.DBType() == "dameng" || m.reader.DBType() == "sqlserver")
}
func (m *Migrator) isCrossMigration() bool {
	return m.isCrossOracle() || m.isSQLServer() && m.reader.DBType() == "dameng"
}
func crossOracleTable(info *source.TableDDLInfo, seqs []source.SequenceInfo) *source.TableDDLInfo {
	out := cloneSourceTableDDLInfo(info)
	for _, s := range seqs {
		if s.Native == nil {
			continue
		}
		for _, b := range s.Native.Bindings {
			if s.Unsupported != "" && b.Kind == "default" {
				continue
			}
			if b.TableName != info.TableName {
				continue
			}
			for i := range out.Columns {
				if out.Columns[i].Name == b.ColumnName {
					out.Columns[i].SQLServerDeferredDefault = true
				}
			}
		}
	}
	return out
}
func (m *Migrator) crossOracleSequence(s source.SequenceInfo, selected map[string]bool) source.SequenceInfo {
	n := *s.Native
	n.OracleCross = true
	n.SourceName = n.Name
	n.Name = m.objName(n.Name)
	if len(n.Bindings) == 1 && n.Bindings[0].Kind == "identity" {
		n.Name = dialect.OracleObjectName("SEQ", n.Bindings[0].TableName, n.Bindings[0].ColumnName)
	}
	n.Bindings = append([]source.SequenceBinding(nil), n.Bindings...)
	for i := range n.Bindings {
		b := &n.Bindings[i]
		if !selected[b.TableName] {
			s.Unsupported = "序列绑定包含未选中的源表"
		}
		b.SourceColumnName = b.ColumnName
		b.TableName = m.objName(b.TableName)
		b.ColumnName = m.objName(b.ColumnName)
	}
	s.Native = &n
	s.TableName = m.objName(s.TableName)
	s.ColumnName = m.objName(s.ColumnName)
	if e := dialect.ValidateCrossOracleSequence(s); e != nil {
		s.Unsupported = e.Error()
	}
	return s
}
func crossOracleSequenceBlocked(s source.SequenceInfo, failed map[string]bool) bool {
	if s.Native != nil {
		for _, b := range s.Native.Bindings {
			if failed[b.TableName] {
				return true
			}
		}
	}
	return false
}

func crossOracleRepresentation(c source.ColumnInfo, product string) string {
	t := strings.ToLower(c.DataType)
	switch {
	case product == "sqlserver" && (t == "timestamp" || t == "rowversion"):
		return "rowversion 保存为 RAW(8)，不保留自动更新行为"
	case t == "uniqueidentifier":
		return "GUID 保存为 VARCHAR2(36)，不保留原生 UUID 操作"
	case t == "xml":
		return "XML 保存为 CLOB，需人工适配 XML 操作"
	case t == "time":
		return "TIME 保存为标准时间文本，不保留原生时间类型操作"
	case c.Extra == "auto_increment":
		return "自动编号使用 Oracle 序列与工具触发器；显式 NULL 也会生成编号"
	}
	return ""
}

func (m *Migrator) validateCrossOracleIndex(idx source.IndexInfo, info *source.TableDDLInfo) error {
	if idx.Unsupported != "" {
		return fmt.Errorf("%s", idx.Unsupported)
	}
	if info == nil {
		return fmt.Errorf("索引缺少列元数据")
	}
	d := m.writer.Dialect().(*dialect.OracleDialect)
	for _, name := range idx.Columns {
		found := false
		for _, c := range info.Columns {
			if m.objName(c.Name) != name {
				continue
			}
			found = true
			mapped, e := typemap.OracleTypeForSource(c, m.reader.DBType(), m.cfg.UseNvarchar2, d.CharBytes, d.NCharBytes)
			if e != nil {
				return e
			}
			if slices.Contains([]string{"BLOB", "CLOB", "NCLOB"}, mapped) {
				return fmt.Errorf("索引列 %s 映射为 %s，需要人工适配", name, mapped)
			}
			if idx.IsUnique && !idx.IsPrimary && c.IsNullable {
				return fmt.Errorf("可空唯一键的 NULL 语义跨库不同，需要人工适配")
			}
		}
		if !found {
			return fmt.Errorf("索引列 %s 不存在或为表达式", name)
		}
	}
	return nil
}

func (m *Migrator) recordCrossOracleViewManifest(ctx context.Context) {
	views, err := m.readOracleViews(ctx)
	if err != nil {
		m.repairs.sinkErr = err
		return
	}
	raw := map[string]string{}
	for _, v := range views {
		raw[v.ViewName] = v.Definition
	}
	views = slices.DeleteFunc(views, func(v source.ViewInfo) bool {
		return !slices.Contains(m.cfg.ViewNames, v.ViewName) || m.crossExcluded("view", "", v.ViewName)
	})
	for _, v := range m.planOracleViews(views) {
		o := repair.Object{ID: repairID("view", "", v.view.ViewName), Kind: "view", Name: v.view.ViewName, SourceName: v.sourceName, TargetName: v.view.ViewName, Schema: m.cfg.TargetSchema, SourceDDL: raw[v.sourceName], Status: "pending", RequiresReview: true}
		if v.err != nil {
			o.Error = v.err.Error()
		} else {
			ss := m.writer.Dialect().ViewStatements(m.cfg.TargetSchema, v.view)
			o.DDL, o.Units = dialect.JoinSQL(ss), unitsFor(ss)
		}
		for _, dep := range v.deps {
			if raw[dep] != "" {
				o.Dependencies = append(o.Dependencies, repairID("view", "", m.objName(dep)))
			}
		}
		m.repairs.appendObject(o)
	}
}
