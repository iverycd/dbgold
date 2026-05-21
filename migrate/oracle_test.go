package migrate_test

import (
	"dbgold/diff"
	"dbgold/migrate"
	"dbgold/schema"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOracleGenerateDiffSQL_AddTable(t *testing.T) {
	r := &diff.Result{
		AddedTables: []schema.Table{
			{
				Name: "orders",
				Columns: []schema.Column{
					{Name: "id", Type: "NUMBER", Nullable: false, PrimaryKey: true},
					{Name: "amount", Type: "NUMBER(10,2)", Nullable: false},
				},
			},
		},
	}
	sqls, err := migrate.OracleGenerateDiffSQL(r)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Contains(t, sqls[0], `CREATE TABLE "orders"`)
	assert.Contains(t, sqls[0], `PRIMARY KEY ("id")`)
}

func TestOracleGenerateDiffSQL_DropTable(t *testing.T) {
	r := &diff.Result{
		DroppedTables: []schema.Table{{Name: "old_logs"}},
	}
	sqls, err := migrate.OracleGenerateDiffSQL(r)
	require.NoError(t, err)
	require.Len(t, sqls, 1)
	assert.Equal(t, `DROP TABLE "old_logs"`, sqls[0])
}

func TestOracleGenerateFullMigrationSQL_SequenceFirst(t *testing.T) {
	dst := &schema.FullSchema{
		Schema: schema.Schema{
			Tables: []schema.Table{
				{Name: "users", Columns: []schema.Column{
					{Name: "id", Type: "NUMBER", Nullable: false, PrimaryKey: true},
				}},
			},
		},
		Sequences: []schema.Sequence{
			{Name: "user_seq", Start: 1, Increment: 1},
		},
	}
	sqls, err := migrate.OracleGenerateFullMigrationSQL(nil, dst)
	require.NoError(t, err)
	require.True(t, len(sqls) >= 2)
	assert.Contains(t, sqls[0], `CREATE SEQUENCE "user_seq" START WITH 1`)
	assert.Contains(t, sqls[1], `CREATE TABLE "users"`)
}
