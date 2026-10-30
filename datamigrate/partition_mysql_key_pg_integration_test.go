package datamigrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

func TestMySQLKeyPostgresPartitionIntegration(t *testing.T) {
	myDSN, pgDSN := os.Getenv("DBGOLD_PARTITION_MYSQL_DSN"), os.Getenv("DBGOLD_PARTITION_PG_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || myDSN == "" || pgDSN == "" {
		t.Skip("requires isolated MySQL/PG fixtures and explicit DDL flag")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	my, err := sql.Open("mysql", myDSN)
	require.NoError(t, err)
	defer my.Close()
	pg, err := sql.Open("postgres", pgDSN)
	require.NoError(t, err)
	defer pg.Close()
	var database, version string
	require.NoError(t, my.QueryRowContext(ctx, "SELECT DATABASE(),VERSION()").Scan(&database, &version))
	t.Logf("KEY exact native-expression routing from MySQL %s", version)
	type field struct {
		typ      string
		min, max int64
	}
	for _, tc := range []struct {
		name, method       string
		algorithm, buckets int
		fields             []field
	}{
		{"tiny_algorithm1", "KEY", 1, 3, []field{{"tinyint", -128, 127}}},
		{"unsigned_tiny_algorithm2_linear", "LINEAR KEY", 2, 3, []field{{"tinyint unsigned", 0, 255}}},
		{"small_algorithm2", "KEY", 2, 5, []field{{"smallint", -32768, 32767}}},
		{"medium_algorithm2", "KEY", 2, 3, []field{{"mediumint", -8388608, 8388607}}},
		{"int_algorithm1_linear", "LINEAR KEY", 1, 3, []field{{"int", -2147483648, 2147483647}}},
		{"int_algorithm2", "KEY", 2, 5, []field{{"int", -2147483648, 2147483647}}},
		{"multi_algorithm1", "KEY", 1, 3, []field{{"smallint", -32768, 32767}, {"smallint", -32768, 32767}}},
		{"multi_algorithm2_linear", "LINEAR KEY", 2, 5, []field{{"smallint", -32768, 32767}, {"smallint unsigned", 0, 65535}}},
		{"nested_range_linear", "LINEAR KEY", 2, 3, []field{{"smallint", -32768, 32767}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "dbgold_keypg_" + strconv.FormatInt(time.Now().UnixNano(), 36)
			mq := func(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
			pq := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
			_, err := pg.ExecContext(ctx, "CREATE SCHEMA "+pq(name))
			require.NoError(t, err)
			defer pg.ExecContext(context.Background(), "DROP SCHEMA "+pq(name)+" CASCADE")
			srcTable, dstTable := mq(database)+"."+mq(name), pq(name)+"."+pq(name)
			var defs, myCols, pgCols []string
			defs = append(defs, "`Id` int NOT NULL")
			myCols = append(myCols, "`Id`")
			pgCols = append(pgCols, `"Id"`)
			for i, f := range tc.fields {
				column := string(rune('A' + i))
				defs = append(defs, mq(column)+" "+f.typ+" NULL")
				myCols = append(myCols, mq(column))
				pgCols = append(pgCols, pq(column))
			}
			var parts, partDefs []string
			for i := 0; i < tc.buckets; i++ {
				p := fmt.Sprintf("MiXeD_%d", i)
				parts = append(parts, p)
				partDefs = append(partDefs, "PARTITION "+mq(p))
			}
			// Reverse declared KEY order: MySQL still hashes physical field order.
			var keyColumns []string
			for i := len(tc.fields); i > 0; i-- {
				keyColumns = append(keyColumns, myCols[i])
			}
			create := "CREATE TABLE " + srcTable + " (" + strings.Join(defs, ",") + ") ENGINE=InnoDB PARTITION BY " + tc.method + " ALGORITHM=" + strconv.Itoa(tc.algorithm) + " (" + strings.Join(keyColumns, ",") + ") (" + strings.Join(partDefs, ",") + ")"
			if tc.name == "nested_range_linear" {
				parts = nil
				var parents []string
				for parent := 0; parent < 2; parent++ {
					var subs []string
					for bucket := 0; bucket < tc.buckets; bucket++ {
						p := fmt.Sprintf("Group%d_Leaf%d", parent, bucket)
						parts = append(parts, p)
						subs = append(subs, "SUBPARTITION "+mq(p))
					}
					bound := "300"
					if parent == 1 {
						bound = "MAXVALUE"
					}
					parents = append(parents, "PARTITION "+mq(fmt.Sprintf("Parent%d", parent))+" VALUES LESS THAN ("+bound+") ("+strings.Join(subs, ",")+")")
				}
				create = "CREATE TABLE " + srcTable + " (" + strings.Join(defs, ",") + ") ENGINE=InnoDB PARTITION BY RANGE (`Id`) SUBPARTITION BY " + tc.method + " ALGORITHM=" + strconv.Itoa(tc.algorithm) + " (" + strings.Join(keyColumns, ",") + ") (" + strings.Join(parents, ",") + ")"
			}
			_, err = my.ExecContext(ctx, create)
			require.NoError(t, err)
			defer my.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+srcTable)
			var rows [][]string
			add := func(values ...string) {
				row := []string{strconv.Itoa(len(rows) + 1)}
				row = append(row, values...)
				rows = append(rows, row)
			}
			if len(tc.fields) == 1 {
				f := tc.fields[0]
				add("NULL")
				if f.max-f.min == 255 {
					for v := f.min; v <= f.max; v++ {
						add(strconv.FormatInt(v, 10))
					}
				} else {
					for _, v := range []int64{f.min, f.min + 1, -1, 0, 1, 31, 32, 33, 65, 97, 255, 256, 257, 0x2020, 0x202020, 0x20202020, f.max - 1, f.max} {
						if v >= f.min && v <= f.max {
							add(strconv.FormatInt(v, 10))
						}
					}
					rng := rand.New(rand.NewSource(836))
					for i := 0; i < 512; i++ {
						add(strconv.FormatInt(f.min+rng.Int63n(f.max-f.min+1), 10))
					}
				}
			} else {
				for _, a := range []string{"NULL", strconv.FormatInt(tc.fields[0].min, 10), "0", "32", "8224", strconv.FormatInt(tc.fields[0].max, 10)} {
					for _, b := range []string{"NULL", "0", "32", "8224", strconv.FormatInt(tc.fields[1].max, 10)} {
						add(a, b)
					}
				}
				rng := rand.New(rand.NewSource(836))
				for i := 0; i < 512; i++ {
					add(strconv.FormatInt(tc.fields[0].min+rng.Int63n(tc.fields[0].max-tc.fields[0].min+1), 10), strconv.FormatInt(tc.fields[1].min+rng.Int63n(tc.fields[1].max-tc.fields[1].min+1), 10))
				}
			}
			var values []string
			for _, row := range rows {
				values = append(values, "("+strings.Join(row, ",")+")")
			}
			_, err = my.ExecContext(ctx, "INSERT INTO "+srcTable+" VALUES "+strings.Join(values, ","))
			require.NoError(t, err)
			r, err := source.NewMySQL(myDSN, database, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			w, err := target.NewPostgres(pgDSN, name, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer w.Close()
			job := &Job{LogCh: make(chan string, 16384)}
			cfg := Config{Mode: "include", Filter: name, Content: "both", TargetDBType: "postgres", TargetSchema: name, PageSize: 128, MaxParallel: 1, ValidateData: true, CaptureRepair: true}
			preview := NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
			require.True(t, preview.Ready, "issues=%+v fatal=%s", preview.Issues, preview.FatalError)
			report := NewMigrator(r, w, job, cfg).Run(ctx)
			if report.HasFailures() {
				if report.PartitionMigration != nil {
					for _, entry := range report.PartitionMigration.Tables {
						if entry.Plan == nil {
							continue
						}
						actual, _ := w.InspectPartitionTable(ctx, name)
						f, e := os.CreateTemp("", "dbgold-keypg-diagnostic-*.json")
						if e == nil {
							json.NewEncoder(f).Encode(map[string]interface{}{"expected": entry.Plan.Target, "actual": actual})
							f.Close()
							t.Log("canonical diagnostic:", f.Name())
						}
					}
				}
				t.Fatalf("KEY migration failed: partition=%+v tables=%+v data=%+v", report.PartitionMigration, report.Tables, report.Data)
			}
			plan := report.PartitionMigration.Tables[0].Plan
			expression := plan.Target.Keys[0].Expression
			if expression == "" && plan.Target.Partitions[0].Subpartition != nil {
				expression = plan.Target.Partitions[0].Subpartition.Keys[0].Expression
			}
			t.Logf("%d source rows; native expression %d bytes", len(rows), len(expression))
			require.Len(t, plan.KeyColumns, len(tc.fields), "nested parents must deduplicate identical frozen key columns")
			read := func(db *sql.DB, query string) []string {
				result, err := db.QueryContext(ctx, query)
				require.NoError(t, err)
				defer result.Close()
				var out []string
				for result.Next() {
					values := make([]sql.NullInt64, len(tc.fields)+1)
					args := make([]interface{}, len(values))
					for i := range values {
						args[i] = &values[i]
					}
					require.NoError(t, result.Scan(args...))
					row := make([]string, len(values))
					for i, v := range values {
						row[i] = "NULL"
						if v.Valid {
							row[i] = strconv.FormatInt(v.Int64, 10)
						}
					}
					out = append(out, strings.Join(row, "/"))
				}
				require.NoError(t, result.Err())
				return out
			}
			compare := func(multiplier int) {
				for _, p := range parts {
					a := read(my, "SELECT "+strings.Join(myCols, ",")+" FROM "+srcTable+" PARTITION ("+mq(p)+") ORDER BY `Id`")
					b := read(pg, "SELECT "+strings.Join(pgCols, ",")+" FROM "+pq(name)+"."+pq(p)+` ORDER BY "Id"`)
					var want []string
					for i := 0; i < multiplier; i++ {
						want = append(want, a...)
					}
					require.ElementsMatch(t, want, b, "native leaf %s", p)
				}
			}
			compare(1)
			for _, destination := range []struct {
				db    *sql.DB
				table string
				quote func(string) string
			}{{my, srcTable, mq}, {pg, dstTable, pq}} {
				_, err = destination.db.ExecContext(ctx, "UPDATE "+destination.table+" SET "+destination.quote("A")+"="+strconv.FormatInt(tc.fields[0].max, 10)+" WHERE "+destination.quote("Id")+"=1")
				require.NoError(t, err)
				future := []string{"1000000"}
				for _, f := range tc.fields {
					future = append(future, strconv.FormatInt(f.min, 10))
				}
				_, err = destination.db.ExecContext(ctx, "INSERT INTO "+destination.table+" VALUES ("+strings.Join(future, ",")+")")
				require.NoError(t, err)
			}
			compare(1)
			for _, bad := range []int64{tc.fields[0].min - 1, tc.fields[0].max + 1} {
				badRow := []string{"2000000", strconv.FormatInt(bad, 10)}
				if tc.name == "nested_range_linear" {
					badRow[0] = "2"
				} // Within first parent's RANGE, so the KEY guard must reject it.
				for i := 1; i < len(tc.fields); i++ {
					badRow = append(badRow, "0")
				}
				_, err = pg.ExecContext(ctx, "INSERT INTO "+dstTable+" VALUES ("+strings.Join(badRow, ",")+")")
				require.Error(t, err, "root must reject values outside the source integer domain")
				_, err = pg.ExecContext(ctx, "INSERT INTO "+pq(name)+"."+pq(parts[0])+" VALUES ("+strings.Join(badRow, ",")+")")
				require.Error(t, err, "direct leaf writes must enforce the same native expression guard")
				_, err = pg.ExecContext(ctx, "UPDATE "+dstTable+` SET "A"=`+strconv.FormatInt(bad, 10)+` WHERE "Id"=1`)
				require.Error(t, err, "UPDATE cannot route an out-of-domain value")
			}
			compare(1)
			cfg.Content = "data_only"
			cfg.ValidateData = false
			cfg.CaptureRepair = false
			appended := NewMigrator(r, w, job, cfg).Run(ctx)
			require.False(t, appended.HasFailures(), "partition=%+v data=%+v", appended.PartitionMigration, appended.Data)
			compare(2)
			if tc.name == "tiny_algorithm1" {
				_, err = pg.ExecContext(ctx, "ALTER TABLE "+dstTable+` ADD CONSTRAINT extra_domain CHECK ("Id">=0)`)
				require.NoError(t, err)
				blocked := NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
				require.False(t, blocked.Ready, "additional target CHECK may shrink valid future source rows")
				_, err = pg.ExecContext(ctx, "ALTER TABLE "+dstTable+` DROP CONSTRAINT extra_domain`)
				require.NoError(t, err)
				_, err = my.ExecContext(ctx, "ALTER TABLE "+srcTable+" ADD CONSTRAINT `source_domain` CHECK (`Id`>=0)")
				require.NoError(t, err)
				hasChecks, checkErr := r.PartitionHasChecks(ctx, name)
				require.NoError(t, checkErr)
				require.True(t, hasChecks)
				blocked = NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
				require.False(t, blocked.Ready, "source CHECK requires a complete constraint migration model")
				var count int
				require.NoError(t, pg.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dstTable).Scan(&count))
				require.Equal(t, 2*(len(rows)+1), count)
			}
			if tc.name == "nested_range_linear" {
				// NULL hashes to residue 2; shrink only that physical leaf while
				// the root and intermediate parent retain nullable integer keys.
				leaf := pq(name) + "." + pq(parts[2])
				_, err = pg.ExecContext(ctx, "ALTER TABLE "+leaf+` ALTER COLUMN "A" SET NOT NULL`)
				require.NoError(t, err)
				actual, err := w.InspectPartitionTable(ctx, name)
				require.NoError(t, err)
				require.NoError(t, partition.Equivalent(plan.Target, actual))
				blocked := NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
				require.False(t, blocked.Ready, "leaf-only NOT NULL drift must block nested KEY append")
				_, err = pg.ExecContext(ctx, "ALTER TABLE "+leaf+` ALTER COLUMN "A" DROP NOT NULL`)
				require.NoError(t, err)
			}
			if tc.name == "unsigned_tiny_algorithm2_linear" {
				// Current data contains no NULL after removal, so only explicit
				// frozen column-domain verification can detect this future gap.
				_, err = my.ExecContext(ctx, "DELETE FROM "+srcTable+" WHERE `A` IS NULL")
				require.NoError(t, err)
				_, err = pg.ExecContext(ctx, "DELETE FROM "+dstTable+` WHERE "A" IS NULL`)
				require.NoError(t, err)
				_, err = pg.ExecContext(ctx, "ALTER TABLE "+dstTable+` ALTER COLUMN "A" SET NOT NULL`)
				require.NoError(t, err)
				actual, err := w.InspectPartitionTable(ctx, name)
				require.NoError(t, err)
				require.NoError(t, partition.Equivalent(plan.Target, actual), "partition metadata alone cannot detect NOT NULL drift")
				blocked := NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
				require.False(t, blocked.Ready, "target NOT NULL must not shrink a nullable source KEY domain")
				_, err = pg.ExecContext(ctx, "ALTER TABLE "+dstTable+` ALTER COLUMN "A" DROP NOT NULL`)
				require.NoError(t, err)

				// Preserve the exact routing expression while replacing its input
				// column with numeric: casts hide the new rounding domain from the
				// ordinary expression fingerprint, so storage must be checked too.
				numeric := append([]dialect.Statement(nil), plan.Statements...)
				changed := false
				for i := range numeric {
					if strings.HasPrefix(strings.TrimSpace(numeric[i].SQL), "CREATE TABLE ") && strings.Contains(numeric[i].SQL, `"A" int`) {
						numeric[i].SQL = strings.Replace(numeric[i].SQL, `"A" int`, `"A" numeric`, 1)
						changed = true
					}
				}
				require.True(t, changed)
				require.NoError(t, w.CreateTable(ctx, numeric))
				_, err = pg.ExecContext(ctx, "INSERT INTO "+dstTable+" VALUES (0,0.25)")
				require.NoError(t, err)
				actual, err = w.InspectPartitionTable(ctx, name)
				require.NoError(t, err)
				require.NoError(t, partition.Equivalent(plan.Target, actual), "bigint CAST can hide a changed numeric input column")
				blocked = NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
				require.False(t, blocked.Ready, "numeric replacement must block KEY append even with matching partition expression")
				var numericRows int
				require.NoError(t, pg.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dstTable).Scan(&numericRows))
				require.Equal(t, 1, numericRows)

				// Remove only the guard while retaining the same source hash, buckets,
				// leaf names and types. Catalog verification must catch the widening.
				original := plan.Target.Keys[0].Expression
				unguarded := strings.TrimSuffix(strings.TrimPrefix(original, "("), ")")
				start := strings.Index(unguarded, " ELSE ")
				require.Greater(t, start, 0)
				unguarded = strings.TrimSuffix(unguarded[start+6:], " END")
				modified := append([]dialect.Statement(nil), plan.Statements...)
				found := false
				for i := range modified {
					if strings.Contains(modified[i].SQL, original) {
						modified[i].SQL = strings.Replace(modified[i].SQL, original, "("+unguarded+")", 1)
						found = true
					}
				}
				require.True(t, found)
				require.NoError(t, w.CreateTable(ctx, modified))
				_, err = pg.ExecContext(ctx, "INSERT INTO "+dstTable+" VALUES (0,-1)")
				require.NoError(t, err, "tampered table really accepts formerly forbidden values")
				blocked = NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
				require.False(t, blocked.Ready, "removing native expression guard must block append")
				actual, err = w.InspectPartitionTable(ctx, name)
				require.NoError(t, err)
				require.Error(t, partition.Equivalent(plan.Target, actual))
				var count int
				require.NoError(t, pg.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dstTable).Scan(&count))
				require.Equal(t, 1, count)
			}
		})
	}
}
