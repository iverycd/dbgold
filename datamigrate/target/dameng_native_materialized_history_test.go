package target

import (
	"context"
	"database/sql"
	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"regexp"
	"testing"
)

func TestMaterializedHistoryRetainsNativeCompleteRequirement(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	m.ExpectExec(regexp.QuoteMeta(`ALTER MATERIALIZED VIEW "Mixed"."Need""Full" NEVER REFRESH`)).WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, historyRestoreUnusableState(context.Background(), conn, "Mixed", damengnative.MaterializedHistoryView{Name: `Need"Full`, NativeStaleness: "NEEDS_FULL_REFRESH"}))
	// Working FAST baselines must not be invalidated when only stored data is
	// stale; that was the original source of failed FAST after migration.
	require.NoError(t, historyRestoreUnusableState(context.Background(), nil, "Mixed", damengnative.MaterializedHistoryView{Name: "Valid", NativeStaleness: "NEEDS_COMPILE"}))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedHistoryClockBarrierUsesServerNaturalSecondBoundary(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	query := regexp.QuoteMeta(`SELECT TO_CHAR(NOW(6),'YYYY-MM-DD HH24:MI:SS.FF6') FROM DUAL`)
	m.ExpectQuery(query).WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow("2026-10-22 09:00:00.999999"))
	m.ExpectQuery(query).WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow("2026-10-22 09:00:01.110000"))
	require.NoError(t, historyClockBarrier(context.Background(), conn, "2026-10-22 09:00:00.500000"))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedHistoryClockBarrierHonorsCancellationBeforeSQL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var conn *sql.Conn
	require.ErrorIs(t, historyClockBarrier(ctx, conn, "2026-10-22 09:00:00.500000"), context.Canceled)
}

func TestMaterializedLogEpochsKeepInclusiveConsumedBoundaryAndSequenceOrder(t *testing.T) {
	p := &damengnative.MaterializedHistoryPlan{Schema: "Target", Views: []damengnative.MaterializedHistoryView{{SourceRefresh: "2026-01-01 00:00:00.100000"}, {SourceRefresh: "2026-01-01 00:00:00.200000"}}}
	l := damengnative.MaterializedHistoryLog{Staging: "LogStage", Columns: []damengnative.Column{{Name: "SNAPTIME$$", DataType: "DATETIME", Writable: true}, {Name: "SEQUENCE$$", DataType: "BIGINT", Writable: true}}}
	q, err := historyLogProjection(p, l, []string{"2026-10-22 09:00:00.100000", "2026-10-22 09:00:00.200000"})
	require.NoError(t, err)
	require.Contains(t, q, `WHEN S."SNAPTIME$$"<=TIMESTAMP '2026-01-01 00:00:00.100000' THEN TIMESTAMP '2026-10-22 09:00:00.099999'`)
	require.Contains(t, q, `WHEN S."SNAPTIME$$"<=TIMESTAMP '2026-01-01 00:00:00.200000' THEN TIMESTAMP '2026-10-22 09:00:00.100001'`)
	require.Contains(t, q, `ROW_NUMBER() OVER(ORDER BY S."SEQUENCE$$")-COUNT(*) OVER()-1`)
}

func TestMaterializedLogStringRowIDDoesNotBecomePhysicalTypedColumn(t *testing.T) {
	p := &damengnative.MaterializedHistoryPlan{Schema: "Target", Views: []damengnative.MaterializedHistoryView{{SourceRefresh: "2026-01-01 00:00:00.100000"}}, RowIDMaps: []damengnative.MaterializedRowIDMap{{BaseTable: "Base", Table: "RIDMap"}}}
	l := damengnative.MaterializedHistoryLog{BaseTable: "Base", Staging: "LogStage", Columns: []damengnative.Column{{Name: "M_ROW$$", DataType: "VARCHAR", Writable: true}, {Name: "SNAPTIME$$", DataType: "DATETIME", Writable: true}, {Name: "SEQUENCE$$", DataType: "BIGINT", Writable: true}}}
	q, err := historyLogProjection(p, l, []string{"2026-10-22 09:00:00.100000"})
	require.NoError(t, err)
	require.Contains(t, q, `WHERE M."SourceRID"=S."M_ROW$$"`)
	require.NotContains(t, q, "CHARTOROWID")
	require.NotContains(t, q, "ROWIDTOCHAR")
}

func TestMaterializedPartitionExchangeKeepsActualStorageLayout(t *testing.T) {
	b := damengnative.MaterializedExchangeBuffer{Name: "Buffer", TablePartition: "PartMixed", Subpartition: true}
	require.Equal(t, `ALTER TABLE "Target"."Storage" EXCHANGE SUBPARTITION "PartMixed" WITH TABLE "Target"."Buffer"`, historyExchangeSQL("Target", b, "Storage"))
	require.Equal(t, `SELECT X FROM "Target"."Stage" SUBPARTITION("PartMixed") S`, historyPartitionProjection(`SELECT X FROM "Target"."Stage" S`, b))
}
