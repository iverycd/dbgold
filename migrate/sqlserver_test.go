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
	sqls, err := migrate.SQLServerGenerateDiffSQL(r, false)
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
	sqls, err := migrate.SQLServerGenerateDiffSQL(r, false)
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
	sqls, err := migrate.SQLServerGenerateFullMigrationSQL(nil, dst, false)
	require.NoError(t, err)
	require.True(t, len(sqls) >= 2)
	assert.Contains(t, sqls[0], "CREATE TABLE [users]")
	assert.Contains(t, sqls[len(sqls)-1], "CREATE OR ALTER VIEW [v_users]")
}

func TestSQLServerGenerateDiffSQL_LowerCaseNames(t *testing.T) {
	r := &diff.Result{
		AddedTables: []schema.Table{
			{
				Name: "Orders",
				Columns: []schema.Column{
					{Name: "ID", Type: "INT", Nullable: false, PrimaryKey: true, AutoIncrement: true},
					{Name: "Amount", Type: "DECIMAL(10,2)", Nullable: false},
				},
			},
		},
	}
	sqls, err := migrate.SQLServerGenerateDiffSQL(r, true)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Contains(t, sqls[0], "CREATE TABLE [orders]")
	assert.Contains(t, sqls[0], "[id]")
	assert.Contains(t, sqls[0], "[amount]")
}

func TestSQLServerGenerateDiffSQL_LowerCase_ModifyAndDrop(t *testing.T) {
	r := &diff.Result{
		DroppedTables: []schema.Table{
			{Name: "OldTable"},
		},
		ModifiedTables: []diff.TableDiff{
			{
				TableName: "UserOrder",
				DroppedColumns: []schema.Column{
					{Name: "OldCol"},
				},
				AddedIndexes: []schema.Index{
					{Name: "IDX_Score", Columns: []string{"Score"}, Unique: false},
				},
				DroppedIndexes: []schema.Index{
					{Name: "IDX_Old"},
				},
				AddedForeignKeys: []schema.ForeignKey{
					{Name: "FK_User", Columns: []string{"UserID"}, RefTable: "UserTable", RefColumns: []string{"ID"}},
				},
				DroppedForeignKeys: []schema.ForeignKey{
					{Name: "FK_Old"},
				},
			},
		},
	}
	sqls, err := migrate.SQLServerGenerateDiffSQL(r, true)
	require.NoError(t, err)
	assert.Contains(t, findSQL(sqls, "DROP TABLE"), "[oldtable]")
	dropCol := findSQL(sqls, "DROP COLUMN")
	assert.Contains(t, dropCol, "[userorder]")
	assert.Contains(t, dropCol, "[oldcol]")
	createIdx := findSQL(sqls, "CREATE INDEX")
	assert.Contains(t, createIdx, "[idx_score]")
	assert.Contains(t, createIdx, "[userorder]")
	assert.Contains(t, createIdx, "[score]")
	assert.Contains(t, findSQL(sqls, "DROP INDEX"), "[idx_old]")
	addFK := findSQL(sqls, "ADD CONSTRAINT")
	assert.Contains(t, addFK, "[fk_user]")
	assert.Contains(t, addFK, "[userid]")
	assert.Contains(t, addFK, "[usertable]")
	assert.Contains(t, addFK, "[id]")
	assert.Contains(t, findSQL(sqls, "DROP CONSTRAINT"), "[fk_old]")
}
