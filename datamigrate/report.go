package datamigrate

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
	Tables      CategoryReport  `json:"tables"`
	Data        CategoryReport  `json:"data"`
	PrimaryKeys CategoryReport  `json:"primaryKeys"`
	Views       CategoryReport  `json:"views"`
	Indexes     CategoryReport  `json:"indexes"`
	Constraints CategoryReport  `json:"constraints"`
	Sequences   CategoryReport  `json:"sequences"`
	Triggers    CategoryReport  `json:"triggers"`
	RowCounts   []TableRowCount `json:"rowCounts"`
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
		Triggers:    newCategoryReport(),
		RowCounts:   []TableRowCount{},
	}
}
