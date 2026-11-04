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
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// seedMySQLScanFixtures creates deterministic data in the caller's fresh, disposable
// schema. Seeding and verification stay outside the migration timing interval.
func seedMySQLScanFixtures(t *testing.T, db *sql.DB, rows int) []string {
	t.Helper()
	require.Positive(t, rows)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, query, args...)
		require.NoError(t, err, query)
	}

	exec("CREATE TABLE scan_dense (id BIGINT PRIMARY KEY, tag VARCHAR(32) NOT NULL) ENGINE=InnoDB")
	exec("INSERT INTO scan_dense VALUES (0,'row-0')")
	for size := 1; size < rows; size += min(size, rows-size) {
		count := min(size, rows-size)
		exec("INSERT INTO scan_dense SELECT id+?, CONCAT('row-',id+?) FROM scan_dense WHERE id<?", size, size, count)
	}

	// Most keys form one tight cluster while the tail has large gaps. Explicit
	// signed extrema exercise boundary arithmetic even on a tiny smoke dataset.
	exec("CREATE TABLE scan_sparse LIKE scan_dense")
	sparseRows := max(1, rows/4)
	exec(`INSERT INTO scan_sparse SELECT CASE WHEN id<? THEN -1000000000+id
		ELSE 1000000000+id*104729 END, tag FROM scan_dense WHERE id<?`, sparseRows*3/4, sparseRows)
	exec("INSERT INTO scan_sparse VALUES (-9223372036854775808,'min'),(9223372036854775807,'max')")

	// All these unsigned keys are representable by the current GaussDB BIGINT
	// mapping. Full uint64 coverage belongs in the source-reader tests.
	exec("CREATE TABLE scan_unsigned (id BIGINT UNSIGNED PRIMARY KEY, tag VARCHAR(32) NOT NULL) ENGINE=InnoDB")
	exec("INSERT INTO scan_unsigned SELECT id,tag FROM scan_dense WHERE id<?", min(rows, 40003))
	exec("INSERT INTO scan_unsigned VALUES (9223372036854775806,'high-1'),(9223372036854775807,'high')")

	exec("CREATE TABLE scan_wide (id BIGINT PRIMARY KEY, body TEXT, tag VARCHAR(32) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	exec(`INSERT INTO scan_wide SELECT id, IF(MOD(id,17)=0,NULL,
		REPEAT(CONCAT('扫描-',id,'-'), 50+MOD(id,137))), tag FROM scan_dense WHERE id<?`, min(rows, 40003))

	exec("CREATE TABLE scan_composite (tenant INT NOT NULL, seq BIGINT NOT NULL, tag VARCHAR(32) NOT NULL, PRIMARY KEY(tenant,seq)) ENGINE=InnoDB")
	exec("INSERT INTO scan_composite SELECT MOD(id,11), id DIV 11,tag FROM scan_dense WHERE id<?", min(rows, 40003))

	// Genuine duplicate and all-NULL rows make it possible to detect validation
	// that compares sets instead of multisets when there is no primary key.
	exec("CREATE TABLE scan_no_pk (n INT, tag VARCHAR(32)) ENGINE=InnoDB")
	exec(`INSERT INTO scan_no_pk SELECT IF(MOD(id,11)=0,NULL,MOD(id,13)),
		IF(MOD(id,11)=0,NULL,CONCAT('dup-',MOD(id,7))) FROM scan_dense WHERE id<?`, min(rows, 40003))

	exec("CREATE TABLE scan_empty LIKE scan_dense")
	exec("CREATE TABLE scan_single LIKE scan_dense")
	exec("INSERT INTO scan_single VALUES (-7,'only')")

	names := []string{"scan_dense", "scan_sparse", "scan_unsigned", "scan_wide", "scan_composite", "scan_no_pk", "scan_empty", "scan_single"}
	expected := []int{rows, sparseRows + 2, min(rows, 40003) + 2, min(rows, 40003), min(rows, 40003), min(rows, 40003), 0, 1}
	for i, table := range names {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n))
		require.Equal(t, expected[i], n, table)
	}
	return names
}

// A target CHECK fails in the middle of a two-row COPY. It must roll back the
// entire batch while allowing already committed batches and the next table.
func TestMySQLScanFailureIntegration(t *testing.T) {
	sourceDSN := os.Getenv("DBGOLD_MYSQL_SCAN_SOURCE_DSN")
	targetDSN := os.Getenv("DBGOLD_MYSQL_SCAN_GAUSS_DSN")
	if sourceDSN == "" || targetDSN == "" {
		t.Skip("requires dedicated MySQL and GaussDB test instances")
	}
	admin, err := sql.Open("mysql", sourceDSN)
	require.NoError(t, err)
	defer admin.Close()
	dst, err := sql.Open("opengauss", targetDSN)
	require.NoError(t, err)
	defer dst.Close()
	exec := func(t *testing.T, db *sql.DB, query string) {
		t.Helper()
		_, err := db.ExecContext(t.Context(), query)
		require.NoError(t, err, query)
	}
	ns := fmt.Sprintf("dbgold_fail_%d", time.Now().UnixNano())
	exec(t, admin, "CREATE DATABASE "+ns)
	defer exec(t, admin, "DROP DATABASE "+ns)
	exec(t, admin, "CREATE TABLE "+ns+".bad (id BIGINT PRIMARY KEY) ENGINE=InnoDB")
	exec(t, admin, "CREATE TABLE "+ns+".good LIKE "+ns+".bad")
	exec(t, admin, "INSERT INTO "+ns+".bad VALUES (1),(2),(3),(4),(5),(6),(7),(8),(9),(10),(11),(12),(13),(14),(15),(16),(17),(18),(19),(20),(21),(22),(23),(24)")
	exec(t, admin, "INSERT INTO "+ns+".good VALUES (101),(102),(103)")
	cfg, err := mysql.ParseDSN(sourceDSN)
	require.NoError(t, err)
	cfg.DBName = ns
	for _, strategy := range []string{"offset", "stream", "range"} {
		t.Run(strategy, func(t *testing.T) {
			schema := ns + "_" + strategy
			exec(t, dst, "CREATE SCHEMA "+schema)
			defer exec(t, dst, "DROP SCHEMA "+schema+" CASCADE")
			exec(t, dst, "CREATE TABLE "+schema+".bad (id BIGINT PRIMARY KEY, CONSTRAINT scan_reject_8 CHECK (id <> 8))")
			exec(t, dst, "CREATE TABLE "+schema+".good (id BIGINT PRIMARY KEY)")
			reader, err := source.NewMySQL(cfg.FormatDSN(), ns, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer reader.Close()
			writer, err := target.NewGaussDB(targetDSN, schema, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer writer.Close()
			job := &Job{LogCh: make(chan string, 512)}
			report := NewMigrator(reader, writer, job, Config{MySQLReadStrategy: strategy, Mode: "all", Content: "data_only", TargetDBType: "gaussdb", TargetSchema: schema, PageSize: 2, MaxParallel: 1, IntraTableParallel: 2}).Run(t.Context())
			require.Equal(t, 1, report.Data.Success, "%+v", report.Data)
			require.Equal(t, 1, report.Data.Failed)
			require.Len(t, report.Data.Items, 1)
			require.Equal(t, "bad", report.Data.Items[0].Name)
			require.Contains(t, report.Data.Items[0].Error, "scan_reject_8")
			var saved, invalid, independent int
			require.NoError(t, dst.QueryRow("SELECT COUNT(*), COUNT(CASE WHEN id IN (7,8) THEN 1 END) FROM "+schema+".bad").Scan(&saved, &invalid))
			require.Positive(t, saved, "previously committed data should be preserved")
			require.Zero(t, invalid, "both rows in the rejected batch must roll back")
			require.NoError(t, dst.QueryRow("SELECT COUNT(*) FROM "+schema+".good WHERE id IN (101,102,103)").Scan(&independent))
			require.Equal(t, 3, independent)
			require.Len(t, report.RowCounts, 1)
			require.Equal(t, "good", report.RowCounts[0].Table)
			require.True(t, report.RowCounts[0].Match)
			t.Logf("%s: %d earlier/independent-range rows retained, both rows of invalid COPY batch absent, next table copied %d rows", strategy, saved, independent)
		})
	}
}

// The source supports BIGINT UNSIGNED beyond the target dialect's BIGINT.
// Check the actual MySQL comparison and decoding across range boundaries here.
func TestMySQLScanUnsignedIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_MYSQL_SCAN_SOURCE_DSN")
	if dsn == "" {
		t.Skip("requires a dedicated MySQL test instance")
	}
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	defer db.Close()
	exec := func(query string) {
		t.Helper()
		_, err := db.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	ns := fmt.Sprintf("dbgold_edge_%d", time.Now().UnixNano())
	exec("CREATE DATABASE " + ns)
	defer exec("DROP DATABASE " + ns)
	exec("CREATE TABLE " + ns + ".UnsignedEdge (MiXed BIGINT UNSIGNED PRIMARY KEY)")
	expected := []string{"0", "9223372036854775806", "9223372036854775807", "9223372036854775808", "9223372036854775809", "18446744073709551614", "18446744073709551615"}
	exec("INSERT INTO " + ns + ".UnsignedEdge VALUES (" + strings.Join(expected, "),(") + ")")
	r, err := source.NewMySQL(dsn, ns, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	spans, err := r.MySQLScanRanges(t.Context(), "UnsignedEdge", 2)
	require.NoError(t, err)
	require.Len(t, spans, 2)
	var seen []string
	for i := range spans {
		require.Equal(t, "MiXed", spans[i].Key)
		require.NoError(t, r.StreamMySQLTable(t.Context(), "UnsignedEdge", &spans[i], 2, func(_, _ []string, rows [][]any) error {
			for _, row := range rows {
				seen = append(seen, fmt.Sprint(row[0]))
			}
			return nil
		}))
	}
	require.ElementsMatch(t, expected, seen)
}
