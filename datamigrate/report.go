package datamigrate

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

// ObjectResult 单个失败对象的详情
type ObjectResult struct {
	Name  string `json:"name"`
	DDL   string `json:"ddl"`
	Error string `json:"error"`
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
	Tables      CategoryReport `json:"tables"`
	Data        CategoryReport `json:"data"`
	PrimaryKeys CategoryReport `json:"primaryKeys"`
	Views       CategoryReport `json:"views"`
	Indexes     CategoryReport `json:"indexes"`
	Constraints CategoryReport `json:"constraints"`
	Sequences   CategoryReport `json:"sequences"`
	Triggers    CategoryReport `json:"triggers"`
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
		Triggers:    newCategoryReport(),
	}
}

func quoteColumns(cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = fmt.Sprintf(`"%s"`, c)
	}
	return strings.Join(quoted, ", ")
}

// SequenceDDL 从 SequenceInfo 重建 PostgreSQL 序列 DDL
func SequenceDDL(seq source.SequenceInfo) string {
	seqName := fmt.Sprintf("seq_%s_%s", seq.TableName, seq.ColumnName)
	return fmt.Sprintf(
		"CREATE SEQUENCE IF NOT EXISTS \"%s\" START %d;\nALTER TABLE \"%s\" ALTER COLUMN \"%s\" SET DEFAULT nextval('\"%s\"')",
		seqName, seq.StartValue, seq.TableName, seq.ColumnName, seqName,
	)
}

// IndexDDL 从 IndexInfo 重建 PostgreSQL 索引/主键 DDL
func IndexDDL(idx source.IndexInfo) string {
	cols := quoteColumns(idx.Columns)
	if idx.IsPrimary {
		return fmt.Sprintf(`ALTER TABLE "%s" ADD PRIMARY KEY (%s)`, idx.TableName, cols)
	}
	unique := ""
	if idx.IsUnique {
		unique = "UNIQUE "
	}
	return fmt.Sprintf(`CREATE %sINDEX "%s" ON "%s" (%s)`, unique, idx.IndexName, idx.TableName, cols)
}

// FKDDL 从 FKInfo 重建 PostgreSQL 外键 DDL
func FKDDL(fk source.FKInfo) string {
	cols := quoteColumns(fk.Columns)
	refCols := quoteColumns(fk.RefColumns)
	s := fmt.Sprintf(`ALTER TABLE "%s" ADD CONSTRAINT "%s" FOREIGN KEY (%s) REFERENCES "%s" (%s)`,
		fk.TableName, fk.ConstraintName, cols, fk.RefTable, refCols)
	if fk.OnDelete != "" {
		s += " ON DELETE " + fk.OnDelete
	}
	if fk.OnUpdate != "" {
		s += " ON UPDATE " + fk.OnUpdate
	}
	return s
}
