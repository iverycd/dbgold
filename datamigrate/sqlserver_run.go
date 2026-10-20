package datamigrate

import (
	"context"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlserverview"
	"dbgold/datamigrate/target"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
)

type sqlServerInspector interface {
	CheckNames(context.Context, []string) error
	ValidateExisting(context.Context, *source.TableDDLInfo) error
	RebuildDependencies(context.Context, map[string]bool) ([]dialect.Statement, error)
	DropDependencies(context.Context, []dialect.Statement) error
	ObjectExists(context.Context, string) (bool, error)
}

func (m *Migrator) isSQLServer() bool { return m.cfg.TargetDBType == "sqlserver" }
func sqlServerResult(c *CategoryReport, name, ddl string, err error) {
	c.Total++
	if err == nil {
		c.Success++
		return
	}
	c.Failed++
	c.Items = append(c.Items, ObjectResult{Name: name, DDL: ddl, Error: err.Error()})
}
func (m *Migrator) sqlServerOptions() error {
	if m.reader.DBType() != "mysql" {
		return fmt.Errorf("SQL Server 目标仅支持 MySQL 5.7/8.0 源库")
	}
	if m.cfg.ChangeOwner || m.cfg.Distributed || m.cfg.CharInLength || m.cfg.UseNvarchar2 {
		return fmt.Errorf("SQL Server 不支持 owner、分布式、CHAR 长度单位及 NVARCHAR2 选项")
	}
	return nil
}
func (m *Migrator) runSQLServer(ctx context.Context) MigrationReport {
	report := newMigrationReport()
	failGlobal := func(e error) MigrationReport {
		sqlServerResult(&report.Tables, "迁移准备", "", e)
		m.log.Errorf("SQL Server 迁移准备失败: %v", e)
		return report
	}
	if e := m.sqlServerOptions(); e != nil {
		return failGlobal(e)
	}
	inspector, ok := m.writer.(sqlServerInspector)
	if !ok {
		return failGlobal(fmt.Errorf("SQL Server Writer 缺少预检能力"))
	}
	dia, ok := m.writer.Dialect().(*dialect.SQLServerDialect)
	if !ok {
		return failGlobal(fmt.Errorf("缺少 SQL Server 方言"))
	}
	all, e := m.reader.ListTables(ctx)
	if e != nil {
		return failGlobal(e)
	}
	tables := FilterTables(all, m.cfg.Mode, m.cfg.Filter)
	if m.cfg.objectMode() && len(m.cfg.TableNames) > 0 {
		tables = nil
		for _, t := range m.cfg.TableNames {
			if slices.Contains(all, t) && !slices.Contains(tables, t) {
				tables = append(tables, t)
			}
		}
	}
	sort.Strings(tables)
	names := []string{}
	for _, t := range tables {
		names = append(names, m.objName(t))
	}
	if e = inspector.CheckNames(ctx, names); e != nil {
		return failGlobal(e)
	}
	if count, err := m.reader.GetTriggerCount(ctx); err != nil {
		report.Triggers.Total = -1
	} else {
		report.Triggers.Total = count
	}
	rebuild := !m.cfg.objectMode() && m.cfg.Content != "data_only"
	failed := map[string]bool{}
	ddl := map[string][]dialect.Statement{}
	sourceInfo := map[string]*source.TableDDLInfo{}
	for _, t := range tables {
		info, err := m.reader.GetTableDDLInfo(ctx, t)
		var ss []dialect.Statement
		if err == nil {
			ss, err = dia.CreateTableStatements(m.cfg.TargetSchema, info, "mysql", dialect.TypeOpt{}, m.objName)
		}
		if err == nil {
			sourceInfo[t] = info
			copy := *info
			copy.TableName = m.objName(t)
			copy.Columns = slices.Clone(info.Columns)
			cols := []string{}
			for i := range copy.Columns {
				copy.Columns[i].Name = m.objName(copy.Columns[i].Name)
				cols = append(cols, copy.Columns[i].Name)
			}
			err = inspector.CheckNames(ctx, cols)
			if err == nil {
				dia.Tables[copy.TableName] = &copy
				if !rebuild {
					err = inspector.ValidateExisting(ctx, &copy)
				}
			}
		}
		if err != nil {
			failed[t] = true
			sqlServerResult(&report.Tables, t, "", err)
			continue
		}
		ddl[t] = ss
	}
	if rebuild {
		selected := map[string]bool{}
		for _, t := range tables {
			if !failed[t] {
				selected[m.objName(t)] = true
			}
		}
		var drops []dialect.Statement
		for len(selected) > 0 {
			drops, e = inspector.RebuildDependencies(ctx, selected)
			if e == nil {
				break
			}
			var dep *target.SQLServerDependencyError
			if errors.As(e, &dep) {
				delete(selected, dep.Table)
				for _, t := range tables {
					if m.objName(t) == dep.Table {
						failed[t] = true
						sqlServerResult(&report.Tables, t, "", e)
					}
				}
				continue
			}
			return failGlobal(e)
		}
		m.logPreparationComplete(ctx)
		if len(selected) > 0 {
			if e = inspector.DropDependencies(ctx, drops); e != nil {
				return failGlobal(e)
			}
		}
		for _, t := range tables {
			if failed[t] {
				continue
			}
			e = m.repairTable(ctx, t, ddl[t])
			sqlServerResult(&report.Tables, t, dialect.JoinSQL(ddl[t]), e)
			if e != nil {
				failed[t] = true
			} else {
				m.log.DDLf("创建表 %s ... OK", t)
			}
		}
	} else if m.cfg.objectMode() {
		m.logPreparationComplete(ctx)
	}
	dataFailed := map[string]bool{}
	if !m.cfg.objectMode() && m.cfg.Content != "schema_only" {
		// A table is serial so an invalid page stops later pages. Tables still run
		// in parallel; each Writer transaction is isolated on its own connection.
		if m.cfg.IntraTableParallel > 1 {
			m.log.Warn("SQL Server 每表按页串行写入，确保失败后停止该表；表间并发保持生效")
		}
		type result struct {
			table            string
			before, src, dst int64
			err              error
		}
		out := make(chan result, len(tables))
		sem := make(chan struct{}, max(1, m.cfg.MaxParallel))
		var wg sync.WaitGroup
		dataTables := tables
		if m.cfg.Content == "data_only" {
			fks, err := m.reader.GetForeignKeys(ctx)
			if err != nil {
				return failGlobal(fmt.Errorf("读取仅数据模式外键依赖: %w", err))
			}
			var blocked []string
			dataTables, blocked = sqlServerDataOrder(tables, fks)
			for _, t := range blocked {
				dataFailed[t] = true
				sqlServerResult(&report.Data, t, "", fmt.Errorf("已有外键形成循环依赖，不能按依赖顺序追加数据"))
			}
		}
		m.logPreparationComplete(ctx)
		migrateOne := func(t string) {
			r := result{table: t}
			if m.cfg.Content == "data_only" {
				r.before, r.err = m.writer.CountRows(ctx, m.objName(t))
			}
			if r.err == nil {
				pk, err := m.reader.GetPrimaryKey(ctx, t)
				r.err = err
				if err == nil {
					ok, msg := m.migrateTableDataSerial(ctx, t, pk)
					if !ok {
						r.err = fmt.Errorf("%s；本批次已回滚，之前提交的数据可能保留", msg)
					}
				}
			}
			if r.err == nil {
				r.src, r.err = m.reader.CountRows(ctx, t)
			}
			if r.err == nil {
				r.dst, r.err = m.writer.CountRows(ctx, m.objName(t))
			}
			out <- r
		}
		for _, t := range dataTables {
			if failed[t] {
				continue
			}
			if m.cfg.Content == "data_only" {
				migrateOne(t)
				continue
			}
			wg.Add(1)
			go func(t string) { defer wg.Done(); sem <- struct{}{}; defer func() { <-sem }(); migrateOne(t) }(t)
		}
		wg.Wait()
		close(out)
		for r := range out {
			sqlServerResult(&report.Data, r.table, "", r.err)
			if r.err != nil {
				dataFailed[r.table] = true
			} else {
				report.RowCounts = append(report.RowCounts, TableRowCount{Table: r.table, Src: r.src, Dst: r.dst - r.before, Match: r.src == r.dst-r.before})
			}
		}
		sort.Slice(report.RowCounts, func(i, j int) bool { return report.RowCounts[i].Table < report.RowCounts[j].Table })
	}
	want := func(kind string) bool {
		if m.cfg.objectMode() {
			return slices.Contains(m.cfg.Objects, kind)
		}
		return m.cfg.Content != "data_only"
	}
	normIndex := func(i source.IndexInfo) source.IndexInfo {
		i.TableName = m.objName(i.TableName)
		i.IndexName = m.objName(i.IndexName)
		i.Columns = slices.Clone(i.Columns)
		for j := range i.Columns {
			i.Columns[j] = m.objName(i.Columns[j])
		}
		return i
	}
	eligible := func(t string) bool { return slices.Contains(tables, t) }
	dependency := func(t string) error {
		if failed[t] || dataFailed[t] {
			return fmt.Errorf("依赖表 %s 迁移失败，未创建对象", t)
		}
		return nil
	}
	for _, kind := range []string{"primary_keys", "indexes"} {
		if !want(kind) {
			continue
		}
		var indexes []source.IndexInfo
		var err error
		category := &report.Indexes
		if kind == "primary_keys" {
			indexes, err = m.reader.GetPrimaryKeys(ctx)
			category = &report.PrimaryKeys
		} else {
			indexes, err = m.reader.GetIndexes(ctx)
		}
		if err != nil {
			recordMetadataFailure(category, err)
			continue
		}
		for _, i := range indexes {
			if !eligible(i.TableName) {
				continue
			}
			original := i.TableName
			i = normIndex(i)
			var ss []dialect.Statement
			if i.Unsupported == "" {
				ss = dia.IndexStatements(m.cfg.TargetSchema, i)
			}
			stage := ""
			err = dependency(original)
			if err == nil {
				stage = "execute"
				if i.Unsupported != "" {
					stage = "unsupported"
				}
				err = m.repairIndex(ctx, i)
			}
			sqlServerResult(category, i.IndexName, dialect.JoinSQL(ss), err)
			if err != nil {
				item := &category.Items[len(category.Items)-1]
				item.Table, item.Stage = original, stage
			}
		}
	}
	// Seed correction is also needed after append, even when other DDL is off.
	if want("sequences") || !m.cfg.objectMode() && m.cfg.Content == "data_only" {
		seqs, err := m.reader.GetSequences(ctx)
		if err != nil {
			recordMetadataFailure(&report.Sequences, err)
		} else {
			for _, s := range seqs {
				if !eligible(s.TableName) {
					continue
				}
				original := s.TableName
				s.TableName = m.objName(s.TableName)
				s.ColumnName = m.objName(s.ColumnName)
				ss := dia.SequenceStatements(m.cfg.TargetSchema, s, "")
				err = dependency(original)
				if err == nil {
					err = m.repairSequence(ctx, s, "")
				}
				sqlServerResult(&report.Sequences, s.TableName+"."+s.ColumnName, dialect.JoinSQL(ss), err)
			}
		}
	}
	if want("foreign_keys") {
		fks, err := m.reader.GetForeignKeys(ctx)
		if err != nil {
			recordMetadataFailure(&report.Constraints, err)
		} else {
			for _, f := range fks {
				if !eligible(f.TableName) {
					continue
				}
				err = dependency(f.TableName)
				if err == nil {
					err = dependency(f.RefTable)
				}
				if err == nil && !eligible(f.RefTable) {
					err = fmt.Errorf("引用表 %s 未纳入迁移范围", f.RefTable)
				}
				f.TableName = m.objName(f.TableName)
				f.RefTable = m.objName(f.RefTable)
				f.ConstraintName = m.objName(f.ConstraintName)
				f.Columns = slices.Clone(f.Columns)
				f.RefColumns = slices.Clone(f.RefColumns)
				for j := range f.Columns {
					f.Columns[j] = m.objName(f.Columns[j])
					f.RefColumns[j] = m.objName(f.RefColumns[j])
				}
				ss := dia.ForeignKeyStatements(m.cfg.TargetSchema, f)
				if err == nil {
					err = m.repairFK(ctx, f)
				}
				sqlServerResult(&report.Constraints, f.ConstraintName, dialect.JoinSQL(ss), err)
			}
		}
	}
	if want("comments") {
		comments, err := m.reader.GetComments(ctx)
		if err != nil {
			recordMetadataFailure(&report.Comments, err)
		} else {
			for _, c := range comments {
				if !eligible(c.TableName) {
					continue
				}
				err = dependency(c.TableName)
				c.TableName = m.objName(c.TableName)
				c.ColumnName = m.objName(c.ColumnName)
				ss := dia.CommentStatements(m.cfg.TargetSchema, c)
				if err == nil {
					err = m.repairComment(ctx, c)
				}
				sqlServerResult(&report.Comments, c.TableName+"."+c.ColumnName, dialect.JoinSQL(ss), err)
			}
		}
	}
	if !m.cfg.objectMode() && m.cfg.Content != "data_only" {
		for t := range dataFailed {
			failed[t] = true
		}
		selectedViews := m.cfg.ViewNames
		if len(selectedViews) == 0 {
			selectedViews = nil
		}
		m.sqlServerViews(ctx, selectedViews, &report.Views, failed, sourceInfo)
	}
	m.log.Donef("SQL Server 迁移完成：数据成功 %d 张，失败 %d 张；失败对象请查看报告", report.Data.Success, report.Data.Failed)
	return report
}

