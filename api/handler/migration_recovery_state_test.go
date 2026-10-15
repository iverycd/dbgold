package handler

import (
	"encoding/json"
	"testing"

	"dbgold/datamigrate/repair"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func recoveryStateTable() repair.Object {
	return repair.Object{
		ID: "table-orders", Kind: "table", Name: "ORDERS", SourceName: "ORDERS", SourceTable: "ORDERS",
		Table: "orders", TargetName: "orders", Schema: "public", Status: "succeeded", DataStatus: "succeeded",
		OriginalStatus: "succeeded", ColumnMapping: map[string]string{"ID": "id"},
		Units: []repair.Unit{{ID: "create", SQL: "CREATE TABLE orders (id integer)", Status: "succeeded"}},
	}
}

func recoveryStateData(status string) repair.Object {
	return repair.Object{
		ID: "data-orders", Kind: "data", Name: "ORDERS", SourceName: "ORDERS", SourceTable: "ORDERS",
		Table: "orders", TargetName: "orders", Schema: "public", Status: status, DataStatus: status,
		OriginalStatus: "failed", OriginalError: "numeric field overflow", Dependencies: []string{"table-orders"},
	}
}

func TestRecoveryObjectUsesUnresolvedGroupedDataStateWithoutMutatingRecords(t *testing.T) {
	for _, status := range []string{"failed", "unknown", "unverified", "needs_verification", "running", "pending", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			for _, dependencies := range [][]string{{"table-orders"}, nil} {
				table, data := recoveryStateTable(), recoveryStateData(status)
				data.Dependencies = dependencies
				objects := []repair.Object{data, table}
				before := repairJSON(objects)
				got := recoveryObjectWithDataState(table, objects)
				expected := table
				expected.DataStatus = "unknown"
				require.Equal(t, expected, got, "a successful table must not suppress unfinished grouped data")
				require.Equal(t, before, repairJSON(objects), "deriving the preview input must not rewrite original errors, metadata or statuses")
				require.Equal(t, "succeeded", table.DataStatus)
			}
		})
	}
	// Legacy data records may have no separate data_status field.
	table, data := recoveryStateTable(), recoveryStateData("failed")
	data.DataStatus = ""
	require.Equal(t, "unknown", recoveryObjectWithDataState(table, []repair.Object{table, data}).DataStatus)
}

func TestRecoveryObjectPreservesCompletedDataAndOriginalPrimaryIdentity(t *testing.T) {
	for _, status := range []string{"succeeded", "success", "repaired", "not_required"} {
		t.Run(status, func(t *testing.T) {
			table, data := recoveryStateTable(), recoveryStateData(status)
			require.Equal(t, table, recoveryObjectWithDataState(table, []repair.Object{table, data}))
		})
	}
	for _, status := range []string{"failed", "unknown", "pending", "not_required", ""} {
		t.Run("primary-"+status, func(t *testing.T) {
			table := recoveryStateTable()
			table.DataStatus = status
			require.Equal(t, table, recoveryObjectWithDataState(table, []repair.Object{table, recoveryStateData("failed")}))
		})
	}
	data := recoveryStateData("failed")
	require.Equal(t, data, recoveryObjectWithDataState(data, []repair.Object{recoveryStateTable(), data}), "standalone data recovery keeps its own original identity and state")
}

