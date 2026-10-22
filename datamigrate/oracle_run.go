package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/oracleview"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
)

func (m *Migrator) isOracle() bool { return m.cfg.TargetDBType == "oracle" }

type oracleViewPlan struct {
	view source.ViewInfo
	ddl  string
	deps []string
	err  error
}
type oraclePlan struct {
	tables       []string
	info         map[string]*source.TableDDLInfo
	ddl          map[string][]dialect.Statement
	pks, indexes []source.IndexInfo
	sequences    []source.SequenceInfo
	fks          []source.FKInfo
	comments     []source.CommentInfo
	views        []oracleViewPlan
	warnings     []valueconv.MigrationWarning
}
type oracleInspector interface {
	PreflightOracle(context.Context, map[string]*source.TableDDLInfo, []target.OracleObject, []source.SequenceInfo, bool, bool) error
}

func (m *Migrator) oracleSourceSchema() string {
	if s, ok := m.reader.(interface{ SourceSchema() string }); ok {
		return s.SourceSchema()
	}
	return ""
}
func (m *Migrator) readOracleViews(ctx context.Context) ([]source.ViewInfo, error) {
	if s, ok := m.reader.(interface {
		GetViewsForTarget(context.Context, string) ([]source.ViewInfo, error)
	}); ok {
		return s.GetViewsForTarget(ctx, "oracle")
	}
	return nil, fmt.Errorf("MySQL Reader 未提供 Oracle 原始视图读取能力")
}

