package migrate_test

import (
	"dbgold/diff"
	"dbgold/migrate"
	"dbgold/schema"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLServerGenerateDiffSQL_AddTable(t *testing.T) {
	r := &diff.Result{
		AddedTables: []schema.Table{
			{
				Name: "orders",
				Columns: []schema.Column{
					{Name: "id", Type: "INT", Nullable: false, PrimaryKey: true, AutoIncrement: true},
					{Name: "amount", Type: "DECIMAL(10,2)", Nullable: false},
				},
			},
		},
	}
	sqls, err := migrate.SQLServerGenerateDiffSQL(r)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Contains(t, sqls[0], "CREATE TABLE [orders]")
	assert.Contains(t, sqls[0], "[id] INT IDENTITY(1,1) NOT NULL")
	assert.Contains(t, sqls[0], "PRIMARY KEY ([id])")
}

func TestSQLServerGenerateDiffSQL_DropTable(t *testing.T) {
	r := &diff.Result{
		DroppedTables: []schema.Table{{Name: "old_logs"}},
	}
	sqls, err := migrate.SQLServerGenerateDiffSQL(r)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Equal(t, "DROP TABLE IF EXISTS [old_logs]", sqls[0])
}

func TestSQLServerGenerateFullMigrationSQL(t *testing.T) {
	dst := &schema.FullSchema{
		Schema: schema.Schema{
			Tables: []schema.Table{
				{Name: "users", Columns: []schema.Column{
					{Name: "id", Type: "INT", Nullable: false, PrimaryKey: true, AutoIncrement: true},
				}},
			},
		},
		Views: []schema.View{
			{Name: "v_users", Def: "SELECT * FROM users"},
		},
	}
	sqls, err := migrate.SQLServerGenerateFullMigrationSQL(nil, dst)
	require.NoError(t, err)
	require.True(t, len(sqls) >= 2)
	assert.Contains(t, sqls[0], "CREATE TABLE [users]")
	assert.Contains(t, sqls[len(sqls)-1], "CREATE OR ALTER VIEW [v_users]")
}
