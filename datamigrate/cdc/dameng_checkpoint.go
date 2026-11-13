package cdc

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"

	"gitee.com/chunanyong/dm"
)

// DM DDL commits implicitly. Schema maintenance and bootstrap cleanup must
// never run on the transaction that applies source rows and their checkpoint.
var damengCheckpointDDLMu sync.Mutex
var damengBootstrapMu sync.Mutex

var damengCheckpointColumns = []struct{ name, definition string }{
	{"JOB_ID", "VARCHAR(64) PRIMARY KEY"},
	{"BINLOG_FILE", "VARCHAR(1024)"},
	{"BINLOG_POSITION", "BIGINT DEFAULT 4 NOT NULL"},
	{"GTID", "CLOB"},
	{"BOOTSTRAP_STATE", "VARCHAR(32) DEFAULT 'completed' NOT NULL"},
	{"EFFECTIVE_TABLES", "CLOB"},
	{"EXCLUDED_TABLES", "CLOB"},
	{"MANIFEST_HASH", "VARCHAR(64)"},
	{"FAILED_OBJECTS", "CLOB"},
	{"FAILURE_REPORT_VERSION", "INT DEFAULT 0 NOT NULL"},
	{"LOCATOR_STRATEGY_VERSION", "INT DEFAULT 0 NOT NULL"},
	{"LOCATOR_STRATEGIES", "CLOB"},
	{"CLEANUP_PLAN", "CLOB"},
	{"SOURCE_SCHEMA_VERSION", "INT DEFAULT 0 NOT NULL"},
	{"SOURCE_SCHEMA", "CLOB"},
	{"SOURCE_SCHEMA_HASH", "VARCHAR(64)"},
	{"ACK_DDL_FILE", "VARCHAR(1024)"},
	{"ACK_DDL_POSITION", "BIGINT DEFAULT 0 NOT NULL"},
	{"ACK_DDL_GTID", "CLOB"},
	{"UPDATED_AT", "TIMESTAMP DEFAULT CURRENT_TIMESTAMP NOT NULL"},
}

func (a *DamengApplier) ensureCheckpoint(ctx context.Context) error {
	damengCheckpointDDLMu.Lock()
	defer damengCheckpointDDLMu.Unlock()
	columns, err := a.checkpointColumns(ctx)
	if err != nil {
		return err
	}
	if len(columns) == 0 {
		defs := make([]string, len(damengCheckpointColumns))
		for i, c := range damengCheckpointColumns {
			defs[i] = c.name + " " + c.definition
		}
		if _, err = a.db.ExecContext(ctx, "CREATE TABLE "+a.checkpointTable()+" ("+strings.Join(defs, ",")+")"); err != nil {
			// A second process may have initialized the same schema. Only accept the
			// race when the complete expected table is now observable.
			actual, readErr := a.checkpointColumns(ctx)
			if readErr != nil || len(actual) != len(damengCheckpointColumns) {
				return fmt.Errorf("创建达梦 CDC 检查点: %w", err)
			}
			for _, c := range damengCheckpointColumns {
				if !actual[c.name] {
					return err
				}
			}
		}
		return nil
	}
	for _, c := range damengCheckpointColumns {
		if columns[c.name] {
			continue
		}
		if c.name == "JOB_ID" || c.name == "BINLOG_FILE" || c.name == "BINLOG_POSITION" || c.name == "GTID" {
			return fmt.Errorf("达梦 CDC 检查点缺少基础列 %s", c.name)
		}
		if _, err = a.db.ExecContext(ctx, "ALTER TABLE "+a.checkpointTable()+" ADD "+c.name+" "+c.definition); err != nil {
			return fmt.Errorf("升级达梦 CDC 检查点 %s: %w", c.name, err)
		}
	}
	return nil
}

