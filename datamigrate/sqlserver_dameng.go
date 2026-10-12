package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/tsqldameng"
	"dbgold/datamigrate/typemap"
)

func (m *Migrator) isSQLServerDameng() bool {
	return m.reader.DBType() == "sqlserver" && m.writer.DBType() == "dameng"
}

// Freeze validated metadata for all later phases. In particular, post-DDL never
// re-reads a wider object set than the one checked before the first target write.
type sqlServerDamengSnapshot struct {
	source.Reader
	tables       map[string]*source.TableDDLInfo
	pks, indexes []source.IndexInfo
	seqs         []source.SequenceInfo
	fks          []source.FKInfo
	views        []source.ViewInfo
	comments     []source.CommentInfo
}

func (s *sqlServerDamengSnapshot) GetTableDDLInfo(_ context.Context, t string) (*source.TableDDLInfo, error) {
	v, ok := s.tables[t]
	if !ok {
		return nil, fmt.Errorf("表 %s 不在预检范围", t)
	}
	return v, nil
}
func (s *sqlServerDamengSnapshot) GetPrimaryKeys(context.Context) ([]source.IndexInfo, error) {
	return s.pks, nil
}
func (s *sqlServerDamengSnapshot) GetPrimaryKey(_ context.Context, t string) ([]string, error) {
	for _, p := range s.pks {
		if p.TableName == t {
			return p.Columns, nil
		}
	}
	return nil, nil
}
func (s *sqlServerDamengSnapshot) GetIndexes(context.Context) ([]source.IndexInfo, error) {
	return s.indexes, nil
}
func (s *sqlServerDamengSnapshot) GetSequences(context.Context) ([]source.SequenceInfo, error) {
	return s.seqs, nil
}
func (s *sqlServerDamengSnapshot) GetForeignKeys(context.Context) ([]source.FKInfo, error) {
	return s.fks, nil
}
func (s *sqlServerDamengSnapshot) GetViews(context.Context) ([]source.ViewInfo, error) {
	return s.views, nil
}
func (s *sqlServerDamengSnapshot) GetComments(context.Context) ([]source.CommentInfo, error) {
	return s.comments, nil
}

