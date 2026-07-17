package cdc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"dbgold/datamigrate/valueconv"
	_ "github.com/lib/pq"
)

var ErrRowConflict = errors.New("target row conflict")

type RowConflictError struct {
	Table      string
	Action     string
	BeforeHash string
	Reason     string
}

func (e *RowConflictError) Error() string {
	return fmt.Sprintf("表 %s %s 整行定位冲突: %s", e.Table, e.Action, e.Reason)
}
func (e *RowConflictError) Unwrap() error { return ErrRowConflict }

type PostgresApplier struct {
	db     *sql.DB
	schema string
	jobID  string
	lower  bool
	conv   *valueconv.PostgresValueConverter
}

func NewPostgresApplier(ctx context.Context, dsn, schema, jobID string, lower bool) (*PostgresApplier, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	a := &PostgresApplier{db: db, schema: schema, jobID: jobID, lower: lower, conv: valueconv.NewPostgres()}
	if err = a.ensureCheckpoint(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

func (a *PostgresApplier) Close() error { return a.db.Close() }

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func (a *PostgresApplier) name(s string) string {
	if a.lower {
		return strings.ToLower(s)
	}
	return s
}
func (a *PostgresApplier) qualified(table string) string {
	return quoteIdent(a.schema) + "." + quoteIdent(a.name(table))
}
func (a *PostgresApplier) checkpointTable() string {
	return quoteIdent(a.schema) + "." + quoteIdent(CheckpointTableName)
}

func (a *PostgresApplier) ensureCheckpoint(ctx context.Context) error {
	if _, err := a.db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			job_id text PRIMARY KEY, binlog_file text NOT NULL DEFAULT '', binlog_position bigint NOT NULL DEFAULT 4,
			gtid text NOT NULL DEFAULT '', bootstrap_state text NOT NULL DEFAULT 'completed',
			effective_tables text NOT NULL DEFAULT '[]', excluded_tables text NOT NULL DEFAULT '[]',
			manifest_hash text NOT NULL DEFAULT '', failed_objects text NOT NULL DEFAULT '[]',
			failure_report_version integer NOT NULL DEFAULT 0, locator_strategy_version integer NOT NULL DEFAULT 0,
			locator_strategies text NOT NULL DEFAULT '[]', updated_at timestamptz NOT NULL DEFAULT now())`, a.checkpointTable())); err != nil {
		return err
	}
	for _, statement := range []string{
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS bootstrap_state text NOT NULL DEFAULT 'completed'`, a.checkpointTable()),
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS effective_tables text NOT NULL DEFAULT '[]'`, a.checkpointTable()),
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS excluded_tables text NOT NULL DEFAULT '[]'`, a.checkpointTable()),
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS manifest_hash text NOT NULL DEFAULT ''`, a.checkpointTable()),
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS failed_objects text NOT NULL DEFAULT '[]'`, a.checkpointTable()),
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS failure_report_version integer NOT NULL DEFAULT 0`, a.checkpointTable()),
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS locator_strategy_version integer NOT NULL DEFAULT 0`, a.checkpointTable()),
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS locator_strategies text NOT NULL DEFAULT '[]'`, a.checkpointTable()),
	} {
		if _, err := a.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (a *PostgresApplier) LoadCheckpoint(ctx context.Context) (Position, bool, error) {
	record, exists, err := a.LoadBootstrapRecord(ctx)
	if err != nil || !exists {
		return Position{}, exists, err
	}
	if record.State != "completed" {
		return Position{}, false, fmt.Errorf("checkpoint bootstrap 状态不是 completed: %s", record.State)
	}
	return record.Position, true, nil
}

func (a *PostgresApplier) LoadBootstrapRecord(ctx context.Context) (BootstrapRecord, bool, error) {
	var record BootstrapRecord
	var n int64
	var effectiveJSON, excludedJSON, failedObjectsJSON, locatorStrategiesJSON string
	err := a.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT binlog_file, binlog_position, gtid, bootstrap_state,
		effective_tables, excluded_tables, manifest_hash, failed_objects, failure_report_version,
		locator_strategy_version, locator_strategies
		FROM %s WHERE job_id=$1`, a.checkpointTable()), a.jobID).
		Scan(&record.Position.File, &n, &record.Position.GTID, &record.State, &effectiveJSON, &excludedJSON, &record.ManifestHash,
			&failedObjectsJSON, &record.FailureReportVersion, &record.LocatorStrategyVersion, &locatorStrategiesJSON)
	if err == sql.ErrNoRows {
		return BootstrapRecord{}, false, nil
	}
	if err != nil {
		return BootstrapRecord{}, false, err
	}
	record.Position.Pos = uint32(n)
	if err = json.Unmarshal([]byte(effectiveJSON), &record.EffectiveTables); err != nil {
		return BootstrapRecord{}, false, fmt.Errorf("解析 checkpoint effective_tables 失败: %w", err)
	}
	if err = json.Unmarshal([]byte(excludedJSON), &record.ExcludedTables); err != nil {
		return BootstrapRecord{}, false, fmt.Errorf("解析 checkpoint excluded_tables 失败: %w", err)
	}
	if err = json.Unmarshal([]byte(failedObjectsJSON), &record.FailedObjects); err != nil {
		return BootstrapRecord{}, false, fmt.Errorf("解析 checkpoint failed_objects 失败: %w", err)
	}
	if err = json.Unmarshal([]byte(locatorStrategiesJSON), &record.LocatorStrategies); err != nil {
		return BootstrapRecord{}, false, fmt.Errorf("解析 checkpoint locator_strategies 失败: %w", err)
	}
	return record, true, nil
}

