package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// This opt-in fixture runs the real Oracle reader and migration pipeline into
// DM. Dedicated DBGOLD_ schemas are required; only unique test tables are dropped.
func TestOracleDamengIntervalIntegration(t *testing.T) {
	oraDSN, dmDSN := os.Getenv("DBGOLD_INTERVAL_ORACLE_DSN"), os.Getenv("DBGOLD_INTERVAL_DM_DSN")
	oraSchema, dmSchema := os.Getenv("DBGOLD_INTERVAL_ORACLE_SCHEMA"), os.Getenv("DBGOLD_INTERVAL_DM_SCHEMA")
	if os.Getenv("DBGOLD_INTERVAL_ALLOW_DDL") != "1" || oraDSN == "" || dmDSN == "" {
		t.Skip("requires dedicated Oracle/DM instances and DBGOLD_INTERVAL_ALLOW_DDL=1")
	}
	require.True(t, strings.HasPrefix(oraSchema, "DBGOLD_"))
	require.True(t, strings.HasPrefix(dmSchema, "DBGOLD_"))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	ora, e := sql.Open("oracle", oraDSN)
	require.NoError(t, e)
	defer ora.Close()
	dm, e := sql.Open("dm", dmDSN)
	require.NoError(t, e)
	defer dm.Close()
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	var oraVersion, dmVersion string
	require.NoError(t, ora.QueryRowContext(ctx, `SELECT BANNER FROM V$VERSION WHERE BANNER LIKE 'Oracle%' AND ROWNUM=1`).Scan(&oraVersion))
	require.NoError(t, dm.QueryRowContext(ctx, `SELECT SVR_VERSION FROM V$INSTANCE`).Scan(&dmVersion))
	t.Logf("%s → %s", oraVersion, dmVersion)
	for _, kind := range []string{"number", "decimal", "month", "second", "timestampday", "microsecond", "wide_number", "wide_month"} {
		for _, content := range []string{"both", "schema_only"} {
			t.Run(kind+"/"+content, func(t *testing.T) {
				name := "IFL_" + strings.ToUpper(strconv.FormatInt(time.Now().UnixNano(), 36))
				srcRoot, dstRoot := q(oraSchema)+"."+q(name), q(dmSchema)+"."+q(name)
				keyType, rule, transition, empty, used, before, hole, future := "NUMBER(5)", "10", "0", "45", "25", "-1", "0", "65"
				if kind == "wide_number" {
					keyType = "NUMBER(10)"
				}
				if kind == "decimal" {
					keyType, rule, transition, empty, used, before, hole, future = "NUMBER(5,2)", "0.5", "0", "2.25", "1.25", "-0.25", "0.25", "3.25"
				}
				if kind == "month" {
					keyType, rule, transition, empty, used, before, hole, future = "DATE", "NUMTOYMINTERVAL(1,'MONTH')", "DATE '5000-01-01'", "DATE '5000-05-01'", "DATE '5000-03-01'", "DATE '4999-12-31'", "DATE '5000-02-01'", "DATE '5000-08-01'"
				}
				if kind == "wide_month" {
					keyType, rule, transition, empty, used, before, hole, future = "DATE", "NUMTOYMINTERVAL(1,'MONTH')", "DATE '2024-01-01'", "DATE '2024-05-01'", "DATE '2024-03-01'", "DATE '2023-12-31'", "DATE '2024-02-01'", "DATE '2024-08-01'"
				}
				if kind == "second" {
					keyType, rule, transition, empty, used, before, hole, future = "DATE", "NUMTODSINTERVAL(1,'DAY')", "DATE '9900-01-01'", "DATE '9900-01-05'", "DATE '9900-01-03'", "DATE '9899-12-31'", "DATE '9900-01-02'", "DATE '9900-01-08'"
				}
				if kind == "timestampday" {
					keyType, rule, transition, empty, used, before, hole, future = "TIMESTAMP(6)", "NUMTODSINTERVAL(1,'DAY')", "TIMESTAMP '9900-01-01 00:00:00'", "TIMESTAMP '9900-01-05 00:00:00.654321'", "TIMESTAMP '9900-01-03 00:00:00.123456'", "TIMESTAMP '9899-12-31 23:59:59.999999'", "TIMESTAMP '9900-01-02 00:00:00.123456'", "TIMESTAMP '9900-01-08 00:00:00.654321'"
				}
				if kind == "microsecond" {
					keyType, rule, transition, empty, used, before, hole, future = "TIMESTAMP(6)", "NUMTODSINTERVAL(0.000001,'SECOND')", "TIMESTAMP '2024-01-01 00:00:00'", "TIMESTAMP '2024-01-01 00:00:00.000005'", "TIMESTAMP '2024-01-01 00:00:00.000003'", "TIMESTAMP '2023-12-31 23:59:59'", "TIMESTAMP '2024-01-01 00:00:00.000001'", "TIMESTAMP '2024-01-01 00:00:00.000008'"
				}
				_, e = ora.ExecContext(ctx, "CREATE TABLE "+srcRoot+`("K" `+keyType+` NOT NULL,"V" VARCHAR2(20) NOT NULL) PARTITION BY RANGE("K") INTERVAL (`+rule+`) (PARTITION "P0" VALUES LESS THAN (`+transition+`))`)
				require.NoError(t, e)
				defer func() {
					cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
					defer stop()
					_, _ = ora.ExecContext(cleanup, "DROP TABLE "+srcRoot+" CASCADE CONSTRAINTS PURGE")
					_, _ = dm.ExecContext(cleanup, "DROP TABLE "+dstRoot+" CASCADE CONSTRAINTS")
				}()
				tx, e := ora.BeginTx(ctx, nil)
				require.NoError(t, e)
				_, e = tx.ExecContext(ctx, "INSERT INTO "+srcRoot+" VALUES("+empty+",'empty')")
				require.NoError(t, e)
				require.NoError(t, tx.Rollback())
				_, e = ora.ExecContext(ctx, "INSERT INTO "+srcRoot+" VALUES("+used+",'used')")
				require.NoError(t, e)
				_, e = ora.ExecContext(ctx, "INSERT INTO "+srcRoot+" VALUES("+before+",'before')")
				require.NoError(t, e)
				r, e := source.NewOracle(oraDSN, oraSchema, source.ConnPoolConfig{})
				require.NoError(t, e)
				defer r.Close()
				original, e := r.PartitionMetadata(ctx, name)
				require.NoError(t, e)
				blocked := kind == "number" || kind == "decimal" || kind == "microsecond" || strings.HasPrefix(kind, "wide_")
				if !blocked {
					require.Empty(t, original.Unsupported)
				}
				require.Len(t, original.Partitions, 3)
				require.NotNil(t, original.Interval)
				for _, p := range original.Partitions {
					t.Logf("Oracle leaf %s ordinal=%d automatic=%t lower=%v upper=%v", p.Name, p.Ordinal, p.IsInterval, p.Lower, p.Upper)
				}
				w, e := target.NewDaMeng(dmDSN, dmSchema, target.ConnPoolConfig{})
				require.NoError(t, e)
				defer w.Close()
				cfg := Config{Mode: "include", Filter: name, Content: content, TargetDBType: "dameng", TargetSchema: dmSchema, PageSize: 1, MaxParallel: 1, ValidateData: true, CaptureRepair: true}
				m := NewMigrator(r, w, &Job{LogCh: make(chan string, 16384)}, cfg)
				pre := m.PreflightPartitions(ctx)
				if blocked {
					require.False(t, pre.Ready, "complete key domain or precise source interval evidence must fail closed")
					report := m.Run(ctx)
					require.True(t, report.HasFailures())
					var created int
					require.NoError(t, dm.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=? AND TABLE_NAME=?`, dmSchema, name).Scan(&created))
					require.Zero(t, created)
					if kind == "number" || kind == "decimal" {
						// Establish why this seemingly portable native numeric rule
						// must be blocked: Oracle accepts the column-domain maximum,
						// DM cannot materialize its required above-precision boundary.
						_, e = dm.ExecContext(ctx, "CREATE TABLE "+dstRoot+`("K" `+keyType+` NOT NULL,"V" VARCHAR(20) NOT NULL) PARTITION BY RANGE("K") INTERVAL (`+rule+`) (PARTITION "P0" VALUES LESS THAN (`+transition+`))`)
						require.NoError(t, e)
						maximum := "99999"
						if kind == "decimal" {
							maximum = "999.99"
						}
						for idx, db := range []struct {
							conn *sql.DB
							root string
						}{{ora, srcRoot}, {dm, dstRoot}} {
							tx, e := db.conn.BeginTx(ctx, nil)
							require.NoError(t, e)
							_, insertErr := tx.ExecContext(ctx, "INSERT INTO "+db.root+" VALUES("+maximum+",'maximum')")
							_ = tx.Rollback()
							if idx == 0 {
								require.NoError(t, insertErr)
							} else {
								require.Error(t, insertErr)
								t.Logf("native numeric domain mismatch preserved as explicit block: %v", insertErr)
							}
						}
						_, e = dm.ExecContext(ctx, "DROP TABLE "+dstRoot)
						require.NoError(t, e)
						assertIntervalNumericAlternativesAreNotEquivalent(t, ctx, ora, dm, oraSchema, dmSchema, name, srcRoot, dstRoot, kind)
					}
					return
				}
				require.True(t, pre.Ready, "%+v", pre)
				report := m.Run(ctx)
				if report.HasFailures() {
					t.Fatalf("interval migration failed: tables=%+v data=%+v partitions=%+v validation=%+v", report.Tables, report.Data, report.PartitionMigration, report.DataValidation)
				}
				require.NotNil(t, report.PartitionMigration)
				require.Len(t, report.PartitionMigration.Tables, 1)
				entry := report.PartitionMigration.Tables[0]
				require.Equal(t, "succeeded", entry.Status, entry.Error)
				require.NotNil(t, report.RepairManifest)
				foundMarker := false
				for _, object := range report.RepairManifest.Objects {
					if object.Kind == "table" && object.SourceTable == name {
						for _, unit := range object.Units {
							if unit.SQL == partition.IntervalMaterializationSQL(entry.Plan) {
								require.Equal(t, "succeeded", unit.Status, unit.Error)
								foundMarker = true
							}
						}
					}
				}
				require.True(t, foundMarker, "materialization must have its own persisted successful repair unit")
				actual, e := w.InspectPartitionTable(ctx, name)
				require.NoError(t, e)
				require.NoError(t, partition.Equivalent(entry.Plan.Target, actual))
				require.Len(t, actual.Partitions, 3)
				var count int
				require.NoError(t, dm.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dstRoot).Scan(&count))
				if content == "both" {
					require.Equal(t, 2, count)
					require.NotNil(t, report.DataValidation)
				} else {
					require.Zero(t, count)
				}
				srcCounts, e := r.CountPartitionRows(ctx, name, original)
				require.NoError(t, e)
				dstCounts, e := w.CountPartitionRows(ctx, name, actual)
				require.NoError(t, e)
				for _, mapping := range entry.Plan.Mapping {
					got := dstCounts[partition.LeafKey(mapping.TargetSchema, mapping.TargetName)]
					if content == "both" {
						require.Equal(t, srcCounts[partition.LeafKey(mapping.SourceSchema, mapping.SourceName)], got)
					} else {
						require.Zero(t, got)
					}
				}
				// Re-run the authenticated repair step after content exists: no DML is
				// needed and an already complete topology must be handled read-only.
				require.NoError(t, w.MaterializePartitionIntervals(ctx, entry.Plan))
				if content == "schema_only" {
					// Recreate a missing nearer automatic leaf after a farther one
					// already exists. Native physical positions may change, bounds
					// and future routing must remain exact.
					_, e = dm.ExecContext(ctx, "ALTER TABLE "+dstRoot+" DROP PARTITION "+q(actual.Partitions[1].Name))
					require.NoError(t, e)
					require.NoError(t, w.MaterializePartitionIntervals(ctx, entry.Plan))
					repaired, e := w.InspectPartitionTable(ctx, name)
					require.NoError(t, e)
					require.NoError(t, partition.Equivalent(entry.Plan.Target, repaired))
				}
				for _, value := range []string{hole, future} {
					_, e = dm.ExecContext(ctx, "INSERT INTO "+dstRoot+" VALUES("+value+",'future')")
					require.NoError(t, e)
				}
				after, e := w.InspectPartitionTable(ctx, name)
				require.NoError(t, e)
				require.Len(t, after.Partitions, 5)
				require.Equal(t, actual.Interval, after.Interval)
				for _, p := range after.Partitions {
					if p.IsInterval {
						_, e = source.IntervalBoundaryIndex(after.Interval, p.Upper[0])
						require.NoError(t, e)
					}
				}
				_, e = dm.ExecContext(ctx, "UPDATE "+dstRoot+" SET \"K\"="+future+" WHERE \"K\"="+hole)
				require.Error(t, e, "disabled row movement must survive migration")
				maximum := "99999"
				if kind == "decimal" {
					maximum = "999.99"
				}
				if kind == "month" || kind == "second" {
					maximum = "TO_DATE('9999-12-31 23:59:59','YYYY-MM-DD HH24:MI:SS')"
				}
				if kind == "timestampday" {
					maximum = "TIMESTAMP '9999-12-31 23:59:59.999999'"
				}
				for _, db := range []struct {
					conn *sql.DB
					root string
				}{{ora, srcRoot}, {dm, dstRoot}} {
					tx, e := db.conn.BeginTx(ctx, nil)
					require.NoError(t, e)
					_, insertErr := tx.ExecContext(ctx, "INSERT INTO "+db.root+" VALUES("+maximum+",'maximum')")
					_ = tx.Rollback()
					if kind == "number" || kind == "decimal" {
						require.NoError(t, insertErr, "full numeric key domain maximum must be accepted even when the automatic upper bound exceeds key precision")
					} else {
						require.Error(t, insertErr, "automatic upper bound beyond year 9999 must be rejected by both products")
					}
				}
				t.Logf("verified %s: empty leaves, sparse gaps, root/leaf rows, native future INSERT and disabled cross-partition UPDATE", fmt.Sprint(kind, "/", content))
			})
		}
	}
}

// Widening the physical key with an inline CHECK (or SQL DOMAIN) fixes the
// maximum valid value but still changes rejected-input semantics: DM allocates
// an automatic leaf before its CHECK fails, and rollback retains that leaf.
// Keep this real-engine counterexample next to the migration fail-closed test.
func assertIntervalNumericAlternativesAreNotEquivalent(t *testing.T, ctx context.Context, ora, dm *sql.DB, oraSchema, dmSchema, name, srcRoot, dstRoot, kind string) {
	t.Helper()
	wider, step, maximum, rejected, roundedRejected, extraUpper := "NUMBER(6,0)", "10", "99999", "100000", "99999.5", "100010"
	if kind == "decimal" {
		wider, step, maximum, rejected, roundedRejected, extraUpper = "NUMBER(6,2)", "0.5", "999.99", "1000", "999.995", "1000.5"
	}
	countPartitions := func(t *testing.T, db *sql.DB, schema string, oracle bool) int {
		query := `SELECT COUNT(*) FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME=?`
		if oracle {
			query = `SELECT COUNT(*) FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=:1 AND TABLE_NAME=:2`
		}
		var count int
		require.NoError(t, db.QueryRowContext(ctx, query, schema, name).Scan(&count))
		return count
	}
	rollbackInsert := func(t *testing.T, db *sql.DB, root, value string) error {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, insertErr := tx.ExecContext(ctx, "INSERT INTO "+root+" VALUES("+value+",'probe')")
		require.NoError(t, tx.Rollback())
		return insertErr
	}
	for _, mechanism := range []string{"check", "domain"} {
		for _, value := range []string{rejected, roundedRejected} {
			t.Run(mechanism+"/"+value, func(t *testing.T) {
				domain := `"` + dmSchema + `"."D_` + name + `"`
				keyType, check := wider, `, CONSTRAINT "DOMAIN_LIMIT" CHECK("K">=-`+maximum+` AND "K"<=`+maximum+`)`
				if mechanism == "domain" {
					_, err := dm.ExecContext(ctx, "CREATE DOMAIN "+domain+" AS "+wider+" CHECK(VALUE>=-"+maximum+" AND VALUE<="+maximum+")")
					require.NoError(t, err)
					defer func() {
						_, err := dm.ExecContext(context.Background(), "DROP DOMAIN "+domain)
						require.NoError(t, err)
					}()
					keyType, check = domain, ""
				}
				_, err := dm.ExecContext(ctx, "CREATE TABLE "+dstRoot+`("K" `+keyType+` NOT NULL,"V" VARCHAR(20) NOT NULL`+check+`) PARTITION BY RANGE("K") INTERVAL(`+step+`) (PARTITION "P0" VALUES LESS THAN(0))`)
				require.NoError(t, err)
				defer func() {
					_, err := dm.ExecContext(context.Background(), "DROP TABLE "+dstRoot)
					require.NoError(t, err)
				}()
				require.NoError(t, rollbackInsert(t, dm, dstRoot, maximum), "wider storage does accept the source-domain maximum")
				require.Equal(t, 2, countPartitions(t, dm, dmSchema, false))
				sourceBefore := countPartitions(t, ora, oraSchema, true)
				require.ErrorContains(t, rollbackInsert(t, ora, srcRoot, value), "ORA-01438")
				require.Equal(t, sourceBefore, countPartitions(t, ora, oraSchema, true), "Oracle rejects before allocating a leaf")
				require.ErrorContains(t, rollbackInsert(t, dm, dstRoot, value), "-6604")
				require.Equal(t, 3, countPartitions(t, dm, dmSchema, false), "DM rejected input nevertheless creates a persistent extra leaf")
				var count int
				require.NoError(t, dm.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dstRoot).Scan(&count))
				require.Zero(t, count, "rollback removes rows but cannot remove the automatic leaf")
				rows, err := dm.QueryContext(ctx, `SELECT HIGH_VALUE FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME=?`, dmSchema, name)
				require.NoError(t, err)
				var uppers []string
				for rows.Next() {
					var upper string
					require.NoError(t, rows.Scan(&upper))
					uppers = append(uppers, strings.TrimSpace(upper))
				}
				require.NoError(t, rows.Err())
				require.NoError(t, rows.Close())
				require.Contains(t, uppers, extraUpper)
				t.Logf("%s rejects %s but retains automatic upper=%s; Oracle topology is unchanged", mechanism, value, extraUpper)
			})
		}
	}
	// Native RANGE accepts a column name, not a CAST expression that could
	// independently widen only the routing-bound representation.
	for _, expression := range []string{`CAST("K" AS ` + wider + `)`, `"K"+0`} {
		_, err := dm.ExecContext(ctx, "CREATE TABLE "+dstRoot+`("K" NUMBER(5) NOT NULL,"V" VARCHAR(20)) PARTITION BY RANGE(`+expression+`) INTERVAL(10) (PARTITION "P0" VALUES LESS THAN(0))`)
		if err == nil {
			_, _ = dm.ExecContext(context.Background(), "DROP TABLE "+dstRoot)
		}
		require.ErrorContains(t, err, "-2007", "native expression alternative must not be assumed supported")
	}
}
