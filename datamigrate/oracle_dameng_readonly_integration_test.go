package datamigrate

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// This diagnostic never creates tables or writes data. In particular, it can
// inspect an existing test connection without enabling the separate DDL suite.
func TestOracleDamengReadOnlyCharacterCapabilities(t *testing.T) {
	dsn := os.Getenv("ORACLE_DM_TEST_TARGET_DSN")
	if dsn == "" {
		t.Skip("实库未验收：只读字符能力诊断需要 ORACLE_DM_TEST_TARGET_DSN；不会创建或修改业务对象")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.PingContext(ctx))
	for _, kind := range []string{"VARCHAR", "NVARCHAR", "NCHAR"} {
		t.Run(kind, func(t *testing.T) {
			// A narrow CAST may apply its length in bytes even when the DDL
			// syntax uses CHAR units. Log it without calling it a table-capacity
			// failure; only the actual column can establish that capacity.
			var narrowLength, bufferedLength sql.NullInt64
			err := db.QueryRowContext(ctx, "SELECT LENGTH(CAST(? AS "+kind+"(2 CHAR))),LENGTH(RTRIM(CAST(? AS "+kind+"(32 CHAR)))) FROM DUAL", "汉A", "汉A").Scan(&narrowLength, &bufferedLength)
			require.NoError(t, err)
			t.Logf("%s CAST(2 CHAR) length=%+v；带足够多字节缓冲并移除定长补空格后 length=%+v", kind, narrowLength, bufferedLength)
			require.True(t, bufferedLength.Valid)
			require.Equal(t, int64(2), bufferedLength.Int64)
		})
	}
	w, err := target.NewDaMeng(dsn, os.Getenv("ORACLE_DM_TEST_TARGET_SCHEMA"), target.ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	infos := []*source.TableDDLInfo{{TableName: "DBGOLD_READONLY_CAPABILITY", Columns: []source.ColumnInfo{
		{Name: "N", DataType: "NCHAR", Length: 2, DataLength: 8, CharUsed: "C"},
		{Name: "V", DataType: "NVARCHAR2", Length: 2, DataLength: 8, CharUsed: "C"},
	}}}
	require.NoError(t, w.PrepareOracleDameng(ctx, infos, dialect.TypeOpt{CharInLength: true}), "字符能力检查不能把窄 CAST 的字节截断误判成真实列的字符容量不足")
	// The legacy method remains callable for compatibility, but no invalid
	// SELECT CAST expression may block migration in a permissive target mode.
	require.NoError(t, w.PrepareOracleDamengLoad(ctx))
}