func (m *Migrator) prepareOracle(ctx context.Context) (*oraclePlan, error) {
	if m.reader.DBType() != "mysql" {
		return nil, fmt.Errorf("Oracle 目标仅支持 MySQL 源库")
	}
	d, ok := m.writer.Dialect().(*dialect.OracleDialect)
	if !ok {
		return nil, fmt.Errorf("Oracle 目标缺少 Oracle 方言")
	}
	all, err := m.reader.ListTables(ctx)
	if err != nil {
		return nil, err
	}
	tables := slices.Clone(FilterTables(all, m.cfg.Mode, m.cfg.Filter))
	if m.cfg.objectMode() && len(m.cfg.TableNames) > 0 {
		tables = nil
		for _, t := range m.cfg.TableNames {
			if slices.Contains(all, t) && !slices.Contains(tables, t) {
				tables = append(tables, t)
			}
		}
	}
	sort.Strings(tables)
	m.freezeDataValidation(ctx, tables)
	p := &oraclePlan{tables: tables, info: map[string]*source.TableDDLInfo{}, ddl: map[string][]dialect.Statement{}}
	selected := map[string]bool{}
	names := map[string]string{}
	objects := []target.OracleObject{}
	add := func(kind, name, table, origin string) error {
		if e := dialect.ValidateOracleName(name); e != nil {
			return e
		}
		ns := kind
		if kind == "TABLE" || kind == "VIEW" || kind == "SEQUENCE" {
			ns = "RELATION"
		}
		key := ns + "\x00" + name
		if old, exists := names[key]; exists {
			return fmt.Errorf("Oracle 名称冲突: %s 与 %s 映射为 %s", old, origin, name)
		}
		names[key] = origin
		objects = append(objects, target.OracleObject{Kind: kind, Name: name, Table: table})
		return nil
	}
	rebuild := !m.cfg.objectMode() && m.cfg.Content != "data_only"
	for _, table := range tables {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		selected[table] = true
		if err = add("TABLE", m.objName(table), m.objName(table), table); err != nil {
			return nil, err
		}
		info, e := m.reader.GetTableDDLInfo(ctx, table)
		if e != nil {
			return nil, fmt.Errorf("读取表 %s: %w", table, e)
		}
		if info == nil || len(info.Columns) == 0 {
			return nil, fmt.Errorf("表 %s 无列元数据", table)
		}
		p.info[m.objName(table)] = info
		colNames := map[string]bool{}
		for _, c := range info.Columns {
			name := m.objName(c.Name)
			if err = dialect.ValidateOracleName(name); err != nil {
				return nil, fmt.Errorf("表 %s: %w", table, err)
			}
			if colNames[name] {
				return nil, fmt.Errorf("表 %s 的列名转大写后冲突: %s", table, name)
			}
			colNames[name] = true
			if !m.cfg.objectMode() {
				mapped, e := typemap.OracleType(c, m.cfg.UseNvarchar2, d.CharBytes, d.NCharBytes)
				if e != nil {
					return nil, fmt.Errorf("表 %s: %w", table, e)
				}
				if rebuild {
					warn := ""
					switch c.DataType {
					case "enum", "set":
						warn = "ENUM/SET 保存为字符串，取值范围约束未迁移"
					case "varchar", "char":
						if mapped == "CLOB" || mapped == "NCLOB" {
							warn = "字符列超过 Oracle 安全容量，使用 " + mapped
						}
					}
					if warn != "" {
						p.warnings = append(p.warnings, valueconv.MigrationWarning{Table: m.objName(table), Column: name, Reason: warn, Count: 1})
					}
					if c.Default != nil && (*c.Default == "" || ((c.DataType == "date" || c.DataType == "datetime" || c.DataType == "timestamp") && valueconv.OracleZeroDate(*c.Default))) {
						p.warnings = append(p.warnings, valueconv.MigrationWarning{Table: m.objName(table), Column: name, Reason: "空字符串/零日期默认值转 NULL", Count: 1})
					}
				}
			}
		}
		if rebuild {
			stmts, e := d.CreateTableStatements(m.cfg.TargetSchema, info, "mysql", dialect.TypeOpt{CharInLength: true, UseNvarchar2: m.cfg.UseNvarchar2}, m.objName)
			if e != nil {
				return nil, e
			}
			p.ddl[table] = stmts
		}
	}
	// Tables are names to validate in every mode, but only rebuilt tables are DDL.
	if !rebuild {
		objects = nil
	}
	want := func(kind string) bool {
		return m.cfg.Content != "data_only" && (!m.cfg.objectMode() || slices.Contains(m.cfg.Objects, kind))
	}
	if want("primary_keys") {
		pks, e := m.reader.GetPrimaryKeys(ctx)
		if e != nil {
			return nil, e
		}
		for _, i := range pks {
			if !selected[i.TableName] {
				continue
			}
			i = oracleIndexNames(i, m.objName)
			p.pks = append(p.pks, i)
			if e = add("CONSTRAINT", d.IndexName(i), i.TableName, "主键 "+i.TableName); e != nil {
				return nil, e
			}
			if e = add("INDEX", d.IndexName(i), i.TableName, "主键索引 "+i.TableName); e != nil {
				return nil, e
			}
		}
	}
	if want("indexes") {
		idxs, e := m.reader.GetIndexes(ctx)
		if e != nil {
			return nil, e
		}
		for _, i := range idxs {
			if !selected[i.TableName] {
				continue
			}
			i = oracleIndexNames(i, m.objName)
			p.indexes = append(p.indexes, i)
			if e = add("INDEX", d.IndexName(i), i.TableName, "索引 "+i.TableName+"."+i.IndexName); e != nil {
				return nil, e
			}
		}
	}
	if want("sequences") {
		seqs, e := m.reader.GetSequences(ctx)
		if e != nil {
			return nil, e
		}
		for _, s := range seqs {
			if !selected[s.TableName] {
				continue
			}
			s.TableName, s.ColumnName = m.objName(s.TableName), m.objName(s.ColumnName)
			if _, e = dialect.OracleSequenceStart(s); e != nil {
				return nil, e
			}
			p.sequences = append(p.sequences, s)
			if e = add("SEQUENCE", d.SequenceName(s), s.TableName, "自增 "+s.TableName+"."+s.ColumnName); e != nil {
				return nil, e
			}
			if e = add("TRIGGER", d.TriggerName(s), s.TableName, "自增触发器 "+s.TableName+"."+s.ColumnName); e != nil {
				return nil, e
			}
		}
	}
	if want("foreign_keys") {
		fks, e := m.reader.GetForeignKeys(ctx)
		if e != nil {
			return nil, e
		}
		for _, f := range fks {
			if !selected[f.TableName] {
				continue
			}
			if !m.cfg.objectMode() && !selected[f.RefTable] {
				f.Unsupported = "引用表未纳入迁移范围，外键未创建"
			}
			f.TableName, f.RefTable, f.ConstraintName = m.objName(f.TableName), m.objName(f.RefTable), m.objName(f.ConstraintName)
			f.Columns = oracleNames(f.Columns, m.objName)
			f.RefColumns = oracleNames(f.RefColumns, m.objName)
			p.fks = append(p.fks, f)
			if e = add("CONSTRAINT", d.ForeignKeyName(f), f.TableName, "外键 "+f.TableName+"."+f.ConstraintName); e != nil {
				return nil, e
			}
		}
	}
	if want("comments") {
		comments, e := m.reader.GetComments(ctx)
		if e != nil {
			return nil, e
		}
		for _, c := range comments {
			if selected[c.TableName] {
				c.TableName = m.objName(c.TableName)
				if c.ColumnName != "" {
					c.ColumnName = m.objName(c.ColumnName)
				}
				p.comments = append(p.comments, c)
			}
		}
	}
	if rebuild {
		for _, index := range append(slices.Clone(p.pks), p.indexes...) {
			if index.Unsupported != "" {
				return nil, fmt.Errorf("表 %s 索引 %s: %s", index.TableName, index.IndexName, index.Unsupported)
			}
			for _, column := range p.info[index.TableName].Columns {
				if !slices.Contains(index.Columns, m.objName(column.Name)) {
					continue
				}
				mapped, e := typemap.OracleType(column, m.cfg.UseNvarchar2, d.CharBytes, d.NCharBytes)
				if e != nil {
					return nil, e
				}
				if mapped == "CLOB" || mapped == "NCLOB" || mapped == "BLOB" {
					return nil, fmt.Errorf("表 %s 列 %s 映射为 %s，无法等价创建索引 %s", index.TableName, column.Name, mapped, index.IndexName)
				}
			}
		}
		for _, foreignKey := range p.fks {
			if e := dialect.ValidateOracleForeignKey(foreignKey); e != nil {
				return nil, fmt.Errorf("表 %s 外键 %s: %w", foreignKey.TableName, foreignKey.ConstraintName, e)
			}
		}
	}
	if rebuild && m.cfg.Mode != "include" {
		views, e := m.readOracleViews(ctx)
		if e != nil {
			return nil, e
		}
		for _, v := range views {
			if e = validateOracleViewColumns(v, m.objName); e != nil {
				return nil, e
			}
			if e = add("VIEW", m.objName(v.ViewName), "", "视图 "+v.ViewName); e != nil {
				return nil, e
			}
			for _, column := range v.Columns {
				objects[len(objects)-1].Columns = append(objects[len(objects)-1].Columns, m.objName(column.Name))
			}
		}
		p.views = m.planOracleViews(views)
		if m.cfg.Mode == "exclude" {
			excluded := map[string]bool{}
			for _, t := range all {
				if !selected[t] {
					excluded[m.objName(t)] = true
				}
			}
			for i := range p.views {
				for _, dep := range p.views[i].deps {
					if excluded[m.objName(dep)] {
						p.views[i].err = fmt.Errorf("视图依赖被排除的表 %s", dep)
					}
				}
			}
		}
	}
	inspector, ok := m.writer.(oracleInspector)
	if !ok {
		return nil, fmt.Errorf("Oracle Writer 缺少目标预检能力")
	}
	if err = inspector.PreflightOracle(ctx, p.info, objects, p.sequences, rebuild, m.cfg.Content == "data_only"); err != nil {
		return nil, err
	}
	return p, nil
}

