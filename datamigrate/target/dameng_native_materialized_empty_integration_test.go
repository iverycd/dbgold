package target

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

func TestDamengNativeMaterializedEmptyTouchIntegration(t *testing.T) {
	dsn, schema := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("空基表原生状态验收需要独立目标 DSN/Schema 和 DDL 开关")
	}
	w, err := NewDaMengNative(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, clustered := range []bool{false, true} {
		for _, interrupted := range []bool{false, true} {
			t.Run(fmt.Sprint("clustered", clustered, "interrupted", interrupted), func(t *testing.T) {
				suffix := fmt.Sprintf("%X", time.Now().UnixNano())
				base, view, child, seq := "EmptyB"+suffix, "EmptyV"+suffix, "EmptyC"+suffix, "EmptyS"+suffix
				q, mv, cq, sq := damengnative.Qualified(schema, base), damengnative.Qualified(schema, view), damengnative.Qualified(schema, child), damengnative.Qualified(schema, seq)
				t.Cleanup(func() {
					cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
					defer stop()
					for _, s := range []string{"DROP TABLE " + cq + " CASCADE CONSTRAINTS", "DROP MATERIALIZED VIEW " + mv, "DROP TABLE " + q + " CASCADE CONSTRAINTS", "DROP SEQUENCE " + sq} {
						_, _ = w.db.ExecContext(cleanup, s)
					}
				})
				exec := func(s string) { _, e := w.db.ExecContext(ctx, s); require.NoError(t, e, s) }
				exec("CREATE SEQUENCE " + sq + " START WITH 1 INCREMENT BY 1")
				var sequence int64
				require.NoError(t, w.db.QueryRowContext(ctx, "SELECT "+sq+".NEXTVAL FROM DUAL").Scan(&sequence))
				require.EqualValues(t, 1, sequence)
				key := "PRIMARY KEY"
				if clustered {
					key = "CLUSTER PRIMARY KEY"
				}
				exec("CREATE TABLE " + q + "(\"Id\" INT IDENTITY(100,2) " + key + ",\"V\" INT DEFAULT ON NULL " + sq + ".NEXTVAL NOT NULL,CHECK(\"V\">0),CHECK(\"V\"<100))")
				exec("CREATE TABLE " + cq + "(\"Parent\" INT)")
				exec("INSERT INTO " + cq + " VALUES(999)")
				exec("ALTER TABLE " + cq + " ADD FOREIGN KEY(\"Parent\") REFERENCES " + q + "(\"Id\") ENABLE NOVALIDATE")
				exec("CREATE MATERIALIZED VIEW LOG ON " + q + " WITH PRIMARY KEY(\"V\")")
				exec("CREATE MATERIALIZED VIEW " + mv + " REFRESH FAST WITH PRIMARY KEY ON DEMAND AS SELECT \"Id\",\"V\" FROM " + q)
				conn, e := w.db.Conn(ctx)
				require.NoError(t, e)
				defer conn.Close()
				ddl, e := historyEmptyReadDDL(ctx, conn, schema, base)
				require.NoError(t, e)
				var def string
				require.NoError(t, conn.QueryRowContext(ctx, `SELECT DATA_DEFAULT FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? AND COLUMN_NAME='V'`, schema, base).Scan(&def))
				p := &damengnative.MaterializedEmptyTouch{Table: base, DefinitionSQL: ddl, Columns: []damengnative.MaterializedEmptyColumn{{Name: "Id", NotNull: true}, {Name: "V", NotNull: true, DefaultSQL: def, DefaultOnNull: true}}, IdentityRestore: []string{"ALTER TABLE " + q + " DROP IDENTITY", "ALTER TABLE " + q + " ADD COLUMN \"Id\" IDENTITY(100,2)"}}
				if clustered {
					p.Columns[0].ValueSQL = "CAST(0 AS INT)"
				}
				constraints, e := historyEmptyConstraints(ctx, conn, schema, base)
				require.NoError(t, e)
				for _, c := range constraints {
					p.Constraints = append(p.Constraints, damengnative.MaterializedEmptyConstraint{Name: c.name, Table: c.table, Kind: c.kind, Columns: c.columns, Enabled: c.enabled, Generated: true, DefinitionSQL: c.definition, NoValidate: c.noValidate, Clustered: clustered && c.kind == "P"})
				}
				if interrupted {
					for _, c := range constraints {
						if c.kind == "R" {
							exec("ALTER TABLE " + damengnative.Qualified(schema, c.table) + " DISABLE CONSTRAINT " + damengnative.Quote(c.name))
						}
					}
					for _, c := range constraints {
						if c.kind == "P" && !clustered {
							exec("ALTER TABLE " + q + " DISABLE CONSTRAINT " + damengnative.Quote(c.name))
						}
					}
					exec(p.IdentityRestore[0])
					exec("ALTER TABLE " + q + " ALTER COLUMN \"V\" DROP DEFAULT")
					if !clustered {
						exec("ALTER TABLE " + q + " ALTER COLUMN \"Id\" SET NULL")
					}
					exec("ALTER TABLE " + q + " ALTER COLUMN \"V\" SET NULL")
				}
				require.NoError(t, nativeHistoryEmptyTouch(ctx, conn, schema, p))
				got, e := historyEmptyReadDDL(ctx, conn, schema, base)
				require.NoError(t, e)
				require.True(t, nativeDefinitionsEqual(ddl, got), got)
				var count, current int64
				require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q).Scan(&count))
				require.Zero(t, count)
				require.NoError(t, conn.QueryRowContext(ctx, "SELECT IDENT_CURRENT(?) FROM DUAL", q).Scan(&current))
				require.EqualValues(t, 100, current)
				require.NoError(t, conn.QueryRowContext(ctx, "SELECT "+sq+".CURRVAL FROM DUAL").Scan(&sequence))
				require.EqualValues(t, 1, sequence, "不能执行原 DEFAULT ON NULL")
				require.Eventually(t, func() bool {
					var state string
					e := conn.QueryRowContext(ctx, `SELECT STALENESS FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME=?`, schema, view).Scan(&state)
					return e == nil && state == "NEEDS_COMPILE"
				}, 3*time.Second, 20*time.Millisecond)
				exec("INSERT INTO " + q + " DEFAULT VALUES")
				exec("REFRESH MATERIALIZED VIEW " + mv + " FAST")
				require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+mv+" WHERE \"Id\"=100 AND \"V\"=2").Scan(&count))
				require.EqualValues(t, 1, count)
			})
		}
	}
}

