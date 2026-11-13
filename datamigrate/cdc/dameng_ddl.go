package cdc

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"gitee.com/chunanyong/dm"
)

// AcknowledgeDamengDDL verifies the source schema at the DDL boundary before
// atomically replacing BOTH the target checkpoint and its decoding metadata.
func AcknowledgeDamengDDL(ctx context.Context, cfg Config, p Position) (BootstrapRecord, error) {
	a, err := newDamengApplier(ctx, cfg)
	if err != nil {
		return BootstrapRecord{}, err
	}
	defer a.Close()
	current, exists, err := a.LoadBootstrapRecord(ctx)
	if err != nil {
		return current, err
	}
	if !exists || current.State != "completed" {
		return current, fmt.Errorf("DDL 确认缺少已完成的目标检查点")
	}
	if err = current.SourceSchema.Validate(cfg.SourceDatabase); err != nil {
		return current, err
	}
	// The durable acknowledgement marker distinguishes a retried acknowledgement
	// from an unrelated checkpoint that happens to be beyond the DDL boundary.
	acknowledged, err := a.ddlAcknowledged(ctx, p)
	if err != nil {
		return current, err
	}
	if acknowledged {
		if !PositionReached(current.Position, p) {
			return current, fmt.Errorf("DDL 确认标记与目标检查点不一致")
		}
		return current, nil
	}
	if PositionReached(current.Position, p) {
		return current, fmt.Errorf("目标检查点已经越过待确认 DDL，拒绝覆盖")
	}
	cfg.TableNames = current.EffectiveTables
	snapshot, err := CaptureSourceSchema(ctx, cfg, p)
	if err != nil {
		return current, err
	}
	tables, err := snapshot.TableInfos(cfg.SourceDatabase, current.EffectiveTables)
	if err != nil {
		return current, err
	}
	failures, err := ValidateTargetTableCompatibility(ctx, cfg, tables)
	if err != nil {
		return current, err
	}
	if len(failures) > 0 {
		return current, fmt.Errorf("DDL 后目标结构不兼容: %v", failures)
	}
	if err = a.CheckBusinessTriggers(ctx, tables); err != nil {
		return current, err
	}
	resolved, err := ResolveLocatorStrategies(ctx, cfg, tables)
	if err != nil {
		return current, err
	}
	next := current
	next.Position, next.SourceSchema, next.LocatorStrategies = p, snapshot, LocatorStrategiesFromTables(resolved)
	if err = a.acknowledgeDDL(ctx, current, next); err != nil {
		return current, err
	}
	return next, nil
}
func (a *DamengApplier) ddlAcknowledged(ctx context.Context, p Position) (bool, error) {
	var file, gtid sql.NullString
	var pos uint32
	err := a.db.QueryRowContext(ctx, "SELECT ACK_DDL_FILE,ACK_DDL_POSITION,ACK_DDL_GTID FROM "+a.checkpointTable()+" WHERE JOB_ID=?", a.jobID).Scan(&file, &pos, &gtid)
	if err != nil {
		return false, err
	}
	return PositionEquivalent(Position{File: file.String, Pos: pos, GTID: gtid.String}, p), nil
}
func (a *DamengApplier) acknowledgeDDL(ctx context.Context, expected, next BootstrapRecord) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var file, gtid, hash sql.NullString
	var pos uint32
	var state string
	err = tx.QueryRowContext(ctx, "SELECT BINLOG_FILE,BINLOG_POSITION,GTID,SOURCE_SCHEMA_HASH,BOOTSTRAP_STATE FROM "+a.checkpointTable()+" WHERE JOB_ID=? FOR UPDATE", a.jobID).Scan(&file, &pos, &gtid, &hash, &state)
	if err != nil {
		return err
	}
	actual := Position{File: file.String, Pos: pos, GTID: gtid.String}
	if state != "completed" || !PositionEquivalent(actual, expected.Position) || hash.String != expected.SourceSchema.Hash {
		return fmt.Errorf("目标检查点或源结构已变化，拒绝确认旧 DDL")
	}
	if PositionReached(actual, next.Position) {
		return fmt.Errorf("DDL 确认不得回退或覆盖检查点")
	}
	schemaJSON, err := json.Marshal(next.SourceSchema)
	if err != nil {
		return err
	}
	locatorJSON, err := json.Marshal(next.LocatorStrategies)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE "+a.checkpointTable()+` SET BINLOG_FILE=?,BINLOG_POSITION=?,GTID=?,SOURCE_SCHEMA_VERSION=?,SOURCE_SCHEMA=?,SOURCE_SCHEMA_HASH=?,LOCATOR_STRATEGIES=?,ACK_DDL_FILE=?,ACK_DDL_POSITION=?,ACK_DDL_GTID=?,UPDATED_AT=CURRENT_TIMESTAMP WHERE JOB_ID=?`, next.Position.File, int64(next.Position.Pos), damengOptionalClob(next.Position.GTID), next.SourceSchema.Version, dm.NewClob(string(schemaJSON)), next.SourceSchema.Hash, dm.NewClob(string(locatorJSON)), next.Position.File, int64(next.Position.Pos), damengOptionalClob(next.Position.GTID), a.jobID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("DDL 确认更新检查点数量异常: %d", n)
	}
	return tx.Commit()
}
