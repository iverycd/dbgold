package datamigrate

import (
	"errors"
	"fmt"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"dbgold/datamigrate/valueconv"
)

// ObjectResult 单个失败对象的详情
type ObjectResult struct {
	ObjectKind    string                       `json:"object_kind,omitempty"`
	Name          string                       `json:"name"`
	SourceName    string                       `json:"source_name,omitempty"`
	TargetName    string                       `json:"target_name,omitempty"`
	TargetTable   string                       `json:"target_table,omitempty"`
	TargetSchema  string                       `json:"target_schema,omitempty"`
	Table         string                       `json:"table,omitempty"` // Original source table name.
	Stage         string                       `json:"stage,omitempty"` // metadata | preflight | unsupported | execute | dependency | task
	DDL           string                       `json:"ddl"`
	SourceDDL     string                       `json:"source_ddl,omitempty"` // Source definition for reference, never executable target SQL.
	Error         string                       `json:"error"`
	CommittedRows int64                        `json:"committed_rows,omitempty"`
	FailedPage    int                          `json:"failed_page,omitempty"`
	SuggestedDDL  string                       `json:"suggested_ddl,omitempty"`
	Suggestion    string                       `json:"suggestion,omitempty"`
	ColumnRenames []dialect.DamengColumnRename `json:"column_renames,omitempty"`
}

func objectResultForError(name, ddl string, err error) ObjectResult {
	result := ObjectResult{Name: name, DDL: ddl, Error: err.Error()}
	var issue *dialect.DamengColumnNameError
	if errors.As(err, &issue) {
		result.DDL = issue.OriginalDDL
		result.SuggestedDDL = issue.SuggestedDDL
		result.Suggestion = issue.Suggestion
		result.ColumnRenames = issue.ColumnRenames
	}
	return result
}

// CategoryReport 一类对象的迁移统计
type CategoryReport struct {
	TotalUnknown bool           `json:"total_unknown,omitempty"`
	Total        int            `json:"total"`
	Success      int            `json:"success"`
	Failed       int            `json:"failed"`
	Items        []ObjectResult `json:"items"`
}

// TriggerAdaptationSummary describes recorded automatic-numbering trigger
// outcomes without adding the owning sequence failures to category totals.
type TriggerAdaptationSummary struct {
	Mode     string                  `json:"mode,omitempty"`
	Items    []OracleMySQLAutoResult `json:"items,omitempty"`
	Recorded bool                    `json:"recorded"`
	Total    int                     `json:"total"`
	Success  int                     `json:"success"`
	Failed   int                     `json:"failed"`
	Blocked  int                     `json:"blocked"`
	Pending  int                     `json:"pending"`
	Unknown  int                     `json:"unknown"`
}

type OracleMySQLAutoResult struct {
	Table             string `json:"table"`
	Column            string `json:"column"`
	Trigger           string `json:"trigger"`
	SequenceOwner     string `json:"sequence_owner"`
	Sequence          string `json:"sequence"`
	TargetTable       string `json:"target_table"`
	TargetColumn      string `json:"target_column"`
	SharedSequence    bool   `json:"shared_sequence"`
	ConversionStatus  string `json:"conversion_status"`
	CalibrationStatus string `json:"calibration_status"`
	Start             string `json:"start,omitempty"`
	Error             string `json:"error,omitempty"`
}

// A metadata failure is one failure event, not one discovered database object.
// Keep Failed nonzero for existing job-status and report consumers.
func recordMetadataFailure(category *CategoryReport, err error) {
	category.TotalUnknown = true
	category.Failed++
	category.Items = append(category.Items, ObjectResult{Name: "元数据读取", Stage: "metadata", Error: err.Error()})
}

