package datamigrate

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecoveryConflictingDataStateStillReloadsRegardlessOfRowCounts(t *testing.T) {
	for _, targetRows := range []int64{5, 2} {
		t.Run(fmt.Sprintf("target_rows_%d", targetRows), func(t *testing.T) {
			r, w, cfg, object, ops, truncated := recoveryFixture(t)
			object.Status = "succeeded"
			// The handler conservatively resolves a successful table with an
			// unfinished data member to unknown before both recovery previews.
			object.DataStatus = "unknown"
			w.count = targetRows

			preview, err := previewRecovery(context.Background(), r, w, cfg, object, nil, nil, ops)
			require.NoError(t, err)
			require.True(t, preview.DataRequired)
			require.Equal(t, int64(5), preview.SourceRows)
			require.Equal(t, targetRows, preview.TargetRows)

			result, err := runRecovery(context.Background(), r, w, cfg, preview, true, nil, ops)
			require.NoError(t, err)
			require.Equal(t, "succeeded", result.DataStatus)
			require.Equal(t, int64(5), result.CommittedRows)
			require.Equal(t, int64(5), w.count)
			require.Equal(t, 1, *truncated)
			require.Equal(t, 3, w.writes, "matching row counts must not skip the data import")
		})
	}
}
