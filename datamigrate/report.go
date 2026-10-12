package datamigrate

import (
	"errors"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/valueconv"
)

// ObjectResult 单个失败对象的详情
type ObjectResult struct {
	Name          string                       `json:"name"`
	DDL           string                       `json:"ddl"`
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
	Total   int            `json:"total"`
	Success int            `json:"success"`
	Failed  int            `json:"failed"`
	Items   []ObjectResult `json:"items"`
}

// MigrationReport 完整迁移报告
type MigrationReport struct {
	Warnings    []valueconv.MigrationWarning `json:"warnings,omitempty"`
	Preflight   *PGMySQLPreflight            `json:"preflight,omitempty"`
	Tables      CategoryReport               `json:"tables"`
	Data        CategoryReport               `json:"data"`
	PrimaryKeys CategoryReport               `json:"primaryKeys"`
	Views       CategoryReport               `json:"views"`
	Indexes     CategoryReport               `json:"indexes"`
	Constraints CategoryReport               `json:"constraints"`
	Sequences   CategoryReport               `json:"sequences"`
	Comments    CategoryReport               `json:"comments"`
	Triggers    CategoryReport               `json:"triggers"`
	RowCounts   []TableRowCount              `json:"rowCounts"`
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
