package target

import (
	"context"
	"database/sql/driver"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

// This suite only uses an explicitly authorized, precreated empty schema. Each
// constraint failure occurs after a successful first row in the same batch.
func TestDamengNativeIntegrationDefaultOnNullFailedBatchRollsBackBeforeRestoringDDL(t *testing.T) {
	dsn, schema := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("DEFAULT ON NULL 失败回滚实库测试需要独立空测试模式及 DAMENG_DM_TEST_TARGET_DSN / TARGET_SCHEMA、DAMENG_DM_TEST_ALLOW_DDL=1")
	}
	w, err := NewDaMengNative(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var count int
	require.NoError(t, w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=?`, w.schema).Scan(&count))
	require.Zero(t, count, "显式指定的测试 schema 必须最初为空")
	for _, tc := range []struct {
		name       string
		constraint string
		rows       [][]any
	}{
		{name: "check_second_row", constraint: `CHECK("V">0)`, rows: [][]any{{int64(1), int64(10)}, {int64(2), int64(-1)}}},
		{name: "unique_second_row", constraint: `UNIQUE("A")`, rows: [][]any{{int64(1), int64(10)}, {int64(1), int64(11)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("NDEFAULT%X", time.Now().UnixNano())
			sequence := name + "S"
			qualified, sequenceSQL := damengnative.Qualified(w.schema, name), damengnative.Qualified(w.schema, sequence)
			require.NoError(t, w.ExecuteDamengNative(ctx, "CREATE SEQUENCE "+sequenceSQL+" START WITH 2 INCREMENT BY 1 NOCACHE"))
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
				defer stop()
				require.NoError(t, w.ExecuteDamengNative(cleanup, "DROP SEQUENCE "+sequenceSQL))
			})
			// ExecuteDamengNative creates the fixture in its own SET SCHEMA
			// session. The frozen expression deliberately keeps that implicit
			// owner; CopyDamengNativeBatch receives a fresh default-schema session.
			defaultSQL := damengnative.Quote(sequence) + ".NEXTVAL"
			require.NoError(t, w.ExecuteDamengNative(ctx, "CREATE TABLE "+qualified+`("A" INT,"V" INT DEFAULT ON NULL `+defaultSQL+` NOT NULL,CONSTRAINT `+damengnative.Quote(name+"C")+" "+tc.constraint+")"))
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
				defer stop()
				require.NoError(t, w.ExecuteDamengNative(cleanup, "DROP TABLE "+qualified+" CASCADE CONSTRAINTS"))
			})
			var originalDefault string
			require.NoError(t, w.db.QueryRowContext(ctx, `SELECT DATA_DEFAULT FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? AND COLUMN_NAME='V'`, w.schema, name).Scan(&originalDefault))
			require.Equal(t, defaultSQL, originalDefault, "原生目录保留未限定模式的默认表达式")
			table := damengnative.Table{Schema: w.schema, Name: name, Columns: []damengnative.Column{
				{Name: "A", DataType: "INT", SQLType: "INT", Writable: true},
				{Name: "V", DataType: "INT", SQLType: "INT", Writable: true, DefaultOnNull: true, DefaultSQL: defaultSQL},
			}}
			err = w.CopyDamengNativeBatch(ctx, table, tc.rows)
			require.Error(t, err, "第二条约束失败必须使整批失败")
			require.ErrorContains(t, err, name+"C", "必须实际执行到第二条并收到该约束失败")
			var rows, next int64
			require.NoError(t, w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qualified).Scan(&rows))
			require.Zero(t, rows, "恢复默认值的 DDL 不能提交此前成功的第一行")
			require.NoError(t, w.db.QueryRowContext(ctx, `SELECT LAST_NUMBER FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=? AND SEQUENCE_NAME=?`, w.schema, sequence).Scan(&next))
			require.EqualValues(t, 2, next, "复制显式值及失败清理均不能消费默认序列")
			var restoredDefault, restoredDDL string
			require.NoError(t, w.db.QueryRowContext(ctx, `SELECT DATA_DEFAULT FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? AND COLUMN_NAME='V'`, w.schema, name).Scan(&restoredDefault))
			require.Equal(t, originalDefault, restoredDefault, "恢复原始默认表达式")
			require.NoError(t, w.db.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TABLE',?,?) FROM DUAL`, name, w.schema).Scan(&restoredDDL))
			onNull, err := damengnative.NativeDefaultOnNullColumns(restoredDDL)
			require.NoError(t, err)
			require.True(t, onNull["V"], "原始 DEFAULT ON NULL 属性必须恢复")
			// Unqualified native defaults retain their source session's schema
			// resolution semantics, so ordinary business work uses that schema.
			business, err := w.db.Conn(ctx)
			require.NoError(t, err)
			defer func() { _ = business.Raw(func(any) error { return driver.ErrBadConn }); _ = business.Close() }()
			_, err = business.ExecContext(ctx, "SET SCHEMA "+damengnative.Quote(w.schema))
			require.NoError(t, err)
			_, err = business.ExecContext(ctx, "INSERT INTO "+qualified+`("A") VALUES(3)`)
			require.NoError(t, err)
			var value int64
			require.NoError(t, business.QueryRowContext(ctx, `SELECT "V" FROM `+qualified+` WHERE "A"=3`).Scan(&value))
			require.EqualValues(t, 2, value, "后续省略默认列的业务插入必须正常续号")
		})
	}
}
