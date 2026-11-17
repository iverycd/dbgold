package datamigrate

import (
	"context"
	"testing"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

func TestSQLServerAppendContentValidatesIncomingRowsAndKeepsOldRows(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "existing row preserved"
		if changed {
			name = "existing row changed"
		}
		t.Run(name, func(t *testing.T) {
			r, w, m := contentFixture()
			m.cfg.TargetDBType, m.cfg.Content = "sqlserver", "data_only"
			w.rows["users"] = [][]any{{int64(99), "old"}}
			ctx := context.Background()
			m.freezeDataValidation(ctx, []string{"Users"})
			defer m.closeValidationBaselines()
			require.NoError(t, m.sqlServerAppendValidationError("Users", 1))
			require.ErrorContains(t, m.sqlServerAppendValidationError("Users", 2), "冻结内容基线后行数已变化")
			w.rows["users"] = append(w.rows["users"], r.rows["Users"]...)
			if changed {
				w.rows["users"][0][1] = "corrupted"
			}
			m.markDataValidationCopied("Users")
			report := MigrationReport{DataValidation: &validation.Report{}}
			m.validateDataContent(ctx, &report)
			result := report.DataValidation.Tables[0]
			want := validation.StatusPassed
			if changed {
				want = validation.StatusMismatch
			}
			require.Equal(t, want, result.Status, result.Error)
			require.EqualValues(t, 1, result.SourceRows)
			require.EqualValues(t, 1, result.TargetRows)
		})
	}
}

func TestSQLServerAppendValidationStopsOnUnavailableBaseline(t *testing.T) {
	_, w, m := contentFixture()
	m.cfg.TargetDBType, m.cfg.Content = "sqlserver", "data_only"
	w.metadata.Columns[1].Kind = validation.Unsupported
	m.freezeDataValidation(context.Background(), []string{"Users"})
	defer m.closeValidationBaselines()
	require.ErrorContains(t, m.sqlServerAppendValidationError("Users", 0), "追加前原值基线冻结失败")
	require.ErrorContains(t, m.sqlServerAppendValidationError("Unknown", 0), "追加前原值基线缺失")
}

func TestSQLServerAppendValidationReusesEmptyPartitionBaseline(t *testing.T) {
	_, w, m := contentFixture()
	m.cfg.TargetDBType, m.cfg.Content = "sqlserver", "data_only"
	m.partitionState = &partitionRunState{tables: map[string]*PartitionTableResult{
		"Users": {Plan: &partition.Plan{}, Baseline: map[string]int64{"First": 0}},
	}}
	m.freezeDataValidation(context.Background(), []string{"Users"})
	defer m.closeValidationBaselines()
	require.Zero(t, w.scans)
	require.NoError(t, m.sqlServerAppendValidationError("Users", 0))
	require.ErrorContains(t, m.sqlServerAppendValidationError("Users", 1), "冻结内容基线后行数已变化")
}
