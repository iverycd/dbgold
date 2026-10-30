package datamigrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// One explicitly authorized isolated MySQL instance is sufficient. Each case
// creates a unique source table and target database and cleans up only those.
func TestMySQLNativeKeyPartitionIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_PARTITION_MYSQL_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || dsn == "" {
		t.Skip("requires isolated MySQL fixture and explicit DDL flag")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	defer db.Close()
	var database, version string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT DATABASE(),VERSION()").Scan(&database, &version))
	t.Logf("native KEY same-server MySQL %s", version)
	for _, scenario := range []string{"key_algorithm1", "key_default2", "linear_algorithm1", "linear_default2", "implicit_primary", "implicit_unique", "nested_key", "storage_types"} {
		t.Run(scenario, func(t *testing.T) {
			name := fmt.Sprintf("dbgold_key_%d", time.Now().UnixNano())
			q := func(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
			_, err := db.ExecContext(ctx, "CREATE DATABASE "+q(name))
			require.NoError(t, err)
			defer db.ExecContext(context.Background(), "DROP DATABASE "+q(name))
			targetTable := q(name) + "." + q(name)
			sourceTable := q(database) + "." + q(name)
			constraints := ""
			method := "KEY ALGORITHM=1 (`Txt`,`Big`)"
			switch scenario {
			case "key_default2":
				method = "KEY (`Txt`,`Big`)"
			case "linear_algorithm1":
				method = "LINEAR KEY ALGORITHM=1 (`Txt`,`Big`)"
			case "linear_default2":
				method = "LINEAR KEY (`Txt`,`Big`)"
			case "implicit_primary":
				method = "KEY ()"
				constraints = ", PRIMARY KEY (`Txt`,`Big`)"
			case "implicit_unique":
				method = "LINEAR KEY ()"
				constraints = ", UNIQUE KEY `Z` (`Txt`,`Big`), UNIQUE KEY `A` (`Big`,`Txt`)"
			case "storage_types":
				method = "KEY (`Bytes`,`Bits`,`Amount`,`Txt`,`Big`)"
			}
			nullable := "NULL"
			if constraints != "" {
				nullable = "NOT NULL"
			}
			ddl := "CREATE TABLE " + sourceTable + " (`Small` int NOT NULL, `Big` bigint unsigned " + nullable + ", `Txt` varchar(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci " + nullable + ", `Amount` decimal(20,4), `Bits` bit(12), `Bytes` varbinary(8), `label` varchar(20)" + constraints + ") ENGINE=InnoDB PARTITION BY " + method + " (PARTITION `MiXeD_A`,PARTITION `MiXeD_B`,PARTITION `MiXeD_C`)"
			parts := []string{"MiXeD_A", "MiXeD_B", "MiXeD_C"}
			if scenario == "nested_key" {
				ddl = "CREATE TABLE " + sourceTable + " (`Small` int NOT NULL, `Big` bigint unsigned NULL, `Txt` varchar(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NULL, `Amount` decimal(20,4), `Bits` bit(12), `Bytes` varbinary(8), `label` varchar(20)) ENGINE=InnoDB PARTITION BY RANGE (`Small`) SUBPARTITION BY LINEAR KEY ALGORITHM=1 (`Txt`,`Big`) (PARTITION `Neg` VALUES LESS THAN (0) (SUBPARTITION `N_A`,SUBPARTITION `N_B`,SUBPARTITION `N_C`),PARTITION `Pos` VALUES LESS THAN MAXVALUE (SUBPARTITION `P_A`,SUBPARTITION `P_B`,SUBPARTITION `P_C`))"
				parts = []string{"N_A", "N_B", "N_C", "P_A", "P_B", "P_C"}
			}
			_, err = db.ExecContext(ctx, ddl)
			require.NoError(t, err)
			defer db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+sourceTable)
			insert := "INSERT INTO " + sourceTable + " VALUES (-2,18446744073709551615,'Á',123.4567,b'101010101010',X'00FF','max'),(-1,0,'a ',0,b'0',X'00','zero'),(0,7,'中文',-99.0001,b'111111111111',X'FF','unicode'),(1,8,'ß',1.2500,b'1',X'0102','eszett')"
			_, err = db.ExecContext(ctx, insert)
			require.NoError(t, err)
			if constraints == "" {
				_, err = db.ExecContext(ctx, "INSERT INTO "+sourceTable+" VALUES(2,NULL,NULL,NULL,NULL,NULL,'null')")
				require.NoError(t, err)
			}
			r, err := source.NewMySQL(dsn, database, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			w, err := target.NewMySQL(dsn, name, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer w.Close()
			job := &Job{LogCh: make(chan string, 16384)}
			cfg := Config{Mode: "include", Filter: name, Content: "both", TargetDBType: "mysql", TargetSchema: name, PageSize: 2, MaxParallel: 1, ValidateData: true, CaptureRepair: true}
			preflight := NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
			require.True(t, preflight.Ready, "issues=%+v fatal=%s", preflight.Issues, preflight.FatalError)
			report := NewMigrator(r, w, job, cfg).Run(ctx)
			if report.HasFailures() {
				if report.PartitionMigration != nil {
					for _, entry := range report.PartitionMigration.Tables {
						if entry.Plan == nil {
							continue
						}
						actual, inspectErr := w.InspectPartitionTable(ctx, name)
						want, _ := partition.CanonicalSpec(entry.Plan.Target)
						got, _ := partition.CanonicalSpec(actual)
						a, _ := json.Marshal(want)
						b, _ := json.Marshal(got)
						t.Logf("expected=%s actual=%s inspect=%v", a, b, inspectErr)
					}
				}
				t.Fatalf("KEY migration failed: partition=%+v tables=%+v data=%+v", report.PartitionMigration, report.Tables, report.Data)
			}
			read := func(table, leaf string) []string {
				rows, err := db.QueryContext(ctx, "SELECT CONCAT(COALESCE(CAST(`Big` AS CHAR),'NULL'),'/',COALESCE(HEX(`Txt`),'NULL'),'/',COALESCE(CAST(`Amount` AS CHAR),'NULL'),'/',COALESCE(HEX(`Bits`),'NULL'),'/',COALESCE(HEX(`Bytes`),'NULL'),'/',`label`) FROM "+table+" PARTITION ("+q(leaf)+") ORDER BY `label`")
				require.NoError(t, err)
				defer rows.Close()
				var out []string
				for rows.Next() {
					var row string
					require.NoError(t, rows.Scan(&row))
					out = append(out, row)
				}
				require.NoError(t, rows.Err())
				return out
			}
			compare := func() {
				for _, leaf := range parts {
					require.Equal(t, read(sourceTable, leaf), read(targetTable, leaf), "native leaf %s", leaf)
				}
			}
			compare()
			for _, table := range []string{sourceTable, targetTable} {
				_, err = db.ExecContext(ctx, "UPDATE "+table+" SET `Big`=18446744073709551614, `Txt`='A  ', `Amount`=-1234.5678, `Bits`=b'101',`Bytes`=X'FAFB' WHERE `label`='unicode'")
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, "INSERT INTO "+table+" VALUES(3,999,'résumé',999.0123,b'10001',X'ABCDEF','future')")
				require.NoError(t, err)
			}
			compare()
			// A second real migration into a nonempty native target must validate
			// storage/algorithm identity. Exclude duplicate PK/UK scenarios here.
			if constraints == "" {
				cfg.Content = "data_only"
				cfg.ValidateData = false
				cfg.CaptureRepair = false
				appendReport := NewMigrator(r, w, job, cfg).Run(ctx)
				require.False(t, appendReport.HasFailures(), "partition=%+v data=%+v", appendReport.PartitionMigration, appendReport.Data)
				for _, leaf := range parts {
					sourceRows := read(sourceTable, leaf)
					targetRows := read(targetTable, leaf)
					doubled := append(append([]string{}, sourceRows...), sourceRows...)
					require.ElementsMatch(t, doubled, targetRows, "append must preserve all field contents within the native leaf")
				}
				if scenario == "key_default2" {
					var before, after int64
					require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+targetTable).Scan(&before))
					_, err = db.ExecContext(ctx, "ALTER TABLE "+targetTable+" MODIFY COLUMN `Txt` varchar(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL")
					require.NoError(t, err)
					blocked := NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
					require.False(t, blocked.Ready, "different native KEY collation must block append")
					require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+targetTable).Scan(&after))
					require.Equal(t, before, after)
				}
			}
		})
	}
}
