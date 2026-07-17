package sqlserver

import (
	"dbgold/schema"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAppendUniqueTrigger(t *testing.T) {
	var triggers []schema.Routine
	seen := make(map[int64]struct{})

	appendUniqueTrigger(&triggers, seen, 42, "orders_audit", "CREATE TRIGGER orders_audit ON orders AFTER INSERT, UPDATE AS SELECT 1;", true)
	appendUniqueTrigger(&triggers, seen, 42, "orders_audit", "duplicate event row", true)
	appendUniqueTrigger(&triggers, seen, 43, "encrypted_trigger", "", false)

	if assert.Len(t, triggers, 1) {
		assert.Equal(t, "orders_audit", triggers[0].Name)
		assert.Equal(t, "TRIGGER", triggers[0].Type)
		assert.Equal(t, "CREATE TRIGGER orders_audit ON orders AFTER INSERT, UPDATE AS SELECT 1\nGO", triggers[0].Body)
	}
}
