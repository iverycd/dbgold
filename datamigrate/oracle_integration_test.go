package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// Run on both MySQL 5.7/8.0 and Oracle 11g/19c using dedicated test users.
// No database globals or users are created. Cleanup touches only unique fixtures.
func TestMySQLOracleIntegration(t *testing.T) {
	srcDSN, dstDSN, schema := os.Getenv("DBGOLD_MYSQL_ORACLE_TEST_DSN"), os.Getenv("DBGOLD_ORACLE_TEST_DSN"), os.Getenv("DBGOLD_ORACLE_TEST_SCHEMA")
	if srcDSN == "" || dstDSN == "" || schema == "" || os.Getenv("DBGOLD_ORACLE_TEST_ALLOW_DDL") != "1" {
		t.Skip("requires dedicated DBGOLD_MYSQL_ORACLE_TEST_DSN, DBGOLD_ORACLE_TEST_DSN/SCHEMA and DBGOLD_ORACLE_TEST_ALLOW_DDL=1")
	}
	require.True(t, strings.HasPrefix(schema, "DBGOLD_"), "integration schema must start with DBGOLD_")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	srcDB, e := sql.Open("mysql", srcDSN)
	require.NoError(t, e)
	defer srcDB.Close()
	var database string
	require.NoError(t, srcDB.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database))
	require.NotEmpty(t, database)
	dstDB, e := sql.Open("oracle", dstDSN)
	require.NoError(t, e)
	defer dstDB.Close()
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	parent, child, v1, v2 := "Op"+suffix, "Oc"+suffix, "Ov"+suffix, "Ow"+suffix
	d := dialect.NewOracle()
	qn := func(name string) string { return d.QualifyTable(schema, strings.ToUpper(name)) }
	defer func() {
		cleanup, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		for _, v := range []string{v2, v1} {
			_, _ = srcDB.ExecContext(cleanup, "DROP VIEW IF EXISTS `"+v+"`")
			_, _ = dstDB.ExecContext(cleanup, "DROP VIEW "+qn(v))
		}
		for _, table := range []string{child, parent} {
			_, _ = srcDB.ExecContext(cleanup, "DROP TABLE IF EXISTS `"+table+"`")
			_, _ = dstDB.ExecContext(cleanup, "DROP TABLE "+qn(table)+" CASCADE CONSTRAINTS")
		}
		seq := d.SequenceName(source.SequenceInfo{TableName: strings.ToUpper(parent), ColumnName: "ID"})
		_, _ = dstDB.ExecContext(cleanup, "DROP SEQUENCE "+d.QualifyTable(schema, seq))
	}()
	_, e = srcDB.ExecContext(ctx, "CREATE TABLE `"+parent+"` (Id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, Label VARCHAR(100) NULL DEFAULT '', Body LONGTEXT NULL, Amount DECIMAL(38,18) NOT NULL, Bits BIT(64) NULL, CreatedAt DATETIME(6) NULL, Duration TIME(6) NULL, Payload LONGBLOB NULL, Huge DOUBLE NULL, OnlyNull DECIMAL(20,0) NULL, LaterText TEXT NULL, Day DATE NULL, PRIMARY KEY(Id), KEY label_index(Label)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	require.NoError(t, e)
	_, e = srcDB.ExecContext(ctx, "CREATE TABLE `"+child+"` (Id BIGINT NOT NULL PRIMARY KEY, ParentId BIGINT UNSIGNED, CONSTRAINT `f"+suffix+"` FOREIGN KEY(ParentId) REFERENCES `"+parent+"`(Id) ON DELETE CASCADE) ENGINE=InnoDB")
	require.NoError(t, e)
	amount := "12345678901234567890.123456789012345678"
	body := strings.Repeat("中文😀", 2000)
	_, e = srcDB.ExecContext(ctx, "INSERT INTO `"+parent+"` (Id,Label,Body,Amount,Bits,CreatedAt,Duration,Payload,Huge,Day) VALUES (9223372036854775808,'',?,?,b'1111111111111111111111111111111111111111111111111111111111111111','2024-02-29 12:34:56.123456','-838:59:59.999999',?,1e308,'2024-02-29')", body, amount, []byte{0, 1, 255, 0})
	require.NoError(t, e)
	c, e := srcDB.Conn(ctx)
	require.NoError(t, e)
	var originalSQLMode string
	require.NoError(t, c.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&originalSQLMode))
	defer c.Close()
	_, e = c.ExecContext(ctx, "SET SESSION sql_mode=''")
	require.NoError(t, e)
	_, e = c.ExecContext(ctx, "INSERT INTO `"+parent+"` (Id,Label,Body,Amount,CreatedAt,Payload,LaterText) VALUES (9223372036854775809,NULL,'',0,'0000-00-00 00:00:00',?,'后续文本')", []byte{})
	_, restoreErr := c.ExecContext(ctx, "SET SESSION sql_mode=?", originalSQLMode)
	c.Close()
	require.NoError(t, e)
	require.NoError(t, restoreErr)
	_, e = srcDB.ExecContext(ctx, "INSERT INTO `"+child+"` VALUES (1,9223372036854775808)")
	require.NoError(t, e)
	_, e = srcDB.ExecContext(ctx, "CREATE VIEW `"+v1+"` AS SELECT Id,IFNULL(Label,'Empty') AS Label FROM `"+parent+"`")
	require.NoError(t, e)
	_, e = srcDB.ExecContext(ctx, "CREATE VIEW `"+v2+"` AS SELECT Id,Label FROM `"+v1+"`")
	require.NoError(t, e)
	r, e := source.NewMySQLForOracle(srcDSN, database, source.ConnPoolConfig{})
	require.NoError(t, e)
	defer r.Close()
	w, e := target.NewOracle(dstDSN, schema, target.ConnPoolConfig{})
	require.NoError(t, e)
	defer w.Close()
	makeMigrator := func() *Migrator {
		return NewMigrator(r, w, &Job{LogCh: make(chan string, 4096)}, Config{TargetDBType: "oracle", TargetSchema: schema, Mode: "include", Filter: parent + "," + child, Content: "both", PageSize: 1, MaxParallel: 2})
	}
	for iteration := 0; iteration < 2; iteration++ {
		m := makeMigrator()
		if iteration == 1 {
			m.cfg.PageSize = 501 // Mixed and all-NULL arrays, including LOBs.
			m.cfg.UseNvarchar2 = true
		}
		report := m.Run(ctx)
		require.False(t, report.HasFailures(), "%+v", report)
		require.Equal(t, 2, report.Data.Success)
		require.NotEmpty(t, report.Warnings)
		results := m.MigrateViews(ctx, []string{v2, v1})
		require.Len(t, results, 2)
		for _, result := range results {
			require.Empty(t, result.Error, result)
		}
		var gotAmount, gotBits, gotTime string
		var length int64
		var huge float64
		e = dstDB.QueryRowContext(ctx, "SELECT TO_CHAR(AMOUNT,'FM99999999999999999999D999999999999999999','NLS_NUMERIC_CHARACTERS=''.,'''),TO_CHAR(BITS),TO_CHAR(CREATEDAT,'YYYY-MM-DD HH24:MI:SS.FF6'),DBMS_LOB.GETLENGTH(BODY),HUGE FROM "+qn(parent)+" WHERE ID=9223372036854775808").Scan(&gotAmount, &gotBits, &gotTime, &length, &huge)
		require.NoError(t, e)
		require.Equal(t, amount, gotAmount)
		require.Equal(t, "18446744073709551615", gotBits)
		require.Equal(t, "2024-02-29 12:34:56.123456", gotTime)
		require.Positive(t, length)
		require.Equal(t, 1e308, huge)
		var n int
		require.NoError(t, dstDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qn(parent)+" WHERE CREATEDAT IS NULL AND ID=9223372036854775809 AND DBMS_LOB.GETLENGTH(BODY)=0 AND DBMS_LOB.GETLENGTH(PAYLOAD)=0").Scan(&n))
		require.Equal(t, 1, n)
		require.NoError(t, dstDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qn(parent)+" WHERE ONLYNULL IS NULL").Scan(&n))
		require.Equal(t, 2, n)
		var gotDay, gotDuration, gotLater string
		require.NoError(t, dstDB.QueryRowContext(ctx, "SELECT TO_CHAR(DAY,'YYYY-MM-DD'),DURATION FROM "+qn(parent)+" WHERE ID=9223372036854775808").Scan(&gotDay, &gotDuration))
		require.Equal(t, "2024-02-29", gotDay)
		require.Equal(t, "-838:59:59.999999", gotDuration)
		require.NoError(t, dstDB.QueryRowContext(ctx, "SELECT DBMS_LOB.SUBSTR(LATERTEXT,100,1) FROM "+qn(parent)+" WHERE ID=9223372036854775809").Scan(&gotLater))
		require.Equal(t, "后续文本", gotLater)
		_, e = dstDB.ExecContext(ctx, "INSERT INTO "+qn(parent)+" (AMOUNT) VALUES (0)")
		require.NoError(t, e)
		require.NoError(t, dstDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qn(parent)+" WHERE ID>9223372036854775809").Scan(&n))
		require.Equal(t, 1, n)
	}
}
