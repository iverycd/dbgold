package cdc

import (
	"database/sql/driver"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSelectUniqueLocatorUsesSmallestStableCandidate(t *testing.T) {
	table := TableInfo{Name: "accounts", UniqueIndexes: []UniqueIndexInfo{
		{Name: "uq_tenant_email", Columns: []string{"tenant_id", "email"}},
		{Name: "uq_code", Columns: []string{"code"}},
	}}
	require.True(t, selectUniqueLocator(&table, [][]string{{"code"}, {"email", "tenant_id"}}, false))
	require.Equal(t, LocatorUniqueKey, table.LocatorStrategy)
	require.Equal(t, "uq_code", table.LocatorIndex)
	require.Equal(t, []string{"code"}, table.LocatorColumns)
}

func TestSelectUniqueLocatorRequiresMatchingTargetIndex(t *testing.T) {
	table := TableInfo{Name: "accounts", UniqueIndexes: []UniqueIndexInfo{{Name: "uq_code", Columns: []string{"Code"}}}}
	require.False(t, selectUniqueLocator(&table, [][]string{{"other"}}, true))
	require.True(t, selectUniqueLocator(&table, [][]string{{"code"}}, true))
}

func TestEligibleUniqueIndexPartRejectsUnsafeIndexes(t *testing.T) {
	require.True(t, eligibleUniqueIndexPart(true, "code", false, "BTREE", true, "NO"))
	require.False(t, eligibleUniqueIndexPart(true, "code", false, "BTREE", true, "YES"), "nullable unique key")
	require.False(t, eligibleUniqueIndexPart(true, "code", true, "BTREE", true, "NO"), "prefix unique key")
	require.False(t, eligibleUniqueIndexPart(false, "", false, "BTREE", false, ""), "functional key part")
	require.False(t, eligibleUniqueIndexPart(true, "code", false, "HASH", true, "NO"), "non-BTREE key")
}

func TestApplyLocatorStrategiesRejectsMissingColumn(t *testing.T) {
	tables := []TableInfo{{Name: "events", Columns: []string{"id", "payload"}}}
	err := ApplyLocatorStrategies(tables, []LocatorStrategy{{Table: "events", Strategy: LocatorUniqueKey, Columns: []string{"missing"}}})
	require.ErrorContains(t, err, "已不存在")
}

type fixedResult int64

func (fixedResult) LastInsertId() (int64, error)   { return 0, nil }
func (r fixedResult) RowsAffected() (int64, error) { return int64(r), nil }

var _ driver.Result = fixedResult(0)

func TestRequireOneFullRowProducesOnlyDigest(t *testing.T) {
	table := &TableInfo{Name: "events"}
	err := requireOneFullRow(table, "delete", []any{"sensitive-value", nil, []byte{1, 2}}, fixedResult(0))
	var conflict *RowConflictError
	require.ErrorAs(t, err, &conflict)
	require.Len(t, conflict.BeforeHash, 64)
	require.NotContains(t, conflict.Error(), "sensitive-value")
	require.NoError(t, requireOneFullRow(table, "delete", nil, fixedResult(1)))
}

func TestLocatorChangedSupportsCompositeUniqueKey(t *testing.T) {
	table := &TableInfo{Columns: []string{"tenant", "email", "name"}, LocatorColumns: []string{"tenant", "email"}}
	require.False(t, locatorChanged(table, []any{1, "a@x", "old"}, []any{1, "a@x", "new"}))
	require.True(t, locatorChanged(table, []any{1, "a@x", "old"}, []any{1, "b@x", "old"}))
}
