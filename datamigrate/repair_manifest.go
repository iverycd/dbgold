package datamigrate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

// RepairManifest is an additive report extension. Original category results stay immutable.
type RepairManifest struct {
	Version int             `json:"version"`
	Config  Config          `json:"config"`
	Objects []repair.Object `json:"objects"`
}
type repairRecorder struct {
	mu      sync.Mutex
	objects []repair.Object
	sinkErr error
}

func repairID(kind, table, name string) string { return repair.Hash([]string{kind, table, name})[:32] }
func unitsFor(stmts []dialect.Statement) []repair.Unit {
	units := make([]repair.Unit, 0, len(stmts))
	for i, s := range stmts {
		if strings.TrimSpace(s.SQL) == "" {
			continue
		}
		destructive := destructiveRepairUnit(s.SQL)
		units = append(units, repair.Unit{ID: fmt.Sprintf("step-%d", i+1), SQL: s.SQL, OriginalSQL: s.SQL, Status: "pending", Selected: !destructive, Destructive: destructive})
	}
	return units
}
func destructiveRepairUnit(s string) bool {
	t := strings.ToUpper(strings.TrimSpace(s))
	return strings.HasPrefix(t, "DROP ") || strings.HasPrefix(t, "TRUNCATE ") || strings.HasPrefix(t, "DELETE ") || strings.Contains(t, "DROP TABLE")
}
func (m *Migrator) manifestLocked() RepairManifest {
	cfg := m.cfg
	cfg.RepairSink = nil
	return RepairManifest{Version: 1, Config: cfg, Objects: slices.Clone(m.repairs.objects)}
}
func (m *Migrator) flushRepairsLocked() error {
	if m.cfg.RepairSink == nil {
		return nil
	}
	if err := m.cfg.RepairSink(m.manifestLocked()); err != nil {
		m.repairs.sinkErr = err
		return fmt.Errorf("保存迁移对象执行记录失败: %w", err)
	}
	return nil
}
func (m *Migrator) beginRepair(ctx context.Context) {
	m.repairs = &repairRecorder{}
	tables, err := m.reader.ListTables(ctx)
	if err != nil {
		return
	}
	tables = FilterTables(tables, m.cfg.Mode, m.cfg.Filter)
	if m.cfg.objectMode() && len(m.cfg.TableNames) > 0 {
		tables = nil
		for _, t := range m.cfg.TableNames {
			if !slices.Contains(tables, t) {
				tables = append(tables, t)
			}
		}
	}
	selected := map[string]bool{}
	add := func(o repair.Object, ss []dialect.Statement) {
		o.ID = repairID(o.Kind, o.Table, o.TargetName)
		o.Schema = m.cfg.TargetSchema
		o.Status = "pending"
		o.RequiresReview = true
		o.Units = unitsFor(ss)
		o.DDL = dialect.JoinSQL(ss)
		if o.Dependencies == nil {
			o.Dependencies = []string{}
		}
		if o.Kind != "table" && o.Table != "" {
			o.Dependencies = append(o.Dependencies, repairID("table", o.Table, o.Table))
		}
		m.repairs.objects = append(m.repairs.objects, o)
	}
	for _, t := range tables {
		excluded := false
		for _, e := range m.cfg.ExcludedObjects {
			if e.Kind == "table" && e.Name == t {
				excluded = true
			}
		}
		if excluded {
			continue
		}
		selected[t] = true
		info, e := m.reader.GetTableDDLInfo(ctx, t)
		o := repair.Object{Kind: "table", Name: t, SourceName: t, SourceTable: t, Table: m.objName(t), TargetName: m.objName(t), ColumnMapping: map[string]string{}, DataStatus: "pending"}
		if m.cfg.Content == "schema_only" || m.cfg.objectMode() || len(m.cfg.ViewNames) > 0 {
			o.DataStatus = "not_required"
		}
		var ss []dialect.Statement
		if e == nil && info != nil {
			if m.writer.DBType() == "dameng" {
				_, _ = m.planDamengTable(ctx, t)
			}
			o.SourceColumns = slices.Clone(info.Columns)
			o.Columns = slices.Clone(info.Columns)
			for i, c := range info.Columns {
				n := m.targetColumn(t, c.Name)
				o.ColumnMapping[c.Name] = n
				o.Columns[i].Name = n
				o.Columns[i].DataType = m.writer.Dialect().MapType(c, m.reader.DBType(), dialect.TypeOpt{CharInLength: m.cfg.CharInLength, UseNvarchar2: m.cfg.UseNvarchar2})
			}
			if !m.cfg.objectMode() && m.cfg.Content != "data_only" && len(m.cfg.ViewNames) == 0 {
				ss, _, e = m.buildCreateTableDDL(ctx, t)
			}
		}
		if e != nil {
			o.Error = e.Error()
		}
		add(o, ss)
	}
	want := func(kind string) bool {
		if len(m.cfg.ViewNames) > 0 {
			return kind == "views"
		}
		if m.cfg.Content == "data_only" {
			return false
		}
		return !m.cfg.objectMode() || slices.Contains(m.cfg.Objects, kind)
	}
	addIndex := func(v source.IndexInfo) {
		if !selected[v.TableName] {
			return
		}
		src, originalName := v.TableName, v.IndexName
		v.TableName = m.objName(src)
		v.IndexName = m.objName(v.IndexName)
		v.Columns = slices.Clone(v.Columns)
		for i, c := range v.Columns {
			v.Columns[i] = m.targetColumn(src, c)
		}
		kind := "index"
		if v.IsPrimary {
			kind = "primary_key"
		}
		name := dialect.IndexName(m.writer.Dialect(), v)
		var ss []dialect.Statement
		if v.Unsupported == "" {
			ss = m.writer.Dialect().IndexStatements(m.cfg.TargetSchema, v)
		}
		add(repair.Object{Kind: kind, Name: name, Table: v.TableName, SourceTable: src, SourceName: originalName, TargetName: name, Index: &v, Error: v.Unsupported}, ss)
	}
	if want("primary_keys") {
		if values, e := m.reader.GetPrimaryKeys(ctx); e == nil {
			for _, v := range values {
				v.IsPrimary = true
				addIndex(v)
			}
		}
	}
	if want("indexes") {
		if values, e := m.reader.GetIndexes(ctx); e == nil {
			for _, v := range values {
				addIndex(v)
			}
		}
	}
	if want("sequences") {
		if values, e := m.reader.GetSequences(ctx); e == nil {
			for _, v := range values {
				if !selected[v.TableName] {
					continue
				}
				src := v.TableName
				v.TableName = m.objName(src)
				v.ColumnName = m.targetColumn(src, v.ColumnName)
				name := dialect.SequenceName(m.writer.Dialect(), v)
				owner := ""
				if m.cfg.ChangeOwner {
					owner = m.cfg.TargetSchema
				}
				add(repair.Object{Kind: "sequence", Name: name, Table: v.TableName, SourceTable: src, TargetName: name, Sequence: &v}, m.writer.Dialect().SequenceStatements(m.cfg.TargetSchema, v, owner))
			}
		}
	}
	if want("foreign_keys") && (m.cfg.Mode != "include" || m.cfg.objectMode() || m.isSQLServerDameng() || m.isOracle() || m.isSQLServer() || m.isPGMySQL()) {
		if values, e := m.reader.GetForeignKeys(ctx); e == nil {
			for _, v := range values {
				if !selected[v.TableName] {
					continue
				}
				src, ref, originalName := v.TableName, v.RefTable, v.ConstraintName
				v.TableName = m.objName(src)
				v.RefTable = m.objName(ref)
				v.ConstraintName = m.objName(v.ConstraintName)
				v.Columns = slices.Clone(v.Columns)
				v.RefColumns = slices.Clone(v.RefColumns)
				for i, c := range v.Columns {
					v.Columns[i] = m.targetColumn(src, c)
				}
				for i, c := range v.RefColumns {
					v.RefColumns[i] = m.targetColumn(ref, c)
				}
				add(repair.Object{Kind: "foreign_key", Name: v.ConstraintName, Table: v.TableName, SourceTable: src, SourceName: originalName, TargetName: v.ConstraintName, ForeignKey: &v, Dependencies: []string{repairID("table", v.RefTable, v.RefTable)}}, m.writer.Dialect().ForeignKeyStatements(m.cfg.TargetSchema, v))
			}
		}
	}
	if want("comments") {
		if values, e := m.reader.GetComments(ctx); e == nil {
			for _, v := range values {
				if !selected[v.TableName] {
					continue
				}
				src := v.TableName
				v.TableName = m.objName(src)
				if v.ColumnName != "" {
					v.ColumnName = m.targetColumn(src, v.ColumnName)
				}
				name := v.TableName
				if v.ColumnName != "" {
					name += "." + v.ColumnName
				}
				ss := m.writer.Dialect().CommentStatements(m.cfg.TargetSchema, v)
				if len(ss) > 0 || m.writer.DBType() == "mysql" {
					add(repair.Object{Kind: "comment", Name: name, Table: v.TableName, SourceTable: src, TargetName: name, Comment: &v}, ss)
				}
			}
		}
	}
	if want("views") {
		var values []source.ViewInfo
		if r, ok := m.reader.(interface {
			GetViewsForTarget(context.Context, string) ([]source.ViewInfo, error)
		}); ok {
			values, _ = r.GetViewsForTarget(ctx, m.writer.DBType())
		} else {
			values, _ = m.reader.GetViews(ctx)
		}
		for _, v := range values {
			src := v.ViewName
			if len(m.cfg.ViewNames) > 0 && !slices.Contains(m.cfg.ViewNames, src) {
				continue
			}
			v.ViewName = m.objName(src)
			add(repair.Object{Kind: "view", Name: v.ViewName, SourceName: src, TargetName: v.ViewName}, m.writer.Dialect().ViewStatements(m.cfg.TargetSchema, v))
		}
	}
	// Administrative attributes are separate recoverable work even when their
	// parent failed before the original migration reached these operations.
	for _, parent := range slices.Clone(m.repairs.objects) {
		if m.cfg.ChangeOwner && m.cfg.TargetSchema != "" && m.writer.Dialect().Caps().SupportsChangeOwner && (parent.Kind == "table" || parent.Kind == "view" || parent.Kind == "sequence") {
			kind := strings.ToUpper(parent.Kind)
			o := repair.Object{Kind: "owner", Name: parent.TargetName, Table: parent.TargetName, SourceTable: parent.SourceTable, TargetName: kind + ":" + parent.TargetName}
			add(o, m.repairOwnerStatements(kind, parent.TargetName, m.cfg.TargetSchema))
			m.repairs.objects[len(m.repairs.objects)-1].Dependencies = []string{parent.ID}
		}
		if parent.Kind == "primary_key" && parent.Index != nil && m.cfg.Distributed && m.writer.Dialect().Caps().SupportsDistribute {
			add(repair.Object{Kind: "distribution", Name: parent.Table, Table: parent.Table, SourceTable: parent.SourceTable, TargetName: parent.Table, Dependencies: []string{parent.ID}}, m.repairDistributeStatements(parent.Table, parent.Index.Columns))
		}
	}
	for i := range m.repairs.objects {
		o := &m.repairs.objects[i]
		if o.ForeignKey == nil {
			continue
		}
		for _, key := range m.repairs.objects {
			if key.Index != nil && (key.Index.IsPrimary || key.Index.IsUnique) && key.Table == o.ForeignKey.RefTable && slices.Equal(key.Index.Columns, o.ForeignKey.RefColumns) {
				o.Dependencies = append(o.Dependencies, key.ID)
				break
			}
		}
	}
	// Remove explicitly excluded objects without creating fictitious failed work.
	kept := m.repairs.objects[:0]
	for _, o := range m.repairs.objects {
		excluded := false
		for _, e := range m.cfg.ExcludedObjects {
			if e.Kind == o.Kind && (e.Name == o.SourceName || e.Name == o.Name) && (e.Table == "" || e.Table == o.SourceTable) {
				excluded = true
			}
		}
		if !excluded {
			kept = append(kept, o)
		}
	}
	m.repairs.objects = kept
	// Keep the mapping used to generate each structured object; later manual column
	// changes can regenerate its draft without guessing replacements in SQL text.
	for i := range m.repairs.objects {
		o := &m.repairs.objects[i]
		if o.Kind == "table" {
			continue
		}
		for _, table := range m.repairs.objects {
			if table.Kind == "table" && o.ForeignKey != nil && table.Table == o.ForeignKey.RefTable {
				o.RefColumnMapping = map[string]string{}
				for k, v := range table.ColumnMapping {
					o.RefColumnMapping[k] = v
				}
			}
			if table.Kind == "table" && table.Table == o.Table {
				o.ColumnMapping = map[string]string{}
				for k, v := range table.ColumnMapping {
					o.ColumnMapping[k] = v
				}
			}
		}
	}
	m.repairs.mu.Lock()
	defer m.repairs.mu.Unlock()
	_ = m.flushRepairsLocked()
}

