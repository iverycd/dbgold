package dialect

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
)

func TestDaMengMySQLZeroDateDefaults(t *testing.T) {
	for _, tc := range []struct{ typ, def, want string }{
		{"date", "0000-00-00", "1900-01-01"},
		{"datetime", "0000-00-00 00:00:00", "1900-01-01 00:00:00"},
		{"timestamp", "('0000-00-00 00:00:00.000000')", "1900-01-01 00:00:00"},
		{"date", "0000-12-15", "1900-01-01"},
		{"datetime", "2024-00-15 12:34:56", "1900-01-01 00:00:00"},
		{"timestamp", "2024-12-00 12:34:56", "1900-01-01 00:00:00"},
		{"varchar", "0000-00-00 00:00:00", "0000-00-00 00:00:00"},
		{"datetime", "2024-02-29 12:34:56.123456", "2024-02-29 12:34:56.123456"},
	} {
		for _, nullable := range []bool{false, true} {
			t.Run(tc.typ+"/"+tc.def, func(t *testing.T) {
				info := &source.TableDDLInfo{TableName: "device_baseinfo", Columns: []source.ColumnInfo{
					{Name: "equipment_validity", DataType: tc.typ, Length: 100, IsNullable: nullable, Default: strptr(tc.def)},
					{Name: "next_test_time", DataType: "timestamp", Default: strptr("CURRENT_TIMESTAMP")},
				}}
				stmts, err := NewDaMeng().CreateTableStatements("ZNFXPT", info, "mysql", TypeOpt{}, identity)
				if err != nil {
					t.Fatal(err)
				}
				ddl := JoinSQL(stmts)
				if !strings.Contains(ddl, "DEFAULT '"+tc.want+"'") || !strings.Contains(ddl, "DEFAULT CURRENT_TIMESTAMP") {
					t.Fatalf("unexpected defaults: %s", ddl)
				}
				if strings.Contains(ddl, "NOT NULL DEFAULT '"+tc.want+"'") == nullable {
					t.Fatalf("nullability changed: %s", ddl)
				}
			})
		}
	}
}

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
	want := `DROP TABLE "APP"."USERS" CASCADE CONSTRAINTS;
CREATE TABLE "APP"."USERS" (
  "ID" BIGINT IDENTITY(1, 1) NOT NULL,
  "NAME" VARCHAR2(100) NOT NULL,
  "AGE" INT DEFAULT '0',
  "CREATED" TIMESTAMP DEFAULT CURRENT_TIMESTAMP
)`
	if got := JoinSQL(stmts); got != want {
		t.Errorf("dameng create table mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestDaMengMySQLZeroLengthCharacterColumns(t *testing.T) {
	info := &source.TableDDLInfo{
		TableName: "TABLE_01LIXIANGMINGXI",
		Columns: []source.ColumnInfo{
			{Name: "计划编号", DataType: "char", Length: 0, IsNullable: false},
			{Name: "需求行项目清单ID", DataType: "char", Length: 0, IsNullable: false},
			{Name: "备注", DataType: "varchar", Length: 0, IsNullable: true},
			{Name: "立项单号", DataType: "varchar", Length: 200, IsNullable: false, Default: strptr("未编号")},
			{Name: "状态代码", DataType: "char", Length: 8, IsNullable: true},
		},
	}
	stmts, err := NewDaMeng().CreateTableStatements("EPOINTBID_TP8_0_51_2_JS", info, "mysql", TypeOpt{CharInLength: true}, identity)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := `DROP TABLE "EPOINTBID_TP8_0_51_2_JS"."TABLE_01LIXIANGMINGXI" CASCADE CONSTRAINTS;
CREATE TABLE "EPOINTBID_TP8_0_51_2_JS"."TABLE_01LIXIANGMINGXI" (
  "计划编号" CHAR(1 CHAR) NOT NULL,
  "需求行项目清单ID" CHAR(1 CHAR) NOT NULL,
  "备注" VARCHAR2(1 CHAR),
  "立项单号" VARCHAR2(200 CHAR) NOT NULL DEFAULT '未编号',
  "状态代码" CHAR(8 CHAR)
)`
	if got := JoinSQL(stmts); got != want {
		t.Errorf("dameng zero-length character DDL mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	for i, length := range []int64{0, 0, 0, 200, 8} {
		if got := info.Columns[i].Length; got != length {
			t.Errorf("source column %s length changed: got %d, want %d", info.Columns[i].Name, got, length)
		}
	}
}

func TestDaMengIndexAndFK_Golden(t *testing.T) {
	d := NewDaMeng()
	// 主键
	pk := source.IndexInfo{TableName: "t", IsPrimary: true, Columns: []string{"id"}}
	if got := JoinSQL(d.IndexStatements("APP", pk)); got != `ALTER TABLE "APP"."T" ADD PRIMARY KEY ("ID")` {
		t.Errorf("pk mismatch: %s", got)
	}
	// 唯一索引(无 IF NOT EXISTS,索引名加表名前缀去重)
	uq := source.IndexInfo{TableName: "t", IndexName: "uq", IsUnique: true, Columns: []string{"x"}}
	if got := JoinSQL(d.IndexStatements("APP", uq)); got != `CREATE UNIQUE INDEX "T_UQ" ON "APP"."T" ("X")` {
		t.Errorf("unique mismatch: %s", got)
	}
	// 外键:有 ON UPDATE 应被丢弃,ON DELETE CASCADE 保留
	fk := source.FKInfo{
		TableName: "child", ConstraintName: "fk1",
		Columns: []string{"pid"}, RefTable: "parent", RefColumns: []string{"id"},
		OnDelete: "CASCADE", OnUpdate: "RESTRICT",
	}
	want := `ALTER TABLE "APP"."CHILD" ADD CONSTRAINT "FK1" FOREIGN KEY ("PID") REFERENCES "APP"."PARENT" ("ID") ON DELETE CASCADE`
	if got := JoinSQL(d.ForeignKeyStatements("APP", fk)); got != want {
		t.Errorf("fk mismatch:\n got: %s\nwant: %s", got, want)
	}
	// 外键 ON DELETE NO ACTION 应省略
	fk2 := source.FKInfo{TableName: "c", ConstraintName: "fk2", Columns: []string{"a"}, RefTable: "p", RefColumns: []string{"b"}, OnDelete: "NO ACTION"}
	want2 := `ALTER TABLE "APP"."C" ADD CONSTRAINT "FK2" FOREIGN KEY ("A") REFERENCES "APP"."P" ("B")`
	if got := JoinSQL(d.ForeignKeyStatements("APP", fk2)); got != want2 {
		t.Errorf("fk2 mismatch:\n got: %s\nwant: %s", got, want2)
	}
}

func TestDaMengFulltextIndexStatements(t *testing.T) {
	d := NewDaMeng()
	for _, tc := range []struct {
		name, schema string
		index        source.IndexInfo
		want         string
	}{
		{
			name: "single text column", schema: "App",
			index: source.IndexInfo{TableName: "Articles", IndexName: "IdxBody", Columns: []string{"Body"}, Method: "FULLTEXT"},
			want:  `CREATE CONTEXT INDEX "ARTICLES_IDXBODY" ON "APP"."ARTICLES" ("BODY") SYNC TRANSACTION`,
		},
		{
			name: "no B-tree attributes", schema: "App",
			index: source.IndexInfo{TableName: "Articles", IndexName: "IdxBody", Columns: []string{"Body"}, Method: "fulltext", IsUnique: true, Descending: []bool{true}},
			want:  `CREATE CONTEXT INDEX "ARTICLES_IDXBODY" ON "APP"."ARTICLES" ("BODY") SYNC TRANSACTION`,
		},
		{
			name: "quoted identifiers", schema: `Mixed"Schema`,
			index: source.IndexInfo{TableName: `Mixed"Table`, IndexName: `By"Body`, Columns: []string{`Body"Text`}, Method: "FullText"},
			want:  `CREATE CONTEXT INDEX "MIXED""TABLE_BY""BODY" ON "MIXED""SCHEMA"."MIXED""TABLE" ("BODY""TEXT") SYNC TRANSACTION`,
		},
		{
			name:  "current schema",
			index: source.IndexInfo{TableName: "Articles", IndexName: "IdxBody", Columns: []string{"Body"}, Method: "FULLTEXT"},
			want:  `CREATE CONTEXT INDEX "ARTICLES_IDXBODY" ON "ARTICLES" ("BODY") SYNC TRANSACTION`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := JoinSQL(d.IndexStatements(tc.schema, tc.index)); got != tc.want {
				t.Errorf("fulltext mismatch:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
	// The new method branch must leave ordinary key directions unchanged.
	ordinary := source.IndexInfo{TableName: "Articles", IndexName: "ByTitle", Columns: []string{"Title"}, Method: "BTREE", Descending: []bool{true}}
	if got := JoinSQL(d.IndexStatements("App", ordinary)); got != `CREATE INDEX "ARTICLES_BYTITLE" ON "APP"."ARTICLES" ("TITLE" DESC)` {
		t.Errorf("ordinary index changed: %s", got)
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
