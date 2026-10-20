package datamigrate

import (
	"errors"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/valueconv"
)

// ObjectResult 单个失败对象的详情
type ObjectResult struct {
	Name          string                       `json:"name"`
	SourceName    string                       `json:"source_name,omitempty"`
	TargetName    string                       `json:"target_name,omitempty"`
	TargetTable   string                       `json:"target_table,omitempty"`
	TargetSchema  string                       `json:"target_schema,omitempty"`
	Table         string                       `json:"table,omitempty"` // Original source table name.
	Stage         string                       `json:"stage,omitempty"` // metadata | preflight | unsupported | execute
	DDL           string                       `json:"ddl"`
	SourceDDL     string                       `json:"source_ddl,omitempty"` // Source definition for reference, never executable target SQL.
	Error         string                       `json:"error"`
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

// A metadata failure is one failure event, not one discovered database object.
// Keep Failed nonzero for existing job-status and report consumers.
func recordMetadataFailure(category *CategoryReport, err error) {
	category.TotalUnknown = true
	category.Failed++
	category.Items = append(category.Items, ObjectResult{Name: "元数据读取", Stage: "metadata", Error: err.Error()})
}

// MigrationReport 完整迁移报告
type MigrationReport struct {
	CompatibilityFunctions *target.CompatibilityFunctionsReport `json:"compatibility_functions,omitempty"`
	RepairManifest         *RepairManifest                      `json:"repair_manifest,omitempty"`
	AutoRepairs            []AutoRepair                         `json:"auto_repairs,omitempty"`
	Warnings               []valueconv.MigrationWarning         `json:"warnings,omitempty"`
	Preflight              *PGMySQLPreflight                    `json:"preflight,omitempty"`
	Tables                 CategoryReport                       `json:"tables"`
	Data                   CategoryReport                       `json:"data"`
	PrimaryKeys            CategoryReport                       `json:"primaryKeys"`
	Views                  CategoryReport                       `json:"views"`
	Indexes                CategoryReport                       `json:"indexes"`
	Constraints            CategoryReport                       `json:"constraints"`
	Sequences              CategoryReport                       `json:"sequences"`
	Comments               CategoryReport                       `json:"comments"`
	Triggers               CategoryReport                       `json:"triggers"`
	RowCounts              []TableRowCount                      `json:"rowCounts"`
}

// AutoRepair is emitted only after successful CREATE or existing-column validation.
type AutoRepair struct {
	Type        string `json:"type"`
	Table       string `json:"table"`
	TargetTable string `json:"target_table"`
	Source      string `json:"source"`
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
	if r.CompatibilityFunctions != nil && r.CompatibilityFunctions.Failed > 0 {
		return true
	}
	if r.Preflight != nil && !r.Preflight.Ready {
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
