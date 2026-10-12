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

// Opt-in: creates only uniquely named test fixtures in an explicitly supplied
// SQL Server database and DM schema. Never run against production connections.
func TestSQLServerDamengIntegration(t *testing.T) {
	srcDSN, dstDSN, schema := os.Getenv("SQLSERVER_DM_TEST_SOURCE_DSN"), os.Getenv("SQLSERVER_DM_TEST_TARGET_DSN"), os.Getenv("SQLSERVER_DM_TEST_SCHEMA")
	if srcDSN == "" || dstDSN == "" || schema == "" {
		t.Skip("需要 SQLSERVER_DM_TEST_SOURCE_DSN / SQLSERVER_DM_TEST_TARGET_DSN / SQLSERVER_DM_TEST_SCHEMA")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	src, e := sql.Open("sqlserver", srcDSN)
	require.NoError(t, e)
	t.Cleanup(func() { src.Close() })
	dst, e := sql.Open("dm", dstDSN)
	require.NoError(t, e)
	t.Cleanup(func() { dst.Close() })
	prefix := fmt.Sprintf("DbgDm_%d", time.Now().UnixNano())
	parent, child, view := prefix+"Parent", prefix+"Child", prefix+"View"
	sq := func(n string) string { return "[dbo].[" + n + "]" }
	dm := dialect.NewDaMeng()
	dq := func(n string) string { return dm.QualifyTable(schema, n) }
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		for _, q := range []string{"DROP VIEW " + sq(view), "DROP TABLE " + sq(child), "DROP TABLE " + sq(parent)} {
			_, _ = src.ExecContext(c, q)
		}
		for _, q := range []string{"DROP VIEW " + dq(view), "DROP TABLE " + dq(child) + " CASCADE CONSTRAINTS", "DROP TABLE " + dq(parent) + " CASCADE CONSTRAINTS"} {
			_, _ = dst.ExecContext(c, q)
		}
	})
	exec := func(q string) { _, e := src.ExecContext(ctx, q); require.NoError(t, e) }
	exec("CREATE TABLE " + sq(parent) + ` (ROWID int IDENTITY(10,5) NOT NULL PRIMARY KEY, ROW_ID int NULL, Name nvarchar(100) NOT NULL DEFAULT N'默认', Amount decimal(38,18), Uid uniqueidentifier, Payload varbinary(max), Stamp rowversion, AtTime datetimeoffset(6), Flag bit, EmptyText nvarchar(10), NullableText nvarchar(10))`)
	exec("INSERT INTO " + sq(parent) + ` (Name,Amount,Uid,Payload,AtTime,Flag,EmptyText) VALUES (N'AbC中文😀',12345678901234567890.123456789012345678,'00112233-4455-6677-8899-aabbccddeeff',0x00AAFF,'2024-02-29T23:34:56.123456-03:30',1,N''), (N'Keep',NULL,NULL,NULL,NULL,0,NULL), (N'Deleted',NULL,NULL,NULL,NULL,NULL,NULL)`)
	exec("DELETE FROM " + sq(parent) + " WHERE Name=N'Deleted'")
	exec("CREATE INDEX [" + prefix + "IX] ON " + sq(parent) + " (Name)")
	exec("CREATE TABLE " + sq(child) + " (Id int NOT NULL PRIMARY KEY, ParentId int NOT NULL, CONSTRAINT [" + prefix + "FK] FOREIGN KEY(ParentId) REFERENCES " + sq(parent) + "(ROWID) ON DELETE CASCADE)")
	exec("INSERT INTO " + sq(child) + " VALUES (1,10)")
	exec("CREATE VIEW " + sq(view) + " AS SELECT ROWID, ROW_ID, Name FROM " + sq(parent) + " WHERE Flag=1")
	exec("EXEC sys.sp_addextendedproperty @name=N'MS_Description',@value=N'迁移注释',@level0type=N'SCHEMA',@level0name=N'dbo',@level1type=N'TABLE',@level1name=N'" + parent + "'")
	reader, e := source.NewSQLServerForDameng(srcDSN, "", source.ConnPoolConfig{})
	require.NoError(t, e)
	defer reader.Close()
	writer, e := target.NewDaMeng(dstDSN, schema, target.ConnPoolConfig{})
	require.NoError(t, e)
	defer writer.Close()
	cfg := Config{TargetDBType: "dameng", TargetSchema: schema, Mode: "include", Filter: prefix + "*", Content: "both", CharInLength: true, PageSize: 1, MaxParallel: 1}
	m := NewMigrator(reader, writer, &Job{LogCh: make(chan string, 4096)}, cfg)
	report := m.Run(ctx)
	require.Zero(t, report.Tables.Failed+report.Data.Failed+report.PrimaryKeys.Failed+report.Indexes.Failed+report.Constraints.Failed+report.Sequences.Failed+report.Comments.Failed, "%+v", report)
	require.Len(t, report.RowCounts, 2)
	require.Len(t, report.AutoRepairs, 1)
	require.Equal(t, "ROWID", report.AutoRepairs[0].Source)
	require.Equal(t, "ROW_ID_2", report.AutoRepairs[0].Target)
	for _, n := range report.RowCounts {
		require.True(t, n.Match)
	}
	var name, amount, uid, offset string
	var blob, version []byte
	var empty, nullable sql.NullString
	e = dst.QueryRowContext(ctx, `SELECT "NAME",CAST("AMOUNT" AS VARCHAR(80)),"UID","PAYLOAD","STAMP",TO_CHAR("ATTIME",'YYYY-MM-DD HH24:MI:SS.FF6 TZH:TZM'),"EMPTYTEXT","NULLABLETEXT" FROM `+dq(parent)+` WHERE "ROW_ID_2"=10`).Scan(&name, &amount, &uid, &blob, &version, &offset, &empty, &nullable)
	require.NoError(t, e)
	require.Equal(t, "AbC中文😀", name)
	require.Equal(t, "12345678901234567890.123456789012345678", amount)
	require.Equal(t, "00112233-4455-6677-8899-AABBCCDDEEFF", strings.ToUpper(uid))
	require.Equal(t, []byte{0, 0xaa, 0xff}, blob)
	require.Equal(t, "2024-02-29 23:34:56.123456 -03:30", offset)
	require.True(t, empty.Valid)
	require.Empty(t, empty.String)
	require.False(t, nullable.Valid)
	var sourceVersion []byte
	require.NoError(t, src.QueryRowContext(ctx, "SELECT Stamp FROM "+sq(parent)+" WHERE ROWID=10").Scan(&sourceVersion))
	require.Equal(t, sourceVersion, version)
	// last source identity was deleted: continuation must use 25, not MAX(ROWID)+5=20.
	_, e = dst.ExecContext(ctx, "INSERT INTO "+dq(parent)+` ("NAME") VALUES ('After')`)
	require.NoError(t, e)
	var next int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT \"ROW_ID_2\" FROM "+dq(parent)+` WHERE "NAME"='After'`).Scan(&next))
	require.Equal(t, 25, next)
	// A fresh migrator retains the raw source reader for a view-only run.
	vm := NewMigrator(reader, writer, &Job{LogCh: make(chan string, 1024)}, cfg)
	results := vm.MigrateViews(ctx, []string{view})
	require.Len(t, results, 1)
	require.Empty(t, results[0].Error)
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq(view)).Scan(&count))
	require.Equal(t, 1, count)
	var repairedID int
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT "ROW_ID_2" FROM `+dq(view)).Scan(&repairedID))
	require.Equal(t, 10, repairedID)
}
