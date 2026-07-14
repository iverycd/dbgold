package cdc

import (
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"testing"
)

func TestPrimaryKeyChanged(t *testing.T) {
	table := &TableInfo{Columns: []string{"tenant", "id", "name"}, PrimaryKey: []int{0, 1}}
	if primaryKeyChanged(table, []any{1, 2, "a"}, []any{1, 2, "b"}) {
		t.Fatal("non-key update reported as key change")
	}
	if !primaryKeyChanged(table, []any{1, 2, "a"}, []any{1, 3, "a"}) {
		t.Fatal("composite key change not detected")
	}
}

func TestMergeGTID(t *testing.T) {
	base := "3E11FA47-71CA-11E1-9E33-C80AA9429562:1-5"
	got := mergeGTID(base, "3e11fa47-71ca-11e1-9e33-c80aa9429562:6")
	set, err := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, got)
	want, _ := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, "3e11fa47-71ca-11e1-9e33-c80aa9429562:1-6")
	if err != nil || !set.Equal(want) {
		t.Fatalf("merged GTID invalid: %q, %v", got, err)
	}
}

func TestDDLPredicate(t *testing.T) {
	for _, q := range []string{"ALTER TABLE users ADD x int", " create index ix on t(id)", "TRUNCATE TABLE t"} {
		if !ddlPrefix.MatchString(q) {
			t.Fatalf("DDL not detected: %s", q)
		}
	}
	if ddlPrefix.MatchString("UPDATE users SET name='x'") {
		t.Fatal("DML detected as DDL")
	}
}
