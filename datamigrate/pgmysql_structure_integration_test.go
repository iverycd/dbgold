package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

// Exercise the full structure-preview/copy/report path against an isolated
// MySQL database. The source fixture deliberately includes invalid data.
func TestPGMySQLStructureCopyIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_PGMYSQL_TARGET_DSN")
	if dsn == "" {
		t.Skip("set DBGOLD_PGMYSQL_TARGET_DSN for disposable MySQL fixtures")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, scenario := range []string{"success", "numeric", "charset", "json", "unique", "check", "seed_data", "seed_catalog"} {
		t.Run(scenario, func(t *testing.T) {
			ns := fmt.Sprintf("dbgold_structure_%d", time.Now().UnixNano())
			charset := "utf8mb4"
			if scenario == "charset" {
				charset = "latin1"
			}
			if _, err := db.ExecContext(ctx, "CREATE DATABASE `"+ns+"` CHARACTER SET "+charset); err != nil {
				t.Fatal(err)
			}
			defer db.ExecContext(context.Background(), "DROP DATABASE `"+ns+"`")
			w, err := target.NewMySQL(dsn, ns, target.ConnPoolConfig{})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			r := numericReader([][]any{{"1"}, {"2"}, {"3"}, {"4"}, {"5"}})
			table := &r.snapshot.Tables[0]
			switch scenario {
			case "numeric":
				r.rows[3][0] = "1e55"
			case "charset":
				table.Info.Columns[0] = source.ColumnInfo{Name: "id", DataType: "varchar", Length: 20, IsNullable: true}
				r.rows[3][0] = "中文😀"
			case "json":
				table.Info.Columns[0] = source.ColumnInfo{Name: "id", DataType: "json", IsNullable: true}
				r.rows[3][0] = "{invalid"
			case "unique":
				table.Indexes = []source.IndexInfo{{TableName: "MixedTable", IndexName: "ux", Columns: []string{"id"}, IsUnique: true, Method: "btree"}}
				r.rows[3][0] = "1"
			case "check":
				table.Checks = []source.PGObject{{Kind: "check", Table: "MixedTable", Name: "positive", Definition: "CHECK (id > 0)"}}
				r.rows[3][0] = "-1"
			case "seed_data", "seed_catalog":
				table.Info.Columns[0] = source.ColumnInfo{Name: "id", DataType: "int4", Extra: "auto_increment"}
				next := int64(2)
				if scenario == "seed_catalog" {
					next = 100
				}
				table.Sequences = []source.PGSequence{{SequenceInfo: source.SequenceInfo{TableName: "MixedTable", ColumnName: "id", StartValue: next}, Name: "seq", Increment: 1, Min: 1, Max: 2147483647}}
			}
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 256)}, Config{Mode: "all", Content: "both", PageSize: 2, TargetSchema: ns})
			p := m.PreflightPGMySQL(ctx)
			if !p.Ready || p.CheckScope != "structure" || p.RowsChecked != 0 || r.pages != 0 {
				t.Fatalf("preview=%+v pages=%d", p, r.pages)
			}
			report := m.Run(ctx)
			wantSuccess := scenario == "success" || strings.HasPrefix(scenario, "seed_")
			if report.HasFailures() == wantSuccess || !report.Preflight.Ready {
				t.Fatalf("report=%+v", report)
			}
			count, err := w.CountRows(ctx, "MixedTable")
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "numeric", "charset", "json":
				// The third row in batch two must not survive the fourth-row
				// failure; batch one remains committed and batch three is not read.
				if count != 2 || r.pages != 2 || report.Data.Failed != 1 {
					t.Fatal(count, r.pages, report.Data)
				}
				for _, part := range []string{"MixedTable", "id", "第 2 批", "已提交 2 行"} {
					if !strings.Contains(report.Data.Items[0].Error, part) {
						t.Fatal(report.Data.Items)
					}
				}
			case "unique", "check":
				if count != 5 || report.Indexes.Failed+report.Constraints.Failed != 1 {
					t.Fatal(count, report)
				}
			default:
				if count != 5 || len(report.RowCounts) != 1 || !report.RowCounts[0].Match {
					t.Fatal(count, report.RowCounts)
				}
				if strings.HasPrefix(scenario, "seed_") {
					result, err := db.ExecContext(ctx, "INSERT INTO `"+ns+"`.`MixedTable` () VALUES ()")
					if err != nil {
						t.Fatal(err)
					}
					id, err := result.LastInsertId()
					want := int64(6)
					if scenario == "seed_catalog" {
						want = 100
					}
					if err != nil || id != want {
						t.Fatal(id, want, err)
					}
				}
			}
		})
	}
}
