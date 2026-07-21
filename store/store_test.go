package store

import (
	"dbgold/config"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDB(t *testing.T) {
	t.Helper()
	cfg := &config.Config{SQLitePath: ":memory:"}
	Init(cfg)
}

func TestCreateAndGetUser(t *testing.T) {
	setupTestDB(t)
	u, err := CreateUser("alice", "password123", "user")
	require.NoError(t, err)
	assert.Equal(t, "alice", u.Username)
	assert.Equal(t, "user", u.Role)

	got, err := GetUserByUsername("alice")
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.ID)
}

func TestIncrementalFailureSummaryColumnsAutoMigrate(t *testing.T) {
	setupTestDB(t)
	require.True(t, DB.Migrator().HasColumn(&IncrementalMigrationJob{}, "failed_object_count"))
	require.True(t, DB.Migrator().HasColumn(&IncrementalMigrationJob{}, "failed_ddl_count"))
	require.True(t, DB.Migrator().HasColumn(&IncrementalMigrationJob{}, "src_conn_name"))
	require.True(t, DB.Migrator().HasColumn(&IncrementalMigrationJob{}, "dst_conn_name"))
}

func TestEnsureAdminExists(t *testing.T) {
	setupTestDB(t)
	err := EnsureAdminExists("admin", "Admin@123")
	require.NoError(t, err)

	err = EnsureAdminExists("admin2", "Pass@456")
	require.NoError(t, err)

	users, _ := ListUsers()
	var admins int
	for _, u := range users {
		if u.Role == "admin" {
			admins++
		}
	}
	assert.Equal(t, 1, admins)
}

func TestConnectionCRUD(t *testing.T) {
	setupTestDB(t)
	c := &Connection{
		Name: "test-mysql", DBType: "mysql",
		Host: "localhost", Port: 3306,
		Database: "testdb", Username: "root", Password: "pass",
	}
	require.NoError(t, CreateConnection(c))
	assert.NotZero(t, c.ID)

	list, err := ListConnections(0, true)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	require.NoError(t, DeleteConnection(c.ID))
	list, _ = ListConnections(0, true)
	assert.Len(t, list, 0)
}

func TestPauseInterruptedIncrementalJobs(t *testing.T) {
	setupTestDB(t)
	running := &IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-running", SrcConnID: 1, DstConnID: 2, Status: "running", Phase: "running", LocatorStrategyVersion: 1}
	cuttingOver := &IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-cutover", SrcConnID: 1, DstConnID: 2, Status: "cutting_over", Phase: "cutting_over", LocatorStrategyVersion: 1}
	validating := &IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-validating", SrcConnID: 1, DstConnID: 2, Status: "validating", Phase: "validating", LocatorStrategyVersion: 1}
	stopped := &IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-stopped", SrcConnID: 1, DstConnID: 2, Status: "stopped", Phase: "stopped"}
	require.NoError(t, CreateIncrementalJob(running))
	require.NoError(t, CreateIncrementalJob(cuttingOver))
	require.NoError(t, CreateIncrementalJob(validating))
	require.NoError(t, CreateIncrementalJob(stopped))
	require.NoError(t, PauseInterruptedIncrementalJobs())
	got, err := GetIncrementalJob("cdc-running")
	require.NoError(t, err)
	assert.Equal(t, "paused_restart", got.Status)
	for _, id := range []string{"cdc-cutover", "cdc-validating"} {
		interrupted, getErr := GetIncrementalJob(id)
		require.NoError(t, getErr)
		assert.Equal(t, "paused_restart", interrupted.Status)
	}
	untouched, err := GetIncrementalJob("cdc-stopped")
	require.NoError(t, err)
	assert.Equal(t, "stopped", untouched.Status)
}

