package dialect

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/tsqldameng"
)

// DamengColumnRename is advice only; it must never become a migration mapping.
type DamengColumnRename struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// DamengColumnNameError carries complete, separately generated manual DDL.
// No executable statements are returned alongside this error.
type DamengColumnNameError struct {
	OriginalDDL   string
	SuggestedDDL  string
	Suggestion    string
	ColumnRenames []DamengColumnRename
	Reason        string
}

func (e *DamengColumnNameError) Error() string { return e.Reason }

func damengForbiddenColumn(name string) bool {
	switch name {
	case "ROWID", "TRXID", "VERSIONS_STARTTIME", "VERSIONS_ENDTIME",
		"VERSIONS_STARTTRXID", "VERSIONS_ENDTRXID", "VERSIONS_OPERATION":
		return true
	}
	return false
}

func (d *DaMengDialect) columnNameIssue(schema string, info *source.TableDDLInfo, srcType string, opt TypeOpt, name NameFunc) *DamengColumnNameError {
	used := make(map[string]bool, len(info.Columns))
	var forbidden []string
	var invalid error
	for _, c := range info.Columns {
		target := strings.ToUpper(name(c.Name))
		if damengForbiddenColumn(target) {
			forbidden = append(forbidden, target)
		}
		if used[target] {
			invalid = fmt.Errorf("目标列名冲突: %s", target)
		}
		if err := tsqldameng.ValidName(target); err != nil {
			invalid = err
		}
		used[target] = true
	}
	if len(forbidden) == 0 {
		return nil
	}
	issue := &DamengColumnNameError{
		Reason:     fmt.Sprintf("表 %s 包含达梦禁止的列名: %s（即使加双引号也不能使用）；未执行该表的建表语句", info.TableName, strings.Join(forbidden, ", ")),
		Suggestion: "字段已建议改名，手动建表后需同步调整数据导入及相关对象引用；当前迁移工具不会自动应用此映射。",
	}
	copyInfo := *info
	copyInfo.Columns = append([]source.ColumnInfo(nil), info.Columns...)
	for i, c := range info.Columns {
		target := strings.ToUpper(name(c.Name))
		if damengForbiddenColumn(target) {
			base := "SRC_" + target
			target = base
			for suffix := 2; used[target]; suffix++ {
				target = fmt.Sprintf("%s_%d", base, suffix)
			}
			used[target] = true
			issue.ColumnRenames = append(issue.ColumnRenames, DamengColumnRename{Source: c.Name, Target: target})
		}
		copyInfo.Columns[i].Name = target
	}
	// Preserve the original generated DDL when generation can finish. Never
	// reconstruct SQL with string replacement (defaults may contain these names).
	stmts, err := d.createTableStatements(schema, info, srcType, opt, name, name)
	if err != nil {
		invalid = err
	} else {
		issue.OriginalDDL = JoinSQL(stmts)
	}
	if err := tsqldameng.ValidName(name(info.TableName)); err != nil {
		invalid = err
	}
	if schema != "" {
		if err := tsqldameng.ValidName(schema); err != nil {
			invalid = err
		}
	}
	if invalid == nil {
		stmts, err = d.createTableStatements(schema, &copyInfo, srcType, opt, name, func(s string) string { return s })
		if err != nil {
			invalid = err
		} else {
			// The renderer returns DROP then CREATE. Only CREATE is manual advice.
			issue.SuggestedDDL = stmts[1].SQL + ";"
		}
	}
	if invalid != nil {
		issue.Suggestion += fmt.Sprintf(" 无法生成完整建议 DDL：%v；请根据列名建议手动调整表结构。", invalid)
	}
	return issue
}