func oracleNames(in []string, name func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = name(s)
	}
	return out
}
func oracleIndexNames(i source.IndexInfo, name func(string) string) source.IndexInfo {
	i.TableName, i.IndexName = name(i.TableName), name(i.IndexName)
	i.Columns = oracleNames(i.Columns, name)
	return i
}

func (m *Migrator) runOracle(ctx context.Context) (report MigrationReport) {
	report = newMigrationReport()
	start := time.Now()
	defer func() {
		if w, ok := m.writer.(interface {
			MigrationWarnings() []valueconv.MigrationWarning
		}); ok {
			report.Warnings = append(report.Warnings, w.MigrationWarnings()...)
		}
		for _, w := range report.Warnings {
			m.log.Warnf("[%s.%s] %s（%d）", w.Table, w.Column, w.Reason, w.Count)
		}
	}()
	if n, e := m.reader.GetTriggerCount(ctx); e == nil {
		report.Triggers.Total = n
	} else {
		report.Triggers.Total = -1
	}
	p, err := m.prepareOracle(ctx)
	if err != nil {
		m.log.Errorf("Oracle 迁移预检失败，未执行目标 DDL: %v", err)
		report.Tables.Total = 1
		report.Tables.Failed = 1
		report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: "Oracle 迁移预检", Error: err.Error()})
		return report
	}
	if ctx.Err() != nil {
		return report
	}
	m.log.Infof("开始 MySQL → Oracle 迁移，共 %d 张表", len(p.tables))
	failed := map[string]bool{}
	m.logPreparationComplete(ctx)
	if !m.cfg.objectMode() && m.cfg.Content != "data_only" {
		m.log.Info("=== 创建 Oracle 表结构 ===")
		for _, t := range p.tables {
			if ctx.Err() != nil {
				return report
			}
			e := m.repairTable(ctx, t, p.ddl[t])
			m.recordOracleResult(&report.Tables, m.objName(t), dialect.JoinSQL(p.ddl[t]), e)
			failed[m.objName(t)] = e != nil
			if e == nil {
				for _, warning := range p.warnings {
					if warning.Table == m.objName(t) {
						report.Warnings = append(report.Warnings, warning)
					}
				}
			}
		}
	}
	dataOK := map[string]bool{}
	if !m.cfg.objectMode() && m.cfg.Content != "schema_only" {
		m.log.Info("=== 写入 Oracle 数据 ===")
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, m.cfg.MaxParallel)
		for _, table := range p.tables {
			if failed[m.objName(table)] {
				continue
			}
			if ctx.Err() != nil {
				break
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break
			}
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(t string) {
				defer wg.Done()
				defer func() { <-sem }()
				ok, text := m.migrateTableData(ctx, t)
				var e error
				if !ok {
					e = fmt.Errorf("%s", text)
				}
				mu.Lock()
				defer mu.Unlock()
				dataOK[t] = ok
				if ok {
					m.markDataValidationCopied(t)
				}
				m.recordOracleResult(&report.Data, m.objName(t), "", e)
			}(table)
		}
		wg.Wait()
	}
	if ctx.Err() != nil {
		return report
	}
	if m.cfg.Content != "data_only" {
		m.log.Info("=== 创建 Oracle 主键、自增、索引、外键和注释 ===")
		d := m.writer.Dialect().(*dialect.OracleDialect)
		sourceTables := map[string]string{}
		for _, table := range p.tables {
			sourceTables[m.objName(table)] = table
		}
		recordIndex := func(category *CategoryReport, i source.IndexInfo) {
			ddl, stage := "", "execute"
			if i.Unsupported == "" {
				ddl = dialect.JoinSQL(d.IndexStatements(m.cfg.TargetSchema, i))
			} else {
				stage = "unsupported"
			}
			err := m.repairIndex(ctx, i)
			m.recordOracleResult(category, d.IndexName(i), ddl, err)
			if err != nil {
				item := &category.Items[len(category.Items)-1]
				item.Table, item.Stage = sourceTables[i.TableName], stage
			}
		}
		for _, i := range p.pks {
			if ctx.Err() != nil {
				return report
			}
			if failed[i.TableName] {
				continue
			}
			recordIndex(&report.PrimaryKeys, i)
		}
		for _, s := range p.sequences {
			if ctx.Err() != nil {
				return report
			}
			if failed[s.TableName] {
				continue
			}
			sql := dialect.JoinSQL(d.SequenceStatements(m.cfg.TargetSchema, s, ""))
			e := m.repairSequence(ctx, s, "")
			m.recordOracleResult(&report.Sequences, d.SequenceName(s), sql, e)
		}
		for _, i := range p.indexes {
			if ctx.Err() != nil {
				return report
			}
			if failed[i.TableName] {
				continue
			}
			recordIndex(&report.Indexes, i)
		}
		for _, f := range p.fks {
			if ctx.Err() != nil {
				return report
			}
			if failed[f.TableName] {
				continue
			}
			var e error
			if failed[f.RefTable] {
				e = fmt.Errorf("引用表 %s 建表失败", f.RefTable)
			} else {
				e = m.repairFK(ctx, f)
			}
			m.recordOracleResult(&report.Constraints, d.ForeignKeyName(f), dialect.JoinSQL(d.ForeignKeyStatements(m.cfg.TargetSchema, f)), e)
		}
		for _, c := range p.comments {
			if ctx.Err() != nil {
				return report
			}
			if failed[c.TableName] {
				continue
			}
			e := m.repairComment(ctx, c)
			name := c.TableName
			if c.ColumnName != "" {
				name += "." + c.ColumnName
			}
			m.recordOracleResult(&report.Comments, name, dialect.JoinSQL(d.CommentStatements(m.cfg.TargetSchema, c)), e)
		}
		for i := range p.views {
			for _, dep := range p.views[i].deps {
				if failed[m.objName(dep)] {
					p.views[i].err = fmt.Errorf("依赖表 %s 建表失败", dep)
				}
			}
		}
		for _, r := range m.executeOracleViews(ctx, p.views) {
			var e error
			if r.Error != "" {
				e = fmt.Errorf("%s", r.Error)
			}
			m.recordOracleResult(&report.Views, r.Name, r.DDL, e)
		}
	}
	for _, t := range p.tables {
		if !dataOK[t] || ctx.Err() != nil {
			continue
		}
		src, e1 := m.reader.CountRows(ctx, t)
		dst, e2 := m.writer.CountRows(ctx, m.objName(t))
		if e1 != nil || e2 != nil {
			err := fmt.Errorf("行数校验失败，源端: %v; 目标端: %v", e1, e2)
			report.Data.Success--
			report.Data.Failed++
			report.Data.Items = append(report.Data.Items, ObjectResult{Name: m.objName(t), Error: err.Error()})
			m.log.Errorf("[%s] %v", t, err)
			continue
		}
		report.RowCounts = append(report.RowCounts, TableRowCount{Table: m.objName(t), Src: src, Dst: dst, Match: src == dst})
	}
	m.logMigrationCompletion("MySQL → Oracle 迁移结束，耗时 %s", time.Since(start).Round(time.Second))
	return report
}
func (m *Migrator) recordOracleResult(c *CategoryReport, name, ddl string, err error) {
	c.Total++
	if err != nil {
		c.Failed++
		c.Items = append(c.Items, ObjectResult{Name: name, DDL: ddl, Error: err.Error()})
		m.log.Errorf("[%s] %v", name, err)
	} else {
		c.Success++
		m.log.DDLf("%s ... OK", name)
	}
}

