package dialect

import (
	"testing"

	"dbgold/datamigrate/source"
)

// identity 模拟 migrator.objName 在 LowerCaseNames=false 时的行为(原样返回)。
func identity(s string) string { return s }

func strptr(s string) *string { return &s }

// TestPostgresCreateTable_Golden 锁定 PostgresDialect 建表 DDL 的逐字符输出,
// 与重构前 migrator.buildCreateTableDDL 的 PG 行为对齐。
func TestPostgresCreateTable_Golden(t *testing.T) {
	d := NewPostgres("postgres")
	info := &source.TableDDLInfo{
		TableName: "users",
		Columns: []source.ColumnInfo{
			{Name: "id", DataType: "bigint", IsNullable: false, Extra: "auto_increment"},
			{Name: "name", DataType: "varchar", Length: 100, IsNullable: false},
			{Name: "age", DataType: "int", IsNullable: true, Default: strptr("0")},
			{Name: "created", DataType: "datetime", IsNullable: true, Default: strptr("CURRENT_TIMESTAMP")},
			{Name: "flag", DataType: "bit", IsNullable: true, Default: strptr("b'0'")},
		},
	}
	stmts, err := d.CreateTableStatements("", info, "mysql", TypeOpt{}, identity)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := `DROP TABLE IF EXISTS "users" CASCADE;
CREATE TABLE "users" (
  "id" bigint NOT NULL,
  "name" varchar(100) NOT NULL,
  "age" int DEFAULT '0',
  "created" timestamp DEFAULT CURRENT_TIMESTAMP,
  "flag" bit DEFAULT B'0'
);`
	if got := JoinSQL(stmts); got != want {
		t.Errorf("create table DDL mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestPostgresCreateTable_WithSchema 验证带 schema 前缀的建表。
func TestPostgresCreateTable_WithSchema(t *testing.T) {
	d := NewPostgres("postgres")
	info := &source.TableDDLInfo{
		TableName: "t1",
		Columns:   []source.ColumnInfo{{Name: "c1", DataType: "int", IsNullable: true}},
	}
	stmts, _ := d.CreateTableStatements("myschema", info, "mysql", TypeOpt{}, identity)
	want := `DROP TABLE IF EXISTS "myschema"."t1" CASCADE;
CREATE TABLE "myschema"."t1" (
  "c1" int
);`
	if got := JoinSQL(stmts); got != want {
		t.Errorf("mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestPostgresIndexStatements_Golden(t *testing.T) {
	d := NewPostgres("postgres")
	cases := []struct {
		idx  source.IndexInfo
		want string
	}{
		{source.IndexInfo{TableName: "t", IsPrimary: true, Columns: []string{"id"}},
			`ALTER TABLE "s"."t" ADD PRIMARY KEY ("id");`},
		{source.IndexInfo{TableName: "t", IndexName: "uq_x", IsUnique: true, Columns: []string{"x"}},
			`CREATE UNIQUE INDEX IF NOT EXISTS "uq_x" ON "s"."t" ("x");`},
		{source.IndexInfo{TableName: "t", IndexName: "ix_y", Columns: []string{"y", "z"}},
			`CREATE INDEX IF NOT EXISTS "ix_y" ON "s"."t" ("y", "z");`},
	}
	for _, c := range cases {
		if got := JoinSQL(d.IndexStatements("s", c.idx)); got != c.want {
			t.Errorf("index mismatch:\n got: %s\nwant: %s", got, c.want)
		}
	}
}

func TestPostgresForeignKey_Golden(t *testing.T) {
	d := NewPostgres("postgres")
	fk := source.FKInfo{
		TableName: "child", ConstraintName: "fk1",
		Columns: []string{"pid"}, RefTable: "parent", RefColumns: []string{"id"},
		OnDelete: "CASCADE", OnUpdate: "NO ACTION",
	}
	want := `ALTER TABLE "s"."child" ADD CONSTRAINT "fk1" FOREIGN KEY ("pid") REFERENCES "s"."parent" ("id") ON DELETE CASCADE ON UPDATE NO ACTION;`
	if got := JoinSQL(d.ForeignKeyStatements("s", fk)); got != want {
		t.Errorf("fk mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestPostgresSequence_Golden(t *testing.T) {
	d := NewPostgres("postgres")
	seq := source.SequenceInfo{TableName: "t", ColumnName: "id", StartValue: 100}
	want := `CREATE SEQUENCE IF NOT EXISTS "s"."seq_t_id" INCREMENT BY 1 START 100;
ALTER TABLE "s"."t" ALTER COLUMN "id" SET DEFAULT nextval('s."seq_t_id"')`
	if got := JoinSQL(d.SequenceStatements("s", seq)); got != want {
		t.Errorf("sequence mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestGaussSeaboxUUID 验证 gaussdb/seabox 的函数默认值与视图 UUID 差异。
func TestGaussSeaboxUUID(t *testing.T) {
	info := &source.TableDDLInfo{
		TableName: "t",
		Columns:   []source.ColumnInfo{{Name: "g", DataType: "char", Length: 36, IsNullable: true, Default: strptr("UUID()")}},
	}
	cases := map[string]string{
		"postgres": ` DEFAULT gen_random_uuid()`,
		"gaussdb":  ` DEFAULT uuid()`,
		"seabox":   ` DEFAULT sys_guid()`,
	}
	for name, frag := range cases {
		d := NewPostgres(name)
		stmts, _ := d.CreateTableStatements("", info, "mysql", TypeOpt{}, identity)
		if got := JoinSQL(stmts); !contains(got, frag) {
			t.Errorf("[%s] expected fragment %q in:\n%s", name, frag, got)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
