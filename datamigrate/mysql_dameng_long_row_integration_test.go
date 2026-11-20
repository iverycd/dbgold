package datamigrate

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func TestMySQLDamengLongRowIntegration(t *testing.T) {
	myDSN, dmDSN := os.Getenv("DBGOLD_MYSQL_SCAN_SOURCE_DSN"), os.Getenv("DBGOLD_MYSQL_SCAN_DM_DSN")
	if myDSN == "" || dmDSN == "" {
		t.Skip("requires disposable MySQL and DM instances")
	}
	my, err := sql.Open("mysql", myDSN)
	require.NoError(t, err)
	t.Cleanup(func() { my.Close() })
	dm, err := sql.Open("dm", dmDSN)
	require.NoError(t, err)
	t.Cleanup(func() { dm.Close() })
	srcDB := fmt.Sprintf("longrow_%d", time.Now().UnixNano())
	contentFixtureExec(t, my, "CREATE DATABASE "+srcDB+" CHARACTER SET utf8")
	t.Cleanup(func() { contentFixtureExec(t, my, "DROP DATABASE "+srcDB) })
	sourceDSN, err := mysql.ParseDSN(myDSN)
	require.NoError(t, err)
	sourceDSN.DBName = srcDB
	r, err := source.NewMySQLForDameng(sourceDSN.FormatDSN(), srcDB, source.ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })

	// Each value fits both the VARCHAR column and a DM array bind. Only the
	// combined UTF-8 bytes exceed the actual DM instance's inline row limit.
	var pageBytes int
	require.NoError(t, dm.QueryRow("SELECT PAGE()").Scan(&pageBytes))
	columnCount := pageBytes/(2*1500) + 2
	t.Logf("DM page size %d bytes, %d long VARCHAR columns on row 41", pageBytes, columnCount)
	columns := make([]string, columnCount)
	wide := make([]string, columnCount)
	myDefs, dmDefs := []string{"id INT PRIMARY KEY"}, []string{"ID INT PRIMARY KEY"}
	for i := range columns {
		columns[i] = fmt.Sprintf("v%d", i)
		wide[i] = fmt.Sprintf("%02d%s", i, strings.Repeat("迁", 499))
		myDefs = append(myDefs, columns[i]+" VARCHAR(1536)")
		dmDefs = append(dmDefs, columns[i]+" VARCHAR2(1536)")
	}
	definition := " (" + strings.Join(myDefs, ",") + ")"
	contentFixtureExec(t, my, "CREATE TABLE "+srcDB+".cg_wide"+definition)
	contentFixtureExec(t, my, "CREATE TABLE "+srcDB+".cg_part"+definition+" PARTITION BY RANGE(id) (PARTITION p0 VALUES LESS THAN (41), PARTITION p1 VALUES LESS THAN MAXVALUE)")
	contentFixtureExec(t, my, "CREATE TABLE "+srcDB+".z_good (id INT PRIMARY KEY)")
	contentFixtureExec(t, my, "INSERT INTO "+srcDB+".z_good VALUES (7)")
	insert := "INSERT INTO " + srcDB + ".cg_wide VALUES (?" + strings.Repeat(",?", columnCount) + ")"
	for id := 1; id <= 41; id++ {
		args := []any{id}
		for _, value := range wide {
			if id < 41 {
				value = "短"
			}
			args = append(args, value)
		}
		contentFixtureExec(t, my, insert, args...)
	}
	contentFixtureExec(t, my, "INSERT INTO "+srcDB+".cg_part SELECT * FROM "+srcDB+".cg_wide")

	checkWide := func(t *testing.T, table string) {
		t.Helper()
		var count int
		require.NoError(t, dm.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&count))
		require.Equal(t, 41, count)
		values := make([]string, columnCount)
		dest := make([]any, columnCount)
		for i := range values {
			dest[i] = &values[i]
		}
		err := dm.QueryRow("SELECT " + strings.Join(columns, ",") + " FROM " + table + " WHERE ID=41").Scan(dest...)
		require.NoError(t, err)
		require.Equal(t, wide, values)
	}
	for _, strategy := range []string{"row", "batch"} {
		t.Run(strategy, func(t *testing.T) {
			schema := fmt.Sprintf("LONGROW_%d", time.Now().UnixNano())
			createDMScanSchema(t, dm, schema)
			w, err := target.NewDaMeng(dmDSN, schema, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer w.Close()
			cfg := Config{Mode: "include", Filter: "cg_wide,cg_part", Content: "both", TargetSchema: schema,
				TargetDBType: "dameng", DamengWriteStrategy: strategy, PageSize: 50, MaxParallel: 1,
				IntraTableParallel: 1, ValidateData: true, CaptureRepair: true}
			run := func() MigrationReport {
				return NewMigrator(r, w, &Job{LogCh: make(chan string, 4096)}, cfg).Run(t.Context())
			}
			report := run()
			require.False(t, report.HasFailures(), "tables=%+v data=%+v validation=%+v", report.Tables, report.Data, report.DataValidation)
			require.Equal(t, 2, report.Data.Success)
			q := dialect.NewDaMeng()
			table := q.QualifyTable(schema, "cg_wide")
			partitioned := q.QualifyTable(schema, "cg_part")
			checkWide(t, table)
			checkWide(t, partitioned)
			if strategy == "batch" {
				// Normal partition migration uses a streaming cursor and scalar
				// writes. Also round-trip the same page using actual DM array binds.
				before, _ := w.BatchWriteStats()
				cols, types, rows, err := r.ReadPage(t.Context(), "cg_part", []string{"id"}, 0, 50)
				require.NoError(t, err)
				contentFixtureExec(t, dm, "TRUNCATE TABLE "+partitioned)
				require.NoError(t, w.CopyMySQLBatchData(t.Context(), "cg_part", cols, types, rows))
				after, _ := w.BatchWriteStats()
				require.Greater(t, after, before)
				checkWide(t, partitioned)
			}

			// Explicitly disable LONG ROW for the old-table baseline even on
			// instances that default to it. Only the generated advice may enable it.
			contentFixtureExec(t, dm, "DROP TABLE "+table)
			contentFixtureExec(t, dm, "CREATE TABLE "+table+" ("+strings.Join(dmDefs, ",")+") STORAGE(DISABLE USING LONG ROW)")
			good := q.QualifyTable(schema, "z_good")
			contentFixtureExec(t, dm, "CREATE TABLE "+good+" (ID INT PRIMARY KEY)")
			cfg.Filter, cfg.Content = "cg_wide,z_good", "data_only"
			var expectedRows int64
			if strategy == "batch" {
				contentFixtureExec(t, dm, "INSERT INTO "+table+" (ID) VALUES (9000)")
				expectedRows = 1
			}
			report = run()
			require.Equal(t, 1, report.Data.Success, "%+v", report.Data)
			require.Len(t, report.Data.Items, 1)
			failure := report.Data.Items[0]
			require.Equal(t, "cg_wide", failure.Name)
			require.Contains(t, failure.Error, "-2665")
			if strategy == "row" {
				require.Contains(t, failure.Error, "页内第 41 行")
			} else {
				require.Contains(t, failure.Error, "页内第 1–41 行")
			}
			require.Equal(t, "ALTER TABLE "+table+" ENABLE USING LONG ROW;", failure.SuggestedDDL)
			contentFixtureExec(t, dm, failure.SuggestedDDL)
			var object repair.Object
			for _, candidate := range report.RepairManifest.Objects {
				if candidate.Kind == "data" && candidate.SourceTable == "cg_wide" {
					object = candidate
				}
			}
			require.NotEmpty(t, object.ID)
			if strategy == "row" {
				// Historical reports can contain just the source name and the
				// original driver error, without a manifest or column mapping.
				old := MigrationReport{Data: CategoryReport{Items: []ObjectResult{{Name: failure.Name, Error: failure.Error}}}}
				objects := LegacyRepairObjects(old, schema)
				require.Len(t, objects, 1)
				object = objects[0]
			}
			preview, err := PreviewRecovery(t.Context(), r, w, cfg, object, nil, nil)
			require.NoError(t, err)
			require.True(t, preview.DataRequired)
			require.Equal(t, expectedRows, preview.TargetRows)
			if expectedRows > 0 {
				_, err = RunRecovery(t.Context(), r, w, cfg, preview, false, nil)
				require.ErrorContains(t, err, "确认清空")
			}
			res, err := RunRecovery(t.Context(), r, w, cfg, preview, expectedRows > 0, nil)
			require.NoError(t, err)
			require.EqualValues(t, 41, res.CommittedRows)
			checkWide(t, table)
			var goodID int
			require.NoError(t, dm.QueryRow("SELECT ID FROM "+good).Scan(&goodID))
			require.Equal(t, 7, goodID)
		})
	}
}
