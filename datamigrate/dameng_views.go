package datamigrate

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/tsqldameng"
)

func (m *Migrator) hasDamengRenames() bool {
	for _, p := range m.dmPlans {
		if p.renamed {
			return true
		}
	}
	return false
}

func (m *Migrator) damengSourceSchema() string {
	if m.isSQLServerDameng() {
		return "dbo"
	}
	return m.dmSourceSchema
}

// Views are created in dependency order. Only successfully created (or verified
// existing) relations enter the catalog, so a failed view cannot feed a child.
// selected=nil means the full migration's view set; otherwise unselected views
// are inspected/validated as existing dependencies and never written.
func (m *Migrator) migrateDamengMappedViews(ctx context.Context, views []source.ViewInfo, selected map[string]bool) []ObjectResult {
	if selected != nil {
		// Inspect only selected views and their transitive view dependencies.
		byName := map[string]source.ViewInfo{}
		for _, v := range views {
			byName[strings.ToUpper(v.ViewName)] = v
		}
		needed := map[string]bool{}
		var include func(string)
		include = func(key string) {
			if needed[key] {
				return
			}
			v, exists := byName[key]
			if !exists {
				return
			}
			needed[key] = true
			parsed, _ := tsqldameng.MappedView(v.Definition, m.cfg.TargetSchema, m.damengSourceSchema(), m.isSQLServerDameng(), nil)
			for _, dep := range parsed.Dependencies {
				include(strings.ToUpper(dep))
			}
		}
		for name := range selected {
			include(strings.ToUpper(name))
		}
		filtered := []source.ViewInfo{}
		for _, v := range views {
			if needed[strings.ToUpper(v.ViewName)] {
				filtered = append(filtered, v)
			}
		}
		views = filtered
	}
	catalog := map[string][]tsqldameng.MappedColumn{}
	for table, p := range m.dmPlans {
		if !m.dmUsable[table] || m.dmBlocked[table] {
			continue
		}
		for _, c := range p.source.Columns {
			catalog[strings.ToUpper(table)] = append(catalog[strings.ToUpper(table)], tsqldameng.MappedColumn{Source: c.Name, Target: p.columns[c.Name]})
		}
	}
	tableNames := map[string]string{}
	if selected != nil {
		tables, err := m.reader.ListTables(ctx)
		if err != nil {
			return []ObjectResult{{Name: "视图依赖表", Error: err.Error()}}
		}
		for _, t := range tables {
			key := strings.ToUpper(t)
			if prev, ok := tableNames[key]; ok {
				return []ObjectResult{{Name: t, Error: "大写表名冲突: " + prev}}
			}
			tableNames[key] = t
		}
	}
	results := []ObjectResult{}
	pending, failed := map[string]source.ViewInfo{}, map[string]bool{}
	wanted := func(name string) bool { return selected == nil || selected[name] }
	failure := func(v source.ViewInfo, err error) {
		key := strings.ToUpper(v.ViewName)
		failed[key] = true
		delete(pending, key)
		delete(catalog, key)
		if wanted(v.ViewName) {
			results = append(results, ObjectResult{Name: m.objName(v.ViewName), DDL: v.Definition, Error: "列名修复后的视图同步失败: " + err.Error()})
			m.log.Warnf("视图 %s 同步失败，需人工处理: %v", v.ViewName, err)
		}
	}
	viewCounts := map[string]int{}
	for _, v := range views {
		viewCounts[strings.ToUpper(v.ViewName)]++
	}
	for _, v := range views {
		key := strings.ToUpper(v.ViewName)
		if viewCounts[key] > 1 || failed[key] || catalog[key] != nil {
			failure(v, fmt.Errorf("目标对象名冲突: %s", key))
			continue
		}
		if err := tsqldameng.ValidName(v.ViewName); err != nil {
			failure(v, err)
			continue
		}
		pending[key] = v
	}
	for len(pending) > 0 {
		progress := false
		for _, original := range views {
			key := strings.ToUpper(original.ViewName)
			v, ok := pending[key]
			if !ok {
				continue
			}
			if err := ctx.Err(); err != nil {
				failure(v, err)
				continue
			}
			converted, err := tsqldameng.MappedView(v.Definition, m.cfg.TargetSchema, m.damengSourceSchema(), m.isSQLServerDameng(), catalog)
			wait := false
			for _, dep := range converted.Dependencies {
				dk := strings.ToUpper(dep)
				if failed[dk] {
					err = fmt.Errorf("依赖对象 %s 同步失败", dep)
					wait = false
					break
				}
				if _, exists := pending[dk]; exists {
					wait = true
					continue
				}
				if catalog[dk] == nil && tableNames[dk] != "" {
					table := tableNames[dk]
					if e := m.ensureDamengReference(ctx, table); e != nil {
						err = e
						failed[dk] = true
						wait = false
						break
					}
					p := m.dmPlans[table]
					for _, c := range p.source.Columns {
						catalog[dk] = append(catalog[dk], tsqldameng.MappedColumn{Source: c.Name, Target: p.columns[c.Name]})
					}
				}
			}
			if wait {
				continue
			}
			// Rebind now that view-only dependencies have been read and verified.
			blocked := false
			for _, dep := range converted.Dependencies {
				if failed[strings.ToUpper(dep)] {
					blocked = true
				}
			}
			if !blocked {
				converted, err = tsqldameng.MappedView(v.Definition, m.cfg.TargetSchema, m.damengSourceSchema(), m.isSQLServerDameng(), catalog)
			}
			if err != nil {
				failure(v, err)
				progress = true
				continue
			}
			v.Definition = converted.SQL
			ddl := dialect.JoinSQL(m.writer.Dialect().ViewStatements(m.cfg.TargetSchema, v))
			if wanted(v.ViewName) {
				err = m.writer.CreateView(ctx, v)
			} else {
				// An unselected view is an existing dependency, never silently rebuilt.
				info := &source.TableDDLInfo{TableName: v.ViewName}
				for _, c := range converted.Columns {
					info.Columns = append(info.Columns, source.ColumnInfo{Name: c.Target})
				}
				validator, ok := m.writer.(interface {
					ValidateDamengColumns(context.Context, *source.TableDDLInfo) error
				})
				if !ok {
					err = fmt.Errorf("无法校验已有视图 %s 的列映射", v.ViewName)
				} else {
					err = validator.ValidateDamengColumns(ctx, info)
				}
			}
			if err != nil {
				v.Definition = ddl
				failure(v, err)
			} else {
				catalog[key] = converted.Columns
				delete(pending, key)
				if wanted(v.ViewName) {
					results = append(results, ObjectResult{Name: m.objName(v.ViewName), DDL: ddl})
					m.log.DDLf("同步视图 %s 的列名引用 ... OK", v.ViewName)
				}
			}
			progress = true
		}
		if !progress {
			for _, v := range views {
				if _, ok := pending[strings.ToUpper(v.ViewName)]; ok {
					failure(v, fmt.Errorf("视图依赖循环或依赖映射不可用"))
				}
			}
		}
	}
	return results
}

func (m *Migrator) migrateDamengViewsOnly(ctx context.Context, names []string) []ObjectResult {
	views, err := m.reader.GetViews(ctx)
	if err != nil {
		return []ObjectResult{{Name: "视图元数据", Error: err.Error()}}
	}
	selected, found := map[string]bool{}, map[string]bool{}
	for _, name := range names {
		selected[name] = true
	}
	for _, v := range views {
		found[v.ViewName] = true
	}
	out := m.migrateDamengMappedViews(ctx, views, selected)
	for _, name := range names {
		if !found[name] {
			out = append(out, ObjectResult{Name: name, Error: "所选视图不存在"})
		}
	}
	return out
}
