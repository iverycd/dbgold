package main

import (
	"testing"
)

func TestAllocateRowsExactAndSkewed(t *testing.T) {
	rows, err := allocateRows(200, 1_000_000, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, value := range rows {
		total += value
		if value > 100_000 {
			t.Fatalf("row cap exceeded: %d", value)
		}
	}
	if total != 1_000_000 {
		t.Fatalf("total=%d", total)
	}
	if rows[96] != 0 || rows[193] != 0 {
		t.Fatalf("expected deterministic empty tables: %d %d", rows[96], rows[193])
	}
	if rows[0] <= rows[50] {
		t.Fatalf("distribution is not skewed: first=%d later=%d", rows[0], rows[50])
	}
}

func TestAllocateRowsRejectsInsufficientCapacity(t *testing.T) {
	if _, err := allocateRows(10, 101, 10); err == nil {
		t.Fatal("expected capacity error")
	}
}

func TestBuildProfileIsDeterministicAndSafe(t *testing.T) {
	cfg := ProfileConfig{TableCount: 12, TotalRows: 1200}
	a, err := buildProfile(cfg, "run_20260722T120000Z")
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildProfile(cfg, "run_20260722T120000Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 12 || a[0] != b[0] {
		t.Fatalf("profile is not deterministic: %#v %#v", a, b)
	}
	for _, table := range a {
		if len(table.Name) > 64 || table.Name[:3] != objectPrefix {
			t.Fatalf("unsafe table name %q", table.Name)
		}
	}
}
