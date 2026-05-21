package migrate_test

import (
	"dbgold/diff"
	"dbgold/migrate"
	"dbgold/schema"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strPtr(s string) *string { return &s }

func TestMySQLGenerateDiffSQL_AddTable(t *testing.T) {
	r := &diff.Result{
		AddedTables: []schema.Table{
			{
				Name: "orders",
				Columns: []schema.Column{
					{Name: "id", Type: "INT", Nullable: false, PrimaryKey: true, AutoIncrement: true},
					{Name: "user_id", Type: "INT", Nullable: false},
				},
			},
		},
	}
	sqls, err := migrate.MySQLGenerateDiffSQL(r)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Contains(t, sqls[0], "CREATE TABLE `orders`")
	assert.Contains(t, sqls[0], "`id` INT NOT NULL AUTO_INCREMENT")
	assert.Contains(t, sqls[0], "PRIMARY KEY (`id`)")
}

func TestMySQLGenerateDiffSQL_DropTable(t *testing.T) {
	r := &diff.Result{
		DroppedTables: []schema.Table{{Name: "old_logs"}},
	}
	sqls, err := migrate.MySQLGenerateDiffSQL(r)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Equal(t, "DROP TABLE IF EXISTS `old_logs`", sqls[0])
}

func TestMySQLGenerateDiffSQL_AddColumn(t *testing.T) {
	r := &diff.Result{
		ModifiedTables: []diff.TableDiff{
			{
				TableName: "users",
				AddedColumns: []schema.Column{
					{Name: "email", Type: "VARCHAR(255)", Nullable: true},
				},
			},
		},
	}
	sqls, err := migrate.MySQLGenerateDiffSQL(r)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Equal(t, "ALTER TABLE `users` ADD COLUMN `email` VARCHAR(255)", sqls[0])
}

func TestMySQLGenerateDiffSQL_DropColumn(t *testing.T) {
	r := &diff.Result{
		ModifiedTables: []diff.TableDiff{
			{
				TableName:      "users",
				DroppedColumns: []schema.Column{{Name: "old_field", Type: "TEXT"}},
			},
		},
	}
	sqls, err := migrate.MySQLGenerateDiffSQL(r)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Equal(t, "ALTER TABLE `users` DROP COLUMN `old_field`", sqls[0])
}

func TestMySQLGenerateDiffSQL_ModifyColumn(t *testing.T) {
	r := &diff.Result{
		ModifiedTables: []diff.TableDiff{
			{
				TableName: "users",
				ModifiedColumns: []diff.ColumnDiff{
					{
						OldColumn:   schema.Column{Name: "age", Type: "INT", Nullable: true},
						Column:      schema.Column{Name: "age", Type: "BIGINT", Nullable: false},
						TypeChanged: true, NullableChanged: true,
					},
				},
			},
		},
	}
	sqls, err := migrate.MySQLGenerateDiffSQL(r)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Equal(t, "ALTER TABLE `users` MODIFY COLUMN `age` BIGINT NOT NULL", sqls[0])
}

func TestMySQLGenerateDiffSQL_AddIndex(t *testing.T) {
	r := &diff.Result{
		ModifiedTables: []diff.TableDiff{
			{
				TableName: "users",
				AddedIndexes: []schema.Index{
					{Name: "idx_email", Columns: []string{"email"}, Unique: true},
				},
			},
		},
	}
	sqls, err := migrate.MySQLGenerateDiffSQL(r)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Equal(t, "CREATE UNIQUE INDEX `idx_email` ON `users` (`email`)", sqls[0])
}

func TestMySQLGenerateFullMigrationSQL(t *testing.T) {
	dst := &schema.FullSchema{
		Schema: schema.Schema{
			Tables: []schema.Table{
				{
					Name: "users",
					Columns: []schema.Column{
						{Name: "id", Type: "INT", Nullable: false, PrimaryKey: true, AutoIncrement: true},
						{Name: "name", Type: "VARCHAR(100)", Nullable: false},
					},
				},
			},
		},
		Views: []schema.View{
			{Name: "v_users", Def: "SELECT * FROM users"},
		},
	}
	sqls, err := migrate.MySQLGenerateFullMigrationSQL(nil, dst)
	require.NoError(t, err)
	assert.True(t, len(sqls) >= 2)
	assert.Contains(t, sqls[0], "CREATE TABLE `users`")
	assert.Contains(t, sqls[len(sqls)-1], "CREATE OR REPLACE VIEW `v_users`")
}

func TestMySQLGenerateSelectiveSQL(t *testing.T) {
	objs := &schema.SelectedObjects{
		Tables: []schema.Table{
			{
				Name: "products",
				Columns: []schema.Column{
					{Name: "id", Type: "INT", Nullable: false, PrimaryKey: true},
				},
			},
		},
		Views: []schema.View{
			{Name: "v_products", Def: "SELECT * FROM products"},
		},
	}
	sqls, err := migrate.MySQLGenerateSelectiveSQL(objs)
	require.NoError(t, err)
	assert.Len(t, sqls, 2)
	assert.Contains(t, sqls[0], "CREATE TABLE `products`")
	assert.Contains(t, sqls[1], "CREATE OR REPLACE VIEW `v_products`")
}
