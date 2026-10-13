package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Explicit DSN opts in to disposable databases only; normal test runs never connect.
func TestRepairMySQLIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_REPAIR_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set DBGOLD_REPAIR_MYSQL_DSN for disposable repair integration databases")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, e := sql.Open("mysql", dsn)
	require.NoError(t, e)
	defer db.Close()
	require.NoError(t, db.PingContext(ctx))
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	srcDB, dstDB := "repair_src_"+suffix, "repair_dst_"+suffix
	for _, name := range []string{srcDB, dstDB} {
		_, e = db.ExecContext(ctx, "CREATE DATABASE `"+name+"`")
		require.NoError(t, e)
		defer db.ExecContext(context.Background(), "DROP DATABASE `"+name+"`")
	}
	_, e = db.ExecContext(ctx, "CREATE TABLE `"+srcDB+"`.Mixed (ID INT NOT NULL AUTO_INCREMENT PRIMARY KEY, Name VARCHAR(40) NOT NULL)")
	require.NoError(t, e)
	_, e = db.ExecContext(ctx, "INSERT INTO `"+srcDB+"`.Mixed (Name) VALUES ('alpha'),('beta'),('gamma'),('delta'),('epsilon')")
	require.NoError(t, e)
	cfgDSN, e := mysql.ParseDSN(dsn)
	require.NoError(t, e)
	cfgDSN.DBName = srcDB
	r, e := source.NewMySQL(cfgDSN.FormatDSN(), srcDB, source.ConnPoolConfig{})
	require.NoError(t, e)
	defer r.Close()
	cfgDSN.DBName = dstDB
	w, e := target.NewMySQL(cfgDSN.FormatDSN(), dstDB, target.ConnPoolConfig{})
	require.NoError(t, e)
	defer w.Close()
	info, e := r.GetTableDDLInfo(ctx, "Mixed")
	require.NoError(t, e)
	obj := repair.Object{ID: "T", Kind: "table", Table: "Mixed", SourceName: "Mixed", SourceTable: "Mixed", TargetName: "Mixed", Schema: dstDB, SourceColumns: info.Columns, ColumnMapping: map[string]string{"ID": "ID", "Name": "Renamed"}, Status: "failed", DataStatus: "pending"}
	for _, c := range info.Columns {
		c.Name = obj.ColumnMapping[c.Name]
		obj.Columns = append(obj.Columns, c)
	}
	bad := "CREATE TABLE `" + dstDB + "`.Mixed (ID INT, Renamed unsupported_repair_type)"
	units, e := target.ExecuteRepair(ctx, w, obj, []repair.Unit{{ID: "create", SQL: bad, OriginalSQL: bad, Selected: true, Status: "failed"}}, nil)
	require.Error(t, e)
	require.Equal(t, "failed", units[0].Status)
	units[0].SQL = "CREATE TABLE `" + dstDB + "`.Mixed (ID INT NOT NULL AUTO_INCREMENT PRIMARY KEY, Renamed VARCHAR(80) NOT NULL)"
	units, e = target.ExecuteRepair(ctx, w, obj, units, nil)
	require.NoError(t, e)
	require.Equal(t, "succeeded", units[0].Status)
	verify, e := target.InspectRepair(ctx, w, obj)
	require.NoError(t, e)
	require.Equal(t, "succeeded", verify.Status)
	cfg := Config{Mode: "include", Filter: "Mixed", Content: "both", PageSize: 2, TargetSchema: dstDB, TargetDBType: "mysql"}
	p, e := PreviewRecovery(ctx, r, w, cfg, obj, nil, obj.ColumnMapping)
	require.NoError(t, e)
	require.Equal(t, int64(0), p.TargetRows)
	res, e := RunRecovery(ctx, r, w, cfg, p, false, nil)
	require.NoError(t, e)
	require.Equal(t, int64(5), res.CommittedRows)
	require.Equal(t, "succeeded", res.DataStatus)
	var name string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT Renamed FROM `"+dstDB+"`.Mixed WHERE ID=3").Scan(&name))
	require.Equal(t, "gamma", name)
	// Column comment repairs preserve the actual widened type and quoting.
	cm := source.CommentInfo{TableName: "Mixed", ColumnName: "Renamed", Comment: "修复 'quoted'; \\ path"}
	commentDDL, e := target.GenerateRepairComment(ctx, w, cm)
	require.NoError(t, e)
	require.Len(t, commentDDL, 1)
	comment := repair.Object{Kind: "comment", Table: "Mixed", Schema: dstDB, TargetName: "Mixed.Renamed", Comment: &cm}
	_, e = target.ExecuteRepair(ctx, w, comment, unitsFor(commentDDL), nil)
	require.NoError(t, e)
	checkedComment, e := target.InspectRepair(ctx, w, comment)
	require.NoError(t, e)
	require.Equal(t, "succeeded", checkedComment.Status)
	var dataType, nullable string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COLUMN_TYPE,IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='Mixed' AND COLUMN_NAME='Renamed'", dstDB).Scan(&dataType, &nullable))
	require.Equal(t, "varchar(80)", dataType)
	require.Equal(t, "NO", nullable)
	p, e = PreviewRecovery(ctx, r, w, cfg, obj, nil, obj.ColumnMapping)
	require.NoError(t, e)
	_, e = RunRecovery(ctx, r, w, cfg, p, false, nil)
	require.ErrorContains(t, e, "目标表非空")
	res, e = RunRecovery(ctx, r, w, cfg, p, true, nil)
	require.NoError(t, e)
	require.Equal(t, int64(5), res.TargetRows)
	_, e = db.ExecContext(ctx, "INSERT INTO `"+dstDB+"`.Mixed (Renamed) VALUES ('next')")
	require.NoError(t, e)
	var id int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT ID FROM `"+dstDB+"`.Mixed WHERE Renamed='next'").Scan(&id))
	require.Greater(t, id, 5)
	// MySQL auto-commits each DDL unit. Preserve the first success and retry only
	// the later failure: replaying CREATE INDEX would raise a duplicate-name error.
	index := repair.Object{Kind: "index", Table: "Mixed", TargetName: "ix_repair", Schema: dstDB}
	sequence := []repair.Unit{{ID: "one", SQL: "CREATE INDEX ix_repair ON `" + dstDB + "`.Mixed (Renamed)", Selected: true}, {ID: "two", SQL: "BROKEN SQL", Selected: true}}
	sequence, e = target.ExecuteRepair(ctx, w, index, sequence, nil)
	require.Error(t, e)
	require.Equal(t, "succeeded", sequence[0].Status)
	require.Equal(t, "failed", sequence[1].Status)
	sequence[1].SQL = "ALTER TABLE `" + dstDB + "`.Mixed COMMENT='repaired'"
	sequence, e = target.ExecuteRepair(ctx, w, index, sequence, nil)
	require.NoError(t, e)
	require.False(t, sequence[0].Selected)
	// New migration reports keep stable source identity and exact actual steps.
	cfgDSN.DBName = dstDB
	freshWriter, e := target.NewMySQL(cfgDSN.FormatDSN(), dstDB, target.ConnPoolConfig{})
	require.NoError(t, e)
	defer freshWriter.Close()
	job := &Job{LogCh: make(chan string, 1024)}
	cfg.CaptureRepair = true
	cfg.Content = "schema_only"
	m := NewMigrator(r, freshWriter, job, cfg)
	report := m.Run(ctx)
	require.NotNil(t, report.RepairManifest)
	var table *repair.Object
	for i := range report.RepairManifest.Objects {
		v := &report.RepairManifest.Objects[i]
		if v.Kind == "table" && v.SourceName == "Mixed" {
			table = v
		}
	}
	require.NotNil(t, table)
	require.Equal(t, "succeeded", table.Status)
	require.Equal(t, "not_required", table.DataStatus)
	require.GreaterOrEqual(t, len(table.Units), 2)
	for _, u := range table.Units {
		require.Equal(t, "succeeded", u.Status)
		require.False(t, u.Selected)
		require.True(t, strings.TrimSpace(u.OriginalSQL) != "")
	}
}