// captureRepairCall keeps the original writer behavior; its observer sees physical units,
// including PL/SQL blocks and rolled-back transaction statements.
func (m *Migrator) captureRepairCall(ctx context.Context, o repair.Object, ss []dialect.Statement, fn func(context.Context) error) error {
	if m.repairs == nil {
		return fn(ctx)
	}
	r := m.repairs
	r.mu.Lock()
	if r.sinkErr != nil {
		e := r.sinkErr
		r.mu.Unlock()
		return e
	}
	o.ID = repairID(o.Kind, o.Table, o.TargetName)
	idx := -1
	for i, v := range r.objects {
		if v.ID == o.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		o.Schema = m.cfg.TargetSchema
		o.Dependencies = []string{}
		if o.Table != "" && o.Kind != "table" {
			o.Dependencies = append(o.Dependencies, repairID("table", o.Table, o.Table))
		}
		r.objects = append(r.objects, o)
		idx = len(r.objects) - 1
	}
	old := &r.objects[idx]
	old.Units = unitsFor(ss)
	old.DDL = dialect.JoinSQL(ss)
	old.Status = "running"
	old.RequiresReview = false
	if o.Index != nil {
		old.Index = o.Index
	}
	if o.Sequence != nil {
		old.Sequence = o.Sequence
	}
	if o.ForeignKey != nil {
		old.ForeignKey = o.ForeignKey
	}
	if o.Comment != nil {
		old.Comment = o.Comment
	}
	if err := m.flushRepairsLocked(); err != nil {
		old.Status = "failed"
		old.Error = err.Error()
		r.mu.Unlock()
		return err
	}
	r.mu.Unlock()
	observed := map[int]bool{}
	ctx = repair.WithObserver(ctx, func(e repair.Event) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		item := &r.objects[idx]
		unit := -1
		for i, u := range item.Units {
			if u.SQL == e.SQL && ((e.Status == "running" && !observed[i]) || (e.Status != "running" && u.Status == "running")) {
				unit = i
				if e.Status == "running" {
					observed[i] = true
				}
				// A transactional statement failure belongs to the most recently
				// started occurrence. Commit/rollback events drain in execution order.
				if e.Status != "failed" {
					break
				}
			}
		}
		if unit < 0 {
			unit = len(item.Units)
			item.Units = append(item.Units, repair.Unit{ID: fmt.Sprintf("step-%d", unit+1), SQL: e.SQL, OriginalSQL: e.SQL, Destructive: destructiveRepairUnit(e.SQL)})
		}
		if e.Status == "running" {
			observed[unit] = true
		}
		item.Units[unit].Status = e.Status
		item.Units[unit].Error = e.Error
		item.Units[unit].Selected = e.Status != "succeeded" && !item.Units[unit].Destructive
		return m.flushRepairsLocked()
	})
	err := fn(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	item := &r.objects[idx]
	item.Status = "succeeded"
	item.Error = ""
	if err != nil {
		item.Status = "failed"
		item.Error = err.Error()
	}
	for i := range item.Units {
		u := &item.Units[i]
		if u.Status == "running" {
			u.Status = "unknown"
		}
		if err == nil && u.Status == "pending" {
			u.Status = "succeeded"
		}
		if err != nil && u.Status == "pending" && len(observed) == 0 {
			u.Status = "unknown"
		}
		u.Selected = u.Status != "succeeded" && !u.Destructive
		if u.Status == "unknown" {
			item.Status = "unknown"
		}
	}
	if e := m.flushRepairsLocked(); e != nil {
		return e
	}
	return err
}
func (m *Migrator) repairTable(ctx context.Context, t string, ss []dialect.Statement) error {
	return m.captureRepairCall(ctx, repair.Object{Kind: "table", Name: t, SourceName: t, SourceTable: t, Table: m.objName(t), TargetName: m.objName(t)}, ss, func(c context.Context) error { return m.writer.CreateTable(c, ss) })
}
func (m *Migrator) repairIndex(ctx context.Context, v source.IndexInfo) error {
	k := "index"
	if v.IsPrimary {
		k = "primary_key"
	}
	n := dialect.IndexName(m.writer.Dialect(), v)
	o := repair.Object{Kind: k, Name: n, Table: v.TableName, TargetName: n, Index: &v}
	if v.Unsupported != "" {
		return m.captureRepairCall(ctx, o, nil, func(context.Context) error { return fmt.Errorf("%s", v.Unsupported) })
	}
	return m.captureRepairCall(ctx, o, m.writer.Dialect().IndexStatements(m.cfg.TargetSchema, v), func(c context.Context) error { return m.writer.CreateIndex(c, v) })
}
func (m *Migrator) repairSequence(ctx context.Context, v source.SequenceInfo, owner string) error {
	n := dialect.SequenceName(m.writer.Dialect(), v)
	return m.captureRepairCall(ctx, repair.Object{Kind: "sequence", Name: n, Table: v.TableName, TargetName: n, Sequence: &v}, m.writer.Dialect().SequenceStatements(m.cfg.TargetSchema, v, owner), func(c context.Context) error { return m.writer.CreateSequence(c, v, owner) })
}
func (m *Migrator) repairFK(ctx context.Context, v source.FKInfo) error {
	return m.captureRepairCall(ctx, repair.Object{Kind: "foreign_key", Name: v.ConstraintName, Table: v.TableName, TargetName: v.ConstraintName, ForeignKey: &v}, m.writer.Dialect().ForeignKeyStatements(m.cfg.TargetSchema, v), func(c context.Context) error { return m.writer.CreateForeignKey(c, v) })
}
func (m *Migrator) repairComment(ctx context.Context, v source.CommentInfo) error {
	n := v.TableName
	if v.ColumnName != "" {
		n += "." + v.ColumnName
	}
	return m.captureRepairCall(ctx, repair.Object{Kind: "comment", Name: n, Table: v.TableName, TargetName: n, Comment: &v}, m.writer.Dialect().CommentStatements(m.cfg.TargetSchema, v), func(c context.Context) error { return m.writer.CreateComment(c, v) })
}
func (m *Migrator) repairView(ctx context.Context, v source.ViewInfo) error {
	return m.captureRepairCall(ctx, repair.Object{Kind: "view", Name: v.ViewName, TargetName: v.ViewName}, m.writer.Dialect().ViewStatements(m.cfg.TargetSchema, v), func(c context.Context) error { return m.writer.CreateView(c, v) })
}
func (m *Migrator) repairViewDependencies(name string, dependencies []string) {
	if m.repairs == nil {
		return
	}
	m.repairs.mu.Lock()
	defer m.repairs.mu.Unlock()
	for i := range m.repairs.objects {
		o := &m.repairs.objects[i]
		if o.Kind != "view" || o.TargetName != m.objName(name) {
			continue
		}
		for _, dep := range dependencies {
			for _, candidate := range m.repairs.objects {
				if (candidate.Kind == "table" || candidate.Kind == "view") && candidate.TargetName == m.objName(dep) && !slices.Contains(o.Dependencies, candidate.ID) {
					o.Dependencies = append(o.Dependencies, candidate.ID)
				}
			}
		}
	}
}
func (m *Migrator) repairOwner(ctx context.Context, kind, name, owner string) error {
	ss := m.repairOwnerStatements(kind, name, owner)
	return m.captureRepairCall(ctx, repair.Object{Kind: "owner", Name: name, Table: name, TargetName: kind + ":" + name}, ss, func(c context.Context) error { return m.writer.ChangeOwner(c, kind, name, owner) })
}
func (m *Migrator) repairOwnerStatements(kind, name, owner string) []dialect.Statement {
	return []dialect.Statement{{SQL: fmt.Sprintf("ALTER %s %s OWNER TO %s", kind, m.writer.Dialect().QualifyTable(m.cfg.TargetSchema, name), m.writer.Dialect().QuoteIdent(owner))}}
}
func (m *Migrator) repairDistributeStatements(t string, cols []string) []dialect.Statement {
	return []dialect.Statement{{SQL: fmt.Sprintf("ALTER TABLE %s DISTRIBUTE BY hash (%s);", m.writer.Dialect().QualifyTable(m.cfg.TargetSchema, t), strings.Join(cols, ", "))}}
}
func (m *Migrator) repairDistribute(ctx context.Context, t string, cols []string) error {
	return m.captureRepairCall(ctx, repair.Object{Kind: "distribution", Name: t, Table: t, TargetName: t}, m.repairDistributeStatements(t, cols), func(c context.Context) error { return m.writer.AlterDistribute(c, t, cols) })
}
func (m *Migrator) repairPlanned(ctx context.Context, v PlannedObject) error {
	table := m.objName(v.Object.Table)
	if v.Object.Kind == "table" {
		table = m.objName(v.Object.Name)
	}
	name := m.objName(v.Object.Name)
	return m.captureRepairCall(ctx, repair.Object{Kind: v.Object.Kind, Name: name, Table: table, TargetName: name, SourceName: v.Object.Name, SourceTable: v.Object.Table}, v.Statements, func(c context.Context) error { return m.writer.CreateTable(c, v.Statements) })
}

