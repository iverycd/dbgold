package datamigrate

import (
	"testing"
	"github.com/stretchr/testify/assert"
)

func TestFilterTables(t *testing.T) {
	tables := []string{"users", "orders", "order_log", "tmp_cache", "audit"}

	t.Run("all mode returns all tables", func(t *testing.T) {
		result := FilterTables(tables, "all", "")
		assert.Equal(t, tables, result)
	})

	t.Run("include mode returns only matching tables", func(t *testing.T) {
		result := FilterTables(tables, "include", "users,orders")
		assert.Equal(t, []string{"users", "orders"}, result)
	})

	t.Run("exclude mode removes matching tables", func(t *testing.T) {
		result := FilterTables(tables, "exclude", "*_log,tmp_*,audit")
		assert.Equal(t, []string{"users", "orders"}, result)
	})

	t.Run("wildcard star matches any suffix", func(t *testing.T) {
		result := FilterTables(tables, "exclude", "order*")
		assert.Equal(t, []string{"users", "tmp_cache", "audit"}, result)
	})

	t.Run("wildcard star matches any prefix", func(t *testing.T) {
		result := FilterTables(tables, "exclude", "*_cache")
		assert.Equal(t, []string{"users", "orders", "order_log", "audit"}, result)
	})

	t.Run("empty filter in include mode returns nothing", func(t *testing.T) {
		result := FilterTables(tables, "include", "")
		assert.Empty(t, result)
	})
}
