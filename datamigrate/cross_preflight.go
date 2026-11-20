package datamigrate

import (
	"context"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"fmt"
	"slices"
)

type CrossDatabaseProfile struct {
	Product       string `json:"product"`
	Version       string `json:"version"`
	Mode          string `json:"mode"`
	Schema        string `json:"schema"`
	LengthUnit    string `json:"length_unit,omitempty"`
	CaseSensitive string `json:"case_sensitive,omitempty"`
	Charset       string `json:"charset"`
	NCharset      string `json:"ncharset"`
	Verification  string `json:"verification"`
}
type CrossPreflight struct {
	Kind               string               `json:"kind"`
	Ready              bool                 `json:"ready"`
	CanStart           bool                 `json:"can_start"`
	Source             CrossDatabaseProfile `json:"source"`
	Target             CrossDatabaseProfile `json:"target"`
	Issues             []CompatibilityIssue `json:"issues"`
	Objects            []PlannedObject      `json:"objects"`
	Excluded           []ObjectRef          `json:"excluded"`
	RowsChecked        int64                `json:"rows_checked"`
	CheckScope         string               `json:"check_scope"`
	PartitionPreflight *PartitionPreflight  `json:"partition_preflight,omitempty"`
}

func (p *CrossPreflight) issue(ref ObjectRef, reason, definition string, warning bool) {
	severity := "error"
	if warning {
		severity = "warning"
	} else {
		p.Ready = false
	}
	p.Issues = append(p.Issues, CompatibilityIssue{Object: ref, Severity: severity, Reason: reason, Definition: definition, Excludable: ref.Kind != "metadata" && ref.Kind != "target", Suggestion: "检查转换差异或排除相关对象；独立对象继续迁移"})
}
func (m *Migrator) crossExcluded(kind, table, name string) bool {
	for _, x := range m.cfg.ExcludedObjects {
		if x.Kind == "table" && (x.Name == table || kind == "table" && x.Name == name) {
			return true
		}
		if x.Kind == kind && (x.Name == name || m.isCrossOracle() && m.objName(x.Name) == m.objName(name)) && (x.Table == "" || x.Table == table || m.isCrossOracle() && m.objName(x.Table) == m.objName(table)) {
			return true
		}
	}
	return false
}
func (m *Migrator) crossTables(tables []string) []string {
	if !m.isCrossMigration() {
		return tables
	}
	return slices.DeleteFunc(slices.Clone(tables), func(t string) bool { return m.crossExcluded("table", t, t) })
}

