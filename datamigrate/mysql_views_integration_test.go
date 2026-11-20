package datamigrate

import (
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

func TestMySQLOverdueViewIntegration(t *testing.T) {
	myDSN, dmDSN := os.Getenv("DBGOLD_MYSQL_SCAN_SOURCE_DSN"), os.Getenv("DBGOLD_MYSQL_SCAN_DM_DSN")
	if myDSN == "" || dmDSN == "" {
		t.Skip("requires disposable MySQL and DM instances")
	}
	ctx := t.Context()
	cfg, err := mysql.ParseDSN(myDSN)
	require.NoError(t, err)
	admin, err := sql.Open("mysql", myDSN)
	require.NoError(t, err)
	defer admin.Close()
	srcName := fmt.Sprintf("dbgold_src_%d", time.Now().UnixNano())
	dstName := srcName + "_dst"
	for _, name := range []string{srcName, dstName} {
		contentFixtureExec(t, admin, "CREATE DATABASE "+name)
		defer admin.Exec("DROP DATABASE " + name)
	}
	cfg.DBName = srcName
	src, err := sql.Open("mysql", cfg.FormatDSN())
	require.NoError(t, err)
	defer src.Close()
	for _, stmt := range []string{
		"CREATE TABLE cg_hetongtiaokuan (Row_ID int PRIMARY KEY, islvyue int, beianguid varchar(8), FuKuanTime datetime, FuKuanNode varchar(8), FuKuanPrice int, FuKuanContent varchar(8), RowGuid varchar(8), SBR_UnitGuid varchar(8), tenantGuid varchar(8))",
		"CREATE TABLE cg_paymentvoucher (tiaokuanguid varchar(8) PRIMARY KEY, AuditStatus varchar(1), SHR_Date datetime)",
		"CREATE TABLE cg_hetonginfo (BeiAnGuid varchar(8) PRIMARY KEY, isbiangeng varchar(1), isterminate int, AuditStatus varchar(1))",
		"CREATE TABLE cg_hetongbiangeng LIKE cg_hetonginfo",
		"CREATE VIEW view_cg_hetongandbiangeng AS SELECT BeiAnGuid, 'No' AS HTNO,'Nm' AS HTNAME,'Org' AS JIANSHEDW,'Win' AS ZBDANWEINAME,'Id' AS ZBDANWEIGUID FROM cg_hetonginfo",
		"INSERT INTO cg_hetonginfo VALUES ('H',NULL,NULL,'3')",
		"INSERT INTO cg_hetongtiaokuan (Row_ID,beianguid,RowGuid,FuKuanTime) VALUES (1,'H','1','2024-10-01 23:50:01'),(2,'H','2','2024-10-02 09:01:02'),(3,'H','3','2024-10-03 10:01:02'),(4,'H','4',NULL),(5,'H','5',now()+INTERVAL 7 DAY-INTERVAL 10 MINUTE),(6,'H','6',now()+INTERVAL 7 DAY+INTERVAL 10 MINUTE),(7,'H','7',now()-INTERVAL 1 DAY)",
		"INSERT INTO cg_paymentvoucher VALUES ('1','3','2024-10-02 00:10:03'),('2','3','2024-10-02 10:02:03'),('3','3','2024-10-03 08:00:00')",
		"CREATE VIEW ViewFns AS SELECT GROUP_CONCAT(RowGuid ORDER BY Row_ID DESC SEPARATOR '|') AS labels, IF(ISNULL(MIN(FuKuanTime)),'bad',DATE_FORMAT(MIN(FuKuanTime),'%Y-%m-%d')) AS fmt, '%Y %m' AS lit, CONVERT('12.30',DECIMAL(8,2)) AS amount, CONVERT('你好' USING utf8mb4) AS hello, UUID() AS guid, DATE_FORMAT(CONVERT(MIN(FuKuanTime),DATE),'%H:%i:%s') AS cast_clock, IF(CAST('2024-10-08 09:01:02' AS DATETIME) < CAST('2024-10-01 09:01:02' AS DATETIME) + INTERVAL 7 DAY,1,0) AS exact_boundary FROM cg_hetongtiaokuan",
		"CREATE VIEW ViewText AS SELECT Row_ID FROM cg_hetongtiaokuan WHERE IFNULL(FuKuanContent,'') <> ''",
		"UPDATE cg_hetongtiaokuan SET FuKuanContent=CASE Row_ID WHEN 1 THEN '' WHEN 2 THEN 'ok' ELSE NULL END",
	} {
		contentFixtureExec(t, src, stmt)
	}
	body, err := os.ReadFile("testdata/mysql_views/overdue.sql")
	require.NoError(t, err)
	contentFixtureExec(t, src, string(body))
	const overdue = "view_hetong_yuqitongji_ysjx"
	readPayments := func(db *sql.DB, schema string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, "SELECT row_id,yuqidaycount,date_format(FuKuanTime,'%Y-%m-%d %H:%i:%s'),date_format(realFuKuanTime,'%Y-%m-%d %H:%i:%s') FROM "+schema+"."+overdue+" ORDER BY row_id")
		require.NoError(t, err)
		defer rows.Close()
		var result []string
		for rows.Next() {
			var id, days int
			var planned string
			var actual sql.NullString
			require.NoError(t, rows.Scan(&id, &days, &planned, &actual))
			result = append(result, fmt.Sprintf("%d/%d/%s/%s", id, days, planned, actual.String))
		}
		require.NoError(t, rows.Err())
		return result
	}
	want := readPayments(src, srcName)
	require.Len(t, want, 4)
	require.Equal(t, "1/1/2024-10-01 23:50:01/2024-10-02 00:10:03", want[0])
	require.Equal(t, "2/0/2024-10-02 09:01:02/2024-10-02 10:02:03", want[1])
	require.True(t, strings.HasPrefix(want[2], "5/"))
	require.Contains(t, want[3], "7/1/")
	for _, kind := range []string{"dameng", "mysql"} {
		t.Run(kind, func(t *testing.T) {
			var w target.Writer
			var db *sql.DB
			schema := dstName
			var reader *source.MySQLReader
			if kind == "dameng" {
				db, err = sql.Open("dm", dmDSN)
				require.NoError(t, err)
				t.Cleanup(func() { db.Close() })
				schema = strings.ToUpper(dstName)
				createDMScanSchema(t, db, schema)
				reader, err = source.NewMySQLForDameng(cfg.FormatDSN(), srcName, source.ConnPoolConfig{})
				require.NoError(t, err)
				w, err = target.NewDaMeng(dmDSN, schema, target.ConnPoolConfig{})
			} else {
				db = admin
				reader, err = source.NewMySQL(cfg.FormatDSN(), srcName, source.ConnPoolConfig{})
				require.NoError(t, err)
				cfgDst := cfg.Clone()
				cfgDst.DBName = dstName
				w, err = target.NewMySQL(cfgDst.FormatDSN(), schema, target.ConnPoolConfig{})
			}
			require.NoError(t, err)
			defer reader.Close()
			defer w.Close()
			conf := Config{Content: "both", Mode: "all", TargetSchema: schema, TargetDBType: kind, PageSize: 10, MaxParallel: 1, CaptureRepair: true}
			job := &Job{LogCh: make(chan string, 4096)}
			m := NewMigrator(reader, w, job, conf)
			report := m.Run(ctx)
			if report.HasFailures() {
				t.Fatalf("migration failed: views %+v; logs %v", report.Views, drainLogs(job))
			}
			require.Equal(t, 4, report.Views.Success)
			var sourceBody, candidate string
			for _, o := range report.RepairManifest.Objects {
				if o.Kind == "view" && o.SourceName == overdue {
					require.Len(t, o.Units, 1)
					candidate = o.Units[0].SQL
					sourceBody = o.SourceDDL
				}
			}
			require.NotEmpty(t, candidate)
			var catalog string
			require.NoError(t, src.QueryRowContext(ctx, "SELECT VIEW_DEFINITION FROM information_schema.VIEWS WHERE TABLE_SCHEMA=? AND TABLE_NAME=?", srcName, overdue).Scan(&catalog))
			require.Equal(t, catalog, sourceBody)
			require.NotContains(t, candidate, "::date")
			require.Equal(t, 4, strings.Count(strings.ToUpper(candidate), "TO_DAYS("))
			if kind == "dameng" {
				require.Contains(t, candidate, "DATEADD(DAY, 7, NOW())")
				require.Contains(t, candidate, "IS NOT NULL")
			}
			require.Equal(t, want, readPayments(db, schema))
			var labels, fmtDate, lit, amount, hello, guid, castClock string
			var exactBoundary int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT labels,fmt,lit,amount,hello,guid,cast_clock,exact_boundary FROM "+schema+".ViewFns").Scan(&labels, &fmtDate, &lit, &amount, &hello, &guid, &castClock, &exactBoundary))
			require.Zero(t, exactBoundary)
			require.Equal(t, "00:00:00", castClock)
			require.Equal(t, "7|6|5|4|3|2|1", labels)
			require.Equal(t, "2024-10-01", fmtDate)
			require.Equal(t, "%Y %m", lit)
			require.Regexp(t, `^12\.3(0)?$`, amount)
			require.Equal(t, "你好", hello)
			require.Regexp(t, `^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$`, guid)
			var textID, textRows int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT MIN(row_id),COUNT(*) FROM "+schema+".ViewText").Scan(&textID, &textRows))
			require.Equal(t, 1, textRows)
			require.Equal(t, 2, textID)
			contentFixtureExec(t, db, "DROP VIEW "+schema+"."+overdue)
			selected := NewMigrator(reader, w, &Job{LogCh: make(chan string, 1024)}, conf)
			results := selected.MigrateViews(ctx, []string{overdue})
			require.Len(t, results, 1)
			require.Empty(t, results[0].Error)
			require.Equal(t, candidate, results[0].DDL)
			if kind == "mysql" {
				contentFixtureExec(t, db, "DELETE FROM "+schema+".cg_paymentvoucher WHERE tiaokuanguid='1'")
				require.NotEqual(t, want, readPayments(db, schema))
				require.Equal(t, want, readPayments(src, srcName))
			}
		})
	}
}