func (a *DamengApplier) checkpointColumns(ctx context.Context) (map[string]bool, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=?`, a.schema, strings.ToUpper(CheckpointTableName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func (a *DamengApplier) LoadCheckpoint(ctx context.Context) (Position, bool, error) {
	record, exists, err := a.LoadBootstrapRecord(ctx)
	if err != nil || !exists {
		return Position{}, exists, err
	}
	if record.State != "completed" {
		return Position{}, false, fmt.Errorf("checkpoint bootstrap 状态不是 completed: %s", record.State)
	}
	return record.Position, true, nil
}

func (a *DamengApplier) LoadBootstrapRecord(ctx context.Context) (BootstrapRecord, bool, error) {
	var r BootstrapRecord
	var n int64
	var file, gtid, effective, excluded, hash, failures, locators, sourceSchema, schemaHash sql.NullString
	var schemaVersion int
	err := a.db.QueryRowContext(ctx, `SELECT BINLOG_FILE,BINLOG_POSITION,GTID,BOOTSTRAP_STATE,EFFECTIVE_TABLES,EXCLUDED_TABLES,MANIFEST_HASH,FAILED_OBJECTS,FAILURE_REPORT_VERSION,LOCATOR_STRATEGY_VERSION,LOCATOR_STRATEGIES,SOURCE_SCHEMA_VERSION,SOURCE_SCHEMA,SOURCE_SCHEMA_HASH FROM `+a.checkpointTable()+` WHERE JOB_ID=?`, a.jobID).
		Scan(&file, &n, &gtid, &r.State, &effective, &excluded, &hash, &failures, &r.FailureReportVersion, &r.LocatorStrategyVersion, &locators, &schemaVersion, &sourceSchema, &schemaHash)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if n < 0 || n > math.MaxUint32 {
		return r, false, fmt.Errorf("无效的达梦检查点 binlog position: %d", n)
	}
	r.Position = Position{File: file.String, Pos: uint32(n), GTID: gtid.String}
	r.ManifestHash = hash.String
	for _, field := range []struct {
		name string
		raw  sql.NullString
		dst  any
	}{
		{"effective_tables", effective, &r.EffectiveTables}, {"excluded_tables", excluded, &r.ExcludedTables},
		{"failed_objects", failures, &r.FailedObjects}, {"locator_strategies", locators, &r.LocatorStrategies},
		{"source_schema", sourceSchema, &r.SourceSchema},
	} {
		if !field.raw.Valid || field.raw.String == "" {
			continue
		}
		if err = json.Unmarshal([]byte(field.raw.String), field.dst); err != nil {
			return r, false, fmt.Errorf("解析达梦 checkpoint %s: %w", field.name, err)
		}
	}
	if r.SourceSchema.Version != schemaVersion || r.SourceSchema.Hash != schemaHash.String {
		return r, false, fmt.Errorf("达梦 CDC 源结构快照版本或摘要不一致")
	}
	return r, true, nil
}

func damengBootstrapValues(r BootstrapRecord) ([]any, error) {
	var payloads []any
	for _, value := range []any{r.EffectiveTables, r.ExcludedTables, r.FailedObjects, r.LocatorStrategies, r.SourceSchema} {
		b, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, dm.NewClob(string(b)))
	}
	return []any{r.Position.File, int64(r.Position.Pos), damengOptionalClob(r.Position.GTID), r.State, payloads[0], payloads[1], r.ManifestHash, payloads[2], r.FailureReportVersion, r.LocatorStrategyVersion, payloads[3], r.SourceSchema.Version, payloads[4], r.SourceSchema.Hash}, nil
}

func damengOptionalClob(value string) any {
	if value == "" {
		return nil
	}
	return dm.NewClob(value)
}

const damengBootstrapAssignments = `BINLOG_FILE=?,BINLOG_POSITION=?,GTID=?,BOOTSTRAP_STATE=?,EFFECTIVE_TABLES=?,EXCLUDED_TABLES=?,MANIFEST_HASH=?,FAILED_OBJECTS=?,FAILURE_REPORT_VERSION=?,LOCATOR_STRATEGY_VERSION=?,LOCATOR_STRATEGIES=?,SOURCE_SCHEMA_VERSION=?,SOURCE_SCHEMA=?,SOURCE_SCHEMA_HASH=?,UPDATED_AT=CURRENT_TIMESTAMP`

func (a *DamengApplier) SaveBootstrapRecord(ctx context.Context, r BootstrapRecord) error {
	damengBootstrapMu.Lock()
	defer damengBootstrapMu.Unlock()
	stored, exists, err := a.LoadBootstrapRecord(ctx)
	if err != nil {
		return err
	}
	if exists {
		switch stored.State {
		case "completed", "aborted":
			if r.State == stored.State && r.ManifestHash == stored.ManifestHash {
				// The checkpoint may already have advanced beyond the snapshot.
				// Replaying report persistence must never rewind it.
				return nil
			}
			return fmt.Errorf("bootstrap 已处于 %s，不能覆盖为 %s", stored.State, r.State)
		case "review_pending":
			if r.State != "aborted" && (r.State != "review_pending" || r.ManifestHash != stored.ManifestHash) {
				return fmt.Errorf("bootstrap 审阅清单已冻结，不能覆盖状态或排除范围")
			}
		case "snapshot_in_progress":
			if r.State == "completed" {
				return fmt.Errorf("bootstrap 必须通过完成确认进入 completed")
			}
		default:
			return fmt.Errorf("无效的已保存 bootstrap 状态: %s", stored.State)
		}
	}
	switch r.State {
	case "snapshot_in_progress", "review_pending", "completed", "aborted":
	default:
		return fmt.Errorf("无效的 bootstrap 状态: %s", r.State)
	}
	values, err := damengBootstrapValues(r)
	if err != nil {
		return err
	}
	// Freeze identities when the review is published, before an operator can
	// replace a failed table while the task is awaiting acknowledgement.
	var cleanupJSON []byte
	if r.State == "review_pending" {
		var frozen sql.NullString
		if exists && stored.State == "review_pending" {
			if err = a.db.QueryRowContext(ctx, `SELECT CLEANUP_PLAN FROM `+a.checkpointTable()+` WHERE JOB_ID=?`, a.jobID).Scan(&frozen); err != nil {
				return err
			}
		}
		if frozen.Valid && frozen.String != "" {
			var plan damengCleanupPlan
			if err = json.Unmarshal([]byte(frozen.String), &plan); err != nil {
				return fmt.Errorf("解析已冻结达梦清理计划: %w", err)
			}
			if plan.Manifest != r.ManifestHash {
				return fmt.Errorf("已冻结达梦清理计划与排除清单不一致")
			}
			// Keep original object identities and already-finished DDL steps.
			// Replanning here could claim a replacement created during review.
		} else {
			plan, planErr := a.planBootstrapCleanup(ctx, r)
			if planErr != nil {
				return planErr
			}
			cleanupJSON, err = json.Marshal(plan)
			if err != nil {
				return err
			}
		}
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE `+a.checkpointTable()+` SET `+damengBootstrapAssignments+` WHERE JOB_ID=?`, append(values, a.jobID)...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO `+a.checkpointTable()+` (BINLOG_FILE,BINLOG_POSITION,GTID,BOOTSTRAP_STATE,EFFECTIVE_TABLES,EXCLUDED_TABLES,MANIFEST_HASH,FAILED_OBJECTS,FAILURE_REPORT_VERSION,LOCATOR_STRATEGY_VERSION,LOCATOR_STRATEGIES,SOURCE_SCHEMA_VERSION,SOURCE_SCHEMA,SOURCE_SCHEMA_HASH,JOB_ID) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, append(values, a.jobID)...)
		if err != nil {
			return err
		}
	} else if n != 1 {
		return fmt.Errorf("达梦 bootstrap 更新了 %d 条检查点", n)
	}
	if cleanupJSON != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE `+a.checkpointTable()+` SET CLEANUP_PLAN=? WHERE JOB_ID=?`, dm.NewClob(string(cleanupJSON)), a.jobID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *DamengApplier) SaveCheckpoint(ctx context.Context, p Position) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = a.saveCheckpointTx(ctx, tx, p); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *DamengApplier) saveCheckpointTx(ctx context.Context, tx *sql.Tx, p Position) error {
	result, err := tx.ExecContext(ctx, `UPDATE `+a.checkpointTable()+` SET BINLOG_FILE=?,BINLOG_POSITION=?,GTID=?,UPDATED_AT=CURRENT_TIMESTAMP WHERE JOB_ID=? AND BOOTSTRAP_STATE='completed'`, p.File, int64(p.Pos), damengOptionalClob(p.GTID), a.jobID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	if n != 0 {
		return fmt.Errorf("达梦 CDC 更新了 %d 条检查点", n)
	}
	// Never advance a pending/aborted bootstrap. Legacy direct checkpoint callers
	// may initialize a new job, but cannot bypass a pre-existing review.
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+a.checkpointTable()+` WHERE JOB_ID=?`, a.jobID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("达梦 bootstrap 尚未完成，不能推进检查点")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+a.checkpointTable()+` (JOB_ID,BINLOG_FILE,BINLOG_POSITION,GTID) VALUES (?,?,?,?)`, a.jobID, p.File, int64(p.Pos), damengOptionalClob(p.GTID))
	return err
}

func (a *DamengApplier) MarkBootstrapAborted(ctx context.Context) error {
	damengBootstrapMu.Lock()
	defer damengBootstrapMu.Unlock()
	result, err := a.db.ExecContext(ctx, `UPDATE `+a.checkpointTable()+` SET BOOTSTRAP_STATE='aborted',UPDATED_AT=CURRENT_TIMESTAMP WHERE JOB_ID=? AND BOOTSTRAP_STATE<>'completed'`, a.jobID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("bootstrap checkpoint 不存在或已完成")
	}
	return nil
}

type damengCleanupItem struct {
	Table      string `json:"table"`
	Constraint string `json:"constraint,omitempty"`
	ObjectID   int64  `json:"object_id"`
	Done       bool   `json:"done"`
}
type damengCleanupPlan struct {
	Manifest string              `json:"manifest"`
	Items    []damengCleanupItem `json:"items"`
}

func (a *DamengApplier) FinalizeBootstrap(ctx context.Context, r BootstrapRecord) error {
	damengBootstrapMu.Lock()
	defer damengBootstrapMu.Unlock()
	stored, exists, err := a.LoadBootstrapRecord(ctx)
	if err != nil {
		return err
	}
	if !exists || (stored.State != "snapshot_in_progress" && stored.State != "review_pending" && stored.State != "completed") {
		return fmt.Errorf("bootstrap checkpoint 不存在或状态不可确认")
	}
	if stored.ManifestHash != r.ManifestHash || r.ManifestHash != HashBootstrapManifest(r) {
		return fmt.Errorf("bootstrap 排除清单已变化，不能确认旧清单")
	}
	if stored.State == "completed" {
		return nil
	}
	var raw sql.NullString
	if err = a.db.QueryRowContext(ctx, `SELECT CLEANUP_PLAN FROM `+a.checkpointTable()+` WHERE JOB_ID=?`, a.jobID).Scan(&raw); err != nil {
		return err
	}
	var plan damengCleanupPlan
	if raw.Valid && raw.String != "" {
		if err = json.Unmarshal([]byte(raw.String), &plan); err != nil {
			return fmt.Errorf("解析达梦清理计划: %w", err)
		}
		if plan.Manifest != r.ManifestHash {
			return fmt.Errorf("达梦清理计划与排除清单不一致")
		}
	} else {
		plan, err = a.planBootstrapCleanup(ctx, r)
		if err != nil {
			return err
		}
		if err = a.saveCleanupPlan(ctx, plan); err != nil {
			return err
		}
	}
	for i := range plan.Items {
		item := &plan.Items[i]
		if item.Done {
			continue
		}
		id, exists, err := a.cleanupObjectID(ctx, *item)
		if err != nil {
			return err
		}
		if exists {
			if id != item.ObjectID {
				return fmt.Errorf("清理对象 %s.%s 已被替换，拒绝删除", a.schema, item.Table)
			}
			statement := "DROP TABLE " + a.qualified(item.Table)
			if item.Constraint != "" {
				statement = "ALTER TABLE " + a.qualified(item.Table) + " DROP CONSTRAINT " + quoteIdent(item.Constraint)
			}
			if _, err = a.db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("清理被排除对象 %s.%s: %w", item.Table, item.Constraint, err)
			}
		}
		item.Done = true
		if err = a.saveCleanupPlan(ctx, plan); err != nil {
			return err
		}
	}
	// DDL is finished. Only now make the completed manifest visible to CDC.
	r.State = "completed"
	values, err := damengBootstrapValues(r)
	if err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE `+a.checkpointTable()+` SET `+damengBootstrapAssignments+` WHERE JOB_ID=? AND BOOTSTRAP_STATE IN ('snapshot_in_progress','review_pending')`, append(values, a.jobID)...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("bootstrap checkpoint 状态已变化")
	}
	return tx.Commit()
}

