package migrate_test

import (
	"dbgold/diff"
	"dbgold/migrate"
	"dbgold/schema"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresGenerateDiffSQL_AddTable(t *testing.T) {
	r := &diff.Result{
		AddedTables: []schema.Table{
			{
				Name: "orders",
				Columns: []schema.Column{
					{Name: "id", Type: "SERIAL", Nullable: false, PrimaryKey: true},
					{Name: "amount", Type: "NUMERIC(10,2)", Nullable: false},
				},
			},
		},
	}
	sqls, err := migrate.PostgresGenerateDiffSQL(r, false)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Contains(t, sqls[0], `CREATE TABLE "orders"`)
	assert.Contains(t, sqls[0], `PRIMARY KEY ("id")`)
}

func TestPostgresGenerateDiffSQL_ModifyColumn(t *testing.T) {
	r := &diff.Result{
		ModifiedTables: []diff.TableDiff{
			{
				TableName: "users",
				ModifiedColumns: []diff.ColumnDiff{
					{
						OldColumn:       schema.Column{Name: "score", Type: "INT", Nullable: true},
						Column:          schema.Column{Name: "score", Type: "BIGINT", Nullable: false},
						TypeChanged:     true,
						NullableChanged: true,
					},
				},
			},
		},
	}
	sqls, err := migrate.PostgresGenerateDiffSQL(r, false)
	require.NoError(t, err)
	assert.Len(t, sqls, 2)
	assert.Equal(t, `ALTER TABLE "users" ALTER COLUMN "score" TYPE BIGINT`, sqls[0])
	assert.Equal(t, `ALTER TABLE "users" ALTER COLUMN "score" SET NOT NULL`, sqls[1])
}

func TestPostgresGenerateDiffSQL_DropIndex(t *testing.T) {
	r := &diff.Result{
		ModifiedTables: []diff.TableDiff{
			{
				TableName:      "users",
				DroppedIndexes: []schema.Index{{Name: "idx_email"}},
			},
		},
	}
	sqls, err := migrate.PostgresGenerateDiffSQL(r, false)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Equal(t, `DROP INDEX "idx_email"`, sqls[0])
}

func TestPostgresGenerateFullMigrationSQL(t *testing.T) {
	dst := &schema.FullSchema{
		Schema: schema.Schema{
			Tables: []schema.Table{
				{
					Name: "users",
					Columns: []schema.Column{
						{Name: "id", Type: "SERIAL", Nullable: false, PrimaryKey: true},
					},
				},
			},
		},
		Sequences: []schema.Sequence{
			{Name: "user_seq", Start: 1, Increment: 1},
		},
	}
	sqls, err := migrate.PostgresGenerateFullMigrationSQL(nil, dst, false)
	require.NoError(t, err)
	assert.True(t, len(sqls) >= 2)
	// Sequence must come first (before table)
	assert.Contains(t, sqls[0], `CREATE SEQUENCE "user_seq"`)
	assert.Contains(t, sqls[1], `CREATE TABLE "users"`)
}

func TestPostgresGenerateDiffSQL_LowerCaseNames(t *testing.T) {
	r := &diff.Result{
		AddedTables: []schema.Table{
			{
				Name: "Orders",
				Columns: []schema.Column{
					{Name: "ID", Type: "SERIAL", Nullable: false, PrimaryKey: true},
					{Name: "Amount", Type: "NUMERIC(10,2)", Nullable: false},
				},
			},
		},
	}
	sqls, err := migrate.PostgresGenerateDiffSQL(r, true)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Contains(t, sqls[0], `CREATE TABLE "orders"`)
	assert.Contains(t, sqls[0], `"id"`)
	assert.Contains(t, sqls[0], `"amount"`)
}

func TestPostgresGenerateDiffSQL_LowerCaseFalse(t *testing.T) {
	r := &diff.Result{
		AddedTables: []schema.Table{
			{
				Name: "Orders",
				Columns: []schema.Column{
					{Name: "ID", Type: "SERIAL", Nullable: false, PrimaryKey: true},
				},
			},
		},
	}
	sqls, err := migrate.PostgresGenerateDiffSQL(r, false)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Contains(t, sqls[0], `CREATE TABLE "Orders"`)
	assert.Contains(t, sqls[0], `"ID"`)
}

func TestPostgresGenerateDiffSQL_LowerCase_ModifyAndDrop(t *testing.T) {
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
	sqls, err := migrate.PostgresGenerateDiffSQL(r, true)
	require.NoError(t, err)
	assert.Contains(t, sqls[0], `DROP TABLE IF EXISTS "oldtable"`)
	assert.Contains(t, findSQL(sqls, "DROP COLUMN"), `"oldcol"`)
	assert.Contains(t, findSQL(sqls, "ALTER COLUMN"), `"score"`)
	assert.Contains(t, findSQL(sqls, "CREATE"), `"idx_score"`)
	assert.Contains(t, findSQL(sqls, "DROP INDEX"), `"idx_old"`)
	assert.Contains(t, findSQL(sqls, "FOREIGN KEY"), `"fk_user"`)
	assert.Contains(t, findSQL(sqls, "DROP CONSTRAINT"), `"fk_old"`)
	assert.Contains(t, findSQL(sqls, "REFERENCES"), `"usertable"`)
}

func TestPostgresGenerateFullMigrationSQL_LowerCase(t *testing.T) {
	def := "view body"
	dst := &schema.FullSchema{
		Schema: schema.Schema{
			Tables: []schema.Table{
				{Name: "Orders", Columns: []schema.Column{{Name: "ID", Type: "SERIAL"}}},
			},
		},
		Sequences: []schema.Sequence{{Name: "SEQ_ORDER", Start: 1, Increment: 1}},
		Views:     []schema.View{{Name: "V_Orders", Def: def}},
	}
	sqls, err := migrate.PostgresGenerateFullMigrationSQL(nil, dst, true)
	require.NoError(t, err)
	assert.Contains(t, findSQL(sqls, "SEQUENCE"), `"seq_order"`)
	assert.Contains(t, findSQL(sqls, "TABLE"), `"orders"`)
	assert.Contains(t, findSQL(sqls, "VIEW"), `"v_orders"`)
}