// MigrationReport 完整迁移报告
type MigrationReport struct {
	SpatialCapabilities     *target.PGSpatialCapabilities        `json:"spatial_capabilities,omitempty"`
	TargetDependencies      []OracleMySQLDependencyResult        `json:"target_dependencies,omitempty"`
	PartitionMigration      *PartitionPreflight                  `json:"partition_migration,omitempty"`
	NativeSchema            *NativeSchemaReport                  `json:"native_schema,omitempty"`
	DataValidation          *validation.Report                   `json:"dataValidation,omitempty"`
	TriggerMigrationEnabled bool                                 `json:"trigger_migration_enabled,omitempty"`
	CompatibilityFunctions  *target.CompatibilityFunctionsReport `json:"compatibility_functions,omitempty"`
	RepairManifest          *RepairManifest                      `json:"repair_manifest,omitempty"`
	TriggerAdaptation       *TriggerAdaptationSummary            `json:"trigger_adaptation,omitempty"`
	AutoRepairs             []AutoRepair                         `json:"auto_repairs,omitempty"`
	Warnings                []valueconv.MigrationWarning         `json:"warnings,omitempty"`
	Preflight               *PGMySQLPreflight                    `json:"preflight,omitempty"`
	CrossPreflight          *CrossPreflight                      `json:"cross_preflight,omitempty"`
	PGOraclePreflight       *PGOraclePreflight                   `json:"pg_oracle_preflight,omitempty"`
	Tables                  CategoryReport                       `json:"tables"`
	Data                    CategoryReport                       `json:"data"`
	PrimaryKeys             CategoryReport                       `json:"primaryKeys"`
	Views                   CategoryReport                       `json:"views"`
	Indexes                 CategoryReport                       `json:"indexes"`
	Constraints             CategoryReport                       `json:"constraints"`
	Sequences               CategoryReport                       `json:"sequences"`
	Comments                CategoryReport                       `json:"comments"`
	Triggers                CategoryReport                       `json:"triggers"`
	RowCounts               []TableRowCount                      `json:"rowCounts"`
	DataIssues              []DataIssue                          `json:"data_issues,omitempty"`
}

type NativeCategoryReport struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	CategoryReport
}

type NativeSchemaReport struct {
	Preflight     *damengnative.Preflight `json:"preflight"`
	Inventory     []damengnative.Object   `json:"inventory"`
	Categories    []NativeCategoryReport  `json:"categories"`
	Verification  []damengnative.Issue    `json:"verification"`
	pendingPlan   *damengnative.Preflight
	pendingStates map[string]error
}

// AutoRepair is emitted only after a successful adjustment or existing-column validation.
type AutoRepair struct {
	Type        string `json:"type"`
	Table       string `json:"table"`
	TargetTable string `json:"target_table"`
	Source      string `json:"source"`
	SourceType  string `json:"source_type,omitempty"`
	Target      string `json:"target"`
	Reason      string `json:"reason"`
	Status      string `json:"status"` // applied | reused
}

// TableRowCount 记录单张表的源/目标行数对比
type TableRowCount struct {
	Table string `json:"table"`
	Src   int64  `json:"src"`
	Dst   int64  `json:"dst"`
	Match bool   `json:"match"`
}

// DataIssue records what failed while the copy or verification stage is known.
// It supplements the original category counters without changing job outcomes.
type DataIssue struct {
	Table       string `json:"table"`
	TargetTable string `json:"target_table,omitempty"`
	Kind        string `json:"kind"`
	Message     string `json:"message,omitempty"`
}

const (
	copyError         = "copy_error"
	rowCountMismatch  = "row_count_mismatch"
	rowCountError     = "row_count_error"
	contentValidation = "content_validation"
	verificationError = "verification_error"
)

func (r *MigrationReport) addDataIssue(table, target, kind, message string) {
	r.DataIssues = append(r.DataIssues, DataIssue{Table: table, TargetTable: target, Kind: kind, Message: message})
}

// Carry a preparation/verification cause across copy routines that also return
// ordinary read and write errors. Preserve the original error for cancellation.
type dataCheckError struct {
	kind string
	err  error
}

func (e dataCheckError) Error() string { return e.err.Error() }
func (e dataCheckError) Unwrap() error { return e.err }

func copyIssueKind(err error) string {
	var check dataCheckError
	if errors.As(err, &check) {
		return check.kind
	}
	return copyError
}