// PreflightCrossMigration never executes DDL and is repeated by Run. It uses
// the same checked plans as execution; readiness is distinct from partial work.
func (m *Migrator) PreflightCrossMigration(ctx context.Context) *CrossPreflight {
	p := &CrossPreflight{Kind: "cross", Ready: true, CheckScope: "structure", Issues: []CompatibilityIssue{}, Objects: []PlannedObject{}, Excluded: slices.Clone(m.cfg.ExcludedObjects)}
	defer func() {
		if m.partitionState != nil {
			p.PartitionPreflight = m.partitionState.preflight
			if !p.PartitionPreflight.Ready {
				p.Ready = false
			}
		}
	}()
	p.Source = CrossDatabaseProfile{Product: m.reader.DBType(), Schema: m.oracleSourceSchema(), Verification: "能力探测不等于实库认证"}
	p.Target = CrossDatabaseProfile{Product: m.writer.DBType(), Schema: m.cfg.TargetSchema, Verification: "能力探测不等于实库认证"}
	if r, ok := m.reader.(interface {
		CrossProfile(context.Context) (source.CrossProfile, error)
	}); ok {
		v, e := r.CrossProfile(ctx)
		if e != nil {
			p.issue(ObjectRef{Kind: "metadata", Name: "源库能力"}, e.Error(), "", false)
			return p
		}
		p.Source.Version, p.Source.Mode, p.Source.Schema = v.Version, v.Mode, v.Schema
		p.Source.Charset, p.Source.LengthUnit, p.Source.CaseSensitive = v.Charset, v.LengthUnit, v.CaseSensitive
	}
	if w, ok := m.writer.(interface{ EffectiveSchema() string }); ok {
		p.Target.Schema = w.EffectiveSchema()
	}
	if w, ok := m.writer.(interface {
		InspectPGOracle(context.Context) (target.PGOracleProfile, error)
	}); ok {
		v, e := w.InspectPGOracle(ctx)
		if e != nil {
			p.issue(ObjectRef{Kind: "target", Name: "Oracle 能力"}, e.Error(), "", false)
			return p
		}
		p.Target.Version, p.Target.Charset, p.Target.NCharset = v.Version, v.Charset, v.NCharset
	}
	if w, ok := m.writer.(interface {
		SQLServerVersion(context.Context) (int, int, error)
	}); ok {
		major, minor, e := w.SQLServerVersion(ctx)
		if e != nil || major < 11 {
			p.issue(ObjectRef{Kind: "target", Name: "SQL Server 版本"}, fmt.Sprintf("新路径要求 SQL Server 2012+；探测 %d.%d: %v", major, minor, e), "", false)
			return p
		}
		p.Target.Version = fmt.Sprintf("%d.%d", major, minor)
	}
	if m.isCrossOracle() {
		if len(m.cfg.ViewNames) > 0 {
			views, e := m.readOracleViews(ctx)
			if e != nil {
				p.issue(ObjectRef{Kind: "metadata", Name: "视图"}, e.Error(), "", false)
				return p
			}
			views = slices.DeleteFunc(slices.Clone(views), func(v source.ViewInfo) bool {
				return !slices.Contains(m.cfg.ViewNames, v.ViewName) || m.crossExcluded("view", "", v.ViewName)
			})
			for _, name := range m.cfg.ViewNames {
				if !slices.ContainsFunc(views, func(v source.ViewInfo) bool { return v.ViewName == name }) && !m.crossExcluded("view", "", name) {
					p.issue(ObjectRef{Kind: "view", Name: name}, "源视图不存在", "", false)
				}
			}
			failed := map[string]bool{}
			for _, v := range m.planOracleViews(views) {
				if v.err == nil {
					for _, dep := range v.deps {
						if failed[m.objName(dep)] {
							v.err = fmt.Errorf("依赖视图 %s 预检失败", dep)
							break
						}
					}
				}
				if v.err == nil {
					if w, ok := m.writer.(oracleInspector); ok {
						v.err = w.PreflightOracle(ctx, nil, []target.OracleObject{{Kind: "VIEW", Name: v.view.ViewName}}, nil, false, false)
					}
				}
				if v.err != nil {
					failed[v.view.ViewName] = true
				}
				ref := ObjectRef{Kind: "view", Name: v.sourceName}
				if v.err != nil {
					p.issue(ref, v.err.Error(), v.sourceDDL, false)
				} else {
					p.CanStart = true
					p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: v.view.ViewName, Stage: "views", Statements: m.writer.Dialect().ViewStatements(m.cfg.TargetSchema, v.view)})
				}
			}
			return p
		}
		plan, e := m.prepareOracle(ctx)
		if e != nil {
			p.issue(ObjectRef{Kind: "metadata", Name: "迁移准备"}, e.Error(), "", false)
			return p
		}
		for _, t := range plan.tables {
			ref := tableRef(t)
			if e := plan.tableErrors[t]; e != nil {
				p.issue(ref, e.Error(), dialect.JoinSQL(plan.ddl[t]), false)
				continue
			}
			if !m.cfg.objectMode() {
				p.CanStart = true
			}
			p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: m.objName(t), Stage: "tables", Statements: plan.ddl[t]})
			for _, c := range plan.info[m.objName(t)].Columns {
				if warning := crossOracleRepresentation(c, m.reader.DBType()); warning != "" {
					p.issue(ObjectRef{Kind: "column", Table: t, Name: c.Name}, warning, "", true)
				}
			}
		}
		failed := map[string]bool{}
		for name := range plan.tableErrors {
			failed[m.objName(name)] = true
		}
		for _, s := range plan.sequences {
			ref := ObjectRef{Kind: "sequence", Table: s.TableName, Name: m.writer.Dialect().(*dialect.OracleDialect).SequenceName(s)}
			if failed[s.TableName] || crossOracleSequenceBlocked(s, failed) {
				p.issue(ref, "依赖表预检失败", "", false)
				continue
			}
			if s.Unsupported != "" {
				p.issue(ref, s.Unsupported, "", false)
			} else {
				p.CanStart = true
				p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: ref.Name, Stage: "sequences", Statements: m.writer.Dialect().SequenceStatements(m.cfg.TargetSchema, s, "")})
			}
		}
		for _, i := range append(slices.Clone(plan.pks), plan.indexes...) {
			kind := "index"
			if i.IsPrimary {
				kind = "primary_key"
			}
			ref := ObjectRef{Kind: kind, Table: i.TableName, Name: i.IndexName}
			if failed[i.TableName] {
				p.issue(ref, "依赖表预检失败", "", false)
				continue
			}
			if i.Unsupported != "" {
				p.issue(ref, i.Unsupported, "", false)
			} else {
				p.CanStart = true
				p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: m.writer.Dialect().(*dialect.OracleDialect).IndexName(i), Stage: "indexes", Statements: m.writer.Dialect().IndexStatements(m.cfg.TargetSchema, i)})
			}
		}
		for _, f := range plan.fks {
			ref := ObjectRef{Kind: "foreign_key", Table: f.TableName, Name: f.ConstraintName}
			if failed[f.TableName] || failed[f.RefTable] {
				p.issue(ref, "依赖表预检失败", "", false)
				continue
			}
			if e := dialect.ValidateOracleForeignKey(f); e != nil {
				p.issue(ref, e.Error(), "", false)
			} else {
				p.CanStart = true
				p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: m.writer.Dialect().(*dialect.OracleDialect).ForeignKeyName(f), Stage: "constraints", Statements: m.writer.Dialect().ForeignKeyStatements(m.cfg.TargetSchema, f)})
			}
		}
		for _, c := range plan.comments {
			ref := ObjectRef{Kind: "comment", Table: c.TableName, Name: c.ColumnName}
			if failed[c.TableName] {
				p.issue(ref, "依赖表预检失败", "", false)
				continue
			}
			p.CanStart = true
			p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: c.TableName, Stage: "comments", Statements: m.writer.Dialect().CommentStatements(m.cfg.TargetSchema, c)})
		}
		for _, v := range plan.views {
			for _, dep := range v.deps {
				if failed[m.objName(dep)] {
					v.err = fmt.Errorf("依赖对象 %s 预检失败", dep)
				}
			}
			if v.err != nil {
				failed[v.view.ViewName] = true
			}
			ref := ObjectRef{Kind: "view", Name: v.sourceName}
			if v.err != nil {
				p.issue(ref, v.err.Error(), v.sourceDDL, false)
			} else {
				p.CanStart = true
				p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: v.view.ViewName, Stage: "views", Statements: m.writer.Dialect().ViewStatements(m.cfg.TargetSchema, v.view)})
			}
		}
	} else {
		m.preflightCrossSQLServer(ctx, p)
	}
	if ctx.Err() != nil {
		p.CanStart = false
		p.issue(ObjectRef{Kind: "metadata", Name: "任务取消"}, ctx.Err().Error(), "", false)
	}
	return p
}
