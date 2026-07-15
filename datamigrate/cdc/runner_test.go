package cdc

import (
	"testing"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
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

func TestPositionReachedFile(t *testing.T) {
	tests := []struct {
		name      string
		applied   Position
		requested Position
		want      bool
	}{
		{name: "same position", applied: Position{File: "mysql-bin.000010", Pos: 120}, requested: Position{File: "mysql-bin.000010", Pos: 120}, want: true},
		{name: "ahead in file", applied: Position{File: "mysql-bin.000010", Pos: 121}, requested: Position{File: "mysql-bin.000010", Pos: 120}, want: true},
		{name: "behind in file", applied: Position{File: "mysql-bin.000010", Pos: 119}, requested: Position{File: "mysql-bin.000010", Pos: 120}, want: false},
		{name: "later rotated file", applied: Position{File: "mysql-bin.000011", Pos: 4}, requested: Position{File: "mysql-bin.000010", Pos: 999}, want: true},
		{name: "earlier rotated file", applied: Position{File: "mysql-bin.000009", Pos: 999}, requested: Position{File: "mysql-bin.000010", Pos: 4}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := PositionReached(test.applied, test.requested); got != test.want {
				t.Fatalf("PositionReached()=%v, want %v", got, test.want)
			}
		})
	}
}

func TestPositionReachedAndEquivalentGTID(t *testing.T) {
	const sid = "3e11fa47-71ca-11e1-9e33-c80aa9429562"
	applied := Position{GTID: sid + ":1-10"}
	if !PositionReached(applied, Position{GTID: sid + ":1-9"}) {
		t.Fatal("superset GTID should have reached requested set")
	}
	if PositionReached(applied, Position{GTID: sid + ":1-11"}) {
		t.Fatal("smaller GTID set must not report caught up")
	}
	if !PositionEquivalent(Position{GTID: sid + ":1-5:7-10"}, Position{GTID: sid + ":7-10:1-5"}) {
		t.Fatal("logically equal GTID sets should be equivalent")
	}
	if PositionEquivalent(Position{File: "mysql-bin.000001", Pos: 10, GTID: sid + ":1-10"}, Position{File: "mysql-bin.000001", Pos: 11, GTID: sid + ":1-10"}) {
		t.Fatal("different file positions must not be equivalent even when GTID sets match")
	}
}

func TestRegistryCutoverLifecycle(t *testing.T) {
	r := &JobRegistry{jobs: map[string]*control{}}
	ctx, err := r.Register("job")
	if err != nil || ctx == nil {
		t.Fatalf("register failed: %v", err)
	}
	want := Position{File: "mysql-bin.000003", Pos: 88}
	if !r.RequestCutover("job", want) {
		t.Fatal("request cutover failed")
	}
	got, ok := r.CutoverBoundary("job")
	if !ok || got != want {
		t.Fatalf("cutover boundary=%+v, %v", got, ok)
	}
	if r.Cancel("job", "pause") {
		t.Fatal("pause must not race through an active cutover")
	}
	if !r.ClearCutover("job") {
		t.Fatal("clear cutover failed")
	}
	if _, ok = r.CutoverBoundary("job"); ok {
		t.Fatal("cutover boundary still exists after clear")
	}
	if !r.Cancel("job", "pause") {
		t.Fatal("pause request failed")
	}
	if r.RequestCutover("job", want) {
		t.Fatal("cutover must not race through an active pause")
	}
	if action := r.Remove("job"); action != "pause" {
		t.Fatalf("remove action=%q", action)
	}
}

func TestSameColumnSet(t *testing.T) {
	if !sameColumnSet([]string{"tenant_id", "id"}, []string{"id", "tenant_id"}) {
		t.Fatal("same composite key columns should match regardless of order")
	}
	if sameColumnSet([]string{"id"}, []string{"other"}) {
		t.Fatal("different columns must not match")
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
