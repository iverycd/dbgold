package datamigrate

import (
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/assert"
)

func TestSequenceDDL(t *testing.T) {
	seq := source.SequenceInfo{TableName: "users", ColumnName: "id", StartValue: 1}
	ddl := SequenceDDL(seq)
	assert.Contains(t, ddl, `CREATE SEQUENCE IF NOT EXISTS "seq_users_id" START 1`)
	assert.Contains(t, ddl, `ALTER TABLE "users" ALTER COLUMN "id" SET DEFAULT nextval`)
}

func TestIndexDDL_Unique(t *testing.T) {
	idx := source.IndexInfo{TableName: "users", IndexName: "idx_users_email", Columns: []string{"email"}, IsUnique: true}
	ddl := IndexDDL(idx)
	assert.Equal(t, `CREATE UNIQUE INDEX "idx_users_email" ON "users" ("email")`, ddl)
}

func TestIndexDDL_Primary(t *testing.T) {
	idx := source.IndexInfo{TableName: "users", IndexName: "PRIMARY", Columns: []string{"id"}, IsPrimary: true}
	ddl := IndexDDL(idx)
	assert.Equal(t, `ALTER TABLE "users" ADD PRIMARY KEY ("id")`, ddl)
}

func TestIndexDDL_Regular(t *testing.T) {
	idx := source.IndexInfo{TableName: "orders", IndexName: "idx_orders_user", Columns: []string{"user_id"}, IsUnique: false}
	ddl := IndexDDL(idx)
	assert.Equal(t, `CREATE INDEX "idx_orders_user" ON "orders" ("user_id")`, ddl)
}

func TestFKDDL_WithOnDelete(t *testing.T) {
	fk := source.FKInfo{
		TableName: "orders", ConstraintName: "fk_orders_user",
		Columns: []string{"user_id"}, RefTable: "users", RefColumns: []string{"id"},
		OnDelete: "CASCADE", OnUpdate: "",
	}
	ddl := FKDDL(fk)
	assert.Contains(t, ddl, `ADD CONSTRAINT "fk_orders_user"`)
	assert.Contains(t, ddl, `REFERENCES "users" ("id")`)
	assert.Contains(t, ddl, `ON DELETE CASCADE`)
	assert.NotContains(t, ddl, `ON UPDATE`)
}

func TestFKDDL_WithOnUpdate(t *testing.T) {
	fk := source.FKInfo{
		TableName: "orders", ConstraintName: "fk_orders_product",
		Columns: []string{"product_id"}, RefTable: "products", RefColumns: []string{"id"},
		OnDelete: "", OnUpdate: "RESTRICT",
	}
	ddl := FKDDL(fk)
	assert.Contains(t, ddl, `ON UPDATE RESTRICT`)
	assert.NotContains(t, ddl, `ON DELETE`)
}

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
