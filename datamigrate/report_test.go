package datamigrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// 注:原 SequenceDDL/IndexDDL/FKDDL 已下沉到 dialect 包(报告与执行同源),
// 其 DDL 生成测试见 datamigrate/dialect/postgres_test.go。

func TestNewMigrationReport_ItemsNotNil(t *testing.T) {
	r := newMigrationReport()
	assert.NotNil(t, r.Tables.Items)
	assert.NotNil(t, r.Data.Items)
	assert.NotNil(t, r.Views.Items)
	assert.NotNil(t, r.Indexes.Items)
	assert.NotNil(t, r.Constraints.Items)
	assert.NotNil(t, r.Sequences.Items)
	assert.NotNil(t, r.Triggers.Items)
}
