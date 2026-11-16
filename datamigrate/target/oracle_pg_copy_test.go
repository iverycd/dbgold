package target

import (
	"context"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	"github.com/DATA-DOG/go-sqlmock"
	goora "github.com/sijms/go-ora/v2"
	"github.com/stretchr/testify/require"
	"regexp"
	"testing"
	"time"
)

func TestPGOracleWriterTimestampTZArrayUsesTypedNulls(t *testing.T) {
	w, m := oracleMockWriter(t)
	w.ConfigurePGOracle(map[string][]source.ColumnInfo{"T": {{Name: "AT", DataType: "timestamptz", DateTimePrecision: 6, IsNullable: true}}})
	w.columns["T"] = map[string]valueconv.OracleColumn{"AT": {DataType: "TIMESTAMP(6) WITH TIME ZONE", Scale: 6, Nullable: true}}
	m.ExpectBegin()
	p := m.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO "APP"."T" ("AT") VALUES (:p1)`))
	p.ExpectExec().WithArgs(oracleArrayMatch{n: 2, check: func(a []interface{}) bool {
		_, ok := a[0].(*goora.TimeStampTZ)
		v, ok2 := a[1].(goora.TimeStampTZ)
		return ok && oracleBoundNull(a[0]) && ok2 && time.Time(v).Format(time.RFC3339Nano) == "2024-01-01T00:00:00Z"
	}}).WillReturnResult(sqlmock.NewResult(0, 2))
	m.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"AT"}, []string{"TIMESTAMPTZ"}, [][]interface{}{{nil}, {"2024-01-01 08:00:00+08"}}))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPGOracleWriterRetainsSourceLengthAndRollsBack(t *testing.T) {
	w, m := oracleMockWriter(t)
	w.ConfigurePGOracle(map[string][]source.ColumnInfo{"T": {{Name: "V", DataType: "varchar", Length: 3, IsNullable: true}}})
	w.columns["T"] = map[string]valueconv.OracleColumn{"V": {DataType: "VARCHAR2", CharLength: 10, CharUsed: "C", Nullable: true}}
	m.ExpectBegin()
	m.ExpectPrepare("INSERT INTO")
	m.ExpectRollback()
	err := w.CopyData(context.Background(), "T", []string{"V"}, []string{"VARCHAR"}, [][]interface{}{{""}, {"toolong"}})
	require.ErrorContains(t, err, "页内第 2 行")
	require.Empty(t, w.MigrationWarnings())
	require.NoError(t, m.ExpectationsWereMet())
	w.ConfigurePGOracle(map[string][]source.ColumnInfo{})
	err = w.CopyData(context.Background(), "T", []string{"V"}, nil, [][]interface{}{{"a"}})
	require.ErrorContains(t, err, "缺少冻结源元数据")
}
