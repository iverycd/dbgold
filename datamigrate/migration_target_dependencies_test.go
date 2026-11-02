package datamigrate

import (
	"context"
	"dbgold/datamigrate/target"
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
)

type migrationDependencyWriter struct {
	*mockWriter
	snapshot target.MigrationDependencies
	err      error
	calls    int
}

func (w *migrationDependencyWriter) InspectMigrationDependencies(context.Context) (target.MigrationDependencies, error) {
	w.calls++
	return w.snapshot, w.err
}
func TestRetainedTargetDependencyProtectionUsesTransitiveClosure(t *testing.T) {
	w := &migrationDependencyWriter{mockWriter: &mockWriter{}, snapshot: target.MigrationDependencies{Schema: "App", Relations: []target.MigrationDependency{{Schema: "App", Table: "Bad", Name: "BP", RefSchema: "App", RefTable: "Parent"}, {Schema: "App", Table: "Parent", Name: "PG", RefSchema: "App", RefTable: "Grandparent"}, {Schema: "Other", Table: "External", Name: "EX", RefSchema: "App", RefTable: "ExternalParent"}}}}
	m, _ := newTestMigrator(&mockReader{}, w)
	m.blockMigrationTable("Bad", "preflight", errors.New("invalid type"))
	report := newMigrationReport()
	require.True(t, m.protectRetainedMigrationTables(context.Background(), []string{"Bad", "Parent", "Grandparent", "ExternalParent", "Healthy"}, &report))
	for _, name := range []string{"Bad", "Parent", "Grandparent", "ExternalParent"} {
		require.True(t, m.migrationTableBlocked(name), name)
	}
	require.False(t, m.migrationTableBlocked("Healthy"))
	require.Equal(t, 1, w.calls)
}
func TestRetainedTargetDependencyChecksOnlyRunForPartialSchemaTasks(t *testing.T) {
	for _, mode := range []string{"healthy", "all_blocked", "data_only"} {
		t.Run(mode, func(t *testing.T) {
			w := &migrationDependencyWriter{mockWriter: &mockWriter{}, err: errors.New("must not read")}
			m, _ := newTestMigrator(&mockReader{}, w)
			if mode != "healthy" {
				m.blockMigrationTable("Bad", "preflight", errors.New("invalid type"))
			}
			tables := []string{"Bad", "Healthy"}
			if mode == "all_blocked" {
				tables = []string{"Bad"}
			}
			if mode == "data_only" {
				m.cfg.Content = "data_only"
			}
			got := m.protectRetainedMigrationTables(context.Background(), tables, nil)
			require.True(t, got)
			require.Zero(t, w.calls)
		})
	}
}
func TestRetainedTargetDependencyReadFailureStopsBeforeDDL(t *testing.T) {
	w := &migrationDependencyWriter{mockWriter: &mockWriter{}, err: errors.New("permission denied for target catalog")}
	m, _ := newTestMigrator(&mockReader{}, w)
	m.blockMigrationTable("Bad", "preflight", errors.New("invalid type"))
	report := newMigrationReport()
	require.False(t, m.protectRetainedMigrationTables(context.Background(), []string{"Bad", "Healthy"}, &report))
	require.Len(t, report.Tables.Items, 1)
	require.Equal(t, "task", report.Tables.Items[0].Stage)
	require.Empty(t, w.created)
}

func TestRetainedPartitionFailureProtectsParentWithoutDoubleCounting(t *testing.T) {
	w := &migrationDependencyWriter{mockWriter: &mockWriter{}, snapshot: target.MigrationDependencies{Schema: "App", Relations: []target.MigrationDependency{{Schema: "App", Table: "BadPartition", Name: "BP", RefSchema: "App", RefTable: "Parent"}}}}
	m, _ := newTestMigrator(&mockReader{}, w)
	bad := &PartitionTableResult{SourceTable: "BadPartition", TargetTable: "BadPartition", Status: "blocked", Error: "partition unsupported"}
	m.partitionState = &partitionRunState{blocked: map[string]bool{"BadPartition": true}, tables: map[string]*PartitionTableResult{"BadPartition": bad}, preflight: &PartitionPreflight{Tables: []*PartitionTableResult{bad}}}
	report := newMigrationReport()
	report.Tables.Total = 2
	require.True(t, m.protectRetainedMigrationTables(context.Background(), []string{"Parent", "Healthy"}, &report))
	require.True(t, m.migrationTableBlocked("Parent"))
	require.False(t, m.migrationTableBlocked("Healthy"))
	require.False(t, m.migrationTableBlocked("BadPartition"))
	require.Equal(t, 1, report.Tables.Failed)
	require.Len(t, report.Tables.Items, 1)
	require.Equal(t, "Parent", report.Tables.Items[0].Name)
}
func TestRetainedTargetProtectionDoesNotReclassifyCreatedTables(t *testing.T) {
	w := &migrationDependencyWriter{mockWriter: &mockWriter{}, snapshot: target.MigrationDependencies{Schema: "App", Relations: []target.MigrationDependency{{Schema: "App", Table: "Bad", Name: "BP", RefSchema: "App", RefTable: "Created"}}}}
	m, _ := newTestMigrator(&mockReader{}, w)
	m.blockMigrationTable("Bad", "execute", errors.New("drop failed"))
	report := newMigrationReport()
	require.True(t, m.protectRetainedMigrationTables(context.Background(), []string{"Bad", "Created", "Healthy"}, &report, map[string]bool{"Created": true}))
	require.False(t, m.migrationTableBlocked("Created"))
	require.Empty(t, report.Tables.Items)
}

func TestRetainedTargetChecksSkipWhenOnlyCompletedTablesRemain(t *testing.T) {
	w := &migrationDependencyWriter{mockWriter: &mockWriter{}, err: errors.New("must not acquire unnecessary catalog reads")}
	m, _ := newTestMigrator(&mockReader{}, w)
	m.blockMigrationTable("Bad", "execute", errors.New("drop failed"))
	report := newMigrationReport()
	require.True(t, m.protectRetainedMigrationTables(context.Background(), []string{"Good", "Bad"}, &report, map[string]bool{"Good": true}))
	require.Zero(t, w.calls)
	require.Empty(t, report.Tables.Items)
}
