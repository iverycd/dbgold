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
