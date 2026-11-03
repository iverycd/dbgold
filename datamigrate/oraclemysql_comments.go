package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

// MySQL column comments need the complete current target column definition.
// Keep this optional capability separate from the common Writer contract.
type preparedCommentWriter interface {
	PrepareComment(context.Context, source.CommentInfo) ([]dialect.Statement, error)
	ExecuteCommentStatements(context.Context, []dialect.Statement) error
}

func (m *Migrator) isOracleMySQLComments() bool {
	return m.reader.DBType() == "oracle" && m.writer.DBType() == "mysql"
}

func limitMySQLColumnComment(cm source.CommentInfo) source.CommentInfo {
	if cm.ColumnName != "" && utf8.RuneCountInString(cm.Comment) > 1024 {
		cm.Comment = string([]rune(cm.Comment)[:1024])
	}
	return cm
}

// Source spelling and text remain intact; only the target copy is normalized.
func (m *Migrator) targetComment(cm source.CommentInfo) source.CommentInfo {
	target := cm
	target.TableName = m.objName(cm.TableName)
	if cm.ColumnName != "" {
		target.ColumnName = m.targetColumn(cm.TableName, cm.ColumnName)
	}
	if m.isOracleMySQLComments() {
		target = limitMySQLColumnComment(target)
	}
	return target
}

func (m *Migrator) prepareComment(ctx context.Context, cm source.CommentInfo) ([]dialect.Statement, error) {
	if m.isOracleMySQLComments() {
		w, ok := m.writer.(preparedCommentWriter)
		if !ok {
			return nil, fmt.Errorf("目标 Writer 缺少 MySQL 注释准备和执行能力")
		}
		ss, err := w.PrepareComment(ctx, cm)
		if err == nil && len(ss) == 0 {
			err = fmt.Errorf("未生成 MySQL 注释 SQL")
		}
		return ss, err
	}
	return m.writer.Dialect().CommentStatements(m.cfg.TargetSchema, cm), nil
}

// Even preparation failures pass through captureRepairCall so an inventory
// object cannot remain pending after its comment has failed in the report.
func (m *Migrator) repairPreparedComment(ctx context.Context, cm source.CommentInfo, ss []dialect.Statement, preparationErr error) error {
	name := cm.TableName
	if cm.ColumnName != "" {
		name += "." + cm.ColumnName
	}
	o := repair.Object{Kind: "comment", Name: name, Table: cm.TableName, TargetName: name, Comment: &cm}
	return m.captureRepairCall(ctx, o, ss, func(ctx context.Context) error {
		if preparationErr != nil {
			return preparationErr
		}
		if m.isOracleMySQLComments() {
			return m.writer.(preparedCommentWriter).ExecuteCommentStatements(ctx, ss)
		}
		return m.writer.CreateComment(ctx, cm)
	})
}

func (m *Migrator) recordOracleMySQLCommentTruncation(report *MigrationReport, original, effective source.CommentInfo) {
	if !m.isOracleMySQLComments() || original.Comment == effective.Comment {
		return
	}
	reason := fmt.Sprintf("列注释超过 MySQL 1024 字符上限，已从 %d 字符截断到 1024 字符", utf8.RuneCountInString(original.Comment))
	report.AutoRepairs = append(report.AutoRepairs, AutoRepair{
		Type: "oracle_mysql_comment_truncate", Table: original.TableName, TargetTable: effective.TableName,
		Source: original.ColumnName, Target: effective.ColumnName, Reason: reason, Status: "applied",
	})
	m.log.Warnf("列注释截断 [%s.%s]：%s", effective.TableName, effective.ColumnName, reason)
}

// Recovery candidates use current target attributes, including manually widened
// types. A custom SQL draft stays intact and is still subject to review.
func prepareOracleMySQLRecoveryComment(ctx context.Context, w preparedCommentWriter, original, candidate repair.Object) (repair.Object, error) {
	cm := limitMySQLColumnComment(*candidate.Comment)
	candidate.Comment = &cm
	custom := false
	for _, unit := range original.Units {
		custom = custom || strings.TrimSpace(unit.SQL) != strings.TrimSpace(unit.OriginalSQL)
	}
	ss, err := w.PrepareComment(ctx, cm)
	if err == nil && len(ss) == 0 {
		err = fmt.Errorf("未生成恢复列注释 SQL")
	}
	if err != nil {
		if custom && ctx.Err() == nil {
			// A reviewed manual draft is the fallback for a definition the
			// automatic generator cannot safely represent. Scope validation
			// and the existing review gate still apply to its saved SQL.
			candidate.SuggestedDDL = ""
			return candidate, nil
		}
		return candidate, fmt.Errorf("准备恢复列注释 %s.%s: %w", cm.TableName, cm.ColumnName, err)
	}
	candidate.SuggestedDDL = dialect.JoinSQL(ss)
	if !custom {
		sameSQL := len(original.Units) == len(ss)
		if sameSQL {
			for i, statement := range ss {
				sameSQL = sameSQL && strings.TrimSpace(original.Units[i].SQL) == strings.TrimSpace(statement.SQL)
			}
		}
		if !sameSQL {
			candidate.RequiresReview = true
		}
		candidate.Units = unitsFor(ss)
		candidate.DDL = candidate.SuggestedDDL
	}
	return candidate, nil
}
