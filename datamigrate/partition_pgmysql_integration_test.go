package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

func TestPostgresMySQLPartitionIntegration(t *testing.T) {
	pgDSN, myDSN := os.Getenv("DBGOLD_PARTITION_PG_DSN"), os.Getenv("DBGOLD_PARTITION_MYSQL_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || pgDSN == "" || myDSN == "" {
		t.Skip("需要独立 PostgreSQL/MySQL 测试连接及 DBGOLD_PARTITION_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pg, err := sql.Open("postgres", pgDSN)
	require.NoError(t, err)
	defer pg.Close()
	my, err := sql.Open("mysql", myDSN)
	require.NoError(t, err)
	defer my.Close()
	exec := func(db *sql.DB, query string) {
		t.Helper()
		_, err := db.ExecContext(ctx, query)
		require.NoError(t, err, query)
	}
	for _, method := range []string{"RANGE", "LIST"} {
		t.Run(method, func(t *testing.T) {
			name := fmt.Sprintf("dbgold_pgpart_%x", time.Now().UnixNano())
			qp, qm := source.QuotePGIdent(name), "`"+name+"`"
			exec(pg, "CREATE SCHEMA "+qp)
			defer pg.ExecContext(context.Background(), "DROP SCHEMA "+qp+" CASCADE")
			exec(my, "CREATE DATABASE "+qm+" CHARACTER SET utf8mb4 COLLATE utf8mb4_bin")
			defer my.ExecContext(context.Background(), "DROP DATABASE "+qm)
			exec(pg, "CREATE TABLE "+qp+`."Root" ("Id" integer NOT NULL, "Payload" varchar(40)) PARTITION BY `+method+` ("Id")`)
			bounds := []string{"FROM (MINVALUE) TO (10)", "FROM (10) TO (20)", "FROM (20) TO (MAXVALUE)"}
			if method == "LIST" {
				bounds = []string{"IN (-1,0,9)", "IN (10,11)", "IN (20)"}
			}
			for i, leaf := range []string{"Low", "High", "Empty"} {
				exec(pg, "CREATE TABLE "+qp+"."+source.QuotePGIdent(leaf)+" PARTITION OF "+qp+`."Root" FOR VALUES `+bounds[i])
			}
			exec(pg, "INSERT INTO "+qp+`."Root" VALUES (-1,'negative'),(0,'zero'),(9,'边界'),(10,NULL),(11,'duplicate'),(11,'duplicate')`)
			r, err := source.NewPostgres(pgDSN, name, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			w, err := target.NewMySQL(myDSN, name, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer w.Close()
			cfg := Config{Mode: "all", Content: "both", TargetDBType: "mysql", TargetSchema: name, PageSize: 2, MaxParallel: 1, ValidateData: true, CaptureRepair: true}
			job := &Job{LogCh: make(chan string, 8192)}
			preview := NewMigrator(r, w, job, cfg).PreflightPGMySQL(ctx)
			require.True(t, preview.Ready, "issues=%+v partitions=%+v", preview.Issues, preview.PartitionPreflight)
			report := NewMigrator(r, w, job, cfg).Run(ctx)
			require.False(t, report.HasFailures(), "tables=%+v data=%+v partition=%+v validation=%+v", report.Tables, report.Data, report.PartitionMigration, report.DataValidation)
			require.Len(t, report.PartitionMigration.Tables, 1, "physical children must not be independent copies")
			require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
			require.Len(t, report.DataValidation.Tables, 4)
			// Existing content and duplicate multiplicity must survive a second append.
			cfg.Content = "data_only"
			appended := NewMigrator(r, w, job, cfg).Run(ctx)
			require.False(t, appended.HasFailures(), "tables=%+v data=%+v partition=%+v validation=%+v", appended.Tables, appended.Data, appended.PartitionMigration, appended.DataValidation)
			require.Equal(t, validation.StatusPassed, appended.DataValidation.Status)
			require.Len(t, appended.DataValidation.Tables, 4)
			var count int64
			require.NoError(t, my.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qm+".`Root`").Scan(&count))
			require.EqualValues(t, 12, count)
			require.EqualValues(t, 6, appended.DataValidation.Tables[0].TargetRows)
		})
	}
}