func (m *Migrator) finishRepair(_ context.Context, report *MigrationReport) {
	if m.repairs == nil {
		return
	}
	r := m.repairs
	r.mu.Lock()
	defer r.mu.Unlock()
	// Preserve every executable PG plan entry, including ones after an early return.
	if report.Preflight != nil {
		for _, p := range report.Preflight.Objects {
			if p.Stage == "drop_dependency" {
				continue
			}
			table := m.objName(p.Object.Table)
			if p.Object.Kind == "table" {
				table = m.objName(p.Object.Name)
			}
			id := repairID(p.Object.Kind, table, m.objName(p.Object.Name))
			found := false
			for _, o := range r.objects {
				found = found || o.ID == id
			}
			if !found {
				r.objects = append(r.objects, repair.Object{ID: id, Kind: p.Object.Kind, Name: p.Object.Name, SourceName: p.Object.Name, SourceTable: p.Object.Table, Table: table, TargetName: m.objName(p.Object.Name), Schema: m.cfg.TargetSchema, DDL: dialect.JoinSQL(p.Statements), Units: unitsFor(p.Statements), Status: "pending", RequiresReview: true, Dependencies: []string{}})
			}
		}
		// PG → MySQL may embed keys and comments into CREATE TABLE. Preserve
		// their success without offering a duplicate ALTER on a later continuation.
		if !m.cfg.objectMode() && m.cfg.Content != "data_only" && len(m.cfg.ViewNames) == 0 {
			for _, t := range report.Preflight.tables {
				table := m.objName(t.Info.TableName)
				created := false
				for _, o := range r.objects {
					created = created || o.Kind == "table" && o.Table == table && o.Status == "succeeded"
				}
				if !created {
					continue
				}
				for i := range r.objects {
					o := &r.objects[i]
					if o.Table != table || o.Status != "pending" {
						continue
					}
					inline := o.Kind == "comment"
					for _, idx := range t.Indexes {
						if o.Index == nil || o.Index.IndexName != m.objName(idx.IndexName) {
							continue
						}
						inline = true
						for _, planned := range report.Preflight.Objects {
							if planned.Stage == "index" && planned.Object == idxRef(idx) {
								inline = false
							}
						}
					}
					if inline {
						o.Status, o.RequiresReview = "succeeded", false
						for j := range o.Units {
							o.Units[j].Status, o.Units[j].Selected = "succeeded", false
						}
					}
				}
			}
		}
	}
	states := map[string]string{}
	for _, o := range r.objects {
		states[o.ID] = o.Status
	}
	for i := range r.objects {
		o := &r.objects[i]
		if o.Kind == "table" {
			if o.DataStatus != "not_required" {
				o.DataStatus = "pending"
				for _, f := range report.Data.Items {
					if f.Name == o.SourceName || f.Name == o.TargetName {
						o.DataStatus = "failed"
					}
				}
				for _, rc := range report.RowCounts {
					if rc.Table == o.SourceName || rc.Table == o.TargetName {
						if rc.Match && o.DataStatus != "failed" {
							o.DataStatus = "succeeded"
						} else {
							o.DataStatus = "failed"
						}
					}
				}
			}
			if len(o.Units) == 0 && (m.cfg.Content == "data_only" || m.cfg.objectMode()) {
				o.Status = "unknown"
			}
		}
		if o.Status == "pending" {
			for _, dep := range o.Dependencies {
				if s, ok := states[dep]; ok && s != "succeeded" {
					o.Status = "blocked"
					break
				}
			}
		}
	}
	// Attach original preparation/metadata failures that had no execution call.
	for _, legacy := range LegacyRepairObjects(*report, m.cfg.TargetSchema) {
		if legacy.Kind == "data" {
			for _, t := range r.objects {
				if t.Kind == "table" && (legacy.Name == t.SourceName || legacy.Name == t.TargetName) {
					legacy.SourceName = t.SourceName
					legacy.SourceTable = t.SourceTable
					legacy.Table = t.Table
					legacy.TargetName = t.TargetName
					legacy.SourceColumns = t.SourceColumns
					legacy.Columns = t.Columns
					legacy.ColumnMapping = t.ColumnMapping
					legacy.Legacy = false
					legacy.RequiresReview = false
					legacy.DataStatus = "failed"
					legacy.Dependencies = []string{t.ID}
					break
				}
			}
		}

		match := -1
		for i := range r.objects {
			o := &r.objects[i]
			if o.Kind != legacy.Kind || legacy.Table != "" && legacy.Table != o.SourceTable && legacy.Table != o.Table {
				continue
			}
			if o.Name == legacy.Name || o.SourceName == legacy.Name || o.TargetName == legacy.Name || o.DDL == legacy.DDL && legacy.DDL != "" {
				// Older reports omit the table. Keep an ambiguous failure separate
				// instead of assigning it to the first same-named index.
				if match >= 0 {
					match = -1
					break
				}
				match = i
			}
		}
		if match < 0 {
			r.objects = append(r.objects, legacy)
		} else {
			o := &r.objects[match]
			if o.Status == "pending" || o.Status == "blocked" {
				o.Status = "failed"
				o.Error = legacy.Error
			}
			if legacy.SuggestedDDL != "" {
				o.SuggestedDDL = legacy.SuggestedDDL
			}
		}
	}
	for i := range r.objects {
		r.objects[i].OriginalStatus = r.objects[i].Status
		r.objects[i].OriginalError = r.objects[i].Error
	}
	if err := m.flushRepairsLocked(); err != nil {
		report.Data.Failed++
		report.Data.Total++
		report.Data.Items = append(report.Data.Items, ObjectResult{Name: "修复记录持久化", Error: err.Error()})
	}
	manifest := m.manifestLocked()
	report.RepairManifest = &manifest
}

