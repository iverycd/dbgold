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

	list, err := ListConnections()
	require.NoError(t, err)
	assert.Len(t, list, 1)

	require.NoError(t, DeleteConnection(c.ID))
	list, _ = ListConnections()
	assert.Len(t, list, 0)
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

	list, err := ListMigrations()
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
