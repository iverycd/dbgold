package target

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnative"
)

// Physical partition constraints are restored by the logical parent. Their
// native names and validation flags depend on creation order, so a coincidental
// same-name object in a different partition cannot prove (or disprove) them.
// Verify the exact partition ancestry and the complete constraint-type
// multiplicity instead; the root table and its logical constraints separately
// retain their strict definition/state verification.
func nativePartitionConstraintProof(want damengnative.Object, expectedByID, actualByID, actualByName map[string]damengnative.Object) (bool, error) {
	if want.Kind != "constraint" || !want.Managed || !want.Generated {
		return false, nil
	}
	owner, known := expectedByID[want.ParentID]
	if !known || owner.Kind != "table_partition" || !owner.Managed {
		return false, nil
	}
	if want.Table != owner.Name {
		return true, fmt.Errorf("分区派生约束的冻结表归属与物理父对象不同")
	}
	targetOwner, present := actualByName[owner.Kind+"\x00"+owner.Name]
	if !present {
		return true, fmt.Errorf("目标缺少派生约束所属的物理分区 %s", owner.Name)
	}
	left, right := owner, targetOwner
	seenLeft, seenRight := map[string]bool{}, map[string]bool{}
	for {
		if left.Kind != right.Kind || left.Name != right.Name || strings.EqualFold(right.Status, "INVALID") || strings.EqualFold(right.Status, "ERROR") {
			return true, fmt.Errorf("目标派生约束的物理分区父链与源端不同")
		}
		if left.Kind == "table" && !left.Managed {
			break
		}
		if left.Kind != "table_partition" || right.Kind != "table_partition" || !left.Managed || !right.Managed || left.ParentID == "" || right.ParentID == "" || seenLeft[left.ID] || seenRight[right.ID] {
			return true, fmt.Errorf("派生约束缺少可确认的逻辑基表父链")
		}
		seenLeft[left.ID], seenRight[right.ID] = true, true
		var leftKnown, rightKnown bool
		left, leftKnown = expectedByID[left.ParentID]
		right, rightKnown = actualByID[right.ParentID]
		if !leftKnown || !rightKnown {
			return true, fmt.Errorf("派生约束的逻辑基表父对象缺失")
		}
	}
	sourceCount, targetCount := 0, 0
	for _, candidate := range expectedByID {
		if candidate.Kind == "constraint" && candidate.Managed && candidate.Generated && candidate.ParentID == owner.ID && candidate.Table == owner.Name && candidate.ConstraintType == want.ConstraintType {
			sourceCount++
		}
	}
	for _, candidate := range actualByID {
		if candidate.Kind == "constraint" && candidate.Managed && candidate.Generated && candidate.ParentID == targetOwner.ID && candidate.Table == targetOwner.Name && candidate.ConstraintType == want.ConstraintType {
			targetCount++
		}
	}
	if sourceCount != targetCount {
		return true, fmt.Errorf("目标物理分区 %s 的派生约束 %s 数量不同: 源=%d 目标=%d", owner.Name, want.ConstraintType, sourceCount, targetCount)
	}
	return true, nil
}

// Logical anonymous constraints also have database-local generated names.
// Preserve their exact state family under the actual owning table; that table's
// independently checked native units still prove every condition/reference.
func nativeLogicalGeneratedConstraintProof(want damengnative.Object, expectedByID, actualByID, actualByName map[string]damengnative.Object) (bool, error) {
	if want.Kind != "constraint" || !want.Managed || !want.Generated {
		return false, nil
	}
	owner, known := expectedByID[want.ParentID]
	if !known || owner.Kind != "table" || owner.Managed {
		return false, nil
	}
	if want.Table != owner.Name {
		return true, fmt.Errorf("匿名约束的冻结表归属与父对象不同")
	}
	targetOwner, present := actualByName[owner.Kind+"\x00"+owner.Name]
	if !present || targetOwner.Managed || strings.EqualFold(targetOwner.Status, "INVALID") || strings.EqualFold(targetOwner.Status, "ERROR") {
		return true, fmt.Errorf("目标匿名约束缺少可确认的实际逻辑父表 %s", owner.Name)
	}
	sourceCount, targetCount := 0, 0
	matches := func(candidate damengnative.Object, parent damengnative.Object) bool {
		return candidate.Kind == "constraint" && candidate.Managed && candidate.Generated && candidate.ParentID == parent.ID && candidate.Table == parent.Name && candidate.ConstraintType == want.ConstraintType && candidate.Status == want.Status && candidate.ConstraintNoValidate == want.ConstraintNoValidate
	}
	for _, candidate := range expectedByID {
		if matches(candidate, owner) {
			sourceCount++
		}
	}
	for _, candidate := range actualByID {
		if matches(candidate, targetOwner) {
			targetCount++
		}
	}
	if sourceCount != targetCount {
		return true, fmt.Errorf("目标表 %s 的匿名约束类型/启停/VALIDATE状态数量不同: 源=%d 目标=%d", owner.Name, sourceCount, targetCount)
	}
	return true, nil
}