func (m *Migrator) preflightSQLServerDameng(ctx context.Context, tables []string, report *MigrationReport) bool {
	snapshot := &sqlServerDamengSnapshot{Reader: m.reader, tables: map[string]*source.TableDDLInfo{}}
	ready := true
	issue := func(cat *CategoryReport, name string, err error) {
		ready = false
		cat.Total++
		cat.Failed++
		cat.Items = append(cat.Items, objectResultForError(name, "", err))
		m.log.Errorf("SQL Server → 达梦预检 [%s]: %v", name, err)
	}
	names := map[string]string{}
	checkName := func(name, origin string, namespace map[string]string) error {
		if e := tsqldameng.ValidName(name); e != nil {
			return e
		}
		key := strings.ToUpper(name)
		if prev, ok := namespace[key]; ok {
			return fmt.Errorf("大写命名冲突: %s 与 %s → %s", prev, origin, key)
		}
		namespace[key] = origin
		return nil
	}
	selected := map[string]bool{}
	for _, t := range tables {
		selected[t] = true
	}
	if m.cfg.objectMode() {
		for _, t := range m.cfg.TableNames {
			if !selected[t] {
				issue(&report.Tables, t, fmt.Errorf("不在 dbo 可迁移表范围"))
			}
		}
	}
	if len(m.cfg.StripViewSchemas) > 0 {
		issue(&report.Views, "视图模式", fmt.Errorf("此组合不支持删除跨库/schema 前缀"))
	}
	if m.cfg.TargetSchema != "" {
		if e := tsqldameng.ValidName(m.cfg.TargetSchema); e != nil {
			issue(&report.Tables, "目标 schema", e)
		}
	}
	opt := dialect.TypeOpt{CharInLength: m.cfg.CharInLength, UseNvarchar2: m.cfg.UseNvarchar2}
	for _, t := range tables {
		if e := ctx.Err(); e != nil {
			issue(&report.Tables, t, e)
			return false
		}
		if e := checkName(t, "表 "+t, names); e != nil {
			issue(&report.Tables, t, e)
		}
		info, e := m.reader.GetTableDDLInfo(ctx, t)
		if e != nil {
			issue(&report.Tables, t, e)
			continue
		}
		if info == nil {
			issue(&report.Tables, t, fmt.Errorf("缺少列元数据"))
			continue
		}
		snapshot.tables[t] = info
		if m.cfg.objectMode() || m.cfg.Content == "data_only" {
			if validator, ok := m.writer.(interface {
				ValidateSQLServerDamengExisting(context.Context, *source.TableDDLInfo, dialect.TypeOpt) error
			}); ok {
				if e := validator.ValidateSQLServerDamengExisting(ctx, info, opt); e != nil {
					issue(&report.Tables, t, e)
				}
			}
		}
		cols := map[string]string{}
		for _, c := range info.Columns {
			if e := checkName(c.Name, t+"."+c.Name, cols); e != nil {
				issue(&report.Tables, t, e)
			}
			if e := typemap.ValidateSQLServerDamengColumn(c); e != nil {
				issue(&report.Tables, t, e)
			}
			if !m.cfg.objectMode() && m.cfg.Content != "data_only" && c.Default != nil {
				if _, e := tsqldameng.Default(*c.Default); e != nil {
					issue(&report.Tables, t+"."+c.Name, e)
				}
			}
		}
		if !m.cfg.objectMode() && m.cfg.Content != "data_only" {
			if _, e := m.writer.Dialect().CreateTableStatements(m.cfg.TargetSchema, info, "sqlserver", opt, m.objName); e != nil {
				issue(&report.Tables, t, e)
			}
		}
	}
	want := func(kind string) bool { return !m.cfg.objectMode() || slices.Contains(m.cfg.Objects, kind) }
	// Always cache primary keys: data pagination uses them even in data-only mode.
	pks, e := m.reader.GetPrimaryKeys(ctx)
	if e != nil {
		issue(&report.PrimaryKeys, "主键元数据", e)
	}
	validateIndex := func(idx source.IndexInfo) error {
		if idx.Unsupported != "" {
			return fmt.Errorf("%s", idx.Unsupported)
		}
		info := snapshot.tables[idx.TableName]
		if info == nil {
			return fmt.Errorf("表列元数据不可用")
		}
		for _, col := range idx.Columns {
			found := false
			for _, c := range info.Columns {
				if c.Name == col {
					found = true
					t := typemap.SQLServerToDameng(c, opt.CharInLength, opt.UseNvarchar2)
					if t == "CLOB" || t == "BLOB" {
						return fmt.Errorf("索引列 %s 映射为 LOB，不支持普通索引", col)
					}
					if idx.IsUnique && !idx.IsPrimary && c.IsNullable {
						return fmt.Errorf("唯一索引可空列 %s 的 NULL 语义不能等价迁移", col)
					}
				}
			}
			if !found {
				return fmt.Errorf("索引列 %s 不存在", col)
			}
		}
		if len(idx.Columns) == 0 {
			return fmt.Errorf("索引无键列")
		}
		return nil
	}
	for _, p := range pks {
		if selected[p.TableName] {
			snapshot.pks = append(snapshot.pks, p)
			if want("primary_keys") && m.cfg.Content != "data_only" {
				if e := validateIndex(p); e != nil {
					issue(&report.PrimaryKeys, p.TableName, e)
				}
			}
		}
	}
	if want("sequences") {
		seqs, e := m.reader.GetSequences(ctx)
		if e != nil {
			issue(&report.Sequences, "自增元数据", e)
		}
		for _, seq := range seqs {
			if !selected[seq.TableName] {
				continue
			}
			if seq.Unsupported != "" {
				issue(&report.Sequences, seq.TableName, fmt.Errorf("%s", seq.Unsupported))
				continue
			}
			id := seq.IdentityInfo
			if id == nil {
				issue(&report.Sequences, seq.TableName, fmt.Errorf("缺少自增种子/步长元数据"))
				continue
			}
			bounds, ok := m.reader.(interface {
				IdentityBounds(context.Context, string, string) (*int64, *int64, error)
			})
			if !ok {
				issue(&report.Sequences, seq.TableName, fmt.Errorf("读取器不支持自增边界检查"))
				continue
			}
			lo, hi, e := bounds.IdentityBounds(ctx, seq.TableName, seq.ColumnName)
			if e != nil {
				issue(&report.Sequences, seq.TableName, e)
				continue
			}
			if m.cfg.objectMode() || m.cfg.Content == "data_only" {
				if b, ok := m.writer.(interface {
					SQLServerDamengIdentityBounds(context.Context, string, string) (*int64, *int64, error)
				}); ok {
					targetLow, targetHigh, err := b.SQLServerDamengIdentityBounds(ctx, seq.TableName, seq.ColumnName)
					if err != nil {
						issue(&report.Sequences, seq.TableName, err)
						continue
					}
					if targetLow != nil && (lo == nil || *targetLow < *lo) {
						lo = targetLow
					}
					if targetHigh != nil && (hi == nil || *targetHigh > *hi) {
						hi = targetHigh
					}
				}
			}
			next, e := tsqldameng.IdentityNext(id.Seed, id.Increment, id.Last, lo, hi)
			if e != nil {
				issue(&report.Sequences, seq.TableName, e)
				continue
			}
			info := snapshot.tables[seq.TableName]
			if info == nil {
				continue
			}
			for _, c := range info.Columns {
				if c.Name == seq.ColumnName {
					switch strings.ToLower(c.DataType) {
					case "tinyint":
						if next < 0 || next > 255 {
							e = fmt.Errorf("自增下一值超出 tinyint 范围")
						}
					case "smallint":
						if next < -32768 || next > 32767 {
							e = fmt.Errorf("自增下一值超出 smallint 范围")
						}
					case "int":
						if next < -2147483648 || next > 2147483647 {
							e = fmt.Errorf("自增下一值超出 int 范围")
						}
					}
				}
			}
			if e != nil {
				issue(&report.Sequences, seq.TableName, e)
				continue
			}
			seq.StartValue = next
			snapshot.seqs = append(snapshot.seqs, seq)
		}
	}
	if m.cfg.Content != "data_only" {
		if want("indexes") {
			indexes, e := m.reader.GetIndexes(ctx)
			if e != nil {
				issue(&report.Indexes, "索引元数据", e)
			}
			for _, idx := range indexes {
				if !selected[idx.TableName] {
					continue
				}
				if e := checkName(idx.TableName+"_"+idx.IndexName, "索引 "+idx.TableName+"."+idx.IndexName, names); e != nil {
					issue(&report.Indexes, idx.IndexName, e)
				}
				if e := validateIndex(idx); e != nil {
					issue(&report.Indexes, idx.TableName+"."+idx.IndexName, e)
				}
				snapshot.indexes = append(snapshot.indexes, idx)
			}
		}

		if want("foreign_keys") {
			fks, e := m.reader.GetForeignKeys(ctx)
			if e != nil {
				issue(&report.Constraints, "外键元数据", e)
			}
			for _, f := range fks {
				if !selected[f.TableName] {
					continue
				}
				if e := validateIndex(source.IndexInfo{TableName: f.TableName, Columns: f.Columns}); e != nil {
					issue(&report.Constraints, f.ConstraintName, e)
				}
				if selected[f.RefTable] {
					if e := validateIndex(source.IndexInfo{TableName: f.RefTable, Columns: f.RefColumns}); e != nil {
						issue(&report.Constraints, f.ConstraintName, e)
					}
				}
				if f.Unsupported != "" {
					issue(&report.Constraints, f.ConstraintName, fmt.Errorf("%s", f.Unsupported))
				}
				if !m.cfg.objectMode() && !selected[f.RefTable] {
					issue(&report.Constraints, f.ConstraintName, fmt.Errorf("引用表 %s 不在迁移范围", f.RefTable))
				}
				if e := checkName(f.ConstraintName, "外键 "+f.TableName+"."+f.ConstraintName, names); e != nil {
					issue(&report.Constraints, f.ConstraintName, e)
				}
				snapshot.fks = append(snapshot.fks, f)
			}
		}
		if want("comments") {
			comments, e := m.reader.GetComments(ctx)
			if e != nil {
				issue(&report.Comments, "注释元数据", e)
			}
			for _, c := range comments {
				if selected[c.TableName] {
					snapshot.comments = append(snapshot.comments, c)
				}
			}
		}
		if !m.cfg.objectMode() && m.cfg.Mode != "include" {
			views, e := m.reader.GetViews(ctx)
			if e != nil {
				issue(&report.Views, "视图元数据", e)
			}
			for _, v := range views {
				if e := checkName(v.ViewName, "视图 "+v.ViewName, names); e != nil {
					issue(&report.Views, v.ViewName, e)
				}
			}
			ordered, errs := m.prepareSQLServerDamengViews(views, selected)
			for _, e := range errs {
				issue(&report.Views, e.Name, fmt.Errorf("%s", e.Error))
			}
			snapshot.views = ordered
		}
	}
	if ready {
		m.reader = snapshot
	}
	return ready
}

