package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/target"
)

func (m *Migrator) isDamengNative() bool {
	return m.reader.DBType() == "dameng" && m.writer.DBType() == "dameng"
}

// PreflightDamengNative is strictly observational. Starting a job calls it again
// under the existing target-scope lock, before recording or executing any DROP.
func (m *Migrator) PreflightDamengNative(ctx context.Context) *damengnative.Preflight {
	p := &damengnative.Preflight{Kind: "dameng_native", Objects: []damengnative.Object{}, Conflicts: []damengnative.Object{}, Drops: []damengnative.Object{}, Issues: []damengnative.Issue{}}
	issue := func(kind, name, reason string) {
		p.Issues = append(p.Issues, damengnative.Issue{Kind: kind, Name: name, Severity: "error", Reason: reason})
	}
	if !m.isDamengNative() {
		issue("task", "迁移组合", "此预检仅支持达梦 → 达梦")
		return p
	}
	if (m.cfg.Mode != "" && m.cfg.Mode != "all") || strings.TrimSpace(m.cfg.Filter) != "" || (m.cfg.Content != "" && m.cfg.Content != "both") || len(m.cfg.Objects) > 0 || len(m.cfg.ViewNames) > 0 || len(m.cfg.TableNames) > 0 || len(m.cfg.ExcludedObjects) > 0 {
		issue("task", "迁移范围", "达梦同库迁移必须包含整个 Schema 的全部结构与数据，不支持过滤或排除对象")
		return p
	}
	r, rok := m.reader.(damengnative.Reader)
	w, wok := m.writer.(damengnative.Writer)
	if !rok || !wok {
		issue("task", "原生能力", "达梦同库任务必须使用原生 Reader 和 Writer")
		return p
	}
	snap, err := r.SnapshotDamengNative(ctx)
	if err != nil {
		issue("metadata", "源对象清单", err.Error())
		return p
	}
	if snap == nil {
		issue("metadata", "源对象清单", "源快照为空")
		return p
	}
	p.Source, p.Snapshot = snap.Profile, snap
	p.Issues = append(p.Issues, snap.Issues...)
	dst, err := w.InspectDamengNative(ctx)
	if err != nil {
		issue("metadata", "目标对象清单", err.Error())
		return p
	}
	if dst == nil {
		issue("metadata", "目标对象清单", "目标快照为空")
		return p
	}
	p.Target = dst.Profile
	p.Issues = append(p.Issues, dst.Issues...)
	if p.Source.Schema == "" || p.Target.Schema == "" {
		issue("schema", "模式", "源模式和用户预建的目标模式必须存在")
	}
	if p.Source.Instance == "" || p.Target.Instance == "" {
		issue("instance", "实例身份", "无法核验源目标实例身份，已阻止同库误覆盖")
	}
	if nativeSameDatabase(p.Source, p.Target) && p.Source.Schema == p.Target.Schema {
		issue("schema", p.Source.Schema, "源和目标属于同一实例的同一 Schema，禁止迁移")
	}
	if p.Source.Version == "" || p.Source.Version != p.Target.Version {
		issue("version", "DM8 版本", "只支持相同版本的 DM8，源目标版本不同或无法确认")
	}
	version := strings.ToUpper(strings.TrimSpace(p.Source.Version))
	if !strings.Contains(version, "DM8") && !strings.Contains(version, "V8") && !strings.HasPrefix(version, "8.") {
		issue("version", "DM8 版本", "无法确认源库为 DM8，已阻止原生迁移")
	}
	for _, key := range []string{"COMPATIBLE_MODE", "CASE_SENSITIVE", "CHARSET", "PAGE_SIZE"} {
		a, aok := p.Source.Settings[key]
		b, bok := p.Target.Settings[key]
		if !aok || !bok || a != b {
			issue("settings", key, "源目标参数不一致或未能完整读取: "+key)
		}
	}
	for key, a := range p.Source.Settings {
		if b, ok := p.Target.Settings[key]; !ok || a != b {
			issue("settings", key, "源目标语义参数不同或目标缺失: "+key)
		}
	}
	for _, t := range snap.Tables {
		writable := 0
		for _, c := range t.Columns {
			if c.Writable {
				writable++
				if err := target.ValidateDamengNativeColumn(c); err != nil {
					issue("column", t.Name+"."+c.Name, err.Error())
				}
			}
		}
		if writable == 0 && t.Rows > 0 {
			issue("table", t.Name, "非空表没有可写入列，无法无损复制源数据")
		}
	}
	for _, o := range snap.Objects {
		mapped := o
		mapped.Schema = p.Target.Schema
		mapped.Units = append([]damengnative.Unit{}, o.Units...)
		mapped.External = append([]damengnative.Reference{}, o.External...)
		if o.Interval != nil {
			mapped.Interval, err = damengnative.RewriteIntervalPartitions(o.Interval, p.Source.Schema, p.Target.Schema)
			if err != nil {
				issue(o.Kind, o.Name, err.Error())
			}
			if _, ok := w.(interface {
				RestoreDamengNativeIntervalPartitions(context.Context, damengnative.Object, damengnative.Unit) error
			}); !ok {
				issue(o.Kind, o.Name, "目标写入器缺少既有间隔分区恢复能力")
			}
		}
		if o.Materialized != nil {
			materialized := *o.Materialized
			materialized.SnapshotColumns = append([]string{}, o.Materialized.SnapshotColumns...)
			materialized.RowIDColumns = append([]int{}, o.Materialized.RowIDColumns...)
			materialized.Query, _, err = damengnative.RewriteSchema(materialized.Query, p.Source.Schema, p.Target.Schema)
			if err != nil {
				issue(o.Kind, o.Name, err.Error())
			}
			mapped.Materialized = &materialized
			if materialized.History != nil {
				mapped.Materialized.History, err = damengnative.RewriteMaterializedHistory(materialized.History, p.Source.Schema, p.Target.Schema)
				if err != nil {
					issue(o.Kind, o.Name, err.Error())
				}
				if _, ok := w.(interface {
					RestoreDamengNativeMaterializedHistory(context.Context, damengnative.Object, damengnative.Unit) error
				}); !ok {
					issue(o.Kind, o.Name, "目标写入器缺少陈旧快照及增量日志整体恢复能力")
				}
			}
			if materialized.Baseline {
				if _, ok := w.(interface {
					RebaseDamengNativeMaterialized(context.Context, damengnative.Object) error
				}); !ok {
					issue(o.Kind, o.Name, "目标写入器缺少带前后多重集核验的物化视图基线初始化能力")
				}
			}
		}
		for i, u := range mapped.Units {
			if u.Phase == "interval_prepare" && mapped.Interval != nil {
				mapped.Units[i] = damengnative.Unit{SQL: damengnative.IntervalPartitionSQL(p.Target.Schema, o.Name, mapped.Interval), Phase: u.Phase}
				continue
			}
			if u.Phase == "mv_history" && mapped.Materialized != nil && mapped.Materialized.History != nil {
				mapped.Units[i] = damengnative.Unit{SQL: damengnative.MaterializedHistorySQL(p.Target.Schema, o.Name, mapped.Materialized.History), Phase: u.Phase}
				continue
			}
			if o.EventTrigger && u.Phase == "event_activate" {
				mapped.Units[i] = damengnative.Unit{SQL: damengnative.EventTriggerStateSQL(p.Target.Schema, o.Name, o.Status == "ENABLED"), Phase: u.Phase}
				continue
			}
			unit, warnings, e := damengnative.RewriteUnit(u, p.Source.Schema, p.Target.Schema)
			if e != nil {
				issue(o.Kind, o.Name, e.Error())
				continue
			}
			mapped.Units[i] = unit
			if o.Kind == "trigger" && !o.EventTrigger {
				if u.Phase == "restore" {
					mapped.Units[i].Phase = "trigger_restore"
				} else {
					mapped.Units[i].Phase = "triggers"
				}
			}
			for _, warning := range warnings {
				p.Issues = append(p.Issues, damengnative.Issue{ObjectID: o.ID, Kind: o.Kind, Name: o.Name, Severity: "warning", Reason: warning})
			}
		}
		mapped.DropSQL, _, err = damengnative.RewriteSchema(o.DropSQL, p.Source.Schema, p.Target.Schema)
		if err != nil {
			issue(o.Kind, o.Name, err.Error())
		}
		mapped.CompileSQL, _, err = damengnative.RewriteSchema(o.CompileSQL, p.Source.Schema, p.Target.Schema)
		if err != nil {
			issue(o.Kind, o.Name, err.Error())
		}
		if !o.Managed && len(o.Units) == 0 {
			issue(o.Kind, o.Name, "没有可回放的原生 DDL，不能遗漏该对象")
		}
		if o.EventTrigger {
			if _, ok := w.(interface {
				InstallDamengNativeEvent(context.Context, damengnative.Object, damengnative.Unit) error
				WithDamengNativeEventFinalization(context.Context, func(context.Context) error) error
				SetDamengNativeEventState(context.Context, damengnative.Object, damengnative.Unit) error
			}); !ok {
				issue(o.Kind, o.Name, "目标写入器缺少事件触发器安装与最终启用生命周期")
			}
		}
		p.Objects = append(p.Objects, mapped)
	}
	targetObjects := map[string]damengnative.Object{}
	targetNames := map[string]bool{}
	for _, o := range dst.Objects {
		targetNames[o.Schema+"\x00"+o.Name] = true
		if len(p.Objects) > 0 && o.Kind == "trigger" && !o.Managed && o.EventTrigger && (strings.EqualFold(o.Status, "ENABLED") || o.Status == "Y" || o.Status == "1") {
			issue(o.Kind, o.Name, "目标存在无法证明为表 DML 触发器的启用触发器，禁止迁移期间执行 DDL 事件副作用")
		}
		if !o.Managed {
			targetObjects[nativeCollisionKey(o)] = o
		}
	}
	plannedDrops := map[string]bool{}
	for _, o := range p.Objects {
		if conflict, ok := targetObjects[nativeCollisionKey(o)]; ok && !o.Managed && !nativeDerived(o.Kind) {
			p.Conflicts = append(p.Conflicts, conflict)
			if !m.cfg.OverwriteExisting {
				issue(o.Kind, o.Name, "目标存在同名对象；请处理冲突或启用覆盖创建")
			} else if conflict.DropSQL == "" {
				issue(o.Kind, o.Name, "同名目标对象没有可验证的删除语句")
			} else if !plannedDrops[conflict.ID] {
				p.Drops = append(p.Drops, conflict)
				plannedDrops[conflict.ID] = true
			}
		}
		for _, ref := range o.External {
			if ref.Schema == "SYS" || ref.Schema == "SYSPUBLIC" || ref.Schema == "PUBLIC" {
				continue
			}
			if !targetNames[ref.Schema+"\x00"+ref.Name] {
				if checker, ok := w.(interface {
					CheckDamengNativeReference(context.Context, damengnative.Reference) error
				}); ok {
					if e := checker.CheckDamengNativeReference(ctx, ref); e != nil {
						issue(o.Kind, o.Name, e.Error())
					}
				} else {
					issue(o.Kind, o.Name, fmt.Sprintf("缺少外部依赖 %s.%s；请在目标端提前准备", ref.Schema, ref.Name))
				}
			}
		}
	}
	declaredChildren := damengnative.DeclaredTableChildren(p.Objects)
	childConflicts, childIssues := nativeOwnedNameConflicts(append(append([]damengnative.Object{}, p.Objects...), declaredChildren...), dst.Objects, plannedDrops)
	for _, conflict := range childConflicts {
		found := false
		for _, existing := range p.Conflicts {
			found = found || existing.ID == conflict.ID
		}
		if !found {
			p.Conflicts = append(p.Conflicts, conflict)
		}
	}
	p.Issues = append(p.Issues, childIssues...)
	// Do not invalidate target-only dependants while replacing selected objects.
	plannedNames := map[string]bool{}
	for _, o := range p.Objects {
		plannedNames[nativeCollisionKey(o)] = true
	}
	for _, o := range declaredChildren {
		plannedNames[nativeCollisionKey(o)] = true
	}
	for _, o := range dst.Objects {
		if o.Managed && !nativeNamedOwnedObject(o) || plannedNames[nativeCollisionKey(o)] {
			continue
		}
		for _, dep := range o.Dependencies {
			if plannedDrops[dep] {
				issue(o.Kind, o.Name, "目标独有对象依赖待覆盖对象，不能安全覆盖")
			}
		}
	}
	p.Objects = nativePromoteFoundationDependencies(p.Objects)
	p.Objects = damengnative.Order(p.Objects)
	p.Drops = damengnative.Order(p.Drops)
	if checker, ok := w.(interface {
		ValidateDamengNativeObjects(context.Context, []damengnative.Object) []damengnative.Issue
	}); ok {
		checks := append([]damengnative.Object{}, p.Objects...)
		for i := range checks {
			checks[i].Units = append([]damengnative.Unit{}, checks[i].Units...)
			if checks[i].CompileSQL != "" {
				checks[i].Units = append(checks[i].Units, damengnative.Unit{SQL: checks[i].CompileSQL, Phase: "compile"})
			}
			for _, table := range snap.Tables {
				if table.ObjectID != checks[i].ID {
					continue
				}
				for _, unit := range table.Restore {
					sql, _, e := damengnative.RewriteSchema(unit.SQL, p.Source.Schema, p.Target.Schema)
					if e != nil {
						issue(checks[i].Kind, checks[i].Name, e.Error())
						continue
					}
					checks[i].Units = append(checks[i].Units, damengnative.Unit{SQL: sql, Phase: "restore"})
				}
			}
		}
		for _, drop := range p.Drops {
			check := drop
			check.Managed = false
			check.Units = []damengnative.Unit{{SQL: drop.DropSQL, Phase: "drop"}}
			checks = append(checks, check)
		}
		p.Issues = append(p.Issues, checker.ValidateDamengNativeObjects(ctx, checks)...)
	}
	for i, j := 0, len(p.Drops)-1; i < j; i, j = i+1, j-1 {
		p.Drops[i], p.Drops[j] = p.Drops[j], p.Drops[i]
	}
	if !damengnative.HasErrors(p.Issues) {
		p.Issues = append(p.Issues, preflightDamengNativeValues(ctx, r, snap)...)
	}
	p.Ready = !damengnative.HasErrors(p.Issues) && ctx.Err() == nil
	if ctx.Err() != nil {
		issue("task", "预检", ctx.Err().Error())
	}
	p.Fingerprint = damengnative.Fingerprint(struct {
		Source, Target damengnative.Profile
		Objects, Drops []damengnative.Object
	}{p.Source, p.Target, p.Objects, p.Drops})
	return p
}

