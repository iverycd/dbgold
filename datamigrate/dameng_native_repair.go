package datamigrate

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

func (m *Migrator) prepareDamengNativeRepair(p *damengnative.Preflight) error {
	if !m.cfg.CaptureRepair {
		m.cfg.RepairSink, m.cfg.RepairObjectSink = nil, nil
	}
	m.repairs = &repairRecorder{}
	if m.cfg.RepairObjectSink != nil && m.cfg.RepairSink == nil {
		return fmt.Errorf("原生单对象进度保存需要完整清单保存回调")
	}
	sourceObjects := map[string]damengnative.Object{}
	for _, o := range p.Snapshot.Objects {
		sourceObjects[o.ID] = o
	}
	tables := map[string]damengnative.Table{}
	for _, t := range p.Snapshot.Tables {
		tables[t.ObjectID] = t
	}
	add := func(o damengnative.Object, sourceDDL string) {
		item := repair.Object{ID: o.ID, Kind: o.Kind, Name: o.Name, SourceName: o.Name, TargetName: o.Name, Schema: o.Schema, Table: o.Table, SourceTable: o.Table, Dependencies: append([]string{}, o.Dependencies...), Status: "pending", DataStatus: "not_required", Native: &o, NativeFingerprint: damengnative.Fingerprint(o), SourceDDL: sourceDDL, RequiresReview: false, Units: []repair.Unit{}}
		if o.Managed {
			item.DiagnosticOnly = true
		}
		for i, u := range o.Units {
			destructive := destructiveRepairUnit(u.SQL)
			item.Units = append(item.Units, repair.Unit{ID: fmt.Sprintf("native-%d", i+1), SQL: u.SQL, OriginalSQL: u.SQL, Status: "pending", Destructive: destructive, Selected: !destructive})
			if item.DDL != "" {
				item.DDL += "\n"
			}
			item.DDL += u.SQL
		}
		if t, ok := tables[o.ID]; ok {
			item.NativeTable = &t
			item.SourceTable = nativeSourceTable(t)
			item.SourceName = item.SourceTable
			item.Table = t.Name
			item.DataStatus = "pending"
			item.ColumnMapping = map[string]string{}
			for _, c := range t.Columns {
				item.SourceColumns = append(item.SourceColumns, source.ColumnInfo{Name: c.Name, DataType: c.DataType, FormattedType: c.SQLType, Precision: c.Precision, Scale: c.Scale, TypeSchema: c.TypeSchema, TypeKind: c.TypeName})
				item.ColumnMapping[c.Name] = c.Name
			}
			item.Columns = append([]source.ColumnInfo{}, item.SourceColumns...)
		}
		m.repairs.objects = append(m.repairs.objects, item)
	}
	for _, drop := range p.Drops {
		o := drop
		o.ID = "drop-" + o.ID
		o.Kind = "native_drop"
		o.Managed = false
		o.Units = []damengnative.Unit{{SQL: drop.DropSQL, Phase: "drop"}}
		o.Dependencies = nil
		add(o, "")
	}
	for _, o := range p.Objects {
		o.Units = append([]damengnative.Unit{}, o.Units...)
		if t, ok := tables[o.ID]; ok {
			for _, u := range t.Restore {
				mapped, _, err := damengnative.RewriteSchema(u.SQL, p.Source.Schema, p.Target.Schema)
				if err != nil {
					return err
				}
				o.Units = append(o.Units, damengnative.Unit{SQL: mapped, Phase: "restore"})
			}
		}
		if o.CompileSQL != "" {
			o.Units = append(o.Units, damengnative.Unit{SQL: o.CompileSQL, Phase: "compile"})
		}
		var sourceDDL []string
		for _, u := range sourceObjects[o.ID].Units {
			sourceDDL = append(sourceDDL, u.SQL)
		}
		add(o, strings.Join(sourceDDL, "\n"))
	}
	return m.flushRepairsLocked()
}

func (m *Migrator) executeNativeDrop(ctx context.Context, w damengnative.Writer, o damengnative.Object) error {
	return m.executeNativeSavedUnit(ctx, w, "drop-"+o.ID, o.DropSQL)
}

