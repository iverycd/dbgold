package dialect

import (
	"testing"

	"dbgold/datamigrate/source"
)

// TestDaMengCreateTable_Golden 锁定达梦建表 DDL:IDENTITY 自增列、独立 DROP、无 CASCADE。
func TestDaMengCreateTable_Golden(t *testing.T) {
	d := NewDaMeng()
	info := &source.TableDDLInfo{
		TableName: "users",
		Columns: []source.ColumnInfo{
			{Name: "id", DataType: "bigint", IsNullable: false, Extra: "auto_increment"},
			{Name: "name", DataType: "varchar", Length: 100, IsNullable: false},
			{Name: "age", DataType: "int", IsNullable: true, Default: strptr("0")},
			{Name: "created", DataType: "datetime", IsNullable: true, Default: strptr("CURRENT_TIMESTAMP")},
		},
	}
	stmts, err := d.CreateTableStatements("APP", info, "mysql", TypeOpt{}, identity)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := `DROP TABLE "APP"."users" CASCADE CONSTRAINTS;
CREATE TABLE "APP"."users" (
  "id" NUMBER(19) IDENTITY(1, 1) NOT NULL,
  "name" VARCHAR2(100) NOT NULL,
  "age" NUMBER(10) DEFAULT '0',
  "created" TIMESTAMP DEFAULT CURRENT_TIMESTAMP
)`
	if got := JoinSQL(stmts); got != want {
		t.Errorf("dameng create table mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestDaMengIndexAndFK_Golden(t *testing.T) {
	d := NewDaMeng()
	// 主键
	pk := source.IndexInfo{TableName: "t", IsPrimary: true, Columns: []string{"id"}}
	if got := JoinSQL(d.IndexStatements("APP", pk)); got != `ALTER TABLE "APP"."t" ADD PRIMARY KEY ("id")` {
		t.Errorf("pk mismatch: %s", got)
	}
	// 唯一索引(无 IF NOT EXISTS,索引名加表名前缀去重)
	uq := source.IndexInfo{TableName: "t", IndexName: "uq", IsUnique: true, Columns: []string{"x"}}
	if got := JoinSQL(d.IndexStatements("APP", uq)); got != `CREATE UNIQUE INDEX "t_uq" ON "APP"."t" ("x")` {
		t.Errorf("unique mismatch: %s", got)
	}
	// 外键:有 ON UPDATE 应被丢弃,ON DELETE CASCADE 保留
	fk := source.FKInfo{
		TableName: "child", ConstraintName: "fk1",
		Columns: []string{"pid"}, RefTable: "parent", RefColumns: []string{"id"},
		OnDelete: "CASCADE", OnUpdate: "RESTRICT",
	}
	want := `ALTER TABLE "APP"."child" ADD CONSTRAINT "fk1" FOREIGN KEY ("pid") REFERENCES "APP"."parent" ("id") ON DELETE CASCADE`
	if got := JoinSQL(d.ForeignKeyStatements("APP", fk)); got != want {
		t.Errorf("fk mismatch:\n got: %s\nwant: %s", got, want)
	}
	// 外键 ON DELETE NO ACTION 应省略
	fk2 := source.FKInfo{TableName: "c", ConstraintName: "fk2", Columns: []string{"a"}, RefTable: "p", RefColumns: []string{"b"}, OnDelete: "NO ACTION"}
	want2 := `ALTER TABLE "APP"."c" ADD CONSTRAINT "fk2" FOREIGN KEY ("a") REFERENCES "APP"."p" ("b")`
	if got := JoinSQL(d.ForeignKeyStatements("APP", fk2)); got != want2 {
		t.Errorf("fk2 mismatch:\n got: %s\nwant: %s", got, want2)
	}
}

func TestDaMengCaps(t *testing.T) {
	c := NewDaMeng().Caps()
	if c.UsesSequences {
		t.Error("dameng should not use sequences (uses IDENTITY)")
	}
	if !c.IdentityInsert {
		t.Error("dameng should require IDENTITY_INSERT")
	}
}

func TestDaMengComment(t *testing.T) {
	d := NewDaMeng()
	tbl := source.CommentInfo{TableName: "T", Comment: "用户表"}
	if got := JoinSQL(d.CommentStatements("APP", tbl)); got != `COMMENT ON TABLE "APP"."T" IS '用户表'` {
		t.Errorf("table comment mismatch: %s", got)
	}
	col := source.CommentInfo{TableName: "T", ColumnName: "NAME", Comment: "it's"}
	if got := JoinSQL(d.CommentStatements("APP", col)); got != `COMMENT ON COLUMN "APP"."T"."NAME" IS 'it''s'` {
		t.Errorf("column comment mismatch: %s", got)
	}
}