func (m *Migrator) sqlServerViews(ctx context.Context, selected []string, category *CategoryReport, failedTables map[string]bool, infos map[string]*source.TableDDLInfo) {
	if e := m.sqlServerOptions(); e != nil {
		sqlServerResult(category, "迁移准备", "", e)
		return
	}
	inspector, ok := m.writer.(sqlServerInspector)
	if !ok {
		sqlServerResult(category, "迁移准备", "", fmt.Errorf("缺少 SQL Server 预检能力"))
		return
	}
	divisionPrecision := 4
	if r, ok := m.reader.(interface {
		SQLServerViewPrecision(context.Context) (int, error)
	}); ok {
		var err error
		divisionPrecision, err = r.SQLServerViewPrecision(ctx)
		if err != nil {
			sqlServerResult(category, "读取 MySQL 视图数值精度", "", err)
			return
		}
	}
	views, e := m.reader.GetViews(ctx)
	if e != nil {
		recordMetadataFailure(category, e)
		return
	}
	if infos == nil {
		infos = map[string]*source.TableDDLInfo{}
	}
	for _, v := range views {
		infos[v.ViewName] = &source.TableDDLInfo{TableName: v.ViewName, Columns: v.Columns}
	}
	src := ""
	if s, ok := m.reader.(interface{ SourceSchema() string }); ok {
		src = s.SourceSchema()
	}
	allTables, e := m.reader.ListTables(ctx)
	if e != nil {
		recordMetadataFailure(category, e)
		return
	}
	type plan struct {
		view source.ViewInfo
		deps []string
		err  error
	}
	plans := map[string]*plan{}
	names := []string{}
	for _, v := range views {
		if selected != nil && !slices.Contains(selected, v.ViewName) {
			continue
		}
		names = append(names, m.objName(v.ViewName))
		_, deps, _ := sqlserverview.Convert(v.Definition, src, m.cfg.TargetSchema, m.cfg.StripViewSchemas, m.objName, infos, divisionPrecision)
		for _, dep := range deps {
			if infos[dep] == nil && slices.Contains(allTables, dep) {
				info, err := m.reader.GetTableDDLInfo(ctx, dep)
				if err == nil {
					infos[dep] = info
				}
			}
		}
		def, deps, err := sqlserverview.Convert(v.Definition, src, m.cfg.TargetSchema, m.cfg.StripViewSchemas, m.objName, infos, divisionPrecision)
		original := v.ViewName
		v.Columns = slices.Clone(v.Columns)
		colNames := []string{}
		for i := range v.Columns {
			v.Columns[i].Name = m.objName(v.Columns[i].Name)
			colNames = append(colNames, v.Columns[i].Name)
		}
		if err == nil {
			err = inspector.CheckNames(ctx, colNames)
		}
		v.ViewName = m.objName(v.ViewName)
		v.Definition = def
		plans[original] = &plan{v, deps, err}
	}
	if e = inspector.CheckNames(ctx, names); e != nil {
		sqlServerResult(category, "视图名称预检", "", e)
		return
	}
	if selected != nil {
		for _, n := range selected {
			if plans[n] == nil {
				sqlServerResult(category, n, "", fmt.Errorf("源视图不存在"))
			}
		}
	}
	pending := []string{}
	for n := range plans {
		pending = append(pending, n)
	}
	sort.Strings(pending)
	done := map[string]bool{}
	bad := map[string]bool{}
	for len(pending) > 0 {
		progress := false
		next := []string{}
		for _, n := range pending {
			p := plans[n]
			wait := false
			if p.err == nil {
				for _, dep := range p.deps {
					if failedTables[dep] || bad[dep] {
						p.err = fmt.Errorf("依赖对象 %s 迁移失败", dep)
						break
					}
					if plans[dep] != nil && !done[dep] {
						wait = true
						continue
					}
					if plans[dep] == nil {
						exists, err := inspector.ObjectExists(ctx, m.objName(dep))
						if err != nil {
							p.err = err
							break
						}
						if !exists {
							p.err = fmt.Errorf("目标依赖对象 %s 不存在", dep)
							break
						}
					}
				}
			}
			if wait && p.err == nil {
				next = append(next, n)
				continue
			}
			ss := m.writer.Dialect().ViewStatements(m.cfg.TargetSchema, p.view)
			if p.err == nil {
				p.err = m.repairView(ctx, p.view)
			}
			sqlServerResult(category, p.view.ViewName, dialect.JoinSQL(ss), p.err)
			done[n] = true
			bad[n] = p.err != nil
			progress = true
		}
		if !progress {
			for _, n := range next {
				sqlServerResult(category, m.objName(n), "", fmt.Errorf("视图存在循环依赖或依赖无法完成"))
			}
			break
		}
		pending = next
	}
}

// Existing foreign keys remain enabled during append. Parents must commit
// before children; cycles are reported without disabling user constraints.
func sqlServerDataOrder(tables []string, fks []source.FKInfo) (ordered, blocked []string) {
	remaining := map[string]bool{}
	for _, t := range tables {
		remaining[t] = true
	}
	for len(remaining) > 0 {
		progress := false
		for _, t := range tables {
			if !remaining[t] {
				continue
			}
			ready := true
			for _, fk := range fks {
				if fk.TableName == t && remaining[fk.RefTable] {
					ready = false
					break
				}
			}
			if ready {
				ordered = append(ordered, t)
				delete(remaining, t)
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	for _, t := range tables {
		if remaining[t] {
			blocked = append(blocked, t)
		}
	}
	return
}
