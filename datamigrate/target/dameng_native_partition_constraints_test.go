package target

import (
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

func TestNativePartitionConstraintProofIgnoresCoincidentalGeneratedNames(t *testing.T) {
	expected := []damengnative.Object{
		{ID: "source-root", Kind: "table", Name: "Root"},
		{ID: "source-part", Kind: "table_partition", Name: "Root_P", Managed: true, ParentID: "source-root"},
		{ID: "source-check", Kind: "constraint", Name: "CONS134218100", Table: "Root_P", ParentID: "source-part", Managed: true, Generated: true, ConstraintType: "C", ConstraintNoValidate: true, Status: "ENABLED"},
	}
	actual := []damengnative.Object{
		{ID: "target-root", Kind: "table", Name: "Root"},
		{ID: "target-part", Kind: "table_partition", Name: "Root_P", Managed: true, ParentID: "target-root"},
		{ID: "target-check", Kind: "constraint", Name: "CONS134218999", Table: "Root_P", ParentID: "target-part", Managed: true, Generated: true, ConstraintType: "C", ConstraintNoValidate: false, Status: "ENABLED"},
		{ID: "unrelated", Kind: "constraint", Name: "CONS134218100", Table: "Other_P", ParentID: "other-part", Managed: true, Generated: true, ConstraintType: "C", ConstraintNoValidate: false},
	}
	expectedByID, actualByID, actualByName := partitionConstraintProofMaps(expected, actual)
	handled, err := nativePartitionConstraintProof(expected[2], expectedByID, actualByID, actualByName)
	require.True(t, handled)
	require.NoError(t, err, "native child names and validation flags are engine-derived; the proven owner and root semantics determine identity")
}

func TestNativePartitionConstraintProofRequiresCorrectAncestryAndMultiplicity(t *testing.T) {
	for _, variant := range []string{"missing_child", "extra_child", "wrong_root", "missing_root", "wrong_table_field"} {
		t.Run(variant, func(t *testing.T) {
			expected := []damengnative.Object{
				{ID: "source-root", Kind: "table", Name: "Root"},
				{ID: "source-part", Kind: "table_partition", Name: "Root_P", Managed: true, ParentID: "source-root"},
				{ID: "source-pk", Kind: "constraint", Name: "PK_1_999", Table: "Root_P", ParentID: "source-part", Managed: true, Generated: true, ConstraintType: "P"},
			}
			actual := []damengnative.Object{
				{ID: "target-root", Kind: "table", Name: "Root"},
				{ID: "target-part", Kind: "table_partition", Name: "Root_P", Managed: true, ParentID: "target-root"},
				{ID: "target-pk", Kind: "constraint", Name: "OtherGeneratedPK", Table: "Root_P", ParentID: "target-part", Managed: true, Generated: true, ConstraintType: "P"},
			}
			switch variant {
			case "missing_child":
				actual = actual[:2]
			case "extra_child":
				actual = append(actual, damengnative.Object{ID: "extra", Kind: "constraint", Name: "Extra", Table: "Root_P", ParentID: "target-part", Managed: true, Generated: true, ConstraintType: "P"})
			case "wrong_root":
				actual[0].Name = "Other"
			case "missing_root":
				actual[1].ParentID = "absent"
			case "wrong_table_field":
				actual[2].Table = "Other_P"
			}
			expectedByID, actualByID, actualByName := partitionConstraintProofMaps(expected, actual)
			handled, err := nativePartitionConstraintProof(expected[2], expectedByID, actualByID, actualByName)
			require.True(t, handled)
			require.Error(t, err)
		})
	}
}

func TestNativeLogicalConstraintStillUsesStrictIndependentVerification(t *testing.T) {
	root := damengnative.Object{ID: "root", Kind: "table", Name: "Root"}
	constraint := damengnative.Object{ID: "constraint", Kind: "constraint", Name: "CONS134218100", ParentID: "root", Table: "Root", Managed: true, Generated: true, ConstraintType: "C", ConstraintNoValidate: true}
	handled, err := nativePartitionConstraintProof(constraint, map[string]damengnative.Object{"root": root}, nil, nil)
	require.NoError(t, err)
	require.False(t, handled, "logical constraint states must not be relaxed by physical child handling")
}

func partitionConstraintProofMaps(expected, actual []damengnative.Object) (map[string]damengnative.Object, map[string]damengnative.Object, map[string]damengnative.Object) {
	expectedByID, actualByID, actualByName := map[string]damengnative.Object{}, map[string]damengnative.Object{}, map[string]damengnative.Object{}
	for _, object := range expected {
		expectedByID[object.ID] = object
	}
	for _, object := range actual {
		actualByID[object.ID] = object
		actualByName[object.Kind+"\x00"+object.Name] = object
	}
	return expectedByID, actualByID, actualByName
}

func TestNativeLogicalGeneratedConstraintIgnoresNamesButKeepsExactStateCounts(t *testing.T) {
	for _, variant := range []string{"correct", "wrong_validate", "wrong_enabled", "missing_duplicate", "same_name_other_owner"} {
		t.Run(variant, func(t *testing.T) {
			expected := []damengnative.Object{
				{ID: "source-root", Kind: "table", Name: "Root"},
				{ID: "source-check", Kind: "constraint", Name: "CONS134218100", Table: "Root", ParentID: "source-root", Managed: true, Generated: true, ConstraintType: "R", Status: "ENABLED", ConstraintNoValidate: true},
			}
			actual := []damengnative.Object{
				{ID: "target-root", Kind: "table", Name: "Root"},
				{ID: "target-check", Kind: "constraint", Name: "CONS134218999", Table: "Root", ParentID: "target-root", Managed: true, Generated: true, ConstraintType: "R", Status: "ENABLED", ConstraintNoValidate: true},
				{ID: "coincidental-name", Kind: "constraint", Name: "CONS134218100", Table: "Other_P", ParentID: "other-part", Managed: true, Generated: true, ConstraintType: "C", Status: "ENABLED", ConstraintNoValidate: false},
			}
			switch variant {
			case "wrong_validate":
				actual[1].ConstraintNoValidate = false
			case "wrong_enabled":
				actual[1].Status = "DISABLED"
			case "missing_duplicate":
				expected = append(expected, damengnative.Object{ID: "second", Kind: "constraint", Name: "CONS134218101", Table: "Root", ParentID: "source-root", Managed: true, Generated: true, ConstraintType: "R", Status: "ENABLED", ConstraintNoValidate: true})
			case "same_name_other_owner":
				actual = append(actual[:1], actual[2])
			}
			expectedByID, actualByID, actualByName := partitionConstraintProofMaps(expected, actual)
			handled, err := nativeLogicalGeneratedConstraintProof(expected[1], expectedByID, actualByID, actualByName)
			require.True(t, handled)
			if variant == "correct" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
