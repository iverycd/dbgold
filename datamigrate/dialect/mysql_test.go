package dialect

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
)

func TestMySQLCreateTable(t *testing.T) {
	d := NewMySQL()
	info := &source.TableDDLInfo{
		TableName: "users",
		Columns: []source.ColumnInfo{
			{Name: "id", DataType: "int", IsNullable: false, Extra: "auto_increment"},
			{Name: "name", DataType: "varchar", Length: 100, IsNullable: false},
			{Name: "bio", DataType: "longtext", IsNullable: true},
		},
	}
	stmts, err := d.CreateTableStatements("appdb", info, "mysql", TypeOpt{}, identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 2 {
		t.Fatalf("期望 2 条语句(DROP+CREATE),得到 %d", len(stmts))
	}
	if stmts[0].SQL != "DROP TABLE IF EXISTS `appdb`.`users`" {
		t.Errorf("DROP 语句不符: %s", stmts[0].SQL)
	}
	create := stmts[1].SQL
	for _, want := range []string{
		"`appdb`.`users`",
		"`id` int NOT NULL AUTO_INCREMENT",
		"`name` varchar(100) NOT NULL",
		"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("CREATE 缺少 %q\n实际: %s", want, create)
		}
	}
}

func TestMySQLColumnDefaultClause(t *testing.T) {
	d := NewMySQL()
	cases := []struct {
		name   string
		def    string
		myType string
		want   string
	}{
		{"ts_current", "CURRENT_TIMESTAMP", "datetime(6)", " DEFAULT CURRENT_TIMESTAMP"},
		{"sysdate_on_datetime", "SYSDATE", "datetime", " DEFAULT CURRENT_TIMESTAMP"},
		{"current_ts_on_int", "CURRENT_TIMESTAMP", "int", " DEFAULT NULL"},
		{"sys_guid", "SYS_GUID()", "varchar(36)", " DEFAULT NULL"},
		{"number_literal", "0", "int", " DEFAULT 0"},
		{"neg_number", "-5", "int", " DEFAULT -5"},
		{"string_literal", "abc", "varchar(10)", " DEFAULT 'abc'"},
		{"string_escape", "a'b", "varchar(10)", " DEFAULT 'a''b'"},
		{"text_no_default", "x", "longtext", ""},
		{"json_no_default", "x", "json", ""},
		{"bit_literal", "b'101'", "tinyint(1)", " DEFAULT 5"},
		{"true", "TRUE", "tinyint(1)", " DEFAULT 1"},
		{"false", "FALSE", "tinyint(1)", " DEFAULT 0"},
		{"empty", "", "int", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := d.columnDefaultClause(c.def, c.myType)
			if got != c.want {
				t.Errorf("columnDefaultClause(%q, %q) = %q, want %q", c.def, c.myType, got, c.want)
			}
		})
	}
}

func TestMySQLQuoteIdent(t *testing.T) {
	d := NewMySQL()
	if got := d.QuoteIdent("col`x"); got != "`col``x`" {
		t.Errorf("反引号转义错误: %s", got)
	}
	if got := d.QualifyTable("", "t"); got != "`t`" {
		t.Errorf("无 schema 限定错误: %s", got)
	}
	if got := d.QualifyTable("db", "t"); got != "`db`.`t`" {
		t.Errorf("带 schema 限定错误: %s", got)
	}
}

func TestMySQLComment(t *testing.T) {
	d := NewMySQL()
	// 表注释:ALTER TABLE ... COMMENT
	tbl := source.CommentInfo{TableName: "users", Comment: "用户表"}
	if got := JoinSQL(d.CommentStatements("db", tbl)); got != "ALTER TABLE `db`.`users` COMMENT = '用户表'" {
		t.Errorf("表注释不符: %s", got)
	}
	// 列注释:MySQL 不支持(需列类型),应返回空切片
	col := source.CommentInfo{TableName: "users", ColumnName: "name", Comment: "姓名"}
	if stmts := d.CommentStatements("db", col); len(stmts) != 0 {
		t.Errorf("列注释应返回空切片(已知限制),得到 %d 条: %v", len(stmts), stmts)
	}
}