func (a *DamengApplier) saveCleanupPlan(ctx context.Context, plan damengCleanupPlan) error {
	b, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	result, err := a.db.ExecContext(ctx, `UPDATE `+a.checkpointTable()+` SET CLEANUP_PLAN=?,UPDATED_AT=CURRENT_TIMESTAMP WHERE JOB_ID=? AND BOOTSTRAP_STATE IN ('snapshot_in_progress','review_pending')`, dm.NewClob(string(b)), a.jobID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("保存清理计划时 bootstrap 状态已变化")
	}
	return nil
}

func (a *DamengApplier) cleanupObjectID(ctx context.Context, item damengCleanupItem) (int64, bool, error) {
	var id int64
	var err error
	if item.Constraint == "" {
		err = a.db.QueryRowContext(ctx, `SELECT OBJECT_ID FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=? AND OBJECT_TYPE='TABLE'`, a.schema, item.Table).Scan(&id)
	} else {
		err = a.db.QueryRowContext(ctx, `SELECT c.ID FROM SYS.SYSOBJECTS c JOIN SYS.SYSOBJECTS t ON t.ID=c.PID JOIN SYS.SYSOBJECTS s ON s.ID=t.SCHID WHERE s.NAME=? AND t.NAME=? AND c.NAME=? AND c.SUBTYPE$='CONS'`, a.schema, item.Table, item.Constraint).Scan(&id)
	}
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return id, err == nil, err
}