func (m *Migrator) executeNativeSavedUnit(ctx context.Context, w damengnative.Writer, id, sql string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.repairs == nil {
		return w.ExecuteDamengNative(ctx, sql)
	}
	r := m.repairs
	r.mu.Lock()
	if r.sinkErr != nil {
		e := r.sinkErr
		r.mu.Unlock()
		return e
	}
	idx, unit := -1, -1
	for i := range r.objects {
		if r.objects[i].ID != id {
			continue
		}
		idx = i
		for j, u := range r.objects[i].Units {
			if u.SQL == sql && u.Status != "succeeded" {
				unit = j
				break
			}
		}
		break
	}
	if idx < 0 {
		r.mu.Unlock()
		return fmt.Errorf("执行对象不在冻结的原生清单中: %s", id)
	}
	if unit < 0 {
		for _, u := range r.objects[idx].Units {
			if u.SQL == sql && u.Status == "succeeded" {
				r.mu.Unlock()
				return nil
			}
		}
		r.mu.Unlock()
		return fmt.Errorf("执行语句不在冻结的原生对象中: %s", id)
	}
	if r.objects[idx].Units[unit].Status == "unknown" {
		r.mu.Unlock()
		return fmt.Errorf("对象执行结果未知，需先核验目标状态: %s", id)
	}
	object := *r.objects[idx].Native
	nativeUnit := object.Units[unit]
	r.mu.Unlock()
	observed := repair.WithObserver(ctx, func(event repair.Event) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.sinkErr != nil {
			return r.sinkErr
		}
		o := &r.objects[idx]
		u := &o.Units[unit]
		u.Status = event.Status
		u.Error = ""
		if event.Error != "" {
			u.Error = event.Error
		}
		u.Selected = event.Status != "succeeded" && !u.Destructive
		o.Status = "running"
		o.Error = u.Error
		if event.Status == "failed" || event.Status == "unknown" {
			o.Status = event.Status
		}
		return m.flushRepairObjectLocked(*o)
	})
	return damengnative.ExecuteUnit(observed, w, object, nativeUnit)
}

func (m *Migrator) setNativeDataStatus(id string, cause error) error {
	if m.repairs == nil {
		return nil
	}
	r := m.repairs
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.objects {
		if r.objects[i].ID == id {
			o := &r.objects[i]
			o.DataStatus = "succeeded"
			if cause != nil {
				o.DataStatus = "failed"
				o.Error = cause.Error()
			}
			return m.flushRepairObjectLocked(*o)
		}
	}
	return fmt.Errorf("数据表不在原生修复清单中: %s", id)
}

func nativeCategory(report *MigrationReport, kind string) *CategoryReport {
	switch kind {
	case "table":
		return &report.Tables
	case "view":
		return &report.Views
	case "primary_key":
		return &report.PrimaryKeys
	case "index", "context_index":
		return &report.Indexes
	case "constraint", "foreign_key", "check", "unique":
		return &report.Constraints
	case "sequence":
		return &report.Sequences
	case "comment":
		return &report.Comments
	case "trigger":
		return &report.Triggers
	}
	for i := range report.NativeSchema.Categories {
		if report.NativeSchema.Categories[i].Kind == kind {
			return &report.NativeSchema.Categories[i].CategoryReport
		}
	}
	labels := map[string]string{"function": "函数", "procedure": "存储过程", "package": "包", "package_body": "包体", "type": "自定义类型", "type_body": "类型体", "class": "类声明", "class_body": "类体", "domain": "域", "synonym": "私有同义词", "db_link": "私有数据库链接", "materialized_view": "物化视图", "materialized_view_log": "物化视图日志", "grant": "对象授权", "object_grant": "对象授权", "jclass": "Java 类"}
	label := labels[kind]
	if label == "" {
		label = kind
	}
	report.NativeSchema.Categories = append(report.NativeSchema.Categories, NativeCategoryReport{Kind: kind, Label: label, CategoryReport: newCategoryReport()})
	return &report.NativeSchema.Categories[len(report.NativeSchema.Categories)-1].CategoryReport
}