func preflightDamengNativeValues(ctx context.Context, r damengnative.Reader, snapshot *damengnative.Snapshot) []damengnative.Issue {
	var issues []damengnative.Issue
	for _, table := range snapshot.Tables {
		var columns []damengnative.Column
		hasComplex := false
		for _, c := range table.Columns {
			if !c.Writable {
				continue
			}
			columns = append(columns, c)
			hasComplex = hasComplex || c.TypeSchema != "" && c.TypeName != ""
		}
		if !hasComplex || table.Rows == 0 {
			continue
		}
		scan, err := r.OpenDamengNativeScan(ctx, table)
		if err == nil {
			for scan.Next() {
				if err = ctx.Err(); err != nil {
					break
				}
				var values []any
				values, err = scan.Values()
				if err != nil {
					break
				}
				expectedWidth := len(columns)
				if table.CaptureRowID {
					expectedWidth++
				}
				if len(values) != expectedWidth {
					err = fmt.Errorf("原生数据扫描的可写列数量不符")
					break
				}
				for i, c := range columns {
					if c.TypeSchema != "" && c.TypeName != "" {
						if err = target.ValidateDamengNativeValue(c, values[i]); err != nil {
							err = fmt.Errorf("%s.%s: %w", table.Name, c.Name, err)
							break
						}
					}
				}
				if err != nil {
					break
				}
			}
			if err == nil {
				err = scan.Err()
			}
			err = errors.Join(err, scan.Close())
		}
		if err != nil {
			issues = append(issues, damengnative.Issue{ObjectID: table.ObjectID, Kind: "data", Name: table.Name, Severity: "error", Reason: "复杂类型无损读取/绑定预检失败: " + err.Error()})
		}
	}
	return issues
}

