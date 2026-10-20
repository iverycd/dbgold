package datamigrate

import (
	"context"
	"fmt"

	"dbgold/datamigrate/damengnames"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/oracledameng"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/tsqldameng"
)

func (m *Migrator) migrateOracleDamengViewsOnly(ctx context.Context, names []string) []ObjectResult {
	views, err := m.readViews(ctx)
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
	out := m.migrateOracleDamengViews(ctx, views, selected)
	for _, name := range names {
		if !found[name] {
			out = append(out, ObjectResult{Name: name, Error: "所选视图不存在"})
		}
	}
	return out
}

// Only token-bound identifiers are changed. The Oracle SQL never enters the
// legacy PostgreSQL or T-SQL rewriting paths, including during view-only jobs.
func (m *Migrator) migrateOracleDamengViews(ctx context.Context, views []source.ViewInfo, selected map[string]bool) []ObjectResult {
	if len(m.cfg.StripViewSchemas) > 0 {
		return []ObjectResult{{Name: "视图模式", Error: "Oracle → 达梦仅映射源 schema 的对象引用，不支持直接删除 schema 前缀"}}
	}
	tables, err := m.reader.ListTables(ctx)
	if err != nil {
		return []ObjectResult{{Name: "视图依赖表", Error: err.Error()}}
	}
	byName := map[string]source.ViewInfo{}
	for _, v := range views {
		byName[v.ViewName] = v
	}
	needed := map[string]bool{}
	var include func(string)
	include = func(name string) {
		if needed[name] {
			return
		}
		v, exists := byName[name]
		if !exists {
			return
		}
		needed[name] = true
		discovery, _ := oracledameng.ConvertViewWithOptions(v.Definition, m.damengSourceSchema(), m.cfg.TargetSchema, nil, m.oracleDamengSQLOptions())
		for _, dep := range discovery.Dependencies {
			include(dep)
		}
	}
	for _, v := range views {
		if selected == nil || selected[v.ViewName] {
			include(v.ViewName)
		}
	}
	catalog := map[string][]oracledameng.MappedColumn{}
	for table, p := range m.dmPlans {
		if !m.dmUsable[table] || m.dmBlocked[table] {
			continue
		}
		for _, c := range p.source.Columns {
			catalog[table] = append(catalog[table], oracledameng.MappedColumn{Source: c.Name, Target: p.columns[c.Name]})
		}
	}
	results := []ObjectResult{}
	pending, failed := map[string]source.ViewInfo{}, map[string]bool{}
	wanted := func(n string) bool { return selected == nil || selected[n] }
	failure := func(v source.ViewInfo, err error) {
		failed[v.ViewName] = true
		delete(pending, v.ViewName)
		if wanted(v.ViewName) {
			results = append(results, ObjectResult{Name: m.objName(v.ViewName), DDL: v.Definition, Error: err.Error()})
		}
	}
	origins := map[string]int{}
	for _, v := range views {
		if needed[v.ViewName] {
			origins[m.objName(v.ViewName)]++
			for _, table := range tables {
				if m.objName(table) == m.objName(v.ViewName) {
					origins[m.objName(v.ViewName)]++
				}
			}
		}
	}
	for _, v := range views {
		if !needed[v.ViewName] {
			continue
		}
		if e := tsqldameng.ValidName(v.ViewName); e != nil {
			failure(v, e)
		} else if origins[m.objName(v.ViewName)] > 1 {
			failure(v, fmt.Errorf("视图名转大写后冲突: %s", v.ViewName))
		} else {
			pending[v.ViewName] = v
		}
	}
	for len(pending) > 0 {
		progress := false
		for _, original := range views {
			v, exists := pending[original.ViewName]
			if !exists {
				continue
			}
			if err := ctx.Err(); err != nil {
				failure(v, err)
				continue
			}
			discovery, discoveryErr := oracledameng.ConvertViewWithOptions(v.Definition, m.damengSourceSchema(), m.cfg.TargetSchema, nil, m.oracleDamengSQLOptions())
			if discoveryErr != nil {
				failure(v, discoveryErr)
				progress = true
				continue
			}
			wait := false
			var dependencyErr error
			for _, dep := range discovery.Dependencies {
				if failed[dep] {
					dependencyErr = fmt.Errorf("依赖视图 %s 迁移失败", dep)
					break
				}
				if _, exists := pending[dep]; exists {
					wait = true
					continue
				}
				if catalog[dep] != nil {
					continue
				}
				var sourceTable string
				for _, t := range tables {
					if t == dep {
						sourceTable = t
						break
					}
				}
				if sourceTable == "" {
					if dep == "DUAL" {
						// The converter distinguishes unqualified built-in DUAL
						// from a missing explicitly qualified source relation.
						continue
					}
					dependencyErr = fmt.Errorf("依赖对象 %s 不存在或不在可迁移范围", dep)
					break
				}
				p, e := m.planDamengTable(ctx, sourceTable)
				if e == nil && m.dmBlocked[sourceTable] {
					e = fmt.Errorf("依赖表 %s 迁移失败", sourceTable)
				}
				if e == nil {
					if validator, ok := m.writer.(interface {
						ValidateOracleDamengExisting(context.Context, *source.TableDDLInfo, dialect.TypeOpt) error
					}); ok {
						e = prepareOracleDamengWriter(ctx, m.writer, []*source.TableDDLInfo{p.target}, dialect.TypeOpt{CharInLength: m.cfg.CharInLength, UseNvarchar2: m.cfg.UseNvarchar2})
						if e == nil {
							e = validator.ValidateOracleDamengExisting(ctx, p.target, dialect.TypeOpt{CharInLength: m.cfg.CharInLength, UseNvarchar2: m.cfg.UseNvarchar2})
						}
					} else if validator, ok := m.writer.(interface {
						ValidateDamengColumns(context.Context, *source.TableDDLInfo) error
					}); ok {
						e = validator.ValidateDamengColumns(ctx, p.target)
					} else {
						e = fmt.Errorf("无法验证已有依赖表 %s 的目标列", sourceTable)
					}
				}
				if e != nil {
					dependencyErr = e
					break
				}
				for _, c := range p.source.Columns {
					catalog[dep] = append(catalog[dep], oracledameng.MappedColumn{Source: c.Name, Target: p.columns[c.Name]})
				}
			}
			if dependencyErr != nil {
				failure(v, dependencyErr)
				progress = true
				continue
			}
			if wait {
				continue
			}
			if err := prepareOracleDamengWriter(ctx, m.writer, []*source.TableDDLInfo{{TableName: v.ViewName, Columns: v.Columns}}, dialect.TypeOpt{CharInLength: m.cfg.CharInLength, UseNvarchar2: m.cfg.UseNvarchar2}); err != nil {
				failure(v, fmt.Errorf("视图目标能力检查: %w", err))
				progress = true
				continue
			}
			if validator, ok := m.writer.(interface {
				ValidateOracleDamengView(context.Context, string) error
			}); ok {
				if err := validator.ValidateOracleDamengView(ctx, v.Definition); err != nil {
					failure(v, err)
					progress = true
					continue
				}
			}
			converted, err := oracledameng.ConvertViewWithOptions(v.Definition, m.damengSourceSchema(), m.cfg.TargetSchema, catalog, m.oracleDamengSQLOptions())
			if err == nil && len(v.Columns) > 0 {
				v.Columns = append([]source.ColumnInfo(nil), v.Columns...)
				if len(v.Columns) != len(converted.Columns) {
					err = fmt.Errorf("视图 %s 的输出列数与源端元数据不一致", v.ViewName)
				} else {
					names := make([]string, len(v.Columns))
					for i, c := range v.Columns {
						names[i] = c.Name
					}
					targetNames, e := damengnames.Plan(names)
					if e != nil {
						err = e
					} else {
						for i, c := range v.Columns {
							converted.Columns[i] = oracledameng.MappedColumn{Source: c.Name, Target: targetNames[i]}
							v.Columns[i].Name = targetNames[i]
						}
					}
				}
			}
			if err == nil {
				err = m.repairViewDependencies(m.objName(v.ViewName), converted.Dependencies)
			}
			if err == nil && wanted(v.ViewName) {
				v.ViewName, v.Definition = m.objName(v.ViewName), converted.SQL
				err = m.repairView(ctx, v)
			} else if err == nil {
				info := &source.TableDDLInfo{TableName: m.objName(v.ViewName)}
				for _, c := range converted.Columns {
					info.Columns = append(info.Columns, source.ColumnInfo{Name: c.Target})
				}
				validator, ok := m.writer.(interface {
					ValidateDamengColumns(context.Context, *source.TableDDLInfo) error
				})
				if !ok {
					err = fmt.Errorf("无法验证已有依赖视图 %s", v.ViewName)
				} else {
					err = validator.ValidateDamengColumns(ctx, info)
				}
			}
			if err != nil {
				failure(original, err)
			} else {
				catalog[original.ViewName] = converted.Columns
				delete(pending, original.ViewName)
				if wanted(original.ViewName) {
					results = append(results, ObjectResult{Name: m.objName(original.ViewName), DDL: dialect.JoinSQL(m.writer.Dialect().ViewStatements(m.cfg.TargetSchema, v))})
				}
			}
			progress = true
		}
		if !progress {
			for _, v := range views {
				if _, exists := pending[v.ViewName]; exists {
					failure(v, fmt.Errorf("视图依赖循环"))
				}
			}
		}
	}
	return results
}