func (a *PostgresApplier) SaveBootstrapRecord(ctx context.Context, record BootstrapRecord) error {
	effectiveJSON, err := json.Marshal(record.EffectiveTables)
	if err != nil {
		return err
	}
	excludedJSON, err := json.Marshal(record.ExcludedTables)
	if err != nil {
		return err
	}
	failedObjectsJSON, err := json.Marshal(record.FailedObjects)
	if err != nil {
		return err
	}
	locatorStrategiesJSON, err := json.Marshal(record.LocatorStrategies)
	if err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(job_id,binlog_file,binlog_position,gtid,bootstrap_state,effective_tables,excluded_tables,manifest_hash,failed_objects,failure_report_version,locator_strategy_version,locator_strategies,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now()) ON CONFLICT(job_id) DO UPDATE SET
		binlog_file=EXCLUDED.binlog_file,binlog_position=EXCLUDED.binlog_position,gtid=EXCLUDED.gtid,
		bootstrap_state=EXCLUDED.bootstrap_state,effective_tables=EXCLUDED.effective_tables,
		excluded_tables=EXCLUDED.excluded_tables,manifest_hash=EXCLUDED.manifest_hash,
		failed_objects=EXCLUDED.failed_objects,failure_report_version=EXCLUDED.failure_report_version,
		locator_strategy_version=EXCLUDED.locator_strategy_version,locator_strategies=EXCLUDED.locator_strategies,updated_at=now()`, a.checkpointTable()),
		a.jobID, record.Position.File, record.Position.Pos, record.Position.GTID, record.State, string(effectiveJSON), string(excludedJSON), record.ManifestHash,
		string(failedObjectsJSON), record.FailureReportVersion, record.LocatorStrategyVersion, string(locatorStrategiesJSON))
	return err
}

func (a *PostgresApplier) FinalizeBootstrap(ctx context.Context, record BootstrapRecord) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	dropped := map[string]bool{}
	effective := make(map[string]bool, len(record.EffectiveTables))
	for _, table := range record.EffectiveTables {
		effective[a.name(table)] = true
	}
	for _, issue := range record.ExcludedTables {
		if dropped[issue.Table] {
			continue
		}
		dropped[issue.Table] = true
		if err = a.dropForeignKeysReferencing(ctx, tx, issue.Table, effective); err != nil {
			return err
		}
		// PostgreSQL 在同一次 Exec 中执行 DROP + CREATE 时具有事务性；建表
		// 失败意味着原有目标表仍然存在，不能把它当作任务残留删除。
		if issue.Stage == "schema" {
			continue
		}
		if _, err = tx.ExecContext(ctx, "DROP TABLE IF EXISTS "+a.qualified(issue.Table)+" CASCADE"); err != nil {
			return fmt.Errorf("清理被排除表 %s 失败: %w", issue.Table, err)
		}
	}
	effectiveJSON, err := json.Marshal(record.EffectiveTables)
	if err != nil {
		return err
	}
	excludedJSON, err := json.Marshal(record.ExcludedTables)
	if err != nil {
		return err
	}
	failedObjectsJSON, err := json.Marshal(record.FailedObjects)
	if err != nil {
		return err
	}
	locatorStrategiesJSON, err := json.Marshal(record.LocatorStrategies)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET bootstrap_state='completed',
		effective_tables=$2,excluded_tables=$3,manifest_hash=$4,failed_objects=$5,failure_report_version=$6,
		locator_strategy_version=$7,locator_strategies=$8,updated_at=now()
		WHERE job_id=$1 AND bootstrap_state IN ('snapshot_in_progress','review_pending','completed')`, a.checkpointTable()),
		a.jobID, string(effectiveJSON), string(excludedJSON), record.ManifestHash, string(failedObjectsJSON), record.FailureReportVersion,
		record.LocatorStrategyVersion, string(locatorStrategiesJSON))
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("bootstrap checkpoint 不存在或状态不可确认")
	}
	return tx.Commit()
}

