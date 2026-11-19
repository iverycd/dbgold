package cdc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
)

func (a *PostgresApplier) ddlAcknowledged(ctx context.Context, p Position) (bool, error) {
	var saved Position
	var pos int64
	err := a.db.QueryRowContext(ctx, `SELECT ack_ddl_file,ack_ddl_position,ack_ddl_gtid FROM `+a.checkpointTable()+` WHERE job_id=$1`, a.jobID).
		Scan(&saved.File, &pos, &saved.GTID)
	if err != nil {
		return false, err
	}
	if pos < 0 || pos > math.MaxUint32 {
		return false, fmt.Errorf("DDL 确认标记的文件位点超出范围: %d", pos)
	}
	saved.Pos = uint32(pos)
	return PositionEquivalent(saved, p), nil
}

func (a *PostgresApplier) acknowledgeDDL(ctx context.Context, expected, next BootstrapRecord) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if a.cfg.ManagedCheckpoint {
		if err = checkpointStoreFor(a).requireOwner(ctx, tx, true); err != nil {
			return err
		}
	}
	var actual Position
	var pos int64
	var state, hash string
	err = tx.QueryRowContext(ctx, `SELECT binlog_file,binlog_position,gtid,source_schema_hash,bootstrap_state FROM `+a.checkpointTable()+` WHERE job_id=$1 FOR UPDATE`, a.jobID).
		Scan(&actual.File, &pos, &actual.GTID, &hash, &state)
	if err != nil {
		return err
	}
	if pos < 0 || pos > math.MaxUint32 {
		return fmt.Errorf("DDL 确认的检查点文件位点超出范围: %d", pos)
	}
	actual.Pos = uint32(pos)
	if state != "completed" || !PositionEquivalent(actual, expected.Position) || hash != expected.SourceSchema.Hash {
		return fmt.Errorf("目标检查点或源结构已变化，拒绝确认旧 DDL")
	}
	if PositionReached(actual, next.Position) || !PositionReached(next.Position, actual) {
		return fmt.Errorf("DDL 确认不得回退或覆盖检查点")
	}
	schemaJSON, err := json.Marshal(next.SourceSchema)
	if err != nil {
		return err
	}
	locators, err := json.Marshal(next.LocatorStrategies)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+a.checkpointTable()+` SET binlog_file=$1,binlog_position=$2,gtid=$3,
		source_schema_version=$4,source_schema=$5,source_schema_hash=$6,locator_strategies=$7,
		ack_ddl_file=$1,ack_ddl_position=$2,ack_ddl_gtid=$3,updated_at=now() WHERE job_id=$8`, next.Position.File, int64(next.Position.Pos), next.Position.GTID,
		next.SourceSchema.Version, string(schemaJSON), next.SourceSchema.Hash, string(locators), a.jobID)
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