func TestDamengNativeMaterializedEmptyUDTTouchIntegration(t *testing.T) {
	dsn, schema := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("空 UDT 基表原生状态验收需要独立目标 DSN/Schema 和 DDL 开关")
	}
	w, err := NewDaMengNative(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	suffix := fmt.Sprintf("%X", time.Now().UnixNano())
	base, view, typ := "UdtB"+suffix, "UdtV"+suffix, "UdtT"+suffix
	q, mv, tq := damengnative.Qualified(schema, base), damengnative.Qualified(schema, view), damengnative.Qualified(schema, typ)
	domain := damengnative.Qualified(schema, "Dom"+suffix)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, s := range []string{"DROP MATERIALIZED VIEW " + mv, "DROP TABLE " + q + " CASCADE CONSTRAINTS", "DROP TYPE " + tq, "DROP DOMAIN " + domain} {
			_, _ = w.db.ExecContext(cleanup, s)
		}
	})
	exec := func(s string) { _, e := w.db.ExecContext(ctx, s); require.NoError(t, e, s) }
	exec("CREATE TYPE " + tq + " AS OBJECT(\"N\" INT)")
	exec("CREATE DOMAIN " + domain + " AS INT DEFAULT 43 CHECK(VALUE IS NOT NULL AND VALUE>0)")
	exec("CREATE TABLE " + q + "(\"D\" " + tq + " NOT NULL,\"N\" " + domain + " NOT NULL)")
	exec("CREATE MATERIALIZED VIEW LOG ON " + q + " WITH ROWID(\"N\")")
	exec("CREATE MATERIALIZED VIEW " + mv + " REFRESH FAST WITH ROWID ON DEMAND AS SELECT \"N\",ROWID AS \"Rid\" FROM " + q)
	conn, err := w.db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	ddl, err := historyEmptyReadDDL(ctx, conn, schema, base)
	require.NoError(t, err)
	p := &damengnative.MaterializedEmptyTouch{Table: base, DefinitionSQL: ddl, Columns: []damengnative.MaterializedEmptyColumn{{Name: "D", NotNull: true}, {Name: "N", NotNull: true}}}
	constraints, err := historyEmptyConstraints(ctx, conn, schema, base)
	require.NoError(t, err)
	for _, c := range constraints {
		p.Constraints = append(p.Constraints, damengnative.MaterializedEmptyConstraint{Name: c.name, Table: c.table, Kind: c.kind, Columns: c.columns, Enabled: c.enabled, Generated: true, DefinitionSQL: c.definition, NoValidate: c.noValidate})
	}
	require.NoError(t, nativeHistoryEmptyTouch(ctx, conn, schema, p))
	got, err := historyEmptyReadDDL(ctx, conn, schema, base)
	require.NoError(t, err)
	require.True(t, nativeDefinitionsEqual(ddl, got), got)
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q).Scan(&count))
	require.Zero(t, count)
	require.Eventually(t, func() bool {
		var state string
		e := conn.QueryRowContext(ctx, `SELECT STALENESS FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME=?`, schema, view).Scan(&state)
		return e == nil && state == "NEEDS_COMPILE"
	}, 3*time.Second, 20*time.Millisecond)
	exec("REFRESH MATERIALIZED VIEW " + mv + " FAST")
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+mv).Scan(&count))
	require.Zero(t, count)
}

