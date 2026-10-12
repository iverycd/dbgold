package damengnames

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlanNames(t *testing.T) {
	for original, target := range replacements {
		for _, src := range []string{original, strings.ToLower(original), strings.ToLower(original[:1]) + original[1:]} {
			got, err := Plan([]string{src, strings.ToLower(target), target + "_2", "普通列"})
			require.NoError(t, err)
			require.Equal(t, []string{target + "_3", target, target + "_2", "普通列"}, got)
		}
	}
	_, err := Plan([]string{"ROWID", "rowid"})
	require.ErrorContains(t, err, "目标列名冲突")
	got, err := Plan([]string{"rowguid", "row_id", "rowid", "trxid"})
	require.NoError(t, err)
	require.Equal(t, []string{"ROWGUID", "ROW_ID", "ROW_ID_2", "TRX_ID"}, got)
}