func nativeReportKind(o damengnative.Object) string {
	if o.Kind == "constraint" {
		switch o.ConstraintType {
		case "P":
			return "primary_key"
		case "R":
			return "foreign_key"
		case "U":
			return "unique"
		case "C":
			return "check"
		}
	}
	return o.Kind
}

func (m *Migrator) collectNativeResults(report *MigrationReport, p *damengnative.Preflight, states map[string]error) {
	status := map[string]string{}
	ledger := map[string]repair.Object{}
	if m.repairs != nil {
		m.repairs.mu.Lock()
		for _, o := range m.repairs.objects {
			ledger[o.ID] = o
		}
		m.repairs.mu.Unlock()
	}
	for _, o := range p.Objects {
		err := states[o.ID]
		state := "succeeded"
		if item, ok := ledger[o.ID]; ok && !o.Managed {
			for _, u := range item.Units {
				if u.Status != "succeeded" {
					state = "failed"
					if err == nil {
						err = fmt.Errorf("执行步骤未完成: %s", u.Status)
					}
				}
			}
			if item.DataStatus == "failed" {
				state = "failed"
				if err == nil {
					err = fmt.Errorf("表数据复制失败")
				}
			}
		}
		if err != nil {
			state = "failed"
		}
		if o.Managed {
			continue
		}
		status[o.ID] = state
		cat := nativeCategory(report, nativeReportKind(o))
		cat.Total++
		if state == "succeeded" {
			cat.Success++
		} else {
			cat.Failed++
			var ddl []string
			for _, u := range o.Units {
				ddl = append(ddl, u.SQL)
			}
			cat.Items = append(cat.Items, ObjectResult{Name: o.Name, TargetName: o.Name, TargetSchema: o.Schema, Table: o.Table, DDL: strings.Join(ddl, "\n"), Stage: "execute", Error: err.Error()})
		}
	}
	// Managed inventory entries are represented by their parent's result, while
	// retaining every source identity in the report's inventory for reconciliation.
	for _, o := range p.Objects {
		if !o.Managed {
			continue
		}
		state := "succeeded"
		if o.ParentID != "" && status[o.ParentID] != "succeeded" {
			state = "failed"
		}
		status[o.ID] = state
		cat := nativeCategory(report, nativeReportKind(o))
		cat.Total++
		if state == "succeeded" {
			cat.Success++
		} else {
			cat.Failed++
			cat.Items = append(cat.Items, ObjectResult{Name: o.Name, Stage: "dependency", Error: "所属原生对象未完成"})
		}
	}
	for i := range report.NativeSchema.Inventory {
		report.NativeSchema.Inventory[i].Status = status[report.NativeSchema.Inventory[i].ID]
	}
	if m.repairs != nil {
		m.repairs.mu.Lock()
		for i := range m.repairs.objects {
			o := &m.repairs.objects[i]
			if o.Kind == "native_drop" {
				if o.AllUnitsSucceeded() {
					o.Status = "succeeded"
				}
				continue
			}
			if state, ok := status[o.ID]; ok {
				o.Status = state
				if state == "failed" && o.Error == "" {
					o.Error = "原生对象或关联数据未完成"
				}
			}
		}
		m.repairs.mu.Unlock()
	}
}

func (m *Migrator) finishDamengNativeRepair(report *MigrationReport) {
	if m.repairs == nil {
		return
	}
	r := m.repairs
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.objects {
		o := &r.objects[i]
		if o.Status == "running" {
			o.Status = "unknown"
		}
		o.OriginalStatus, o.OriginalError = o.Status, o.Error
	}
	err := m.flushRepairsLocked()
	if err == nil {
		err = r.sinkErr
	}
	if err != nil {
		report.NativeSchema.Verification = append(report.NativeSchema.Verification, nativeFailure("修复记录持久化", err))
	}
	manifest := m.manifestLocked()
	report.RepairManifest = &manifest
}