func (a *PostgresApplier) dropForeignKeysReferencing(ctx context.Context, tx *sql.Tx, excludedTable string, effective map[string]bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT source.relname, constraint_row.conname
		FROM pg_constraint constraint_row
		JOIN pg_class source ON source.oid=constraint_row.conrelid
		JOIN pg_namespace source_ns ON source_ns.oid=source.relnamespace
		JOIN pg_class referenced ON referenced.oid=constraint_row.confrelid
		JOIN pg_namespace referenced_ns ON referenced_ns.oid=referenced.relnamespace
		WHERE constraint_row.contype='f' AND source_ns.nspname=$1 AND referenced_ns.nspname=$1
		AND referenced.relname=$2`, a.schema, a.name(excludedTable))
	if err != nil {
		return fmt.Errorf("读取被排除表 %s 的引用外键失败: %w", excludedTable, err)
	}
	type foreignKey struct{ table, constraint string }
	var constraints []foreignKey
	for rows.Next() {
		var item foreignKey
		if err = rows.Scan(&item.table, &item.constraint); err != nil {
			rows.Close()
			return err
		}
		constraints = append(constraints, item)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, item := range constraints {
		if !effective[item.table] {
			continue
		}
		statement := "ALTER TABLE " + quoteIdent(a.schema) + "." + quoteIdent(item.table) + " DROP CONSTRAINT " + quoteIdent(item.constraint)
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("移除引用被排除表 %s 的外键 %s.%s 失败: %w", excludedTable, item.table, item.constraint, err)
		}
	}
	return nil
}

func (a *PostgresApplier) MarkBootstrapAborted(ctx context.Context) error {
	result, err := a.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET bootstrap_state='aborted',updated_at=now()
		WHERE job_id=$1 AND bootstrap_state!='completed'`, a.checkpointTable()), a.jobID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return fmt.Errorf("bootstrap checkpoint 不存在或已完成")
	}
	return nil
}

