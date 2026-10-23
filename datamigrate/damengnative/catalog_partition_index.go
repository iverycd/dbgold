package damengnative

import "fmt"

// DM creates a physical index on every partition table for both local and
// global root indexes. Those indexes cannot be independently created on a
// partition child. Prove this through SYSHPARTTABLEINFO and the native PID
// chain; an INDEX-looking user name never establishes generated ownership.
func (r *catalogReader) markPartitionDerivedIndexes() error {
	possible := false
	for _, raw := range r.raw {
		if raw.sub != "INDEX" && raw.sub != "CNTIND" {
			continue
		}
		if table, found := r.raw[raw.pid]; found && table.sub == "UTAB" && table.pid > 0 {
			possible = true
			break
		}
	}
	if !possible {
		return nil
	}
	rows, err := r.db.QueryContext(r.ctx, `SELECT i.ID,t.ID,p.BASE_TABLE_ID FROM SYS.SYSOBJECTS i JOIN SYS.SYSOBJECTS t ON t.ID=i.PID JOIN SYS.SYSHPARTTABLEINFO p ON p.PART_TABLE_ID=t.ID JOIN SYS.SYSOBJECTS s ON s.ID=t.SCHID WHERE s.TYPE$='SCH' AND s.NAME=? AND i.SUBTYPE$ IN ('INDEX','CNTIND') ORDER BY i.ID`, r.schema)
	if err != nil {
		return fmt.Errorf("无法确认分区派生索引的原生父对象: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var indexID, partitionID, parentID int64
		if err := rows.Scan(&indexID, &partitionID, &parentID); err != nil {
			return err
		}
		index, indexKnown := r.raw[indexID]
		partition, partitionKnown := r.raw[partitionID]
		parent, parentKnown := r.raw[parentID]
		if !indexKnown || !partitionKnown || !parentKnown || (index.sub != "INDEX" && index.sub != "CNTIND") || index.pid != partitionID || partition.sub != "UTAB" || partition.pid != parentID || parent.sub != "UTAB" {
			return fmt.Errorf("分区派生索引 %d 的目录父链不完整或不一致", indexID)
		}
		owner := r.base(partitionID)
		if owner == nil {
			return fmt.Errorf("分区派生索引 %d 缺少已盘点的物理父对象", indexID)
		}
		for _, position := range r.byRaw[indexID] {
			o := &r.snapshot.Objects[position]
			o.Managed, o.Generated = true, true
			o.ParentID, o.Table = owner.ID, owner.Name
			o.Dependencies = appendUnique(o.Dependencies, owner.ID)
		}
	}
	return rows.Err()
}