func nativeSameDatabase(a, b damengnative.Profile) bool {
	return a.Instance != "" && a.Instance == b.Instance || a.Database != "" && a.Database == b.Database
}

func nativeDerived(kind string) bool {
	return kind == "comment" || kind == "grant" || kind == "object_grant"
}

// A table default/domain/type may depend on a stored function or synonym. Such
// dependencies must exist before table creation even though ordinary programs
// are installed after data. Business triggers are never promoted into loading.
func nativePromoteFoundationDependencies(objects []damengnative.Object) []damengnative.Object {
	byID := map[string]int{}
	for i, o := range objects {
		byID[o.ID] = i
	}
	needed := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if needed[id] {
			return
		}
		i, ok := byID[id]
		if !ok {
			return
		}
		o := objects[i]
		if o.Kind == "trigger" || o.Kind == "materialized_view" || o.Kind == "materialized_view_log" || nativeDerived(o.Kind) {
			return
		}
		needed[id] = true
		for _, dep := range o.Dependencies {
			visit(dep)
		}
	}
	for _, o := range objects {
		for _, u := range o.Units {
			if u.Phase == "before_data" {
				visit(o.ID)
				break
			}
		}
	}
	for id := range needed {
		i := byID[id]
		objects[i].Units = append([]damengnative.Unit{}, objects[i].Units...)
		for j, u := range objects[i].Units {
			if u.Phase == "after_data" && (objects[i].Kind == "function" || objects[i].Kind == "procedure" || objects[i].Kind == "package_body" || objects[i].Kind == "class_body" || objects[i].Kind == "type_body" || objects[i].Kind == "view" || objects[i].Kind == "synonym" || objects[i].Kind == "db_link") {
				objects[i].Units[j].Phase = "before_data"
			}
		}
	}
	return objects
}
func nativeCollisionKey(o damengnative.Object) string {
	ns := "object"
	switch o.Kind {
	case "index", "context_index":
		ns = "index"
	case "primary_key", "foreign_key", "unique", "check", "constraint":
		ns = "constraint"
	case "trigger":
		ns = "trigger"
	case "package_body", "class_body", "type_body":
		ns = "body"
	case "db_link":
		ns = "db_link"
	case "comment", "grant", "object_grant":
		ns = o.Kind
	}
	return ns + "\x00" + o.Name
}

