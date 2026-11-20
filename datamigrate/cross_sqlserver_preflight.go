package datamigrate

import (
	"context"
	"dbgold/datamigrate/target"
	"errors"
	"fmt"
	"slices"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
)

func (m *Migrator) preflightCrossSQLServer(ctx context.Context, p *CrossPreflight) {
	d := m.writer.Dialect().(*dialect.SQLServerDialect)
	d.SetSourceType(m.reader.DBType())
	inspector, ok := m.writer.(sqlServerInspector)
	if !ok {
		p.issue(ObjectRef{Kind: "target", Name: "目标核验"}, "Writer 缺少目标核验能力", "", false)
		return
	}
	if e := m.sqlServerOptions(); e != nil {
		p.issue(ObjectRef{Kind: "target", Name: "迁移选项"}, e.Error(), "", false)
		return
	}
	all, e := m.migrationTables(ctx)
	if e != nil {
		p.issue(ObjectRef{Kind: "metadata", Name: "表列表"}, e.Error(), "", false)
		return
	}
	tables := m.crossTables(FilterTables(all, m.cfg.Mode, m.cfg.Filter))
	if m.cfg.objectMode() && len(m.cfg.TableNames) > 0 {
		tables = slices.DeleteFunc(tables, func(t string) bool { return !slices.Contains(m.cfg.TableNames, t) })
	}
	selected, healthy := map[string]bool{}, map[string]bool{}
	infos := map[string]*source.TableDDLInfo{}
	rebuild := !m.cfg.objectMode() && m.cfg.Content != "data_only"
	if len(m.cfg.ViewNames) > 0 {
		tables = nil
		rebuild = false
	}
	add := func(ref ObjectRef, target, stage string, ss []dialect.Statement, err error) {
		if m.crossExcluded(ref.Kind, ref.Table, ref.Name) {
			return
		}
		if err != nil {
			p.issue(ref, err.Error(), dialect.JoinSQL(ss), false)
			return
		}
		p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: target, Stage: stage, Statements: ss})
		p.CanStart = true
	}
	for _, t := range tables {
		selected[t] = true
		info, e := m.sourceTableDDLInfo(ctx, t)
		var ss []dialect.Statement
		if e == nil {
			e = m.migrationTableError(t)
		}
		if e == nil {
			info, e = m.sqlServerTable(ctx, info)
		}
		if e == nil {
			e = inspector.CheckNames(ctx, []string{m.objName(t)})
		}
		if e == nil {
			d.Tables[m.objName(t)] = m.sqlServerTargetTable(info)
			if rebuild {
				ss, e = d.CreateTableStatements(m.cfg.TargetSchema, info, m.reader.DBType(), dialect.TypeOpt{}, m.objName)
				if e == nil {
					ss, e = m.partitionStatements(t, ss)
				}
			} else {
				e = inspector.ValidateExisting(ctx, m.sqlServerTargetTable(info))
			}
		}
		if e != nil {
			add(tableRef(t), m.objName(t), "tables", ss, e)
			m.blockMigrationTable(t, "preflight", e)
			continue
		}
		healthy[t] = true
		infos[t] = info
		if !m.cfg.objectMode() {
			add(tableRef(t), m.objName(t), "tables", ss, nil)
		}
		for _, w := range m.prepareSQLServerSource(ctx).tables[t].warnings {
			p.issue(ObjectRef{Kind: "column", Table: t, Name: w.Column}, w.Reason, "", true)
		}
	}
	for len(healthy) > 0 {
		scope := map[string]bool{}
		var names []string
		for name := range healthy {
			scope[m.objName(name)] = true
			names = append(names, m.objName(name))
		}
		e := inspector.CheckNames(ctx, names)
		if e == nil && rebuild {
			_, e = inspector.RebuildDependencies(ctx, scope)
		}
		if e == nil {
			break
		}
		var nameErr *target.SQLServerNameError
		var depErr *target.SQLServerDependencyError
		names = nil
		if errors.As(e, &nameErr) {
			names = nameErr.Names
		} else if errors.As(e, &depErr) {
			names = []string{depErr.Table}
		}
		changed := false
		for t := range healthy {
			if slices.Contains(names, m.objName(t)) {
				p.issue(tableRef(t), e.Error(), "", false)
				m.blockMigrationTable(t, "preflight", e)
				delete(healthy, t)
				changed = true
				p.Objects = slices.DeleteFunc(p.Objects, func(o PlannedObject) bool { return o.Object.Kind == "table" && o.Object.Name == t })
			}
		}
		if !changed {
			p.issue(ObjectRef{Kind: "target", Name: "目标依赖核验"}, e.Error(), "", false)
			p.CanStart = false
			return
		}
	}
	p.CanStart = len(p.Objects) > 0
	want := func(kind string) bool {
		return len(m.cfg.ViewNames) == 0 && m.cfg.Content != "data_only" && (!m.cfg.objectMode() || slices.Contains(m.cfg.Objects, kind))
	}
	dep := func(t string) error {
		if !healthy[t] {
			return fmt.Errorf("依赖表 %s 未选中或预检失败", t)
		}
		return nil
	}
	for _, kind := range []string{"primary_keys", "indexes"} {
		if !want(kind) {
			continue
		}
		var indexes []source.IndexInfo
		var err error
		if kind == "primary_keys" {
			indexes, err = m.reader.GetPrimaryKeys(ctx)
		} else {
			indexes, err = m.reader.GetIndexes(ctx)
		}
		if err != nil {
			p.issue(ObjectRef{Kind: "metadata", Name: kind}, err.Error(), "", false)
			continue
		}
		for _, i := range indexes {
			if !selected[i.TableName] {
				continue
			}
			k := "index"
			if i.IsPrimary {
				k = "primary_key"
			}
			ref := ObjectRef{Kind: k, Table: i.TableName, Name: i.IndexName}
			err := dep(i.TableName)
			i = oracleIndexNames(i, m.objName)
			if err == nil {
				err = d.ValidateIndex(i)
			}
			add(ref, i.IndexName, "indexes", d.IndexStatements(m.cfg.TargetSchema, i), err)
		}
	}
	if want("sequences") || m.cfg.Content == "data_only" {
		plan := m.prepareSQLServerSource(ctx)
		if plan.sequenceError != nil {
			p.issue(ObjectRef{Kind: "metadata", Name: "序列"}, plan.sequenceError.Error(), "", false)
		}
		values := m.sqlServerPlannedSequences(ctx, func(t string) bool { return selected[t] })
		if e := m.checkSQLServerSequenceNames(ctx, values); e != nil {
			p.issue(ObjectRef{Kind: "target", Name: "序列名称"}, e.Error(), "", false)
		}
		for _, s := range values {
			name := m.sqlServerSequenceObjectName(s)
			ref := ObjectRef{Kind: "sequence", Table: s.TableName, Name: name}
			var err error
			if s.Unsupported != "" {
				err = fmt.Errorf("%s", s.Unsupported)
			}
			if s.Native != nil {
				for _, b := range s.Native.Bindings {
					if m.migrationTargetTableError(b.TableName) != nil {
						err = fmt.Errorf("依赖表 %s 预检失败", b.TableName)
					}
				}
				if err == nil {
					_, err = dialect.ValidateSQLServerNativeSequence(s)
				}
			} else if m.migrationTargetTableError(s.TableName) != nil {
				err = fmt.Errorf("依赖表 %s 预检失败", s.TableName)
			}
			ss := d.SequenceStatements(m.cfg.TargetSchema, s, "")
			if err == nil && len(ss) == 0 {
				err = fmt.Errorf("序列计划为空")
			}
			if s.Native != nil {
				ref.Name = s.Native.SourceName
			}
			add(ref, name, "sequences", ss, err)
		}
	}
	if want("foreign_keys") {
		fks, err := m.reader.GetForeignKeys(ctx)
		if err != nil {
			p.issue(ObjectRef{Kind: "metadata", Name: "外键"}, err.Error(), "", false)
		}
		for _, f := range fks {
			if !selected[f.TableName] {
				continue
			}
			ref := ObjectRef{Kind: "foreign_key", Table: f.TableName, Name: f.ConstraintName}
			err := dep(f.TableName)
			if err == nil {
				err = dep(f.RefTable)
			}
			f = m.scopeSQLServerForeignKey(f)
			f.TableName, f.RefTable, f.ConstraintName = m.objName(f.TableName), m.objName(f.RefTable), m.objName(f.ConstraintName)
			f.Columns, f.RefColumns = oracleNames(f.Columns, m.objName), oracleNames(f.RefColumns, m.objName)
			if err == nil {
				err = d.ValidateForeignKey(f)
			}
			add(ref, f.ConstraintName, "constraints", d.ForeignKeyStatements(m.cfg.TargetSchema, f), err)
		}
	}
	if want("comments") {
		cs, err := m.reader.GetComments(ctx)
		if err != nil {
			p.issue(ObjectRef{Kind: "metadata", Name: "注释"}, err.Error(), "", false)
		}
		for _, c := range cs {
			if !selected[c.TableName] {
				continue
			}
			ref := ObjectRef{Kind: "comment", Table: c.TableName, Name: c.TableName + "." + c.ColumnName}
			err := dep(c.TableName)
			c.TableName, c.ColumnName = m.objName(c.TableName), m.objName(c.ColumnName)
			add(ref, c.TableName, "comments", d.CommentStatements(m.cfg.TargetSchema, c), err)
		}
	}
	if len(m.cfg.ViewNames) > 0 || rebuild && m.cfg.Mode != "include" {
		names := m.cfg.ViewNames
		if len(names) == 0 {
			names = nil
		}
		plans, err := m.planSQLServerViews(ctx, names, infos)
		if err != nil {
			p.issue(ObjectRef{Kind: "metadata", Name: "视图"}, err.Error(), "", false)
			return
		}
		for _, name := range names {
			if plans[name] == nil && !m.crossExcluded("view", "", name) {
				p.issue(ObjectRef{Kind: "view", Name: name}, "源视图不存在", "", false)
			}
		}
		done := map[string]bool{}
		for len(done) < len(plans) {
			progress := false
			for _, name := range sortedSQLServerViewNames(plans) {
				v := plans[name]
				if done[name] {
					continue
				}
				wait := false
				if v.err == nil {
					for _, dependency := range v.deps {
						if dv := plans[dependency]; dv != nil {
							if !done[dependency] {
								wait = true
								break
							}
							if dv.err != nil {
								v.err = fmt.Errorf("依赖视图 %s 预检失败", dependency)
								break
							}
						} else if rebuild && selected[dependency] && !healthy[dependency] {
							v.err = fmt.Errorf("依赖表 %s 预检失败", dependency)
							break
						}
					}
				}
				if wait && v.err == nil {
					continue
				}
				if v.err == nil {
					v.err = inspector.CheckNames(ctx, []string{v.view.ViewName})
				}
				ss := d.ViewStatements(m.cfg.TargetSchema, v.view)
				if v.err != nil {
					p.issue(ObjectRef{Kind: "view", Name: name}, v.err.Error(), v.sourceDDL, false)
				} else {
					add(ObjectRef{Kind: "view", Name: name}, v.view.ViewName, "views", ss, nil)
				}
				done[name] = true
				progress = true
			}
			if !progress {
				for name, v := range plans {
					if !done[name] {
						p.issue(ObjectRef{Kind: "view", Name: name}, "视图依赖循环", v.sourceDDL, false)
						done[name] = true
					}
				}
			}
		}
	}
}
