package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
)

// Recovery can regenerate the original unit after a column mapping changes.
// New draft units carry no OriginalSQL and cannot establish a new target.
func savedDamengComment(obj repair.Object) (damengnative.CommentStatement, error) {
	var saved damengnative.CommentStatement
	defaultSchema := strings.ToUpper(obj.Schema) // SET SCHEMA uses the cross-database DM dialect.
	for _, unit := range obj.Units {
		if unit.OriginalSQL == "" {
			continue
		}
		cm, err := damengnative.ParseCommentSQL(unit.OriginalSQL, defaultSchema)
		if err != nil {
			return saved, fmt.Errorf("无法识别原始达梦注释: %w", err)
		}
		if saved.Table != "" && !sameDamengCommentTarget(saved, cm) {
			return saved, fmt.Errorf("原始达梦注释包含多个目标")
		}
		saved = cm
	}
	if saved.Table == "" {
		raw := obj.DDL
		if strings.TrimSpace(raw) == "" {
			if obj.Comment == nil {
				return saved, fmt.Errorf("缺少原始目标 DDL 和达梦注释元数据，无法确定修复对象")
			}
			cm := *obj.Comment
			if cm.TableName == "" {
				cm.TableName = obj.Table
			}
			if cm.TableName == "" {
				return saved, fmt.Errorf("达梦注释元数据缺少所属表")
			}
			raw = dialect.JoinSQL(dialect.NewDaMeng().CommentStatements(obj.Schema, cm))
		}
		var err error
		saved, err = damengnative.ParseCommentSQL(raw, defaultSchema)
		if err != nil {
			return saved, fmt.Errorf("无法识别原始达梦注释: %w", err)
		}
	}
	if saved.Schema == "" || (saved.Schema != obj.Schema && saved.Schema != defaultSchema) {
		return saved, fmt.Errorf("达梦注释目标模式必须为 %s", obj.Schema)
	}
	return saved, nil
}

func inspectSavedDamengComment(ctx context.Context, db *sql.DB, schema string, obj repair.Object) (repair.Verification, error) {
	saved, err := savedDamengComment(obj)
	if err != nil {
		return verification("unknown", err.Error())
	}
	if saved.Schema != schema {
		return verification("failed", "目标 schema 与原注释不一致")
	}
	// A completed retry may have changed the text. Ignore unexecuted drafts.
	for i := len(obj.Units) - 1; i >= 0; i-- {
		if obj.Units[i].Status == "succeeded" {
			saved, err = damengCommentRevision(saved, obj.Units[i].SQL)
			if err != nil {
				return verification("unknown", err.Error())
			}
			break
		}
	}
	return inspectDamengComment(ctx, db, saved)
}

func sameDamengCommentTarget(a, b damengnative.CommentStatement) bool {
	return a.Schema == b.Schema && a.Table == b.Table && a.Column == b.Column
}

func damengCommentRevision(saved damengnative.CommentStatement, raw string) (damengnative.CommentStatement, error) {
	cm, err := damengnative.ParseCommentSQL(raw, saved.Schema)
	if err == nil && !sameDamengCommentTarget(saved, cm) {
		err = fmt.Errorf("达梦注释的目标模式、表和列必须与原始目标一致")
	}
	return cm, err
}

func ValidateDamengCommentRepair(obj repair.Object, units []repair.Unit) error {
	saved, err := savedDamengComment(obj)
	if err != nil {
		return err
	}
	for _, unit := range units {
		if !unit.Selected || unit.Status == "succeeded" {
			continue
		}
		if _, err := damengCommentRevision(saved, unit.SQL); err != nil {
			return err
		}
	}
	return nil
}

func inspectDamengComment(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, cm damengnative.CommentStatement) (repair.Verification, error) {
	query := "SELECT COMMENTS FROM ALL_TAB_COMMENTS WHERE OWNER=? AND TABLE_NAME=?"
	args := []any{cm.Schema, cm.Table}
	if cm.Column != "" {
		query = "SELECT COMMENTS FROM ALL_COL_COMMENTS WHERE OWNER=? AND TABLE_NAME=? AND COLUMN_NAME=?"
		args = append(args, cm.Column)
	}
	var actual sql.NullString
	if err := db.QueryRowContext(ctx, query, args...).Scan(&actual); err != nil {
		return repair.Verification{Status: "unknown", Message: "无法读取达梦目标注释"}, err
	}
	if actual.String != cm.Text {
		return verification("failed", "达梦目录中的注释与提交的正文不一致")
	}
	return verification("succeeded", "达梦目录中的注释与提交的正文一致")
}

var errDamengCommentReadback = errors.New("注释 DDL 已执行，无法确认目标注释")

func executeDamengComment(ctx context.Context, conn *sql.Conn, saved damengnative.CommentStatement, raw string) error {
	cm, err := damengCommentRevision(saved, raw)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, raw); err != nil {
		return err
	}
	result, err := inspectDamengComment(ctx, conn, cm)
	if err != nil {
		return fmt.Errorf("%w: %w", errDamengCommentReadback, err)
	}
	if result.Status != "succeeded" {
		return errors.New(result.Message)
	}
	return nil
}