func TestRecoveryObjectDoesNotUseUnrelatedOrAmbiguousData(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*repair.Object, *repair.Object) []repair.Object
	}{
		{"different schema", func(_ *repair.Object, data *repair.Object) []repair.Object { data.Schema = "archive"; return nil }},
		{"target case", func(_ *repair.Object, data *repair.Object) []repair.Object {
			data.Table, data.TargetName = "Orders", "Orders"
			return nil
		}},
		{"source case", func(_ *repair.Object, data *repair.Object) []repair.Object {
			data.SourceTable, data.SourceName = "Orders", "Orders"
			return nil
		}},
		{"source identity", func(_ *repair.Object, data *repair.Object) []repair.Object {
			data.SourceTable, data.SourceName = "ARCHIVE", "ARCHIVE"
			return nil
		}},
		{"inconsistent target fields", func(_ *repair.Object, data *repair.Object) []repair.Object { data.Table = "archive"; return nil }},
		{"inconsistent source fields", func(_ *repair.Object, data *repair.Object) []repair.Object { data.SourceTable = "ARCHIVE"; return nil }},
		{"diagnostic data", func(_ *repair.Object, data *repair.Object) []repair.Object { data.DiagnosticOnly = true; return nil }},
		{"diagnostic table", func(table *repair.Object, _ *repair.Object) []repair.Object { table.DiagnosticOnly = true; return nil }},
		{"missing dependency", func(_ *repair.Object, data *repair.Object) []repair.Object {
			data.Dependencies = []string{"missing-table"}
			return nil
		}},
		{"mixed missing dependency", func(_ *repair.Object, data *repair.Object) []repair.Object {
			data.Dependencies = []string{"table-orders", "missing-table"}
			return nil
		}},
		{"non-table dependency", func(_ *repair.Object, data *repair.Object) []repair.Object {
			data.Dependencies = []string{"index-orders"}
			return []repair.Object{{ID: "index-orders", Kind: "index"}}
		}},
		{"ambiguous target", func(table *repair.Object, data *repair.Object) []repair.Object {
			data.Dependencies = nil
			other := *table
			other.ID = "table-duplicate"
			return []repair.Object{other}
		}},
		{"multiple table dependencies", func(table *repair.Object, data *repair.Object) []repair.Object {
			data.Dependencies = []string{table.ID, "table-duplicate"}
			other := *table
			other.ID = "table-duplicate"
			return []repair.Object{other}
		}},
		{"conflicting group sources with missing table source", func(table *repair.Object, data *repair.Object) []repair.Object {
			table.SourceName, table.SourceTable = "", ""
			other := *data
			other.ID, other.SourceTable, other.SourceName = "data-archive", "ARCHIVE", "ARCHIVE"
			return []repair.Object{other}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table, data := recoveryStateTable(), recoveryStateData("failed")
			extra := tt.mutate(&table, &data)
			objects := append([]repair.Object{table, data}, extra...)
			before := repairJSON(objects)
			require.Equal(t, table, recoveryObjectWithDataState(table, objects))
			require.Equal(t, before, repairJSON(objects))
		})
	}
}

func TestRecoveryObjectExplicitDependencyResolvesDuplicateTarget(t *testing.T) {
	table, data := recoveryStateTable(), recoveryStateData("failed")
	other := table
	other.ID = "table-duplicate"
	require.Equal(t, "unknown", recoveryObjectWithDataState(table, []repair.Object{other, data, table}).DataStatus)
	require.Equal(t, other, recoveryObjectWithDataState(other, []repair.Object{other, data, table}))
}

func TestRecoveryPreviewObjectRereadsGroupedDataWithoutWritingLedger(t *testing.T) {
	db := repairPersistenceDB(t)
	require.NoError(t, db.AutoMigrate(&store.DataMigrationReport{}))
	table, data := recoveryStateTable(), recoveryStateData("failed")
	rows := []store.MigrationRepairObject{
		{JobID: "recovery-state", ObjectID: table.ID, Version: 7, ObjectJSON: repairJSON(table)},
		{JobID: "recovery-state", ObjectID: data.ID, Version: 3, ObjectJSON: repairJSON(data)},
	}
	require.NoError(t, db.Create(&rows).Error)
	before, err := store.ListMigrationRepairObjects("recovery-state")
	require.NoError(t, err)
	for _, stage := range []string{"initial preview", "execution preflight"} {
		got, err := recoveryPreviewObject("recovery-state", table)
		require.NoError(t, err, stage)
		require.Equal(t, "unknown", got.DataStatus, stage)
		require.Equal(t, table.ID, got.ID, stage)
		require.Equal(t, table.Units, got.Units, stage)
	}
	after, err := store.ListMigrationRepairObjects("recovery-state")
	require.NoError(t, err)
	require.Equal(t, before, after, "both preview stages must preserve stored ID, version, timestamps and original JSON")

	// A later preview must inspect current member states rather than cache the
	// first aggregate or write its temporary unknown state onto the primary row.
	data.Status, data.DataStatus = "succeeded", "succeeded"
	require.NoError(t, db.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "recovery-state", data.ID).Update("object_json", repairJSON(data)).Error)
	got, err := recoveryPreviewObject("recovery-state", table)
	require.NoError(t, err)
	require.Equal(t, table, got)
	stored, err := store.GetMigrationRepairObject("recovery-state", table.ID)
	require.NoError(t, err)
	require.Equal(t, 7, stored.Version)
	var original repair.Object
	require.NoError(t, json.Unmarshal([]byte(stored.ObjectJSON), &original))
	require.Equal(t, table, original)
}