func (m *Migrator) planOracleViews(views []source.ViewInfo) []oracleViewPlan {
	list := make([]oracleViewPlan, len(views))
	names := map[string]bool{}
	for i, v := range views {
		names[m.objName(v.ViewName)] = true
		sql, deps, e := oracleview.Convert(v.Definition, m.oracleSourceSchema(), m.cfg.TargetSchema, m.cfg.StripViewSchemas)
		v.ViewName = m.objName(v.ViewName)
		v.Columns = append([]source.ColumnInfo(nil), v.Columns...)
		for ci := range v.Columns {
			v.Columns[ci].Name = m.objName(v.Columns[ci].Name)
		}
		if e == nil {
			v.Definition = sql
		}
		list[i] = oracleViewPlan{view: v, deps: deps, err: e, ddl: dialect.JoinSQL(m.writer.Dialect().ViewStatements(m.cfg.TargetSchema, v))}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].view.ViewName < list[j].view.ViewName })
	out := []oracleViewPlan{}
	done := map[string]bool{}
	for len(list) > 0 {
		progress := false
		next := []oracleViewPlan{}
		for _, v := range list {
			ready := true
			for _, dep := range v.deps {
				n := m.objName(dep)
				if names[n] && !done[n] {
					ready = false
				}
			}
			if !ready {
				next = append(next, v)
				continue
			}
			out = append(out, v)
			done[v.view.ViewName] = true
			progress = true
		}
		if !progress {
			for _, v := range next {
				v.err = fmt.Errorf("视图依赖循环或依赖循环中的对象")
				out = append(out, v)
			}
			break
		}
		list = next
	}
	return out
}
func (m *Migrator) executeOracleViews(ctx context.Context, plans []oracleViewPlan) []ObjectResult {
	out := []ObjectResult{}
	failed := map[string]bool{}
	for _, v := range plans {
		if ctx.Err() != nil {
			break
		}
		e := v.err
		for _, dep := range v.deps {
			if failed[m.objName(dep)] {
				e = fmt.Errorf("依赖视图 %s 迁移失败", dep)
			}
		}
		if e == nil {
			e = m.repairView(ctx, v.view)
		}
		r := ObjectResult{Name: v.view.ViewName, DDL: v.ddl}
		if e != nil {
			r.Error = e.Error()
			failed[v.view.ViewName] = true
		}
		out = append(out, r)
	}
	return out
}
func (m *Migrator) migrateOracleViews(ctx context.Context, names []string) []ObjectResult {
	views, err := m.readOracleViews(ctx)
	if err != nil {
		return []ObjectResult{{Name: "视图元数据", Error: err.Error()}}
	}
	selected := []source.ViewInfo{}
	seen := map[string]bool{}
	objects := []target.OracleObject{}
	for _, v := range views {
		if !slices.Contains(names, v.ViewName) {
			continue
		}
		n := m.objName(v.ViewName)
		if err = validateOracleViewColumns(v, m.objName); err != nil {
			return []ObjectResult{{Name: n, Error: err.Error()}}
		}
		if err = dialect.ValidateOracleName(n); err != nil {
			return []ObjectResult{{Name: n, Error: err.Error()}}
		}
		if seen[n] {
			return []ObjectResult{{Name: n, Error: "视图名称转大写后冲突"}}
		}
		seen[n] = true
		selected = append(selected, v)
		objects = append(objects, target.OracleObject{Kind: "VIEW", Name: n})
		for _, column := range v.Columns {
			objects[len(objects)-1].Columns = append(objects[len(objects)-1].Columns, m.objName(column.Name))
		}
	}
	for _, name := range names {
		found := false
		for _, v := range selected {
			if v.ViewName == name {
				found = true
			}
		}
		if !found {
			return []ObjectResult{{Name: name, Error: "源视图不存在或不可见"}}
		}
	}
	w, ok := m.writer.(oracleInspector)
	if !ok {
		return []ObjectResult{{Name: "Oracle 预检", Error: "Writer 缺少预检能力"}}
	}
	if err = w.PreflightOracle(ctx, nil, objects, nil, false, false); err != nil {
		return []ObjectResult{{Name: "Oracle 预检", Error: err.Error()}}
	}
	return m.executeOracleViews(ctx, m.planOracleViews(selected))
}

func validateOracleViewColumns(v source.ViewInfo, name func(string) string) error {
	seen := map[string]bool{}
	for _, c := range v.Columns {
		n := name(c.Name)
		if e := dialect.ValidateOracleName(n); e != nil {
			return fmt.Errorf("视图 %s: %w", v.ViewName, e)
		}
		if seen[n] {
			return fmt.Errorf("视图 %s 的列名转大写后冲突: %s", v.ViewName, n)
		}
		seen[n] = true
	}
	return nil
}
