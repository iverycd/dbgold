package datamigrate

import (
	"context"
	"database/sql"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"fmt"
	"github.com/stretchr/testify/require"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in only: the environment must name dedicated test databases/schema.
// No credentials are embedded and no existing application objects are touched.
func TestMySQLSQLServer2008Integration(t *testing.T) {
	srcDSN, dstDSN, schema := os.Getenv("DBGOLD_MYSQL_SQLSERVER_TEST_DSN"), os.Getenv("DBGOLD_SQLSERVER_TEST_DSN"), os.Getenv("DBGOLD_SQLSERVER_TEST_SCHEMA")
	if srcDSN == "" || dstDSN == "" || schema == "" || os.Getenv("DBGOLD_SQLSERVER_TEST_ALLOW_DDL") != "1" {
		t.Skip("requires dedicated MySQL/SQL Server test DSNs, DBGOLD_SQLSERVER_TEST_SCHEMA and DBGOLD_SQLSERVER_TEST_ALLOW_DDL=1")
	}
	require.True(t, strings.HasPrefix(schema, "DBGOLD_"), "use an existing dedicated DBGOLD_ schema")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	src, e := sql.Open("mysql", srcDSN)
	require.NoError(t, e)
	defer src.Close()
	dst, e := sql.Open("sqlserver", dstDSN)
	require.NoError(t, e)
	defer dst.Close()
	var database, version string
	require.NoError(t, src.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database))
	require.NotEmpty(t, database)
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductVersion') AS varchar(128))").Scan(&version))
	t.Logf("Actual SQL Server version: %s", version)
	require.True(t, strings.HasPrefix(version, "10.50."), "2008 R2 certification requires actual 10.50.x; newer compatibility modes are not substitutes")
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	parent, child, view := "Sp"+suffix, "Sc"+suffix, "Sv"+suffix
	d := dialect.NewSQLServer()
	qn := func(n string) string { return d.QualifyTable(schema, n) }
	defer func() {
		cleanup, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		_, _ = src.ExecContext(cleanup, "DROP VIEW IF EXISTS `"+view+"`")
		_, _ = dst.ExecContext(cleanup, "IF OBJECT_ID("+dialect.SQLServerLiteral(qn(view))+",N'V') IS NOT NULL DROP VIEW "+qn(view))
		for _, n := range []string{child, parent} {
			_, _ = src.ExecContext(cleanup, "DROP TABLE IF EXISTS `"+n+"`")
			_, _ = dst.ExecContext(cleanup, "IF OBJECT_ID("+dialect.SQLServerLiteral(qn(n))+",N'U') IS NOT NULL DROP TABLE "+qn(n))
		}
	}()
	_, e = src.ExecContext(ctx, "CREATE TABLE `"+parent+"` (Id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, Name VARCHAR(30) NULL COMMENT '中文备注', Amount DECIMAL(30,6) NOT NULL, Stamp DATETIME(6) NOT NULL, Payload VARBINARY(10), UNIQUE KEY U_Name (Name)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='测试表'")
	require.NoError(t, e)
	_, e = src.ExecContext(ctx, "CREATE TABLE `"+child+"` (Id INT NOT NULL PRIMARY KEY, ParentId BIGINT UNSIGNED NOT NULL, CONSTRAINT `FK_"+suffix+"` FOREIGN KEY (ParentId) REFERENCES `"+parent+"`(Id)) ENGINE=InnoDB")
	require.NoError(t, e)
	for _, row := range [][]interface{}{{1, "中文😀", "123456789012345678901234.123456", "2024-02-29 12:34:56.123456", []byte{0, 255, 0}}, {2, nil, "0.000001", "1000-01-01 00:00:00", []byte{}}} {
		_, e = src.ExecContext(ctx, "INSERT INTO `"+parent+"`(Id,Name,Amount,Stamp,Payload) VALUES(?,?,?,?,?)", row...)
		require.NoError(t, e)
	}
	_, e = src.ExecContext(ctx, "ALTER TABLE `"+parent+"` AUTO_INCREMENT=100")
	require.NoError(t, e)
	_, e = src.ExecContext(ctx, "INSERT INTO `"+child+"` VALUES(1,1)")
	require.NoError(t, e)
	_, e = src.ExecContext(ctx, "CREATE VIEW `"+view+"` AS SELECT Id, IFNULL(Name,'missing') AS Label FROM `"+parent+"`")
	require.NoError(t, e)
	reader, e := source.NewMySQLForSQLServer(srcDSN, database, source.ConnPoolConfig{})
	require.NoError(t, e)
	defer reader.Close()
	writer, e := target.NewSQLServer(dstDSN, schema, target.ConnPoolConfig{})
	require.NoError(t, e)
	defer writer.Close()
	job := &Job{LogCh: make(chan string, 2048)}
	m := NewMigrator(reader, writer, job, Config{TargetDBType: "sqlserver", TargetSchema: schema, Mode: "include", Filter: parent + "," + child, PageSize: 1, MaxParallel: 2, Content: "both", ViewNames: []string{view}})
	report := m.Run(ctx)
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, 2, report.Tables.Success)
	require.Equal(t, 1, report.Views.Success)
	var text, amount, stamp string
	var binary []byte
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT Name,CONVERT(varchar(80),Amount),CONVERT(varchar(40),Stamp,126),Payload FROM "+qn(parent)+" WHERE Id=1").Scan(&text, &amount, &stamp, &binary))
	require.Equal(t, "中文😀", text)
	require.Equal(t, "123456789012345678901234.123456", amount)
	require.Contains(t, stamp, "123456")
	require.Equal(t, []byte{0, 255, 0}, binary)
	var next string
	require.NoError(t, dst.QueryRowContext(ctx, "INSERT INTO "+qn(parent)+" (Name,Amount,Stamp) OUTPUT CONVERT(varchar(40),INSERTED.Id) VALUES(N'next',0,'2024-01-01')").Scan(&next))
	require.Equal(t, "100", next)
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT Label FROM "+qn(view)+" WHERE Id=2").Scan(&text))
	require.Equal(t, "missing", text)
	var comments int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM sys.extended_properties WHERE major_id=OBJECT_ID(@p1) AND name=N'MS_Description'", qn(parent)).Scan(&comments))
	require.Equal(t, 2, comments)
	// Append into compatible existing tables; duplicate keys must roll back the
	// current page, leave the previously inserted row, and mark the job failed.
	m.cfg.Content = "data_only"
	report = m.Run(ctx)
	require.True(t, report.HasFailures())
	require.Greater(t, report.Data.Failed, 0)
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qn(parent)).Scan(&count))
	require.Equal(t, 3, count)
}