func (m *Migrator) runDamengNative(ctx context.Context) MigrationReport {
	m.repairs = nil
	report := newMigrationReport()
	report.TriggerMigrationEnabled = true
	p := m.PreflightDamengNative(ctx)
	report.NativeSchema = &NativeSchemaReport{Preflight: p, Inventory: append([]damengnative.Object{}, p.Objects...), Categories: []NativeCategoryReport{}, Verification: []damengnative.Issue{}}
	for i := range report.NativeSchema.Inventory {
		report.NativeSchema.Inventory[i].Status = "pending"
		if !p.Ready {
			report.NativeSchema.Inventory[i].Status = "blocked"
		}
	}
	if !p.Ready {
		for _, issue := range p.Issues {
			if issue.Severity != "warning" {
				m.log.Errorf("达梦原生预检 [%s]: %s", issue.Name, issue.Reason)
			}
		}
		return report
	}
	m.cfg.TargetSchema = p.Target.Schema
	r := m.reader.(damengnative.Reader)
	w := m.writer.(damengnative.Writer)
	if err := m.prepareDamengNativeRepair(p); err != nil {
		report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("修复清单", err))
		return report
	}
	tables := make([]string, 0, len(p.Snapshot.Tables))
	for _, t := range p.Snapshot.Tables {
		if t.Shadow {
			continue
		}
		tables = append(tables, nativeSourceTable(t))
	}
	m.freezeDataValidation(ctx, tables)
	for i := range m.validationTables {
		for _, t := range p.Snapshot.Tables {
			if t.Shadow {
				continue
			}
			if m.validationTables[i].name == nativeSourceTable(t) {
				m.validationTables[i].target = t.Name
			}
		}
	}
	m.logPreparationComplete(ctx)
	m.log.Info("=== 达梦原生迁移：覆盖与基础对象 ===")
	states := map[string]error{}
	for _, drop := range p.Drops {
		if err := m.executeNativeDrop(ctx, w, drop); err != nil {
			report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure(drop.Name, err))
			return report
		}
	}
	if err := m.executeNativePhase(ctx, w, p.Objects, "before_data", states); err != nil {
		report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("基础对象", err))
		m.collectNativeResults(&report, p, states)
		return report
	}
	if err := m.executeNativePhase(ctx, w, p.Objects, "interval_prepare", states); err != nil {
		report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("既有间隔分区", err))
		m.collectNativeResults(&report, p, states)
		return report
	}
	m.log.Info("=== 达梦原生迁移：复制原始数据 ===")
	type result struct {
		table damengnative.Table
		rows  int64
		err   error
	}
	results := make(chan result, len(p.Snapshot.Tables))
	limit := m.cfg.MaxParallel
	if limit < 1 {
		limit = 4
	}
	if limit > 32 {
		limit = 32
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, t := range p.Snapshot.Tables {
		wg.Add(1)
		go func(t damengnative.Table) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- result{table: t, err: ctx.Err()}
				return
			}
			n, e := copyDamengNativeTable(ctx, r, w, t, p.Target.Schema, m.cfg.PageSize, nil)
			results <- result{table: t, rows: n, err: e}
		}(t)
	}
	wg.Wait()
	close(results)
	dataFailed := false
	for res := range results {
		t := res.table
		name := nativeSourceTable(t)
		report.Data.Total++
		err := res.err
		dstRows := int64(-1)
		if err == nil {
			dstRows, err = m.writer.CountRows(ctx, t.Name)
			if err == nil && (res.rows != t.Rows || dstRows != t.Rows) {
				err = fmt.Errorf("行数不同: 冻结源=%d 已复制=%d 目标=%d", t.Rows, res.rows, dstRows)
			}
		}
		report.RowCounts = append(report.RowCounts, TableRowCount{Table: name, Src: t.Rows, Dst: dstRows, Match: err == nil})
		if err != nil {
			dataFailed = true
			report.Data.Failed++
			report.Data.Items = append(report.Data.Items, ObjectResult{Name: name, TargetName: t.Name, Stage: "execute", Error: err.Error()})
			m.log.Errorf("复制 [%s] 失败: %v", name, err)
		} else {
			report.Data.Success++
			m.markDataValidationCopied(name)
			m.log.Dataf("复制 [%s]: %d 行", name, res.rows)
		}
		if e := m.setNativeDataStatus(t.ObjectID, err); e != nil {
			dataFailed = true
			report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure(name, e))
		}
	}
	sort.Slice(report.RowCounts, func(i, j int) bool { return report.RowCounts[i].Table < report.RowCounts[j].Table })
	if dataFailed {
		m.collectNativeResults(&report, p, states)
		return report
	}
	m.log.Info("=== 达梦原生迁移：依赖对象与原始状态 ===")
	for _, phase := range []string{"after_data", "restore"} {
		if err := m.executeNativePhase(ctx, w, p.Objects, phase, states); err != nil {
			report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure(phase, err))
			break
		}
	}
	for _, t := range p.Snapshot.Tables {
		for _, u := range t.Restore {
			sql, _, err := damengnative.RewriteSchema(u.SQL, p.Source.Schema, p.Target.Schema)
			if err == nil {
				err = m.executeNativeSavedUnit(ctx, w, t.ObjectID, sql)
			}
			if err != nil {
				states[t.ObjectID] = err
				report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure(t.Name, err))
			}
		}
	}
	// Successful CREATE does not prove that a procedural object compiled.
	for _, o := range p.Objects {
		if o.Kind == "trigger" {
			continue
		}
		if o.CompileSQL != "" {
			if err := m.executeNativeSavedUnit(ctx, w, o.ID, o.CompileSQL); err != nil {
				states[o.ID] = err
			}
		}
	}
	if err := m.executeNativePhase(ctx, w, p.Objects, "mv_prepare", states); err != nil {
		report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("物化视图基线准备", err))
	}
	if err := m.executeNativePhase(ctx, w, p.Objects, "mv_rebase", states); err != nil {
		report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("物化视图增量基线", err))
	}
	// History recovery is one frozen group: every base table, snapshot staging
	// table and log staging table must be loaded before any baseline is changed.
	// A failed group keeps all business hooks and automatic refreshes paused.
	if nativeHasMaterializedHistory(p.Objects) {
		if len(states) > 0 || damengnative.HasErrors(report.NativeSchema.Verification) {
			m.collectNativeResults(&report, p, states)
			return report
		}
		if err := m.executeNativePhase(ctx, w, p.Objects, "mv_history", states); err != nil {
			report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("陈旧物化快照与增量日志", err))
			m.collectNativeResults(&report, p, states)
			return report
		}
	}
	if err := m.executeNativePhase(ctx, w, p.Objects, "after_history", states); err != nil {
		report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("物化承载表索引", err))
		m.collectNativeResults(&report, p, states)
		return report
	}
	m.markNativeRowIDValidation(p, states)
	if err := m.executeNativePhase(ctx, w, p.Objects, "triggers", states); err != nil {
		report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("触发器", err))
	}
	if err := m.executeNativePhase(ctx, w, p.Objects, "event_create", states); err != nil {
		report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("事件触发器禁用安装", err))
	}
	for _, o := range p.Objects {
		if o.Kind == "trigger" && o.CompileSQL != "" {
			if err := m.executeNativeSavedUnit(ctx, w, o.ID, o.CompileSQL); err != nil {
				states[o.ID] = err
			}
		}
	}
	if err := m.executeNativePhase(ctx, w, p.Objects, "trigger_restore", states); err != nil {
		report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("触发器原始状态", err))
	}
	// Imported event hooks remain disabled throughout every target query and
	// optional full data comparison. Their original states are restored in Run
	// only after validateDataContent has finished successfully.
	verify := *p.Snapshot
	verify.Objects = append([]damengnative.Object{}, p.Snapshot.Objects...)
	for i := range verify.Objects {
		if verify.Objects[i].EventTrigger {
			verify.Objects[i].Status = "DISABLED"
		}
	}
	issues, err := w.VerifyDamengNative(ctx, &verify)
	if err != nil {
		report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("目标核验", err))
	}
	report.NativeSchema.Verification = append(report.NativeSchema.Verification, issues...)
	if ctx.Err() != nil {
		report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("任务", ctx.Err()))
	}
	report.NativeSchema.pendingPlan, report.NativeSchema.pendingStates = p, states
	return report
}

