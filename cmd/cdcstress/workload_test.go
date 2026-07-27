package main

import (
	"math/rand"
	"testing"
)

func TestPickTableUsesCachedSortedTables(t *testing.T) {
	tables := []TableSpec{
		{Name: "cold", Rows: 10},
		{Name: "hot", Rows: 100},
		{Name: "warm", Rows: 50},
	}
	engine := workloadEngine{tablesByRows: sortedTablesByRows(tables)}

	if engine.tablesByRows[0].Name != "hot" || engine.tablesByRows[1].Name != "warm" || engine.tablesByRows[2].Name != "cold" {
		t.Fatalf("tablesByRows is not sorted hottest-first: %+v", engine.tablesByRows)
	}
	if tables[0].Name != "cold" || tables[1].Name != "hot" || tables[2].Name != "warm" {
		t.Fatalf("sorting changed the original table order: %+v", tables)
	}

	// pickTable must use only the immutable cache. A nil state ensures a future
	// change cannot accidentally restore per-operation sorting of state.Tables.
	engine.state = nil
	if got := engine.pickTable(rand.New(rand.NewSource(1))); got.Name == "" {
		t.Fatal("pickTable returned an empty table")
	}
}

func TestPickTablePreservesHotColdDistribution(t *testing.T) {
	tables := make([]TableSpec, 300)
	for i := range tables {
		tables[i] = TableSpec{Index: i + 1, Name: "table", Rows: int64(len(tables) - i)}
	}
	engine := workloadEngine{tablesByRows: sortedTablesByRows(tables)}
	rng := rand.New(rand.NewSource(42))
	hotNames := map[int]bool{1: true, 2: true, 3: true}
	const samples = 100_000
	hot := 0
	for range samples {
		if hotNames[engine.pickTable(rng).Index] {
			hot++
		}
	}
	ratio := float64(hot) / samples
	// 80% of selections are explicitly hot, while the 20% uniform branch can
	// also select a hot table. The expected ratio is approximately 80.2%.
	if ratio < 0.79 || ratio > 0.815 {
		t.Fatalf("hot selection ratio=%f, want approximately 0.802", ratio)
	}
}

func TestPickTableDoesNotAllocatePerOperation(t *testing.T) {
	tables := make([]TableSpec, 3000)
	for i := range tables {
		tables[i] = TableSpec{Index: i + 1, Name: "table", Rows: int64(len(tables) - i)}
	}
	engine := workloadEngine{tablesByRows: sortedTablesByRows(tables)}
	rng := rand.New(rand.NewSource(42))
	allocations := testing.AllocsPerRun(1000, func() {
		_ = engine.pickTable(rng)
	})
	if allocations != 0 {
		t.Fatalf("pickTable allocations per operation=%f, want 0", allocations)
	}
}