func (a *DamengApplier) planBootstrapCleanup(ctx context.Context, r BootstrapRecord) (damengCleanupPlan, error) {
	plan := damengCleanupPlan{Manifest: r.ManifestHash}
	effective := map[string]bool{}
	excluded := map[string]BootstrapIssue{}
	for _, t := range r.EffectiveTables {
		effective[targetObjectName(a.cfg, t)] = true
	}
	for _, issue := range r.ExcludedTables {
		name := targetObjectName(a.cfg, issue.Table)
		if effective[name] || name == strings.ToUpper(CheckpointTableName) {
			return plan, fmt.Errorf("无效的 bootstrap 排除表 %s", issue.Table)
		}
		excluded[name] = issue
	}
	// Constraints are planned before tables, and only on this job's effective
	// or excluded tables. An unrelated referencing table blocks DROP, never CASCADE.
	rows, err := a.db.QueryContext(ctx, `SELECT c.TABLE_NAME,c.CONSTRAINT_NAME,p.TABLE_NAME FROM ALL_CONSTRAINTS c JOIN ALL_CONSTRAINTS p ON p.OWNER=c.R_OWNER AND p.CONSTRAINT_NAME=c.R_CONSTRAINT_NAME WHERE c.OWNER=? AND p.OWNER=? AND c.CONSTRAINT_TYPE='R' ORDER BY c.TABLE_NAME,c.CONSTRAINT_NAME`, a.schema, a.schema)
	if err != nil {
		return plan, err
	}
	var candidates []damengCleanupItem
	for rows.Next() {
		var table, constraint, referenced string
		if err = rows.Scan(&table, &constraint, &referenced); err != nil {
			rows.Close()
			return plan, err
		}
		_, failed := excluded[referenced]
		childIssue, childExcluded := excluded[table]
		if failed && (effective[table] || childExcluded && ownsBootstrapTable(childIssue)) {
			candidates = append(candidates, damengCleanupItem{Table: table, Constraint: constraint})
		}
	}
	if err = closeRows(rows); err != nil {
		return plan, err
	}
	for _, issue := range r.ExcludedTables {
		// schema failures may leave a pre-existing object; there is no transactional
		// DROP+CREATE guarantee on DM. Never delete that object during review.
		if ownsBootstrapTable(issue) {
			candidates = append(candidates, damengCleanupItem{Table: targetObjectName(a.cfg, issue.Table)})
		}
	}
	seen := map[string]bool{}
	for _, item := range candidates {
		key := item.Table + "\x00" + item.Constraint
		if seen[key] {
			continue
		}
		seen[key] = true
		id, exists, err := a.cleanupObjectID(ctx, item)
		if err != nil {
			return plan, err
		}
		if !exists {
			continue
		}
		item.ObjectID = id
		plan.Items = append(plan.Items, item)
	}
	return plan, nil
}

func ownsBootstrapTable(issue BootstrapIssue) bool {
	return issue.Stage == "data" || issue.Stage == "row_count" || issue.Stage == "cdc_compatibility"
}
