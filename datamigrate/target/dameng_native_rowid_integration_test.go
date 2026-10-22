package target

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestDamengNativeRowIDStreamIntegration(t *testing.T) {
	dsn, owner := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" || dsn == "" || owner == "" {
		t.Skip("ROWID 流式映射实库测试需要专用测试模式及 DAMENG_DM_TEST_ALLOW_DDL=1")
	}
	w, err := NewDaMengNative(dsn, owner, ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	r, err := source.NewDaMengNative(dsn, owner, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	prefix := fmt.Sprintf("RowID%X", time.Now().UnixNano())
	src, dst, mapping := prefix+"S", prefix+"T", prefix+"M"
	q := func(name string) string { return damengnative.Qualified(owner, name) }
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, name := range []string{mapping, dst, src} {
			_, _ = w.db.ExecContext(clean, "DROP TABLE "+q(name))
		}
	}()
	for _, name := range []string{src, dst} {
		_, err = w.db.ExecContext(ctx, "CREATE TABLE "+q(name)+` ("Value" VARCHAR(80))`)
		require.NoError(t, err)
	}
	_, err = w.db.ExecContext(ctx, "CREATE TABLE "+q(mapping)+` ("SourceRID" VARCHAR(32) PRIMARY KEY,"TargetRID" VARCHAR(32) NOT NULL)`)
	require.NoError(t, err)
	for _, value := range []string{"same", "same", "NUL\x00Value"} {
		_, err = w.db.ExecContext(ctx, "INSERT INTO "+q(src)+` ("Value") VALUES (?)`, value)
		require.NoError(t, err)
	}
	// Deliberately offset target addresses, so source addresses cannot simply
	// be reused. Equal row values must still map to different inserted rows.
	_, err = w.db.ExecContext(ctx, "INSERT INTO "+q(dst)+` ("Value") VALUES ('removed')`)
	require.NoError(t, err)
	_, err = w.db.ExecContext(ctx, "DELETE FROM "+q(dst))
	require.NoError(t, err)
	table := damengnative.Table{Name: src, Schema: owner, CaptureRowID: true, RowIDMapping: mapping, Columns: []damengnative.Column{{Name: "Value", DataType: "VARCHAR", Writable: true}}}
	scan, err := r.OpenDamengNativeScan(ctx, table)
	require.NoError(t, err)
	defer scan.Close()
	table.Name = dst
	var batch [][]any
	var firstSourceRID string
	for scan.Next() {
		row, e := scan.Values()
		require.NoError(t, e)
		if firstSourceRID == "" {
			firstSourceRID, e = nativeCanonicalRowID(row[1])
			require.NoError(t, e)
		}
		batch = append(batch, row)
		if len(batch) == 2 {
			require.NoError(t, w.CopyDamengNativeBatch(ctx, table, batch))
			batch = nil
		}
	}
	require.NoError(t, scan.Err())
	require.NoError(t, scan.Close())
	require.NoError(t, w.CopyDamengNativeBatch(ctx, table, batch))
	var count int
	err = w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q(mapping)+" m JOIN "+q(src)+` s ON ROWIDTOCHAR(s.ROWID)=m."SourceRID" JOIN `+q(dst)+` d ON ROWIDTOCHAR(d.ROWID)=m."TargetRID" WHERE s."Value"=d."Value"`).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 3, count)
	// A mapping collision occurs after the data INSERT. Its transaction must
	// roll that INSERT back as well, avoiding an untracked duplicate on retry.
	err = w.CopyDamengNativeBatch(ctx, table, [][]any{{"must roll back", firstSourceRID}})
	require.ErrorContains(t, err, "ROWID 映射写入失败")
	err = w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q(dst)).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 3, count)
}