func nativeHasMaterializedHistory(objects []damengnative.Object) bool {
	for _, object := range objects {
		if object.Materialized != nil && object.Materialized.History != nil {
			return true
		}
	}
	return false
}

func nativeFailure(name string, err error) damengnative.Issue {
	return damengnative.Issue{Name: name, Severity: "error", Reason: err.Error()}
}

func (m *Migrator) executeNativePhase(ctx context.Context, w damengnative.Writer, objects []damengnative.Object, phase string, states map[string]error) error {
	type work struct {
		o   damengnative.Object
		sql string
	}
	var pending []work
	for _, o := range objects {
		if o.Managed {
			continue
		}
		for _, u := range o.Units {
			actual := u.Phase
			if actual == "" {
				actual = "after_data"
				if damengnative.Rank(o.Kind) < 3 {
					actual = "before_data"
				}
			}
			if actual == phase {
				pending = append(pending, work{o, u.SQL})
			}
		}
	}
	for len(pending) > 0 {
		progress := false
		next := make([]work, 0, len(pending))
		last := map[string]error{}
		for _, item := range pending {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := m.executeNativeSavedUnit(ctx, w, item.o.ID, item.sql); err != nil {
				last[item.o.ID] = err
				next = append(next, item)
			} else {
				progress = true
				delete(states, item.o.ID)
			}
		}
		if len(next) == 0 {
			return nil
		}
		if !progress {
			var errs []error
			for id, e := range last {
				states[id] = e
				errs = append(errs, e)
			}
			return errors.Join(errs...)
		}
		pending = next
	}
	return nil
}