func TestDiscardLegacyIncrementalJobs(t *testing.T) {
	setupTestDB(t)
	legacy := &IncrementalMigrationJob{OwnerID: 1, JobID: "legacy", Status: "paused_manual", LocatorStrategyVersion: 0}
	current := &IncrementalMigrationJob{OwnerID: 1, JobID: "current", Status: "paused_manual", LocatorStrategyVersion: 1}
	terminal := &IncrementalMigrationJob{OwnerID: 1, JobID: "legacy-stopped", Status: "stopped", LocatorStrategyVersion: 0}
	for _, job := range []*IncrementalMigrationJob{legacy, current, terminal} {
		require.NoError(t, CreateIncrementalJob(job))
	}
	require.NoError(t, DiscardLegacyIncrementalJobs())
	got, _ := GetIncrementalJob("legacy")
	assert.Equal(t, "aborted", got.Status)
	assert.NotNil(t, got.FinishedAt)
	assert.Contains(t, got.Summary, "旧任务不能恢复")
	got, _ = GetIncrementalJob("current")
	assert.Equal(t, "paused_manual", got.Status)
	got, _ = GetIncrementalJob("legacy-stopped")
	assert.Equal(t, "stopped", got.Status)
}

func TestIncrementalOperationalFieldsPersist(t *testing.T) {
	setupTestDB(t)
	job := &IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-fields", SrcConnID: 1, DstConnID: 2, Status: "snapshot"}
	require.NoError(t, CreateIncrementalJob(job))
	require.NoError(t, UpdateIncrementalJob(job.JobID, map[string]any{
		"bootstrap_completed":      true,
		"start_gtid":               "sid:1-10",
		"checkpoint_gtid":          "sid:1-11",
		"source_head_gtid":         "sid:1-12",
		"cutover_gtid":             "sid:1-13",
		"blocking_gtid":            "sid:1-14",
		"checkpoint_position":      101,
		"source_head_position":     202,
		"cutover_position":         303,
		"blocking_position":        404,
		"bootstrap_failure_policy": "review_and_exclude",
		"bootstrap_state":          "review_pending",
		"pending_file":             "mysql-bin.000001",
		"pending_position":         88,
		"effective_table_count":    4,
		"excluded_table_count":     2,
		"bootstrap_manifest_hash":  "manifest",
		"effective_tables_json":    `["a","b","c","d"]`,
	}))
	got, err := GetIncrementalJob(job.JobID)
	require.NoError(t, err)
	assert.True(t, got.BootstrapDone)
	assert.Equal(t, "sid:1-10", got.StartGTID)
	assert.Equal(t, "sid:1-11", got.CheckpointGTID)
	assert.Equal(t, "sid:1-12", got.SourceHeadGTID)
	assert.Equal(t, "sid:1-13", got.CutoverGTID)
	assert.Equal(t, "sid:1-14", got.BlockingGTID)
	assert.Equal(t, uint32(101), got.CheckpointPos)
	assert.Equal(t, uint32(202), got.SourceHeadPos)
	assert.Equal(t, uint32(303), got.CutoverPos)
	assert.Equal(t, uint32(404), got.BlockingPos)
	assert.Equal(t, "review_and_exclude", got.BootstrapPolicy)
	assert.Equal(t, "review_pending", got.BootstrapState)
	assert.Equal(t, "mysql-bin.000001", got.PendingFile)
	assert.Equal(t, uint32(88), got.PendingPos)
	assert.Equal(t, 4, got.EffectiveCount)
	assert.Equal(t, 2, got.ExcludedCount)
	assert.Equal(t, "manifest", got.ManifestHash)
	updated, err := UpdateIncrementalJobIfStatus(job.JobID, []string{"running"}, map[string]any{"status": "validating"})
	require.NoError(t, err)
	assert.False(t, updated)
	updated, err = UpdateIncrementalJobIfStatus(job.JobID, []string{"snapshot"}, map[string]any{"status": "validating"})
	require.NoError(t, err)
	assert.True(t, updated)
}

