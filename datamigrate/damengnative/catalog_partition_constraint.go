package damengnative

import "fmt"

// A physical partition's constraints are engine-owned copies of the logical
// parent's constraints. DM rejects independent ALTER/ADD/DROP on these child
// tables. Prove the native CONS -> partition UTAB -> base UTAB relation through
// SYSHPARTTABLEINFO; generated-looking names are never ownership evidence.
func (r *catalogReader) markPartitionDerivedConstraints() error {
	possible := false
	for _, raw := range r.raw {
		if raw.sub == "CONS" {
			if table, found := r.raw[raw.pid]; found && table.sub == "UTAB" && table.pid > 0 {
				possible = true
				break
			}
		}
	}
	if !possible {
		return nil
	}
	rows, err := r.db.QueryContext(r.ctx, "SELECT c.ID,t.ID,p.BASE_TABLE_ID FROM SYS.SYSOBJECTS c JOIN SYS.SYSOBJECTS t ON t.ID=c.PID JOIN SYS.SYSHPARTTABLEINFO p ON p.PART_TABLE_ID=t.ID JOIN SYS.SYSOBJECTS s ON s.ID=t.SCHID WHERE s.TYPE$='SCH' AND s.NAME=? AND c.SUBTYPE$='CONS' ORDER BY c.ID", r.schema)
	if err != nil {
		return fmt.Errorf("无法确认分区派生约束的原生父对象: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var constraintID, partitionID, parentID int64
		if err := rows.Scan(&constraintID, &partitionID, &parentID); err != nil {
			return err
		}
		constraint, constraintKnown := r.raw[constraintID]
		partition, partitionKnown := r.raw[partitionID]
		parent, parentKnown := r.raw[parentID]
		if !constraintKnown || !partitionKnown || !parentKnown || constraint.sub != "CONS" || constraint.pid != partitionID || partition.sub != "UTAB" || partition.pid != parentID || parent.sub != "UTAB" {
			return fmt.Errorf("分区派生约束 %d 的目录父链不完整或不一致", constraintID)
		}
		owner := r.base(partitionID)
		if owner == nil {
			return fmt.Errorf("分区派生约束 %d 缺少已盘点的物理父对象", constraintID)
		}
		for _, position := range r.byRaw[constraintID] {
			o := &r.snapshot.Objects[position]
			o.Managed, o.Generated = true, true
			o.ParentID, o.Table = owner.ID, owner.Name
			o.Dependencies = appendUnique(o.Dependencies, owner.ID)
		}
	}
	return rows.Err()
}
