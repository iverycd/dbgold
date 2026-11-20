package datamigrate

import (
	"context"
	"fmt"
	"slices"

	"dbgold/datamigrate/dialect"
)

// View preparation reads the source catalog and dependency failures from the current job.
func (m *Migrator) resetViewMigrationState() {
	m.repairs = nil
	m.sqlServerSource = nil
	m.compatibilityFunctions = nil
	m.partitionState = nil
	m.dmPlans, m.dmUsable, m.dmBlocked = nil, nil, nil
	m.resetOscarMySQLViews()
	m.resetSourceTableDDLCache()
	m.tableState.mu.Lock()
	m.tableState.failures = map[string]migrationTableFailure{}
	m.tableState.objects = map[ObjectRef]error{}
	m.tableState.mu.Unlock()
}

// RunSelectedViews records a synchronous view migration without entering the
// full-table pipeline. Dedicated planners own their view-only execution and SQL.
func (m *Migrator) RunSelectedViews(ctx context.Context) (MigrationReport, []ObjectResult) {
	report := newMigrationReport()
	if len(m.cfg.ViewNames) == 0 {
		r := ObjectResult{Name: "视图范围", Stage: "preflight", Error: "必须选择视图"}
		report.Views = CategoryReport{Total: 1, Failed: 1, Items: []ObjectResult{r}}
		return report, report.Views.Items
	}
	previousContent := m.cfg.Content
	m.cfg.Content = "schema_only"
	defer func() { m.cfg.Content = previousContent }()
	if m.isCrossOracle() || m.isOraclePGSQLServer() || m.isPGOracle() {
		report = m.Run(ctx)
		return report, m.plannedViewResults(report)
	}

	m.resetViewMigrationState()
	defer m.clearSourceTableDDLCache()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ctx = context.WithValue(ctx, oracleDamengTaskKey{}, cancel)
	if m.cfg.CaptureRepair {
		m.beginSelectedViewRepair(ctx)
	}
	if m.repairs != nil && m.repairs.sinkErr != nil {
		report.Data = CategoryReport{Total: 1, Failed: 1, Items: []ObjectResult{{Name: "视图迁移准备", Stage: "task", Error: m.repairs.sinkErr.Error()}}}
		return report, report.Data.Items
	}

	var results []ObjectResult
	if m.isPGMySQL() {
		report = m.runPGMySQL(ctx)
	} else {
		results = m.MigrateViews(ctx, m.cfg.ViewNames)
		for _, result := range results {
			report.Views.Total++
			if result.Error != "" {
				report.Views.Failed++
				report.Views.Items = append(report.Views.Items, result)
			} else {
				report.Views.Success++
			}
		}
		report.CompatibilityFunctions = m.CompatibilityFunctions()
	}
	if cause := context.Cause(ctx); cause != nil && !report.HasFailures() {
		report.Data.Total++
		report.Data.Failed++
		report.Data.Items = append(report.Data.Items, ObjectResult{Name: "视图迁移", Stage: "task", Error: cause.Error()})
	}
	if m.cfg.CaptureRepair {
		m.finishRepair(ctx, &report)
	}
	if m.isPGMySQL() {
		return report, m.plannedViewResults(report)
	}
	return report, append(results, report.Data.Items...)
}

func (m *Migrator) beginSelectedViewRepair(ctx context.Context) {
	m.repairs = &repairRecorder{}
	if m.cfg.RepairObjectSink != nil && m.cfg.RepairSink == nil {
		m.repairs.sinkErr = fmt.Errorf("单对象修复记录保存需要配置完整清单保存回调")
		return
	}
	switch {
	case m.isPGMySQL():
		// repairPlanned records the exact units from PG's view-only preflight.
	case m.isSQLServer():
		m.recordSQLServerViewManifest(ctx, m.cfg.ViewNames, nil)
	case m.isOracle():
		m.recordOracleViewManifest(ctx, m.cfg.ViewNames)
	default:
		views, err := m.readViews(ctx)
		if err != nil {
			m.repairs.sinkErr = fmt.Errorf("读取视图元数据: %w", err)
			return
		}
		for _, v := range views {
			if !slices.Contains(m.cfg.ViewNames, v.ViewName) {
				continue
			}
			o, statements := m.viewRepairCandidate(ctx, v)
			o.Status, o.RequiresReview = "pending", true
			o.DDL, o.Units = dialect.JoinSQL(statements), unitsFor(statements)
			m.repairs.appendObject(o)
		}
	}
	if m.repairs.sinkErr == nil {
		_ = m.flushRepairsLocked()
	}
}

// Some dedicated planners report success counts and only include failed rows.
// Keep the original synchronous per-view results alongside their full report.
func (m *Migrator) plannedViewResults(report MigrationReport) []ObjectResult {
	results := append([]ObjectResult{}, report.Views.Items...)
	results = append(results, report.Tables.Items...)
	results = append(results, report.Data.Items...)
	var issues []CompatibilityIssue
	switch {
	case report.Preflight != nil && !report.Preflight.Ready:
		issues = report.Preflight.Issues
	case report.PGOraclePreflight != nil && report.PGOraclePreflight.globalError:
		issues = report.PGOraclePreflight.Issues
	case report.CrossPreflight != nil && !report.CrossPreflight.CanStart && len(results) == 0:
		issues = report.CrossPreflight.Issues
	}
	for _, issue := range issues {
		if issue.Severity == "error" && !slices.ContainsFunc(results, func(item ObjectResult) bool {
			return item.Name == issue.Object.Name && item.Error == issue.Reason
		}) {
			results = append(results, ObjectResult{Name: issue.Object.Name, SourceName: issue.Object.Name, Error: issue.Reason, SourceDDL: issue.Definition, Stage: "preflight"})
		}
	}
	if report.Views.Total == len(m.cfg.ViewNames) && report.Views.Success > 0 {
		for _, sourceName := range m.cfg.ViewNames {
			name := m.objName(sourceName)
			if !slices.ContainsFunc(results, func(item ObjectResult) bool {
				return item.Name == sourceName || item.SourceName == sourceName || item.Name == name
			}) {
				results = append(results, ObjectResult{Name: name, SourceName: sourceName, TargetName: name})
			}
		}
	}
	return results
}
