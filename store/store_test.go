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
	running := &IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-running", SrcConnID: 1, DstConnID: 2, Status: "running", Phase: "running"}
	cuttingOver := &IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-cutover", SrcConnID: 1, DstConnID: 2, Status: "cutting_over", Phase: "cutting_over"}
	validating := &IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-validating", SrcConnID: 1, DstConnID: 2, Status: "validating", Phase: "validating"}
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

func TestIncrementalOperationalFieldsPersist(t *testing.T) {
	setupTestDB(t)
	job := &IncrementalMigrationJob{OwnerID: 1, JobID: "cdc-fields", SrcConnID: 1, DstConnID: 2, Status: "snapshot"}
	require.NoError(t, CreateIncrementalJob(job))
	require.NoError(t, UpdateIncrementalJob(job.JobID, map[string]any{
		"bootstrap_completed":  true,
		"start_gtid":           "sid:1-10",
		"checkpoint_gtid":      "sid:1-11",
		"source_head_gtid":     "sid:1-12",
		"cutover_gtid":         "sid:1-13",
		"blocking_gtid":        "sid:1-14",
		"checkpoint_position":  101,
		"source_head_position": 202,
		"cutover_position":     303,
		"blocking_position":    404,
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
	updated, err := UpdateIncrementalJobIfStatus(job.JobID, []string{"running"}, map[string]any{"status": "validating"})
	require.NoError(t, err)
	assert.False(t, updated)
	updated, err = UpdateIncrementalJobIfStatus(job.JobID, []string{"snapshot"}, map[string]any{"status": "validating"})
	require.NoError(t, err)
	assert.True(t, updated)
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

func TestCreateAndListMigrations(t *testing.T) {
	setupTestDB(t)
	m := &MigrationHistory{
		Type:          "diff",
		SrcConnID:     1,
		SrcDatabase:   "db_src",
		DstConnID:     2,
		DstDatabase:   "db_dst",
		SQLStatements: `["ALTER TABLE users ADD COLUMN email VARCHAR(255)"]`,
		Status:        "success",
	}
	require.NoError(t, CreateMigration(m))
	assert.NotZero(t, m.ID)

	list, err := ListMigrations(0, true)
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "diff", list[0].Type)

	got, err := GetMigration(m.ID)
	require.NoError(t, err)
	assert.Equal(t, m.ID, got.ID)
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
