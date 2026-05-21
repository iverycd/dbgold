package diff_test

import (
	"dbgold/diff"
	"dbgold/schema"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func col(name, typ string, nullable bool) schema.Column {
	return schema.Column{Name: name, Type: typ, Nullable: nullable}
}

func TestCompare_AddedTable(t *testing.T) {
	src := &schema.Schema{
		Tables: []schema.Table{{Name: "users", Columns: []schema.Column{col("id", "int", false)}}},
	}
	dst := &schema.Schema{
		Tables: []schema.Table{
			{Name: "users", Columns: []schema.Column{col("id", "int", false)}},
			{Name: "orders", Columns: []schema.Column{col("id", "int", false)}},
		},
	}
	result := diff.Compare(src, dst)
	require.Len(t, result.AddedTables, 1)
	assert.Equal(t, "orders", result.AddedTables[0].Name)
	assert.Empty(t, result.DroppedTables)
}

func TestCompare_DroppedTable(t *testing.T) {
	src := &schema.Schema{
		Tables: []schema.Table{
			{Name: "users", Columns: []schema.Column{col("id", "int", false)}},
			{Name: "logs", Columns: []schema.Column{col("id", "int", false)}},
		},
	}
	dst := &schema.Schema{
		Tables: []schema.Table{{Name: "users", Columns: []schema.Column{col("id", "int", false)}}},
	}
	result := diff.Compare(src, dst)
	require.Len(t, result.DroppedTables, 1)
	assert.Equal(t, "logs", result.DroppedTables[0].Name)
}

func TestCompare_AddedColumn(t *testing.T) {
	src := &schema.Schema{
		Tables: []schema.Table{{Name: "users", Columns: []schema.Column{col("id", "int", false)}}},
	}
	dst := &schema.Schema{
		Tables: []schema.Table{{Name: "users", Columns: []schema.Column{
			col("id", "int", false),
			col("email", "varchar(255)", true),
		}}},
	}
	result := diff.Compare(src, dst)
	require.Len(t, result.ModifiedTables, 1)
	td := result.ModifiedTables[0]
	assert.Equal(t, "users", td.TableName)
	require.Len(t, td.AddedColumns, 1)
	assert.Equal(t, "email", td.AddedColumns[0].Name)
	assert.Empty(t, td.DroppedColumns)
}

func TestCompare_DroppedColumn(t *testing.T) {
	src := &schema.Schema{
		Tables: []schema.Table{{Name: "users", Columns: []schema.Column{
			col("id", "int", false),
			col("old_col", "text", true),
		}}},
	}
	dst := &schema.Schema{
		Tables: []schema.Table{{Name: "users", Columns: []schema.Column{
			col("id", "int", false),
		}}},
	}
	result := diff.Compare(src, dst)
	require.Len(t, result.ModifiedTables, 1)
	td := result.ModifiedTables[0]
	require.Len(t, td.DroppedColumns, 1)
	assert.Equal(t, "old_col", td.DroppedColumns[0].Name)
}

func TestCompare_ModifiedColumn_TypeChanged(t *testing.T) {
	src := &schema.Schema{
		Tables: []schema.Table{{Name: "users", Columns: []schema.Column{col("name", "varchar(50)", true)}}},
	}
	dst := &schema.Schema{
		Tables: []schema.Table{{Name: "users", Columns: []schema.Column{col("name", "varchar(200)", true)}}},
	}
	result := diff.Compare(src, dst)
	require.Len(t, result.ModifiedTables, 1)
	td := result.ModifiedTables[0]
	require.Len(t, td.ModifiedColumns, 1)
	assert.True(t, td.ModifiedColumns[0].TypeChanged)
	assert.Equal(t, "varchar(200)", td.ModifiedColumns[0].Column.Type)
}

func TestCompare_AddedIndex(t *testing.T) {
	src := &schema.Schema{
		Tables: []schema.Table{{Name: "users", Indexes: []schema.Index{}}},
	}
	dst := &schema.Schema{
		Tables: []schema.Table{{Name: "users", Indexes: []schema.Index{
			{Name: "idx_email", Columns: []string{"email"}, Unique: true},
		}}},
	}
	result := diff.Compare(src, dst)
	require.Len(t, result.ModifiedTables, 1)
	assert.Len(t, result.ModifiedTables[0].AddedIndexes, 1)
}

func TestCompare_NoChanges(t *testing.T) {
	s := &schema.Schema{
		Tables: []schema.Table{{Name: "users", Columns: []schema.Column{col("id", "int", false)}}},
	}
	result := diff.Compare(s, s)
	assert.Empty(t, result.AddedTables)
	assert.Empty(t, result.DroppedTables)
	assert.Empty(t, result.ModifiedTables)
}
