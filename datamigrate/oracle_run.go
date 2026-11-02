package datamigrate

import (
	"context"
	"errors"
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
	sourceName string
	view       source.ViewInfo
	ddl        string
	deps       []string
	err        error
	stage      string
}
type oraclePlan struct {
	tables       []string
	tableErrors  map[string]error
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
	all, err := m.migrationTables(ctx)
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
	p := &oraclePlan{tableErrors: map[string]error{}, tables: tables, info: map[string]*source.TableDDLInfo{}, ddl: map[string][]dialect.Statement{}}
	rejectTarget := func(targetName string, e error) {
		for _, t := range tables {
			if m.objName(t) == targetName {
				p.tableErrors[t] = e
				m.blockMigrationTable(t, "preflight", e)
			}
		}
	}
	viewErrors := map[string]error{}
	viewSources := map[string][]string{}
	rejectView := func(targetName string, e error) bool {
		changed := false
		for _, name := range viewSources[targetName] {
			changed = changed || viewErrors[name] == nil
			viewErrors[name] = e
			m.blockMigrationObject(ObjectRef{Kind: "view", Name: name}, e)
		}
		return changed
	}
	selected := map[string]bool{}
	names := map[string]string{}
	nameTables := map[string]string{}
	nameKinds := map[string]string{}
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
			e := fmt.Errorf("Oracle 名称冲突: %s 与 %s 映射为 %s", old, origin, name)
			rejectTarget(nameTables[key], e)
			rejectTarget(table, e)
			if nameKinds[key] == "VIEW" || kind == "VIEW" {
				rejectView(name, e)
			}
			return e
		}
		names[key] = origin
		nameTables[key] = table
		nameKinds[key] = kind
		objects = append(objects, target.OracleObject{Kind: kind, Name: name, Table: table})
		return nil
	}
	rebuild := !m.cfg.objectMode() && m.cfg.Content != "data_only"
	for _, table := range tables {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		selected[table] = true
		nameErr := add("TABLE", m.objName(table), m.objName(table), table)
		if e := m.migrationTableError(table); e != nil {
			p.tableErrors[table] = e
			continue
		}
		tableErr := func() error {
			if nameErr != nil {
				return nameErr
			}
			info, e := m.sourceTableDDLInfo(ctx, table)
			if e != nil {
				return fmt.Errorf("读取表 %s: %w", table, e)
			}
			if info == nil || len(info.Columns) == 0 {
				return fmt.Errorf("表 %s 无列元数据", table)
			}
			p.info[m.objName(table)] = info
			colNames := map[string]bool{}
			for _, c := range info.Columns {
				name := m.objName(c.Name)
				if err = dialect.ValidateOracleName(name); err != nil {
					return fmt.Errorf("表 %s: %w", table, err)
				}
				if colNames[name] {
					return fmt.Errorf("表 %s 的列名转大写后冲突: %s", table, name)
				}
				colNames[name] = true
				if !m.cfg.objectMode() {
					mapped, e := typemap.OracleType(c, m.cfg.UseNvarchar2, d.CharBytes, d.NCharBytes)
					if e != nil {
						return fmt.Errorf("表 %s: %w", table, e)
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
					return e
				}
				stmts, e = m.partitionStatements(table, stmts)
				if e != nil {
					return e
				}
				p.ddl[table] = stmts
			}
			return nil
		}()
		if tableErr != nil {
			if m.stopMigrationTask(ctx, tableErr) {
				return nil, tableErr
			}
			p.tableErrors[table] = tableErr
			m.blockMigrationTable(table, "preflight", tableErr)
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
				rejectTarget(i.TableName, e)
			}
			if e = add("INDEX", d.IndexName(i), i.TableName, "主键索引 "+i.TableName); e != nil {
				rejectTarget(i.TableName, e)
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
				rejectTarget(i.TableName, e)
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
				rejectTarget(s.TableName, e)
				continue
			}
			p.sequences = append(p.sequences, s)
			if e = add("SEQUENCE", d.SequenceName(s), s.TableName, "自增 "+s.TableName+"."+s.ColumnName); e != nil {
				rejectTarget(s.TableName, e)
			}
			if e = add("TRIGGER", d.TriggerName(s), s.TableName, "自增触发器 "+s.TableName+"."+s.ColumnName); e != nil {
				rejectTarget(s.TableName, e)
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
				rejectTarget(f.TableName, e)
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
			if m.migrationTargetTableError(index.TableName) != nil {
				continue
			}
			if index.Unsupported != "" {
				rejectTarget(index.TableName, fmt.Errorf("表 %s 索引 %s: %s", index.TableName, index.IndexName, index.Unsupported))
				continue
			}
			info := p.info[index.TableName]
			if info == nil {
				rejectTarget(index.TableName, fmt.Errorf("表 %s 无列元数据，未检查索引 %s", index.TableName, index.IndexName))
				continue
			}
			for _, column := range info.Columns {
				if !slices.Contains(index.Columns, m.objName(column.Name)) {
					continue
				}
				mapped, e := typemap.OracleType(column, m.cfg.UseNvarchar2, d.CharBytes, d.NCharBytes)
				if e != nil {
					rejectTarget(index.TableName, e)
					continue
				}
				if mapped == "CLOB" || mapped == "NCLOB" || mapped == "BLOB" {
					rejectTarget(index.TableName, fmt.Errorf("表 %s 列 %s 映射为 %s，无法等价创建索引 %s", index.TableName, column.Name, mapped, index.IndexName))
				}
			}
		}
		for _, foreignKey := range p.fks {
			if e := dialect.ValidateOracleForeignKey(foreignKey); e != nil {
				rejectTarget(foreignKey.TableName, fmt.Errorf("表 %s 外键 %s: %w", foreignKey.TableName, foreignKey.ConstraintName, e))
			}
		}
	}
	if rebuild && m.cfg.Mode != "include" {
		views, e := m.readOracleViews(ctx)
		if e != nil {
			return nil, e
		}
		for _, v := range views {
			viewSources[m.objName(v.ViewName)] = append(viewSources[m.objName(v.ViewName)], v.ViewName)
		}
		for _, v := range views {
			if e = validateOracleViewColumns(v, m.objName); e != nil {
				rejectView(m.objName(v.ViewName), e)
			}
			if e = add("VIEW", m.objName(v.ViewName), "", "视图 "+v.ViewName); e != nil {
				rejectView(m.objName(v.ViewName), e)
				continue
			}
			for _, column := range v.Columns {
				objects[len(objects)-1].Columns = append(objects[len(objects)-1].Columns, m.objName(column.Name))
			}
		}
		p.views = m.planOracleViews(views)
		for i := range p.views {
			if p.views[i].err != nil {
				rejectView(p.views[i].view.ViewName, p.views[i].err)
			}
			if e := viewErrors[p.views[i].sourceName]; e != nil {
				p.views[i].err = e
			}
		}
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
	for {
		healthyInfo := map[string]*source.TableDDLInfo{}
		for name, info := range p.info {
			if m.migrationTargetTableError(name) == nil {
				healthyInfo[name] = info
			}
		}
		healthyObjects := slices.DeleteFunc(slices.Clone(objects), func(o target.OracleObject) bool {
			return o.Table != "" && m.migrationTargetTableError(o.Table) != nil || o.Kind == "VIEW" && len(viewSources[o.Name]) > 0 && viewErrors[viewSources[o.Name][0]] != nil
		})
		healthySequences := slices.DeleteFunc(slices.Clone(p.sequences), func(seq source.SequenceInfo) bool { return m.migrationTargetTableError(seq.TableName) != nil })
		if len(healthyInfo) == 0 && len(healthyObjects) == 0 {
			break
		}
		err = inspector.PreflightOracle(ctx, healthyInfo, healthyObjects, healthySequences, rebuild, m.cfg.Content == "data_only")
		if err == nil {
			break
		}
		if m.stopMigrationTask(ctx, err) {
			return nil, err
		}
		var local *target.OracleTablePreflightError
		if errors.As(err, &local) {
			if _, selected := healthyInfo[local.Table]; !selected {
				return nil, err
			}
			rejectTarget(local.Table, err)
			continue
		}
		var object *target.OracleObjectPreflightError
		if !errors.As(err, &object) || object.Kind != "VIEW" || !rejectView(object.Name, err) {
			return nil, err
		}
	}
	for i := range p.views {
		if e := viewErrors[p.views[i].sourceName]; e != nil {
			p.views[i].err = e
		}
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
		if m.stopMigrationTask(ctx, e) {
			return report
		}
		report.Triggers.Total = -1
	}
	p, err := m.prepareOracle(ctx)
	if err != nil {
		m.stopMigrationTask(ctx, migrationTaskFailure{err: err})
		m.log.Errorf("Oracle 迁移预检失败，未执行目标 DDL: %v", err)
		report.Tables.Total = 1
		report.Tables.Failed = 1
		report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: "Oracle 迁移预检", Stage: "task", Error: err.Error()})
		return report
	}
	if ctx.Err() != nil {
		return report
	}
	m.log.Infof("开始 MySQL → Oracle 迁移，共 %d 张表", len(p.tables))
	failed := map[string]bool{}
	for _, table := range p.tables {
		e := p.tableErrors[table]
		if e == nil {
			continue
		}
		failed[m.objName(table)] = true
		m.recordOracleResult(&report.Tables, m.objName(table), "", e)
		item := &report.Tables.Items[len(report.Tables.Items)-1]
		item.Stage, item.Table, item.SourceName = "preflight", table, table
		item.TargetName, item.TargetTable, item.TargetSchema = m.objName(table), m.objName(table), m.cfg.TargetSchema
	}
	m.logPreparationComplete(ctx)
	completed := map[string]bool{}
	if !m.cfg.objectMode() && m.cfg.Content != "data_only" {
		m.log.Info("=== 创建 Oracle 表结构 ===")
		for _, t := range p.tables {
			if failed[m.objName(t)] {
				continue
			}
			if ctx.Err() != nil {
				return report
			}
			e := m.repairTable(ctx, t, p.ddl[t])
			m.recordOracleResult(&report.Tables, m.objName(t), dialect.JoinSQL(p.ddl[t]), e)
			failed[m.objName(t)] = e != nil
			if e != nil {
				m.blockMigrationTable(t, "execute", e)
				item := &report.Tables.Items[len(report.Tables.Items)-1]
				item.Stage, item.Table, item.SourceName = "execute", t, t
				item.TargetName, item.TargetTable, item.TargetSchema = m.objName(t), m.objName(t), m.cfg.TargetSchema
			}
			if m.stopMigrationTask(ctx, e) {
				return report
			}
			if e != nil {
				if !m.protectRetainedMigrationTables(ctx, p.tables, &report, completed) {
					return report
				}
				for _, table := range p.tables {
					if m.migrationTableBlocked(table) {
						failed[m.objName(table)] = true
					}
				}
			}
			if e == nil {
				completed[t] = true
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
		sem := make(chan struct{}, max(1, m.cfg.MaxParallel))
	dataLoop:
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
				break dataLoop
			}
			if ctx.Err() != nil {
				<-sem
				break dataLoop
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
				if !ok {
					m.blockMigrationTable(t, "data", e)
				}
				if ok {
					m.markDataValidationCopied(t)
				}
				m.recordOracleResult(&report.Data, m.objName(t), "", e)
			}(table)
		}
		wg.Wait()
		for _, table := range p.tables {
			if m.migrationTableBlocked(table) {
				failed[m.objName(table)] = true
			}
		}
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
			m.stopMigrationTask(ctx, err)
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
				report.PrimaryKeys.Total++
				m.recordBlockedObject(&report.PrimaryKeys, d.IndexName(i), sourceTables[i.TableName], "依赖表 "+i.TableName+" 迁移失败")
				continue
			}
			recordIndex(&report.PrimaryKeys, i)
		}
		for _, s := range p.sequences {
			if ctx.Err() != nil {
				return report
			}
			if failed[s.TableName] {
				report.Sequences.Total++
				m.recordBlockedObject(&report.Sequences, d.SequenceName(s), sourceTables[s.TableName], "依赖表 "+s.TableName+" 迁移失败")
				continue
			}
			sql := dialect.JoinSQL(d.SequenceStatements(m.cfg.TargetSchema, s, ""))
			e := m.repairSequence(ctx, s, "")
			m.recordOracleResult(&report.Sequences, d.SequenceName(s), sql, e)
			if m.stopMigrationTask(ctx, e) {
				return report
			}
		}
		for _, i := range p.indexes {
			if ctx.Err() != nil {
				return report
			}
			if failed[i.TableName] {
				report.Indexes.Total++
				m.recordBlockedObject(&report.Indexes, d.IndexName(i), sourceTables[i.TableName], "依赖表 "+i.TableName+" 迁移失败")
				continue
			}
			recordIndex(&report.Indexes, i)
		}
		for _, f := range p.fks {
			if ctx.Err() != nil {
				return report
			}
			if failed[f.TableName] {
				report.Constraints.Total++
				m.recordBlockedObject(&report.Constraints, d.ForeignKeyName(f), sourceTables[f.TableName], "依赖表 "+f.TableName+" 迁移失败")
				continue
			}
			var e error
			if failed[f.RefTable] {
				report.Constraints.Total++
				m.recordBlockedObject(&report.Constraints, d.ForeignKeyName(f), sourceTables[f.TableName], "引用表 "+f.RefTable+" 迁移失败")
				continue
			} else {
				e = m.repairFK(ctx, f)
			}
			m.recordOracleResult(&report.Constraints, d.ForeignKeyName(f), dialect.JoinSQL(d.ForeignKeyStatements(m.cfg.TargetSchema, f)), e)
			if m.stopMigrationTask(ctx, e) {
				return report
			}
		}
		for _, c := range p.comments {
			if ctx.Err() != nil {
				return report
			}
			if failed[c.TableName] {
				report.Comments.Total++
				m.recordBlockedObject(&report.Comments, c.TableName+"."+c.ColumnName, sourceTables[c.TableName], "依赖表 "+c.TableName+" 迁移失败")
				continue
			}
			e := m.repairComment(ctx, c)
			name := c.TableName
			if c.ColumnName != "" {
				name += "." + c.ColumnName
			}
			m.recordOracleResult(&report.Comments, name, dialect.JoinSQL(d.CommentStatements(m.cfg.TargetSchema, c)), e)
			if m.stopMigrationTask(ctx, e) {
				return report
			}
		}
		for i := range p.views {
			for _, dep := range p.views[i].deps {
				if failed[m.objName(dep)] {
					p.views[i].err = fmt.Errorf("依赖表 %s 建表失败", dep)
					p.views[i].stage = "dependency"
				}
			}
		}
		for _, r := range m.executeOracleViews(ctx, p.views) {
			report.Views.Total++
			if r.Error != "" {
				report.Views.Failed++
				report.Views.Items = append(report.Views.Items, r)
			} else {
				report.Views.Success++
			}
		}
	}
	for _, t := range p.tables {
		if !dataOK[t] || ctx.Err() != nil {
			continue
		}
		src, e1 := m.reader.CountRows(ctx, t)
		if m.stopMigrationTask(ctx, e1) {
			return report
		}
		dst, e2 := m.writer.CountRows(ctx, m.objName(t))
		if m.stopMigrationTask(ctx, e2) {
			return report
		}
		if e1 != nil || e2 != nil {
			err := fmt.Errorf("行数校验失败，源端: %v; 目标端: %v", e1, e2)
			report.Data.Success--
			report.Data.Failed++
			report.Data.Items = append(report.Data.Items, ObjectResult{Name: m.objName(t), Error: err.Error()})
			m.blockMigrationTable(t, "validation", err)
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
	counts := map[string]int{}
	for _, view := range views {
		counts[m.objName(view.ViewName)]++
	}
	for i, v := range views {
		sourceName := v.ViewName
		names[m.objName(v.ViewName)] = true
		sql, deps, e := oracleview.Convert(v.Definition, m.oracleSourceSchema(), m.cfg.TargetSchema, m.cfg.StripViewSchemas)
		if err := validateOracleViewColumns(v, m.objName); err != nil {
			e = err
		}
		if err := dialect.ValidateOracleName(m.objName(v.ViewName)); err != nil {
			e = err
		}
		if counts[m.objName(v.ViewName)] > 1 {
			e = fmt.Errorf("视图名称转大写后冲突: %s", m.objName(v.ViewName))
		}
		v.ViewName = m.objName(v.ViewName)
		v.Columns = append([]source.ColumnInfo(nil), v.Columns...)
		for ci := range v.Columns {
			v.Columns[ci].Name = m.objName(v.Columns[ci].Name)
		}
		if e == nil {
			v.Definition = sql
		}
		list[i] = oracleViewPlan{sourceName: sourceName, view: v, deps: deps, err: e, ddl: dialect.JoinSQL(m.writer.Dialect().ViewStatements(m.cfg.TargetSchema, v))}
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
		stage := "execute"
		if e != nil {
			stage = "preflight"
		}
		if v.stage != "" {
			stage = v.stage
		}
		for _, dep := range v.deps {
			if failed[m.objName(dep)] {
				e = fmt.Errorf("依赖视图 %s 迁移失败", dep)
				stage = "dependency"
			}
		}
		if e == nil {
			e = m.repairView(ctx, v.view)
		}
		r := ObjectResult{Name: v.view.ViewName, SourceName: v.sourceName, TargetName: v.view.ViewName, TargetSchema: m.cfg.TargetSchema, DDL: v.ddl}
		if e != nil {
			r.Error = e.Error()
			r.Stage = stage
			failed[v.view.ViewName] = true
			m.blockMigrationObject(ObjectRef{Kind: "view", Name: v.sourceName}, e)
			if m.stopMigrationTask(ctx, e) {
				out = append(out, r)
				break
			}
		}
		out = append(out, r)
	}
	return out
}
func (m *Migrator) migrateOracleViews(ctx context.Context, names []string) []ObjectResult {
	views, err := m.readOracleViews(ctx)
	if err != nil {
		m.stopMigrationTask(ctx, err)
		return []ObjectResult{{Name: "视图元数据", Error: err.Error()}}
	}
	selected := []source.ViewInfo{}
	found := map[string]bool{}
	for _, v := range views {
		if !slices.Contains(names, v.ViewName) {
			continue
		}
		found[v.ViewName] = true
		selected = append(selected, v)
	}
	plans := m.planOracleViews(selected)
	for _, name := range names {
		if !found[name] {
			plans = append(plans, oracleViewPlan{sourceName: name, view: source.ViewInfo{ViewName: m.objName(name)}, err: fmt.Errorf("源视图不存在或不可见")})
		}
	}
	w, ok := m.writer.(oracleInspector)
	if !ok {
		return []ObjectResult{{Name: "Oracle 预检", Error: "Writer 缺少预检能力"}}
	}
	for {
		objects := []target.OracleObject{}
		for _, plan := range plans {
			if plan.err != nil {
				continue
			}
			object := target.OracleObject{Kind: "VIEW", Name: plan.view.ViewName}
			for _, column := range plan.view.Columns {
				object.Columns = append(object.Columns, column.Name)
			}
			objects = append(objects, object)
		}
		if len(objects) == 0 {
			break
		}
		if err = w.PreflightOracle(ctx, nil, objects, nil, false, false); err == nil {
			break
		}
		if m.stopMigrationTask(ctx, err) {
			return []ObjectResult{{Name: "Oracle 预检", Stage: "task", Error: err.Error()}}
		}
		var issue *target.OracleObjectPreflightError
		if !errors.As(err, &issue) || issue.Kind != "VIEW" {
			return []ObjectResult{{Name: "Oracle 预检", Stage: "task", Error: err.Error()}}
		}
		changed := false
		for i := range plans {
			if plans[i].view.ViewName == issue.Name && plans[i].err == nil {
				plans[i].err, changed = err, true
			}
		}
		if !changed {
			return []ObjectResult{{Name: "Oracle 预检", Stage: "task", Error: err.Error()}}
		}
	}
	return m.executeOracleViews(ctx, plans)
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
