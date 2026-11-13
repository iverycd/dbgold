package cdc

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// DamengApplier applies a complete source transaction and its checkpoint on
// one physical DM session. Schema changes never run inside that transaction.
type DamengApplier struct {
	db     *sql.DB
	cfg    Config
	schema string
	jobID  string
}

func newDamengApplier(ctx context.Context, cfg Config) (*DamengApplier, error) {
	cfg.TargetDBType = "dameng"
	cfg.TargetSchema = targetSchemaName(cfg)
	db, err := openTargetDB(cfg)
	if err != nil {
		return nil, err
	}
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	a := &DamengApplier{db: db, cfg: cfg, schema: cfg.TargetSchema, jobID: cfg.JobID}
	if err = a.ensureCheckpoint(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

func (a *DamengApplier) Close() error            { return a.db.Close() }
func (a *DamengApplier) name(name string) string { return targetObjectName(a.cfg, name) }
func (a *DamengApplier) qualified(table string) string {
	return quoteIdent(a.schema) + "." + quoteIdent(a.name(table))
}
func (a *DamengApplier) checkpointTable() string {
	return quoteIdent(a.schema) + "." + quoteIdent(a.name(CheckpointTableName))
}

const damengTriggerLockSQL = `SELECT PARA_VALUE FROM V$DM_INI WHERE PARA_NAME='TRIG_DML_LOCK'`
const damengBusinessTriggersSQL = `SELECT TABLE_NAME,TRIGGER_NAME,STATUS FROM ALL_TRIGGERS WHERE TABLE_OWNER=? ORDER BY TABLE_NAME,TRIGGER_NAME`

// DM releases predating TRIG_DML_LOCK need the stronger EXCLUSIVE lock:
// ROW EXCLUSIVE does not serialize their ENABLE/DISABLE TRIGGER operations.
func damengCDCWriteLock(ctx context.Context, db triggerMetadataReader) (string, error) {
	rows, err := db.QueryContext(ctx, damengTriggerLockSQL)
	if err != nil {
		return "", fmt.Errorf("读取达梦 TRIG_DML_LOCK 失败: %w", err)
	}
	var lockMode string
	if !rows.Next() {
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
		return "EXCLUSIVE", nil
	}
	err = rows.Scan(&lockMode)
	if closeErr := closeRows(rows); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(lockMode) != "0" {
		return "", fmt.Errorf("达梦 TRIG_DML_LOCK 必须为 0，防止 CDC 写入期间并发启用业务触发器")
	}
	return "ROW EXCLUSIVE", nil
}

func checkDamengBusinessTriggers(ctx context.Context, db triggerMetadataReader, cfg Config, tables []TableInfo) error {
	mode, err := damengCDCWriteLock(ctx, db)
	if err != nil {
		return err
	}
	if mode == "EXCLUSIVE" {
		slog.Warn("达梦未提供 TRIG_DML_LOCK，CDC 使用 EXCLUSIVE 表锁隔离业务触发器变更；目标写入和 DDL 将串行等待", "schema", targetSchemaName(cfg))
	}
	return checkDamengEnabledTriggers(ctx, db, cfg, tables)
}

func checkDamengEnabledTriggers(ctx context.Context, db triggerMetadataReader, cfg Config, tables []TableInfo) error {
	selected := make(map[string]bool, len(tables))
	for _, table := range tables {
		selected[targetObjectName(cfg, table.Name)] = true
	}
	rows, err := db.QueryContext(ctx, damengBusinessTriggersSQL, targetSchemaName(cfg))
	if err != nil {
		return fmt.Errorf("检查达梦 CDC 目标触发器失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, name, status string
		if err = rows.Scan(&table, &name, &status); err != nil {
			return err
		}
		status = strings.ToUpper(strings.TrimSpace(status))
		if selected[table] && status != "N" && status != "DISABLED" {
			return fmt.Errorf("CDC 目标表 %s.%s 存在启用或状态未知的业务触发器 %s，必须先停用目标触发器", targetSchemaName(cfg), table, name)
		}
	}
	return rows.Err()
}

func (a *DamengApplier) CheckBusinessTriggers(ctx context.Context, tables []TableInfo) error {
	return checkDamengBusinessTriggers(ctx, a.db, a.cfg, tables)
}

func (a *DamengApplier) Apply(ctx context.Context, changes []Change, p Position) (stats Stats, retErr error) {
	tableSet := map[string]TableInfo{}
	for _, change := range changes {
		if change.Table == nil {
			return Stats{}, fmt.Errorf("CDC 变更缺少表元数据")
		}
		if change.Action != "insert" && change.Action != "update" && change.Action != "delete" {
			return Stats{}, fmt.Errorf("未知 CDC 操作: %s", change.Action)
		}
		tableSet[change.Table.Name] = *change.Table
	}
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return Stats{}, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Stats{}, err
	}
	defer tx.Rollback()
	identityTable := ""
	defer func() {
		if identityTable == "" {
			return
		}
		_ = tx.Rollback()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, e := conn.ExecContext(cleanup, "SET IDENTITY_INSERT "+identityTable+" OFF"); e != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			retErr = errors.Join(retErr, fmt.Errorf("清理达梦 CDC IDENTITY_INSERT 失败: %w", e))
		}
	}()
	tables := make([]TableInfo, 0, len(tableSet))
	for _, table := range tableSet {
		tables = append(tables, table)
	}
	sort.Slice(tables, func(i, j int) bool { return a.qualified(tables[i].Name) < a.qualified(tables[j].Name) })
	lockMode := ""
	if len(tables) > 0 {
		lockMode, err = damengCDCWriteLock(ctx, tx)
		if err != nil {
			return Stats{}, err
		}
	}
	for _, table := range tables {
		if _, err = tx.ExecContext(ctx, "LOCK TABLE "+a.qualified(table.Name)+" IN "+lockMode+" MODE"); err != nil {
			return Stats{}, fmt.Errorf("锁定达梦 CDC 目标表 %s: %w", table.Name, err)
		}
	}
	var metadata targetSchemaMetadata
	if len(tables) > 0 {
		metadata, err = loadDamengTransactionMetadata(ctx, tx, a.cfg, tables)
		if err != nil {
			return Stats{}, err
		}
	}
	setIdentity := func(table string) error {
		if identityTable == table {
			return nil
		}
		if identityTable != "" {
			if _, err := tx.ExecContext(ctx, "SET IDENTITY_INSERT "+identityTable+" OFF"); err != nil {
				return err
			}
			identityTable = ""
		}
		if table != "" {
			// Even an error may leave session state changed; always attempt cleanup.
			identityTable = table
			if _, err := tx.ExecContext(ctx, "SET IDENTITY_INSERT "+table+" ON"); err != nil {
				return err
			}
		}
		return nil
	}
	statements := newDamengStatements(tx)
	defer statements.Close()
	rows := make(map[string]*damengRow, len(tables))
	for _, change := range changes {
		target := metadata.Tables[a.name(change.Table.Name)]
		if target == nil {
			return Stats{}, fmt.Errorf("目标表不存在: %s", a.qualified(change.Table.Name))
		}
		row := rows[change.Table.Name]
		if row == nil {
			var e error
			row, e = newDamengRow(a, change.Table, target)
			if e != nil {
				return Stats{}, e
			}
			rows[change.Table.Name] = row
		}
		if e := row.apply(ctx, statements, change, setIdentity); e != nil {
			return Stats{}, fmt.Errorf("apply %s %s: %w", change.Action, change.Table.Name, e)
		}
		switch change.Action {
		case "insert":
			stats.Inserts++
		case "update":
			stats.Updates++
		case "delete":
			stats.Deletes++
		}
	}
	if err = setIdentity(""); err != nil {
		return Stats{}, err
	}
	if err = statements.Close(); err != nil {
		return Stats{}, err
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

// DM advances IDENTITY on explicit inserts. Do not rebuild an identity during
// cutover: DDL commits implicitly and cannot share the checkpoint transaction.
func (a *DamengApplier) SyncSequences(ctx context.Context, tables []TableInfo) error {
	metadata, err := loadDamengTargetSchemaMetadata(ctx, a.db, a.schema)
	if err != nil {
		return err
	}
	var source *sql.DB
	if a.cfg.SourceDSN != "" {
		source, err = OpenSource(a.cfg.SourceDSN)
		if err != nil {
			return err
		}
		defer source.Close()
	}
	for _, table := range tables {
		target := metadata.Tables[a.name(table.Name)]
		if target == nil {
			return fmt.Errorf("自增校验目标表不存在: %s", table.Name)
		}
		for _, index := range table.AutoIncrement {
			if index < 0 || index >= len(table.Columns) {
				return fmt.Errorf("表 %s 自增列元数据无效", table.Name)
			}
			found := false
			for _, column := range target.Columns {
				found = found || (column.Name == a.name(table.Columns[index]) && column.Identity == "YES")
			}
			if !found {
				return fmt.Errorf("达梦表 %s 的源自增列 %s 缺少对应 IDENTITY，禁止完成切换", table.Name, table.Columns[index])
			}
		}
		for _, column := range target.Columns {
			if column.Identity != "YES" {
				continue
			}
			var sourceNext sql.NullString
			if source != nil {
				if err = source.QueryRowContext(ctx, `SELECT AUTO_INCREMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?`, a.cfg.SourceDatabase, table.Name).Scan(&sourceNext); err != nil {
					return fmt.Errorf("读取源表 %s 自增水位失败: %w", table.Name, err)
				}
			}
			var seedText, currentText, stepText string
			if err = a.db.QueryRowContext(ctx, `SELECT IDENT_SEED(?),IDENT_CURRENT(?),IDENT_INCR(?) FROM DUAL`, a.qualified(table.Name), a.qualified(table.Name), a.qualified(table.Name)).Scan(&seedText, &currentText, &stepText); err != nil {
				return fmt.Errorf("读取目标表 %s 自增水位失败: %w", table.Name, err)
			}
			var maximum sql.NullString
			if err = a.db.QueryRowContext(ctx, "SELECT MAX("+quoteIdent(column.Name)+") FROM "+a.qualified(table.Name)).Scan(&maximum); err != nil {
				return err
			}
			if err = validateDamengIdentityNext(table.Name, column, seedText, currentText, stepText, maximum, sourceNext); err != nil {
				return err
			}
		}
	}
	return nil
}
