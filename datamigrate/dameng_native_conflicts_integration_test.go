package datamigrate

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

func TestDamengNativeOwnedNameConflictIntegration(t *testing.T) {
	srcDSN, dstDSN := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_DSN")
	srcSchema, dstSchema := os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" || srcSchema == "" || dstSchema == "" {
		t.Skip("需要预建专用空 Schema、SOURCE/TARGET_DSN/SCHEMA 和 DAMENG_NATIVE_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	src, err := sql.Open("dm", srcDSN)
	require.NoError(t, err)
	defer src.Close()
	dst, err := sql.Open("dm", dstDSN)
	require.NoError(t, err)
	defer dst.Close()
	srcSchema, err = damengnative.ResolveSchema(ctx, src, srcSchema)
	require.NoError(t, err)
	dstSchema, err = damengnative.ResolveSchema(ctx, dst, dstSchema)
	require.NoError(t, err)
	for _, side := range []struct {
		db     *sql.DB
		schema string
	}{{src, srcSchema}, {dst, dstSchema}} {
		var count int
		require.NoError(t, side.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, side.schema).Scan(&count))
		require.Zero(t, count, "必须使用初始为空的专用测试 Schema")
	}
	for _, scenario := range []string{"constraint_on_retained_table", "index_on_retained_table", "target_only_check"} {
		t.Run(scenario, func(t *testing.T) {
			sq := func(n string) string { return damengnative.Qualified(srcSchema, n) }
			dq := func(n string) string { return damengnative.Qualified(dstSchema, n) }
			exec := func(db *sql.DB, s string) { _, e := db.ExecContext(ctx, s); require.NoError(t, e, s) }
			defer func() {
				clean, stop := context.WithTimeout(context.Background(), 15*time.Second)
				defer stop()
				_, _ = src.ExecContext(clean, "DROP TABLE "+sq("Selected")+" CASCADE CONSTRAINTS")
				for _, table := range []string{"Selected", "Keep"} {
					_, _ = dst.ExecContext(clean, "DROP TABLE "+dq(table)+" CASCADE CONSTRAINTS")
				}
			}()
			exec(src, "CREATE TABLE "+sq("Selected")+`("Id" INT,"Value" INT)`)
			exec(dst, "CREATE TABLE "+dq("Selected")+`("Id" INT,"Value" INT)`)
			exec(dst, "CREATE TABLE "+dq("Keep")+`("Id" INT,"Value" INT)`)
			switch scenario {
			case "constraint_on_retained_table":
				exec(src, "ALTER TABLE "+sq("Selected")+` ADD CONSTRAINT "SharedKey" PRIMARY KEY("Id")`)
				exec(dst, "ALTER TABLE "+dq("Keep")+` ADD CONSTRAINT "SharedKey" PRIMARY KEY("Id")`)
			case "index_on_retained_table":
				for _, side := range []struct {
					db           *sql.DB
					table, index string
				}{{src, sq("Selected"), sq("SharedIndex")}, {dst, dq("Keep"), dq("SharedIndex")}} {
					exec(side.db, "CREATE INDEX "+side.index+" ON "+side.table+`("Id")`)
					exec(side.db, "ALTER TABLE "+side.table+` ADD CONSTRAINT "SharedKey" PRIMARY KEY("Id") USING INDEX `+side.index)
				}
			case "target_only_check":
				exec(dst, "ALTER TABLE "+dq("Selected")+` ADD CONSTRAINT "TargetOnlyCheck" CHECK("Value">0)`)
			}
			exec(src, "INSERT INTO "+sq("Selected")+" VALUES(1,10)")
			exec(dst, "INSERT INTO "+dq("Selected")+" VALUES(7,77)")
			exec(dst, "INSERT INTO "+dq("Keep")+" VALUES(8,88)")
			var before, after string
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TABLE',?,?) FROM DUAL`, "Keep", dstSchema).Scan(&before))
			r, e := source.NewDaMengNative(srcDSN, srcSchema, source.ConnPoolConfig{})
			require.NoError(t, e)
			defer r.Close()
			w, e := target.NewDaMengNative(dstDSN, dstSchema, target.ConnPoolConfig{})
			require.NoError(t, e)
			defer w.Close()
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 1024)}, Config{Mode: "all", Content: "both", OverwriteExisting: true, CaptureRepair: true, TargetDBType: "dameng", TargetSchema: dstSchema})
			preview := m.PreflightDamengNative(ctx)
			require.False(t, preview.Ready, "必须在覆盖任何对象前发现用户命名子对象冲突")
			report := m.Run(ctx)
			require.True(t, report.HasFailures())
			require.Zero(t, report.Data.Total)
			for _, table := range []struct {
				name      string
				id, value int
			}{{"Selected", 7, 77}, {"Keep", 8, 88}} {
				var id, value int
				require.NoError(t, dst.QueryRowContext(ctx, "SELECT \"Id\",\"Value\" FROM "+dq(table.name)).Scan(&id, &value))
				require.Equal(t, table.id, id)
				require.Equal(t, table.value, value)
			}
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TABLE',?,?) FROM DUAL`, "Keep", dstSchema).Scan(&after))
			require.Equal(t, before, after, "目标独有表及其键必须保持原定义")
		})
	}
}
