package handler

import (
	"context"
	"dbgold/datamigrate"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
	"encoding/json"
	"fmt"
)

func withCrossOracleMigrationEvidence(ctx context.Context, owner uint, src, dst *store.Connection, sourceSchema, targetSchema string) (context.Context, error) {
	if dst.DBType != "oracle" || (src.DBType != "dameng" && src.DBType != "sqlserver") {
		return ctx, nil
	}
	if src.DBType == "sqlserver" {
		sourceSchema = "dbo"
	}
	var rows []struct{ ObjectJSON string }
	err := store.DB.WithContext(ctx).Table("migration_repair_objects AS o").Select("o.object_json").
		Joins("JOIN migration_repair_bindings AS b ON b.job_id = o.job_id").
		Joins("JOIN data_migration_jobs AS j ON j.job_id = o.job_id").
		Where("b.src_conn_id = ? AND b.dst_conn_id = ? AND b.source_database = ? AND b.source_schema = ? AND b.target_schema = ?", src.ID, dst.ID, src.Database, sourceSchema, targetSchema).
		Where("b.configured = ? AND b.legacy = ? AND b.source_fingerprint = ? AND b.target_fingerprint = ?", true, false, connectionFingerprint(src), connectionFingerprint(dst)).
		Where("j.owner_id = ? AND j.src_conn_id = ? AND j.dst_conn_id = ? AND j.src_db_type = ? AND j.dst_db_type = ?", owner, src.ID, dst.ID, src.DBType, dst.DBType).
		Where("j.source_connection_database = ? AND j.source_schema = ? AND j.dst_schema = ?", src.Database, sourceSchema, targetSchema).
		Where("j.src_conn_host = ? AND j.src_conn_port = ? AND j.src_conn_username = ? AND j.dst_conn_host = ? AND j.dst_conn_port = ? AND j.dst_conn_database = ? AND j.dst_conn_username = ?", src.Host, src.Port, src.Username, dst.Host, dst.Port, dst.Database, dst.Username).
		Where("j.status IN ?", []string{"done", "failed", "cancelled"}).Order("o.id").Scan(&rows).Error
	if err != nil {
		return ctx, fmt.Errorf("读取 跨库 Oracle 序列身份账本失败: %w", err)
	}

	var saved []source.SequenceInfo
	for _, row := range rows {
		var object repair.Object
		if e := json.Unmarshal([]byte(row.ObjectJSON), &object); e != nil {
			return ctx, fmt.Errorf("跨库 Oracle 修复账本格式错误: %w", e)
		}
		if object.Status != "succeeded" || object.Sequence == nil || object.Sequence.Native == nil || object.Sequence.Native.OracleObjectID <= 0 || !object.TrustedCrossOracleSequence() || object.Schema != targetSchema || object.Sequence.Native.Schema != sourceSchema {
			continue
		}
		valid := true
		for _, u := range object.Units {
			valid = valid && u.Status == "succeeded"
		}
		if valid {
			saved = append(saved, *object.Sequence)
		}
	}
	return datamigrate.WithCrossOracleEvidence(ctx, saved), nil
}