func (a *PostgresApplier) SaveCheckpoint(ctx context.Context, p Position) error {
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

func (a *PostgresApplier) saveCheckpointTx(ctx context.Context, tx *sql.Tx, p Position) error {
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(job_id,binlog_file,binlog_position,gtid,updated_at)
		VALUES($1,$2,$3,$4,now()) ON CONFLICT(job_id) DO UPDATE SET binlog_file=EXCLUDED.binlog_file,
		binlog_position=EXCLUDED.binlog_position,gtid=EXCLUDED.gtid,updated_at=now()`, a.checkpointTable()), a.jobID, p.File, p.Pos, p.GTID)
	return err
}

func (a *PostgresApplier) Apply(ctx context.Context, changes []Change, p Position) (Stats, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return Stats{}, err
	}
	defer tx.Rollback()
	var stats Stats
	for _, ch := range changes {
		switch ch.Action {
		case "insert":
			if err = a.insert(ctx, tx, ch.Table, ch.After); err == nil {
				stats.Inserts++
			}
		case "update":
			if ch.Table.LocatorStrategy == LocatorFullRow {
				err = a.updateFullRow(ctx, tx, ch.Table, ch.Before, ch.After)
			} else {
				if locatorChanged(ch.Table, ch.Before, ch.After) {
					err = a.deleteByLocator(ctx, tx, ch.Table, ch.Before)
				}
				if err == nil {
					err = a.insert(ctx, tx, ch.Table, ch.After)
				}
			}
			if err == nil {
				stats.Updates++
			}
		case "delete":
			if ch.Table.LocatorStrategy == LocatorFullRow {
				err = a.deleteFullRow(ctx, tx, ch.Table, ch.Before)
			} else {
				err = a.deleteByLocator(ctx, tx, ch.Table, ch.Before)
			}
			if err == nil {
				stats.Deletes++
			}
		}
		if err != nil {
			return Stats{}, fmt.Errorf("apply %s %s: %w", ch.Action, ch.Table.Name, err)
		}
	}
	if err = a.saveCheckpointTx(ctx, tx, p); err != nil {
		return Stats{}, err
	}
	if err = tx.Commit(); err != nil {
		return Stats{}, err
	}
	stats.Position, stats.LastEventAt = p, time.Now()
	return stats, nil
}

func locatorChanged(t *TableInfo, before, after []any) bool {
	for _, column := range t.LocatorColumns {
		i := columnIndex(t, column)
		if i < 0 || i >= len(before) || i >= len(after) || !reflect.DeepEqual(before[i], after[i]) {
			return true
		}
	}
	return false
}

func (a *PostgresApplier) insert(ctx context.Context, tx *sql.Tx, t *TableInfo, row []any) error {
	if len(row) != len(t.Columns) {
		return fmt.Errorf("列数不匹配: event=%d metadata=%d", len(row), len(t.Columns))
	}
	cols := make([]string, len(t.Columns))
	marks := make([]string, len(t.Columns))
	vals := make([]any, len(row))
	for i, c := range t.Columns {
		cols[i] = quoteIdent(a.name(c))
		marks[i] = fmt.Sprintf("$%d", i+1)
		vals[i] = a.conv.Convert(row[i], "mysql", t.ColumnTypes[i])
	}
	sqlText := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", a.qualified(t.Name), strings.Join(cols, ","), strings.Join(marks, ","))
	if t.LocatorStrategy == LocatorPrimaryKey || t.LocatorStrategy == LocatorUniqueKey {
		locatorIndexes, err := locatorColumnIndexes(t)
		if err != nil {
			return err
		}
		pk := make([]string, len(locatorIndexes))
		set := make([]string, 0, len(t.Columns))
		pkSet := map[int]bool{}
		for i, p := range locatorIndexes {
			pk[i] = quoteIdent(a.name(t.Columns[p]))
			pkSet[p] = true
		}
		for i, c := range t.Columns {
			if !pkSet[i] {
				q := quoteIdent(a.name(c))
				set = append(set, q+"=EXCLUDED."+q)
			}
		}
		if len(set) == 0 {
			sqlText += " ON CONFLICT (" + strings.Join(pk, ",") + ") DO NOTHING"
		} else {
			sqlText += " ON CONFLICT (" + strings.Join(pk, ",") + ") DO UPDATE SET " + strings.Join(set, ",")
		}
	}
	_, err := tx.ExecContext(ctx, sqlText, vals...)
	return err
}

func (a *PostgresApplier) deleteByLocator(ctx context.Context, tx *sql.Tx, t *TableInfo, row []any) error {
	indexes, err := locatorColumnIndexes(t)
	if err != nil {
		return err
	}
	where := make([]string, len(indexes))
	vals := make([]any, len(indexes))
	for i, p := range indexes {
		where[i] = quoteIdent(a.name(t.Columns[p])) + fmt.Sprintf("=$%d", i+1)
		vals[i] = a.conv.Convert(row[p], "mysql", t.ColumnTypes[p])
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM "+a.qualified(t.Name)+" WHERE "+strings.Join(where, " AND "), vals...)
	return err
}

func locatorColumnIndexes(t *TableInfo) ([]int, error) {
	indexes := make([]int, 0, len(t.LocatorColumns))
	for _, column := range t.LocatorColumns {
		index := columnIndex(t, column)
		if index < 0 {
			return nil, fmt.Errorf("定位列不存在: %s", column)
		}
		indexes = append(indexes, index)
	}
	if len(indexes) == 0 {
		return nil, fmt.Errorf("表 %s 没有定位列", t.Name)
	}
	return indexes, nil
}

func columnIndex(t *TableInfo, column string) int {
	for i, name := range t.Columns {
		if name == column {
			return i
		}
	}
	return -1
}

func (a *PostgresApplier) updateFullRow(ctx context.Context, tx *sql.Tx, t *TableInfo, before, after []any) error {
	if len(before) != len(t.Columns) || len(after) != len(t.Columns) {
		return fmt.Errorf("列数不匹配: before=%d after=%d metadata=%d", len(before), len(after), len(t.Columns))
	}
	set := make([]string, len(t.Columns))
	where := make([]string, len(t.Columns))
	values := make([]any, 0, len(t.Columns)*2)
	for i, column := range t.Columns {
		set[i] = quoteIdent(a.name(column)) + fmt.Sprintf("=$%d", i+1)
		values = append(values, a.conv.Convert(after[i], "mysql", t.ColumnTypes[i]))
	}
	for i, column := range t.Columns {
		parameter := len(t.Columns) + i + 1
		where[i] = quoteIdent(a.name(column)) + fmt.Sprintf(" IS NOT DISTINCT FROM $%d", parameter)
		values = append(values, a.conv.Convert(before[i], "mysql", t.ColumnTypes[i]))
	}
	statement := fmt.Sprintf("UPDATE %s SET %s WHERE ctid=(SELECT ctid FROM %s WHERE %s LIMIT 1)",
		a.qualified(t.Name), strings.Join(set, ","), a.qualified(t.Name), strings.Join(where, " AND "))
	result, err := tx.ExecContext(ctx, statement, values...)
	if err != nil {
		return err
	}
	return requireOneFullRow(t, "update", before, result)
}

func (a *PostgresApplier) deleteFullRow(ctx context.Context, tx *sql.Tx, t *TableInfo, before []any) error {
	if len(before) != len(t.Columns) {
		return fmt.Errorf("列数不匹配: before=%d metadata=%d", len(before), len(t.Columns))
	}
	where := make([]string, len(t.Columns))
	values := make([]any, len(t.Columns))
	for i, column := range t.Columns {
		where[i] = quoteIdent(a.name(column)) + fmt.Sprintf(" IS NOT DISTINCT FROM $%d", i+1)
		values[i] = a.conv.Convert(before[i], "mysql", t.ColumnTypes[i])
	}
	statement := fmt.Sprintf("DELETE FROM %s WHERE ctid=(SELECT ctid FROM %s WHERE %s LIMIT 1)",
		a.qualified(t.Name), a.qualified(t.Name), strings.Join(where, " AND "))
	result, err := tx.ExecContext(ctx, statement, values...)
	if err != nil {
		return err
	}
	return requireOneFullRow(t, "delete", before, result)
}

func requireOneFullRow(t *TableInfo, action string, before []any, result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	payload, _ := json.Marshal(before)
	hash := sha256.Sum256(payload)
	return &RowConflictError{Table: t.Name, Action: action, BeforeHash: fmt.Sprintf("%x", hash[:]), Reason: fmt.Sprintf("期望影响 1 行，实际影响 %d 行", affected)}
}

// SyncSequences moves PostgreSQL sequences to at least MAX(column)+1.
func (a *PostgresApplier) SyncSequences(ctx context.Context, tables []TableInfo) error {
	for _, t := range tables {
		for _, idx := range t.AutoIncrement {
			var seq sql.NullString
			err := a.db.QueryRowContext(ctx, `SELECT pg_get_serial_sequence($1,$2)`, a.schema+"."+a.name(t.Name), a.name(t.Columns[idx])).Scan(&seq)
			if err != nil || !seq.Valid {
				candidate := a.schema + ".seq_" + a.name(t.Name) + "_" + a.name(t.Columns[idx])
				if e := a.db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, candidate).Scan(&seq); e != nil || !seq.Valid {
					return fmt.Errorf("未找到自增列对应序列: %s.%s", t.Name, t.Columns[idx])
				}
			}
			_, err = a.db.ExecContext(ctx, fmt.Sprintf(`SELECT setval($1::regclass, COALESCE((SELECT MAX(%s) FROM %s), 0) + 1, false)`, quoteIdent(a.name(t.Columns[idx])), a.qualified(t.Name)), seq.String)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
