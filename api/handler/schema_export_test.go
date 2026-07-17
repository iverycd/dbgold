package handler

import (
	"dbgold/schema"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildRoutineTriggerSQL(t *testing.T) {
	routines := []schema.Routine{
		{Name: "sync_order", Type: "PROCEDURE", Body: "CREATE PROCEDURE sync_order AS BEGIN NULL; END;\n/"},
	}
	triggers := []schema.Routine{
		{Name: "orders_bi", Type: "TRIGGER", Body: "CREATE TRIGGER orders_bi BEFORE INSERT ON orders BEGIN SET NEW.id = 1; END\nGO"},
	}

	got := buildRoutineTriggerSQL("sqlserver", "sales", routines, triggers)

	assert.Contains(t, got, "函数/存储过程/包数量: 1    触发器数量: 1")
	assert.Contains(t, got, "-- PROCEDURE: sync_order")
	assert.Contains(t, got, "-- TRIGGER: orders_bi")
	assert.Contains(t, got, routines[0].Body)
	assert.Contains(t, got, triggers[0].Body)
	assert.Less(t, strings.Index(got, "-- PROCEDURE: sync_order"), strings.Index(got, "-- TRIGGER: orders_bi"))
}

func TestBuildRoutineTriggerSQLEmpty(t *testing.T) {
	got := buildRoutineTriggerSQL("mysql", "empty_db", nil, nil)

	assert.Contains(t, got, "函数/存储过程/包数量: 0    触发器数量: 0")
	assert.Contains(t, got, "(该库中未发现自定义函数、存储过程或包)")
	assert.Contains(t, got, "(该库中未发现触发器)")
}