func (m *Migrator) prepareSQLServerDamengViews(views []source.ViewInfo, tables map[string]bool) ([]source.ViewInfo, []ObjectResult) {
	pending := map[string]source.ViewInfo{}
	deps := map[string][]string{}
	errors := []ObjectResult{}
	names := map[string]string{}
	for _, v := range views {
		key := strings.ToUpper(v.ViewName)
		if err := tsqldameng.ValidName(v.ViewName); err != nil {
			errors = append(errors, ObjectResult{Name: v.ViewName, Error: err.Error()})
			continue
		}
		if prev, ok := names[key]; ok {
			errors = append(errors, ObjectResult{Name: v.ViewName, Error: "大写命名冲突: " + prev})
			continue
		}
		names[key] = v.ViewName
		sql, refs, e := tsqldameng.View(v.Definition, m.cfg.TargetSchema)
		if e != nil {
			errors = append(errors, ObjectResult{Name: v.ViewName, DDL: v.Definition, Error: e.Error()})
			continue
		}
		v.Definition = sql
		pending[v.ViewName] = v
		deps[v.ViewName] = refs
	}
	ordered := []source.ViewInfo{}
	done := map[string]bool{}
	for t := range tables {
		done[strings.ToUpper(t)] = true
	}
	for len(pending) > 0 {
		progressed := false
		for _, original := range views {
			v, ok := pending[original.ViewName]
			if !ok {
				continue
			}
			ready := true
			for _, d := range deps[v.ViewName] {
				if done[strings.ToUpper(d)] {
					continue
				}
				if _, exists := names[strings.ToUpper(d)]; exists {
					ready = false
					break
				}
				if tables != nil {
					ready = false
					break
				}
			}
			if ready {
				ordered = append(ordered, v)
				done[strings.ToUpper(v.ViewName)] = true
				delete(pending, v.ViewName)
				progressed = true
			}
		}
		if !progressed {
			for _, v := range views {
				if _, ok := pending[v.ViewName]; ok {
					errors = append(errors, ObjectResult{Name: v.ViewName, Error: "视图依赖循环、依赖不兼容视图或表不在迁移范围"})
				}
			}
			break
		}
	}
	return ordered, errors
}

func (m *Migrator) migrateSQLServerDamengViews(ctx context.Context, names []string) []ObjectResult {
	if len(m.cfg.StripViewSchemas) > 0 {
		return []ObjectResult{{Name: "视图模式", Error: "此组合不支持删除跨库/schema 前缀"}}
	}
	views, e := m.reader.GetViews(ctx)
	if e != nil {
		return []ObjectResult{{Name: "视图元数据", Error: e.Error()}}
	}
	selected := []source.ViewInfo{}
	found := map[string]bool{}
	for _, v := range views {
		if slices.Contains(names, v.ViewName) {
			selected = append(selected, v)
			found[v.ViewName] = true
		}
	}
	ordered, errs := m.prepareSQLServerDamengViews(selected, nil)
	for _, n := range names {
		if !found[n] {
			errs = append(errs, ObjectResult{Name: n, Error: "不在 dbo 视图范围"})
		}
	}
	if len(errs) > 0 {
		return errs
	}
	results := []ObjectResult{}
	for _, v := range ordered {
		ddl, e := m.createOneView(ctx, v)
		result := ObjectResult{Name: m.objName(v.ViewName), DDL: ddl}
		if e != nil {
			result.Error = e.Error()
		}
		results = append(results, result)
	}
	return results
}