func TestListIncrementalJobsWithConnectionSnapshotsAndLegacyFallback(t *testing.T) {
	setupTestDB(t)
	src := &Connection{OwnerID: 1, Name: "source-current", DBType: "mysql", Host: "10.0.0.1", Port: 3306,
		Database: "default_source", Username: "reader", Password: "secret"}
	dst := &Connection{OwnerID: 1, Name: "target-current", DBType: "postgres", Host: "10.0.0.2", Port: 5432,
		Database: "target_db", Username: "writer", Password: "secret"}
	require.NoError(t, CreateConnection(src))
	require.NoError(t, CreateConnection(dst))

	snapshot := &IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-snapshot", SrcConnID: src.ID, DstConnID: dst.ID,
		SrcDBType: "mysql", DstDBType: "postgres", SrcDatabase: "selected_source", TargetSchema: "app",
		SrcConnName: "source-at-start", SrcConnHost: "192.0.2.1", SrcConnPort: 3307, SrcConnDatabase: "selected_source", SrcConnUsername: "snapshot_reader",
		DstConnName: "target-at-start", DstConnHost: "192.0.2.2", DstConnPort: 5433, DstConnDatabase: "snapshot_target", DstConnUsername: "snapshot_writer",
		Status: "stopped"}
	legacy := &IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-legacy", SrcConnID: src.ID, DstConnID: dst.ID,
		SrcDatabase: "legacy_selected", TargetSchema: "legacy_schema", Status: "stopped"}
	otherOwner := &IncrementalMigrationJob{OwnerID: 2, JobID: "cdc-other-owner", SrcConnID: src.ID, DstConnID: dst.ID, Status: "stopped"}
	for _, job := range []*IncrementalMigrationJob{snapshot, legacy, otherOwner} {
		require.NoError(t, CreateIncrementalJob(job))
	}

	jobs, err := ListIncrementalJobsWithConn(1, false)
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	byID := make(map[string]IncrementalMigrationJobWithConn, len(jobs))
	for _, job := range jobs {
		byID[job.JobID] = job
	}
	require.NotNil(t, byID["cdc-snapshot"].SrcConn)
	assert.Equal(t, "source-at-start", byID["cdc-snapshot"].SrcConn.Name)
	assert.Equal(t, "192.0.2.2", byID["cdc-snapshot"].DstConn.Host)
	require.NotNil(t, byID["cdc-legacy"].SrcConn)
	assert.Equal(t, "source-current", byID["cdc-legacy"].SrcConn.Name)
	assert.Equal(t, "legacy_selected", byID["cdc-legacy"].SrcConn.Database)
	assert.Equal(t, "postgres", byID["cdc-legacy"].DstDBType)

	require.NoError(t, DeleteConnection(src.ID))
	require.NoError(t, DeleteConnection(dst.ID))
	jobs, err = ListIncrementalJobsWithConn(1, false)
	require.NoError(t, err)
	byID = make(map[string]IncrementalMigrationJobWithConn, len(jobs))
	for _, job := range jobs {
		byID[job.JobID] = job
	}
	assert.NotNil(t, byID["cdc-snapshot"].SrcConn)
	assert.Equal(t, "source-at-start", byID["cdc-snapshot"].SrcConn.Name)
	assert.Nil(t, byID["cdc-legacy"].SrcConn)
	assert.Nil(t, byID["cdc-legacy"].DstConn)
}

func TestHasOpenIncrementalTarget(t *testing.T) {
	setupTestDB(t)
	open := &IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-open", SrcConnID: 1, DstConnID: 2, TargetSchema: "app", Status: "paused_restart"}
	require.NoError(t, CreateIncrementalJob(open))
	exists, err := HasOpenIncrementalTarget(2, "app")
	require.NoError(t, err)
	assert.True(t, exists)
	require.NoError(t, UpdateIncrementalJob(open.JobID, map[string]any{"status": "aborted"}))
	exists, err = HasOpenIncrementalTarget(2, "app")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
