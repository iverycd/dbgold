package migrate_test

import (
	"dbgold/diff"
	"dbgold/migrate"
	"dbgold/schema"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	sqls, err := migrate.MySQLGenerateDiffSQL(r, false)
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
	sqls, err := migrate.MySQLGenerateDiffSQL(r, false)
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
	sqls, err := migrate.MySQLGenerateDiffSQL(r, false)
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
	sqls, err := migrate.MySQLGenerateDiffSQL(r, false)
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
	sqls, err := migrate.MySQLGenerateDiffSQL(r, false)
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
	sqls, err := migrate.MySQLGenerateDiffSQL(r, false)
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
	sqls, err := migrate.MySQLGenerateFullMigrationSQL(nil, dst, false)
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
	sqls, err := migrate.MySQLGenerateSelectiveSQL(objs, false)
	require.NoError(t, err)
	assert.Len(t, sqls, 2)
	assert.Contains(t, sqls[0], "CREATE TABLE `products`")
	assert.Contains(t, sqls[1], "CREATE OR REPLACE VIEW `v_products`")
}

func TestMySQLGenerateDiffSQL_LowerCaseNames(t *testing.T) {
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
	sqls, err := migrate.MySQLGenerateDiffSQL(r, true)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Contains(t, sqls[0], "CREATE TABLE `orders`")
	assert.Contains(t, sqls[0], "`id`")
	assert.Contains(t, sqls[0], "`amount`")
}

func TestMySQLGenerateDiffSQL_LowerCase_ModifyAndDrop(t *testing.T) {
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
				ModifiedColumns: []diff.ColumnDiff{
					{
						Column:      schema.Column{Name: "Score", Type: "BIGINT", Nullable: false},
						TypeChanged: true,
					},
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
	sqls, err := migrate.MySQLGenerateDiffSQL(r, true)
	require.NoError(t, err)
	assert.Contains(t, findSQL(sqls, "DROP TABLE"), "`oldtable`")
	assert.Contains(t, findSQL(sqls, "DROP COLUMN"), "`oldcol`")
	assert.Contains(t, findSQL(sqls, "MODIFY COLUMN"), "`score`")
	assert.Contains(t, findSQL(sqls, "CREATE"), "`idx_score`")
	assert.Contains(t, findSQL(sqls, "DROP INDEX"), "`idx_old`")
	assert.Contains(t, findSQL(sqls, "FOREIGN KEY"), "`fk_user`")
	assert.Contains(t, findSQL(sqls, "DROP FOREIGN KEY"), "`fk_old`")
	assert.Contains(t, findSQL(sqls, "REFERENCES"), "`usertable`")
}
