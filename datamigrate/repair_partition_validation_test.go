package datamigrate

import (
	"context"
	"testing"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

func partitionRepairReport(object repair.Object) MigrationReport {
	report := newMigrationReport()
	report.RowCounts = []TableRowCount{{Table: object.Table, Src: 2, Dst: 2, Match: true}}
	report.PartitionMigration = &PartitionPreflight{Ready: true, CanStart: true, Tables: []*PartitionTableResult{{SourceTable: object.SourceTable, TargetTable: object.Table, Status: "succeeded", Plan: object.PartitionPlan}}}
	report.DataValidation = &validation.Report{Status: validation.StatusPassed, Tables: []validation.TableResult{{SourceTable: object.SourceTable, TargetTable: object.Table, Status: validation.StatusPassed}}}
	for _, mapping := range object.PartitionPlan.Mapping {
		report.DataValidation.Tables = append(report.DataValidation.Tables, validation.TableResult{
			SourceTable: object.SourceTable + " PARTITION " + partition.LeafKey(mapping.SourceSchema, mapping.SourceName),
			TargetTable: object.Table + " PARTITION " + partition.LeafKey(mapping.TargetSchema, mapping.TargetName),
			Status:      validation.StatusPassed,
		})
	}
	return report
}

func TestRepairPartitionVerificationFailureRequiresDataRecovery(t *testing.T) {
	for _, scope := range []string{"root", "leaf", "partition", "missing_leaf"} {
		statuses := []string{validation.StatusMismatch, validation.StatusError, validation.StatusCancelled, validation.StatusIncomplete}
		if scope == "partition" {
			statuses = []string{"failed", "incomplete", "blocked", "ready"}
		}
		if scope == "missing_leaf" {
			statuses = []string{"missing"}
		}
		for _, status := range statuses {
			t.Run(scope+"/"+status, func(t *testing.T) {
				r, w, cfg, object, ops, _ := partitionRecoveryFixture(t)
				w.counts["Root"] = map[string]int64{"Dst.Low": 1, "Dst.High": 1}
				object.Status, object.DataStatus = "succeeded", "pending"
				object.Units = []repair.Unit{{ID: "create", SQL: "CREATE TABLE Root (Id BIGINT)", Status: "succeeded"}}
				m := NewMigrator(r, w, &Job{LogCh: make(chan string, 128)}, cfg)
				m.repairs = &repairRecorder{objects: []repair.Object{object}}
				report := partitionRepairReport(object)
				switch scope {
				case "root":
					report.DataValidation.Tables[0].Status = status
				case "leaf":
					report.DataValidation.Tables[1].Status = status
				case "partition":
					report.PartitionMigration.Tables[0].Status = status
					report.PartitionMigration.Tables[0].Error = "partition evidence failed"
				case "missing_leaf":
					report.DataValidation.Tables = report.DataValidation.Tables[:1]
				}
				report.DataValidation.Finalize()
				m.finishRepair(context.Background(), &report)
				require.Len(t, report.RepairManifest.Objects, 1)
				saved := report.RepairManifest.Objects[0]
				require.Equal(t, "failed", saved.DataStatus, "matching row counts must not replace verification")
				require.Contains(t, saved.Error, "数据核验未通过")
				require.Equal(t, saved.Error, saved.OriginalError)
				require.Equal(t, "succeeded", saved.Status, "completed DDL must remain available to the data recovery entrypoint")
				preview, err := previewRecovery(context.Background(), r, w, cfg, saved, nil, nil, ops)
				require.NoError(t, err)
				require.True(t, preview.DataRequired)
			})
		}
	}
}

func TestRepairPartitionValidationKeepsHealthyAndSchemaOnlyStates(t *testing.T) {
	for _, schemaOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "schema_only"}[schemaOnly], func(t *testing.T) {
			r, w, cfg, object, ops, _ := partitionRecoveryFixture(t)
			w.counts["Root"] = map[string]int64{"Dst.Low": 1, "Dst.High": 1}
			object.Status = "succeeded"
			object.Units = []repair.Unit{{ID: "create", SQL: "CREATE TABLE Root (Id BIGINT)", Status: "succeeded"}}
			report := partitionRepairReport(object)
			if schemaOnly {
				cfg.Content, object.DataStatus = "schema_only", "not_required"
				report.PartitionMigration.Tables[0].Status = "failed"
				report.DataValidation.Tables[0].Status = validation.StatusCancelled
			}
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 128)}, cfg)
			m.repairs = &repairRecorder{objects: []repair.Object{object}}
			m.finishRepair(context.Background(), &report)
			saved := report.RepairManifest.Objects[0]
			if schemaOnly {
				require.Equal(t, "not_required", saved.DataStatus)
			} else {
				require.Equal(t, "succeeded", saved.DataStatus)
			}
			require.Empty(t, saved.Error)
			preview, err := previewRecovery(context.Background(), r, w, cfg, saved, nil, nil, ops)
			require.NoError(t, err)
			require.False(t, preview.DataRequired)
		})
	}
}

func TestRepairPartitionValidationDoesNotMatchTableNamePrefixes(t *testing.T) {
	r, w, cfg, failed, _, _ := partitionRecoveryFixture(t)
	failed.Status, failed.DataStatus = "succeeded", "pending"
	failed.Units = []repair.Unit{{ID: "create", SQL: "CREATE TABLE Root (Id BIGINT)", Status: "succeeded"}}
	healthy := failed
	healthy.ID, healthy.SourceTable, healthy.SourceName, healthy.Table, healthy.TargetName = "healthy", "RootExtra", "RootExtra", "RootExtra", "RootExtra"
	healthy.PartitionPlan = nil
	report := partitionRepairReport(failed)
	report.DataValidation.Tables[1].Status = validation.StatusMismatch
	report.DataValidation.Tables = append(report.DataValidation.Tables, validation.TableResult{SourceTable: "RootExtra", TargetTable: "RootExtra", Status: validation.StatusPassed})
	report.RowCounts = append(report.RowCounts, TableRowCount{Table: "RootExtra", Src: 2, Dst: 2, Match: true})
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 128)}, cfg)
	m.repairs = &repairRecorder{objects: []repair.Object{failed, healthy}}
	m.finishRepair(context.Background(), &report)
	require.Equal(t, "failed", report.RepairManifest.Objects[0].DataStatus)
	require.Equal(t, "succeeded", report.RepairManifest.Objects[1].DataStatus)
	require.Empty(t, report.RepairManifest.Objects[1].Error)
}
