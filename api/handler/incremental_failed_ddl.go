package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"dbgold/datamigrate/cdc"
	"dbgold/store"
	"github.com/gin-gonic/gin"
)

var failedDDLCategoryOrder = []string{
	"table", "sequence", "primary_key", "index", "foreign_key", "view", "comment",
	"data", "row_count", "cdc_compatibility",
}

var failedDDLCategoryLabels = map[string]string{
	"table": "表", "sequence": "序列", "primary_key": "主键", "index": "索引",
	"foreign_key": "外键", "view": "视图", "comment": "注释", "data": "数据写入",
	"row_count": "行数校验", "cdc_compatibility": "CDC 兼容性",
}

// ExportIncrementalFailedDDL returns an editable repair script. Failure
// metadata remains commented, ordinary CREATE/ALTER/COMMENT statements are
// active, and destructive statements are forcibly kept commented.
func ExportIncrementalFailedDDL(c *gin.Context) {
	j, ok := ownedIncremental(c)
	if !ok {
		return
	}
	if j.StartMode != "full_then_cdc" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仅全量快照后持续同步任务支持导出失败 DDL"})
		return
	}
	review, found := loadIncrementalFailureReview(c.Request.Context(), j)
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "该任务没有可导出的全量失败记录"})
		return
	}
	items := exportableBootstrapFailures(review)
	if len(items) == 0 && len(review.Warnings) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "该任务没有可导出的全量失败记录"})
		return
	}

	content := renderIncrementalFailedDDL(j, review, items, time.Now())
	shortID := j.JobID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="incremental-%s-failed-ddl.sql"`, shortID))
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/sql; charset=utf-8", []byte(content))
}

func loadIncrementalFailureReview(ctx context.Context, j *store.IncrementalMigrationJob) (cdc.BootstrapReview, bool) {
	var review cdc.BootstrapReview
	loaded := false
	if strings.TrimSpace(j.BootstrapReport) != "" && json.Unmarshal([]byte(j.BootstrapReport), &review) == nil {
		loaded = true
	}
	// The target checkpoint closes the crash window where PostgreSQL accepted
	// the report but SQLite was not updated. A valid SQLite report remains the
	// preferred source for its original snapshot position and warning details.
	if !loaded || review.FailureReportVersion == 0 {
		if dst, err := store.GetConnection(j.DstConnID); err == nil {
			cfg := cdc.Config{JobID: j.JobID, TargetDSN: buildDSN(dst), TargetDBType: dst.DBType, TargetSchema: j.TargetSchema, LowerCaseNames: j.LowerCaseNames}
			if record, exists, err := cdc.LoadTargetBootstrapRecord(ctx, cfg); err == nil && exists {
				if !loaded {
					review.BootstrapRecord = record
					review.RequestedCount = len(record.EffectiveTables) + len(record.ExcludedTables)
					loaded = true
				} else if record.FailureReportVersion > review.FailureReportVersion {
					review.FailedObjects = record.FailedObjects
					review.FailureReportVersion = record.FailureReportVersion
				}
			}
		}
	}
	return review, loaded
}

func exportableBootstrapFailures(review cdc.BootstrapReview) []cdc.BootstrapFailedObject {
	if review.FailureReportVersion > 0 {
		return append([]cdc.BootstrapFailedObject(nil), review.FailedObjects...)
	}
	// Old records only retained structured failed-table exclusions. Preserve
	// what is recoverable and make the incomplete nature explicit in the file.
	items := make([]cdc.BootstrapFailedObject, 0, len(review.ExcludedTables))
	for _, issue := range review.ExcludedTables {
		category := issue.Stage
		if issue.Stage == "schema" {
			category = "table"
		}
		items = append(items, cdc.BootstrapFailedObject{Category: category, Name: issue.Table, Error: issue.Error, DDL: issue.DDL, Stage: issue.Stage})
	}
	return items
}

func renderIncrementalFailedDDL(j *store.IncrementalMigrationJob, review cdc.BootstrapReview, items []cdc.BootstrapFailedObject, exportedAt time.Time) string {
	groups := make(map[string][]cdc.BootstrapFailedObject)
	for _, item := range items {
		groups[item.Category] = append(groups[item.Category], item)
	}
	for category := range groups {
		sort.SliceStable(groups[category], func(i, k int) bool { return groups[category][i].Name < groups[category][k].Name })
	}

	position := review.Position
	if j.PendingFile != "" || j.PendingGTID != "" {
		position = cdc.Position{File: j.PendingFile, Pos: j.PendingPos, GTID: j.PendingGTID}
	}
	withDDL := 0
	for _, item := range items {
		if strings.TrimSpace(item.DDL) != "" {
			withDDL++
		}
	}
	var b strings.Builder
	commentLine(&b, "DBGold 增量任务全量失败 DDL 修复脚本")
	commentLine(&b, "注意：本文件包含可执行 DDL，请先修正不兼容的字段类型或语法，再连接正确的目标库执行。")
	commentLine(&b, "DROP、TRUNCATE、DELETE 等破坏性语句已强制注释；如确需清理目标对象，请单独审阅后手工执行。")
	commentLine(&b, "建议按文件中的依赖顺序逐类、逐对象执行，不建议未经检查直接整文件运行。")
	commentLine(&b, "Job ID: "+j.JobID)
	commentLine(&b, "源数据库: "+j.SrcDatabase)
	commentLine(&b, "目标 Schema: "+j.TargetSchema)
	commentLine(&b, "快照位点: "+formatIncrementalPosition(position))
	commentLine(&b, "导出时间: "+exportedAt.Format(time.RFC3339))
	commentLine(&b, fmt.Sprintf("失败项: %d，其中包含 DDL: %d", len(items), withDDL))
	if review.FailureReportVersion == 0 {
		commentLine(&b, "历史任务报告可能不完整：旧版本未保存非表对象的原始失败 DDL。")
	}
	b.WriteString("--\n")

	for _, category := range failedDDLCategoryOrder {
		categoryItems := groups[category]
		if len(categoryItems) == 0 {
			continue
		}
		renderFailedDDLGroup(&b, failedDDLCategoryLabels[category], categoryItems)
		delete(groups, category)
	}
	// Preserve forward-compatible or legacy categories rather than silently
	// omitting them from the diagnostic file.
	unknownCategories := make([]string, 0, len(groups))
	for category := range groups {
		unknownCategories = append(unknownCategories, category)
	}
	sort.Strings(unknownCategories)
	for _, category := range unknownCategories {
		renderFailedDDLGroup(&b, category, groups[category])
	}
	if len(review.Warnings) > 0 {
		commentLine(&b, "==================== 其他警告 ====================")
		for _, warning := range review.Warnings {
			commentMultiline(&b, "- ", sanitizeIncrementalLogLine(warning))
		}
	}
	return b.String()
}

func renderFailedDDLGroup(b *strings.Builder, label string, items []cdc.BootstrapFailedObject) {
	commentLine(b, fmt.Sprintf("==================== %s（%d） ====================", label, len(items)))
	for _, item := range items {
		commentLine(b, "对象: "+item.Name)
		commentLine(b, "阶段: "+item.Stage)
		commentMultiline(b, "错误: ", sanitizeIncrementalLogLine(item.Error))
		if strings.TrimSpace(item.DDL) == "" {
			commentLine(b, "DDL: （无）")
		} else {
			commentLine(b, "修复 DDL:")
			writeRepairDDL(b, item.DDL)
		}
		b.WriteString("--\n")
	}
}

func writeRepairDDL(b *strings.Builder, ddl string) {
	ddl = strings.ReplaceAll(ddl, "\r\n", "\n")
	ddl = strings.ReplaceAll(ddl, "\r", "\n")
	ddl = strings.TrimSpace(ddl)
	if ddl == "" {
		return
	}
	if !strings.HasSuffix(ddl, ";") {
		ddl += ";"
	}

	atStatementStart := true
	dangerous := false
	state := sqlTerminatorState{}
	for _, line := range strings.Split(ddl, "\n") {
		trimmed := strings.TrimSpace(line)
		if atStatementStart && trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			dangerous = isDestructiveSQLStart(trimmed)
			if dangerous {
				commentLine(b, "危险操作，默认禁用：")
			}
			atStatementStart = false
		}
		if dangerous {
			commentLine(b, line)
		} else {
			b.WriteString(line)
			b.WriteByte('\n')
		}
		if state.lineTerminatesStatement(line) {
			atStatementStart = true
			dangerous = false
		}
	}
}

func isDestructiveSQLStart(statement string) bool {
	fields := strings.Fields(statement)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToUpper(fields[0]) {
	case "DROP", "TRUNCATE", "DELETE":
		return true
	default:
		return false
	}
}

// sqlTerminatorState recognizes semicolons outside PostgreSQL quoted strings
// and identifiers. It keeps a multi-line destructive statement commented
// until the real statement terminator instead of trusting a semicolon inside
// an object name or literal.
type sqlTerminatorState struct {
	singleQuoted bool
	doubleQuoted bool
}

func (s *sqlTerminatorState) lineTerminatesStatement(line string) bool {
	terminated := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\'':
			if s.doubleQuoted {
				continue
			}
			if s.singleQuoted && i+1 < len(line) && line[i+1] == '\'' {
				i++
				continue
			}
			s.singleQuoted = !s.singleQuoted
		case '"':
			if s.singleQuoted {
				continue
			}
			if s.doubleQuoted && i+1 < len(line) && line[i+1] == '"' {
				i++
				continue
			}
			s.doubleQuoted = !s.doubleQuoted
		case ';':
			if !s.singleQuoted && !s.doubleQuoted {
				terminated = true
			}
		}
	}
	return terminated
}

func commentLine(b *strings.Builder, value string) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.TrimSuffix(value, "\n")
	b.WriteString("-- ")
	b.WriteString(strings.ReplaceAll(value, "\n", "\n-- "))
	b.WriteByte('\n')
}

func commentMultiline(b *strings.Builder, firstPrefix, value string) {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	for i, line := range lines {
		prefix := ""
		if i == 0 {
			prefix = firstPrefix
		}
		commentLine(b, prefix+line)
	}
}