func TestDamengNativeMaterializedEmptyListTouchIntegration(t *testing.T) {
	dsn, schema := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("空 LIST 分区状态验收需要独立目标 DSN/Schema 和 DDL 开关")
	}
	w, err := NewDaMengNative(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, clustered := range []bool{false, true} {
		t.Run(fmt.Sprint("clustered", clustered), func(t *testing.T) {
			suffix := fmt.Sprintf("%X", time.Now().UnixNano())
			base, view := "ListB"+suffix, "ListV"+suffix
			q, mv := damengnative.Qualified(schema, base), damengnative.Qualified(schema, view)
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
				defer stop()
				for _, s := range []string{"DROP MATERIALIZED VIEW " + mv, "DROP TABLE " + q + " CASCADE CONSTRAINTS"} {
					_, _ = w.db.ExecContext(cleanup, s)
				}
			})
			exec := func(s string) { _, e := w.db.ExecContext(ctx, s); require.NoError(t, e, s) }
			key := "PRIMARY KEY"
			if clustered {
				key = "CLUSTER PRIMARY KEY"
			}
			exec("CREATE TABLE " + q + "(\"Id\" INT " + key + ",\"K\" INT NOT NULL) PARTITION BY LIST(\"K\")(PARTITION \"P1\" VALUES(1,2),PARTITION \"P2\" VALUES(3))")
			// Deliberately omit the partition key from the native log. The frozen
			// native partition tuple, rather than a deleted-row guess, routes it.
			exec("CREATE MATERIALIZED VIEW LOG ON " + q + " WITH PRIMARY KEY")
			exec("CREATE MATERIALIZED VIEW " + mv + " REFRESH FAST WITH PRIMARY KEY ON DEMAND AS SELECT \"Id\",\"K\" FROM " + q)
			conn, e := w.db.Conn(ctx)
			require.NoError(t, e)
			defer conn.Close()
			ddl, e := historyEmptyReadDDL(ctx, conn, schema, base)
			require.NoError(t, e)
			p := &damengnative.MaterializedEmptyTouch{Table: base, DefinitionSQL: ddl, Columns: []damengnative.MaterializedEmptyColumn{{Name: "Id", NotNull: true}, {Name: "K", NotNull: true, ValueSQL: "1"}}}
			if clustered {
				p.Columns[0].ValueSQL = "CAST(0 AS INT)"
			}
			constraints, e := historyEmptyConstraints(ctx, conn, schema, base)
			require.NoError(t, e)
			for _, c := range constraints {
				p.Constraints = append(p.Constraints, damengnative.MaterializedEmptyConstraint{Name: c.name, Table: c.table, Kind: c.kind, Columns: c.columns, Enabled: c.enabled, Generated: true, DefinitionSQL: c.definition, NoValidate: c.noValidate, Clustered: clustered && c.kind == "P"})
			}
			require.NoError(t, nativeHistoryEmptyTouch(ctx, conn, schema, p))
			got, e := historyEmptyReadDDL(ctx, conn, schema, base)
			require.NoError(t, e)
			require.True(t, nativeDefinitionsEqual(ddl, got), got)
			require.Eventually(t, func() bool {
				var state string
				e := conn.QueryRowContext(ctx, `SELECT STALENESS FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME=?`, schema, view).Scan(&state)
				return e == nil && state == "NEEDS_COMPILE"
			}, 3*time.Second, 20*time.Millisecond)
			exec("INSERT INTO " + q + " VALUES(1,2)")
			exec("REFRESH MATERIALIZED VIEW " + mv + " FAST")
			var n int
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+mv+" WHERE \"Id\"=1 AND \"K\"=2").Scan(&n))
			require.Equal(t, 1, n)
		})
	}
}
