package main

import (
	"fmt"
	"math"
	"sort"
)

type TableKind string

const (
	kindPrimary   TableKind = "primary"
	kindComposite TableKind = "composite"
	kindUnique    TableKind = "unique"
	kindKeyless   TableKind = "keyless"
	kindWide      TableKind = "wide"
	kindMixedCase TableKind = "mixed_case"
)

type TableSpec struct {
	Index int       `json:"index"`
	Name  string    `json:"name"`
	Kind  TableKind `json:"kind"`
	Rows  int64     `json:"rows"`
}

func buildProfile(cfg ProfileConfig, runID string) ([]TableSpec, error) {
	rows, err := allocateRows(cfg.TableCount, cfg.TotalRows, cfg.MaxRowsPerTable)
	if err != nil {
		return nil, err
	}
	runToken := shortToken(runID)
	tables := make([]TableSpec, cfg.TableCount)
	kinds := []TableKind{kindPrimary, kindComposite, kindUnique, kindKeyless, kindWide, kindMixedCase}
	for i := range tables {
		name := fmt.Sprintf("%s%s_%06d", objectPrefix, runToken, i+1)
		kind := kinds[i%len(kinds)]
		if kind == kindMixedCase {
			name = fmt.Sprintf("%s%s_Mixed_%06d", objectPrefix, runToken, i+1)
		}
		tables[i] = TableSpec{Index: i + 1, Name: name, Kind: kind, Rows: rows[i]}
	}
	return tables, nil
}

func shortToken(runID string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 0, 12)
	for i := 0; i < len(runID) && len(out) < 12; i++ {
		b := runID[i]
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		for j := range alphabet {
			if b == alphabet[j] {
				out = append(out, b)
				break
			}
		}
	}
	if len(out) == 0 {
		return "run"
	}
	return string(out)
}

// allocateRows uses a deterministic Zipf-like distribution. Every 97th table
// is intentionally empty, while a small number of tables receive most rows.
func allocateRows(tableCount int, total, maxPerTable int64) ([]int64, error) {
	if tableCount < 1 || total < 0 {
		return nil, fmt.Errorf("invalid table count or row total")
	}
	rows := make([]int64, tableCount)
	if total == 0 {
		return rows, nil
	}
	if maxPerTable <= 0 {
		maxPerTable = total
	}
	nonEmpty := 0
	weights := make([]float64, tableCount)
	var sum float64
	for i := 0; i < tableCount; i++ {
		if (i+1)%97 == 0 {
			continue
		}
		weights[i] = 1 / math.Pow(float64(i+8), 0.82)
		sum += weights[i]
		nonEmpty++
	}
	if int64(nonEmpty)*maxPerTable < total {
		return nil, fmt.Errorf("total_rows exceeds max_rows_per_table capacity")
	}
	var assigned int64
	for i, weight := range weights {
		if weight == 0 {
			continue
		}
		value := int64(math.Floor(float64(total) * weight / sum))
		if value > maxPerTable {
			value = maxPerTable
		}
		rows[i] = value
		assigned += value
	}
	// Fill the remainder in hottest-first rounds while respecting the cap.
	for assigned < total {
		progress := false
		for i := 0; i < tableCount && assigned < total; i++ {
			if weights[i] == 0 || rows[i] >= maxPerTable {
				continue
			}
			rows[i]++
			assigned++
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("unable to allocate requested rows")
		}
	}
	return rows, nil
}

func sortedTablesByRows(tables []TableSpec) []TableSpec {
	out := append([]TableSpec(nil), tables...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rows == out[j].Rows {
			return out[i].Name < out[j].Name
		}
		return out[i].Rows > out[j].Rows
	})
	return out
}