func LegacyRepairObjects(report MigrationReport, schema string) []repair.Object {
	categories := []struct {
		kind string
		cat  CategoryReport
	}{{"table", report.Tables}, {"data", report.Data}, {"primary_key", report.PrimaryKeys}, {"view", report.Views}, {"index", report.Indexes}, {"foreign_key", report.Constraints}, {"sequence", report.Sequences}, {"comment", report.Comments}}
	result := []repair.Object{}
	for _, c := range categories {
		for i, item := range c.cat.Items {
			if item.Error == "" {
				continue
			}
			o := repair.Object{ID: repair.Hash([]any{c.kind, i, item.Name, item.DDL, item.Error})[:32], Kind: c.kind, Name: item.Name, SourceName: item.Name, TargetName: item.Name, Schema: schema, DDL: item.DDL, SuggestedDDL: item.SuggestedDDL, Error: item.Error, Status: "failed", Legacy: true, OriginalStatus: "failed", OriginalError: item.Error, RequiresReview: true, Dependencies: []string{}, Units: []repair.Unit{}}
			if item.Table != "" {
				o.Table, o.SourceTable = item.Table, item.Table
				o.ID = repair.Hash([]any{c.kind, i, item.Table, item.Name, item.DDL, item.Error})[:32]
			}
			if c.kind == "table" || c.kind == "data" {
				o.Table = item.Name
				o.SourceTable = item.Name
				o.DataStatus = "unknown"
			}
			// Only import a trivially single SQL unit. Complex legacy text must be explicitly arranged.
			trimmed := strings.TrimSuffix(strings.TrimSpace(item.DDL), ";")
			if trimmed != "" && !strings.Contains(trimmed, ";") && !destructiveRepairUnit(trimmed) {
				o.Units = unitsFor([]dialect.Statement{{SQL: trimmed}})
				o.Units[0].Status = "failed"
			}
			result = append(result, o)
		}
	}
	return result
}