func nativeSourceTable(t damengnative.Table) string {
	if t.SourceName != "" {
		return t.SourceName
	}
	return t.Name
}

func copyDamengNativeTable(ctx context.Context, r damengnative.Reader, w damengnative.Writer, t damengnative.Table, targetSchema string, pageSize int, progress func(int64) error) (int64, error) {
	var err error
	targetTable := t
	targetTable.Schema = targetSchema
	targetTable.Columns = append([]damengnative.Column{}, t.Columns...)
	for i, c := range targetTable.Columns {
		if c.TypeSchema == t.Schema {
			targetTable.Columns[i].TypeSchema = targetSchema
		}
		targetTable.Columns[i].SQLType, _, err = damengnative.RewriteSchema(c.SQLType, t.Schema, targetSchema)
		if err != nil {
			return 0, err
		}
		targetTable.Columns[i].DefaultSQL, _, err = damengnative.RewriteSchema(c.DefaultSQL, t.Schema, targetSchema)
		if err != nil {
			return 0, err
		}
	}
	restoreEmptyDefaults := func() error {
		for _, c := range targetTable.Columns {
			if c.Writable && c.DefaultOnNull {
				return w.CopyDamengNativeBatch(ctx, targetTable, nil)
			}
		}
		return nil
	}
	if t.Rows == 0 {
		return 0, restoreEmptyDefaults()
	}
	scan, err := r.OpenDamengNativeScan(ctx, t)
	if err != nil {
		return 0, err
	}
	defer scan.Close()
	if pageSize <= 0 || pageSize > 2000 {
		pageSize = 1000
	}
	var count int64
	bytes := 0
	batch := make([][]any, 0, pageSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := w.CopyDamengNativeBatch(ctx, targetTable, batch); e != nil {
			return e
		}
		count += int64(len(batch))
		batch = nil
		bytes = 0
		if progress != nil {
			return progress(count)
		}
		return nil
	}
	for scan.Next() {
		if err = ctx.Err(); err != nil {
			return count, err
		}
		row, e := scan.Values()
		if e != nil {
			return count, e
		}
		size := 0
		for _, v := range row {
			n, e := target.DamengNativeValueSize(v)
			if e != nil {
				return count, e
			}
			size += n
		}
		if len(batch) > 0 && (len(batch) >= pageSize || bytes+size > 8*1024*1024) {
			if err = flush(); err != nil {
				return count, err
			}
		}
		batch = append(batch, row)
		bytes += size
	}
	if err = scan.Err(); err != nil {
		return count, err
	}
	if err = flush(); err != nil {
		return count, err
	}
	if count == 0 {
		return 0, restoreEmptyDefaults()
	}
	return count, nil
}
