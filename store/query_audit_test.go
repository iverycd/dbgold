package store

import (
	"dbgold/config"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQueryAuditVisibilityAndCleanup(t *testing.T) {
	Init(&config.Config{SQLitePath: ":memory:"})
	userA := User{Username: "query-a", Password: "hash", Enabled: true}
	userB := User{Username: "query-b", Password: "hash", Enabled: true}
	require.NoError(t, DB.Create(&userA).Error)
	require.NoError(t, DB.Create(&userB).Error)

	old := QueryAudit{OwnerID: userA.ID, ConnectionID: 1, ConnectionName: "old", DBType: "mysql", SQLText: "SELECT 1", StatementType: "select", RiskLevel: "readonly", Status: "success", CreatedAt: time.Now().Add(-100 * 24 * time.Hour)}
	recentA := QueryAudit{OwnerID: userA.ID, ConnectionID: 1, ConnectionName: "a", DBType: "postgres", SQLText: "SELECT 2", StatementType: "select", RiskLevel: "readonly", Status: "success", CreatedAt: time.Now()}
	recentB := QueryAudit{OwnerID: userB.ID, ConnectionID: 2, ConnectionName: "b", DBType: "gaussdb", SQLText: "UPDATE t SET v=1", StatementType: "update", RiskLevel: "write", Status: "failed", CreatedAt: time.Now()}
	for _, audit := range []*QueryAudit{&old, &recentA, &recentB} {
		require.NoError(t, CreateQueryAudit(audit))
	}

	mine, err := ListQueryAudits(QueryAuditFilter{OwnerID: userA.ID, Limit: 50})
	require.NoError(t, err)
	require.Len(t, mine, 2)
	require.Equal(t, "query-a", mine[0].Username)

	all, err := ListQueryAudits(QueryAuditFilter{AllOwners: true, Limit: 50})
	require.NoError(t, err)
	require.Len(t, all, 3)

	deleted, err := CleanupExpiredQueryAudits(time.Now().Add(-90 * 24 * time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
}
