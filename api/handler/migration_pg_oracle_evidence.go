package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"dbgold/datamigrate"
	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
)

// Evidence comes only from immutable server-created sequence programs in the
// repair ledger. Neither submitted plans nor a matching target name establish
// ownership. The migrator still compares the current source definition and the
// writer rechecks the target object ID and fingerprint before any data write.
func withPGOracleMigrationEvidence(ctx context.Context, owner uint, src, dst *store.Connection, sourceSchema, targetSchema string) (context.Context, error) {
	if !source.IsPGFamily(src.DBType) || dst.DBType != "oracle" {
		return ctx, nil
	}
	if sourceSchema == "" {
		sourceSchema = "public"
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
		return ctx, fmt.Errorf("读取 PG Oracle 序列身份账本失败: %w", err)
	}
	saved := map[string]pgoracle.SequencePlan{}
	for _, row := range rows {
		var object repair.Object
		if err := json.Unmarshal([]byte(row.ObjectJSON), &object); err != nil {
			return ctx, fmt.Errorf("PG Oracle 修复账本格式错误: %w", err)
		}
		plan := object.PGOracleSequence
		if object.Kind != "sequence" || object.Status != "succeeded" || plan == nil || plan.Identity == nil || !object.TrustedPGOracleSequence() {
			continue
		}
		if object.Units[0].Status != "succeeded" || object.SourceName != plan.Source.Name || plan.Source.Schema != sourceSchema || plan.Schema != targetSchema || plan.Identity.ObjectID <= 0 || plan.Identity.Fingerprint != plan.Fingerprint() {
			continue
		}
		// Keep competing valid identities: the migrator attributes ambiguity to
		// that sequence and its dependent tables, leaving independent work able
		// to proceed. Only exact evidence duplicates may be coalesced here.
		key := plan.Schema + "\x00" + plan.Name + "\x00" + repair.Hash(plan.Identity) + "\x00" + pgOracleEvidenceDefinition(*plan)
		saved[key] = *plan
	}
	keys := make([]string, 0, len(saved))
	for key := range saved {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	plans := make([]pgoracle.SequencePlan, 0, len(keys))
	for _, key := range keys {
		plans = append(plans, saved[key])
	}
	return datamigrate.WithPGOracleRecoveryEvidence(ctx, plans), nil
}

func pgOracleEvidenceDefinition(plan pgoracle.SequencePlan) string {
	plan.Identity = nil
	plan.Source.LastValue, plan.Source.StartValue, plan.Source.IsCalled, plan.Source.Exhausted = 0, 0, false, false
	plan.Source.StartValueText = ""
	return repair.Hash(plan)
}
