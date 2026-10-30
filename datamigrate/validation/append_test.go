package validation

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppendValidationPreservesExistingRowsAndDuplicateMultiplicity(t *testing.T) {
	ctx := context.Background()
	for _, change := range []string{"none", "old_value", "missing_duplicate", "extra_duplicate"} {
		t.Run(change, func(t *testing.T) {
			src, dst, plan := keyedFixtures()
			src.meta.PrimaryKey, dst.meta.PrimaryKey, plan.PrimaryKey = nil, nil, nil
			src.rows = [][]any{{"1", "same"}, {"1", "same"}, {"2", "new"}}
			// Destination metadata and rows have reversed native column order.
			dst.rows = [][]any{{"old", "9"}, {"same", "1"}}
			opt := testOptions(t)
			baseline, err := captureAppendBaseline(ctx, dst, plan, opt)
			require.NoError(t, err)
			require.EqualValues(t, 2, baseline.Rows())
			t.Cleanup(func() { require.NoError(t, baseline.Close()) })
			dst.rows = append(dst.rows, []any{"same", "1"}, []any{"same", "1"}, []any{"new", "2"})
			switch change {
			case "old_value":
				dst.rows[0][0] = "changed-old-value"
			case "missing_duplicate":
				dst.rows = dst.rows[1:]
				dst.rows[0] = []any{"old", "9"}
			case "extra_duplicate":
				dst.rows = append(dst.rows, []any{"same", "1"})
			}
			result := validateAppend(ctx, src, dst, plan, baseline, opt)
			require.Empty(t, result.Error)
			require.EqualValues(t, 3, result.SourceRows)
			if change == "none" {
				require.Equal(t, StatusPassed, result.Status)
				require.EqualValues(t, 3, result.TargetRows)
			} else {
				require.Equal(t, StatusMismatch, result.Status)
				require.NotEmpty(t, result.Samples)
			}
			entries, err := os.ReadDir(opt.tempParent)
			require.NoError(t, err)
			require.Len(t, entries, 1, "only the live baseline remains after validation")
			require.NoError(t, baseline.Close())
			entries, err = os.ReadDir(opt.tempParent)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestAppendValidationRejectsChangedScopeAndReleasedBaseline(t *testing.T) {
	ctx := context.Background()
	src, dst, plan := keyedFixtures()
	dst.rows = [][]any{{"old", "9"}}
	baseline, err := captureAppendBaseline(ctx, dst, plan, testOptions(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = baseline.Close() })
	changed := plan
	changed.TargetTable = "different"
	require.Equal(t, StatusError, ValidateAppend(ctx, src, dst, changed, baseline).Status)
	dst.meta.PrimaryKey = nil
	require.Equal(t, StatusUnsupported, ValidateAppend(ctx, src, dst, plan, baseline).Status)
	require.NoError(t, baseline.Close())
	require.Equal(t, StatusError, ValidateAppend(ctx, src, dst, plan, baseline).Status)
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	require.Equal(t, StatusCancelled, ValidateAppend(ctx, src, dst, plan, baseline).Status)
}

func TestAppendBaselineCaptureErrorsCleanFiles(t *testing.T) {
	for _, failure := range []string{"read", "close", "drift", "unsupported"} {
		t.Run(failure, func(t *testing.T) {
			_, dst, plan := keyedFixtures()
			dst.rows = [][]any{{"old", "9"}}
			switch failure {
			case "read":
				dst.nextError = errors.New("scan interrupted")
			case "close":
				dst.closeError = errors.New("close failed")
			case "drift":
				dst.onDescribe = func(s *memoryScanner) {
					if s.describes == 2 {
						s.meta.PrimaryKey = nil
					}
				}
			case "unsupported":
				plan.SourceColumns[0].Kind = Unsupported
			}
			opt := testOptions(t)
			_, err := captureAppendBaseline(context.Background(), dst, plan, opt)
			require.Error(t, err)
			entries, err := os.ReadDir(opt.tempParent)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}