func countReadError(src, dst error) error {
	if src != nil {
		src = fmt.Errorf("读取源表行数失败: %w", src)
	}
	if dst != nil {
		dst = fmt.Errorf("读取目标表行数失败: %w", dst)
	}
	return errors.Join(src, dst)
}

// Some routes also check the number of rows acknowledged by the writer. A
// failed three-way check can have equal source and target counts.
func (r *MigrationReport) addRowCount(table, target string, count TableRowCount, readErr error, detail string) {
	r.RowCounts = append(r.RowCounts, count)
	switch {
	case readErr != nil:
		r.addDataIssue(table, target, rowCountError, readErr.Error())
	case count.Src != count.Dst:
		if detail == "" {
			detail = fmt.Sprintf("源行数 %d，目标行数 %d", count.Src, count.Dst)
		}
		r.addDataIssue(table, target, rowCountMismatch, detail)
	case !count.Match:
		r.addDataIssue(table, target, verificationError, detail)
	}
}

func (r *MigrationReport) addContentIssue(table, target string, result validation.TableResult) {
	switch result.Status {
	case validation.StatusPassed:
	case validation.StatusMismatch:
		detail := result.Error
		if detail == "" {
			detail = fmt.Sprintf("%s 内容校验发现差异：缺少 %d 行，多出 %d 行，变化 %d 行，重复键 %d 个",
				result.SourceTable, result.MissingRows, result.ExtraRows, result.ChangedRows, result.DuplicateKeys)
		}
		r.addDataIssue(table, target, contentValidation, detail)
	default:
		r.addDataIssue(table, target, verificationError, result.Error)
	}
}

func newCategoryReport() CategoryReport {
	return CategoryReport{Items: []ObjectResult{}}
}

func newMigrationReport() MigrationReport {
	return MigrationReport{
		Tables:      newCategoryReport(),
		Data:        newCategoryReport(),
		PrimaryKeys: newCategoryReport(),
		Views:       newCategoryReport(),
		Indexes:     newCategoryReport(),
		Constraints: newCategoryReport(),
		Sequences:   newCategoryReport(),
		Comments:    newCategoryReport(),
		Triggers:    newCategoryReport(),
		RowCounts:   []TableRowCount{},
	}
}

// HasFailures is shared by all job entrypoints; read/validation failures count.
func (r MigrationReport) HasFailures() bool {
	for _, dependency := range r.TargetDependencies {
		for _, status := range []string{dependency.DropStatus, dependency.RestoreStatus} {
			if status == "failed" || status == "unknown" || status == "blocked" || status == "running" {
				return true
			}
		}
	}
	if r.PartitionMigration != nil {
		if !r.PartitionMigration.Ready {
			return true
		}
		for _, t := range r.PartitionMigration.Tables {
			if t.Status == "failed" || t.Status == "incomplete" || t.Status == "blocked" {
				return true
			}
		}
	}
	if r.NativeSchema != nil {
		if r.NativeSchema.Preflight == nil || !r.NativeSchema.Preflight.Ready || damengnative.HasErrors(r.NativeSchema.Verification) {
			return true
		}
		for _, c := range r.NativeSchema.Categories {
			if c.Failed > 0 || c.Success < c.Total {
				return true
			}
		}
	}
	if r.DataValidation != nil && r.DataValidation.HasFailures() {
		return true
	}
	if r.CompatibilityFunctions != nil && r.CompatibilityFunctions.Failed > 0 {
		return true
	}
	if r.Preflight != nil && !r.Preflight.Ready {
		return true
	}
	if r.CrossPreflight != nil && !r.CrossPreflight.Ready {
		return true
	}
	if r.PGOraclePreflight != nil && !r.PGOraclePreflight.Ready {
		return true
	}
	for _, c := range []CategoryReport{r.Tables, r.Data, r.PrimaryKeys, r.Views, r.Indexes, r.Constraints, r.Sequences, r.Comments, r.Triggers} {
		if c.Failed > 0 {
			return true
		}
	}
	for _, row := range r.RowCounts {
		if !row.Match {
			return true
		}
	}
	return false
}
