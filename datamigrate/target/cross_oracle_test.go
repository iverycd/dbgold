package target

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	"github.com/DATA-DOG/go-sqlmock"
	goora "github.com/sijms/go-ora/v2"
	"github.com/stretchr/testify/require"
)

func TestCrossOracleSevenDigitTimestampBinding(t *testing.T) {
	for _, product := range []string{"dameng", "sqlserver"} {
		t.Run(product, func(t *testing.T) {
			w, m := oracleMockWriter(t)
			w.SetSourceType(product)
			w.ConfigureCrossOracle(map[string]*source.TableDDLInfo{"T": {Columns: []source.ColumnInfo{{Name: "AT", DataType: "datetimeoffset", DateTimePrecision: 7, IsNullable: true}, {Name: "PLAIN", DataType: "datetime2", DateTimePrecision: 7, IsNullable: true}}}})
			w.columns["T"] = map[string]valueconv.OracleColumn{"AT": {DataType: "TIMESTAMP(7) WITH TIME ZONE", Scale: 7, Nullable: true}, "PLAIN": {DataType: "TIMESTAMP(7)", Scale: 7, Nullable: true}}
			tm := time.Date(2026, 1, 2, 3, 4, 5, 123456700, time.FixedZone("", -3*3600-30*60))
			m.ExpectBegin()
			p := m.ExpectPrepare("INSERT INTO")
			p.ExpectExec().WithArgs(oracleArrayMatch{n: 2, check: func(a []any) bool {
				v, ok := a[1].(goora.TimeStampTZ)
				_, offset := time.Time(v).Zone()
				return oracleBoundNull(a[0]) && ok && time.Time(v).Nanosecond() == 123456700 && offset == -12600
			}}, oracleArrayMatch{n: 2, check: func(a []any) bool {
				v, ok := a[1].(goora.TimeStamp)
				return oracleBoundNull(a[0]) && ok && time.Time(v).Nanosecond() == 123456700
			}}).WillReturnResult(sqlmock.NewResult(0, 2))
			m.ExpectCommit()
			require.NoError(t, w.CopyData(context.Background(), "T", []string{"AT", "PLAIN"}, nil, [][]any{{nil, nil}, {tm, tm}}))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
func TestCrossOraclePageFailureKeepsPriorCommit(t *testing.T) {
	w, m := oracleMockWriter(t)
	w.SetSourceType("sqlserver")
	w.ConfigureCrossOracle(map[string]*source.TableDDLInfo{"T": {Columns: []source.ColumnInfo{{Name: "TXT", DataType: "nvarchar", Length: 10}}}})
	w.columns["T"] = map[string]valueconv.OracleColumn{"TXT": {DataType: "NVARCHAR2", CharLength: 10, CharUsed: "C"}}
	m.ExpectBegin()
	p := m.ExpectPrepare("INSERT INTO")
	p.ExpectExec().WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"TXT"}, nil, [][]any{{"中文"}}))
	m.ExpectBegin()
	m.ExpectPrepare("INSERT INTO")
	m.ExpectRollback()
	require.ErrorContains(t, w.CopyData(context.Background(), "T", []string{"TXT"}, nil, [][]any{{""}}), "非空列")
	require.NoError(t, m.ExpectationsWereMet())
}
func TestCrossOracleLOBEmptyIsDistinctFromNull(t *testing.T) {
	w, m := oracleMockWriter(t)
	w.SetSourceType("dameng")
	w.ConfigureCrossOracle(map[string]*source.TableDDLInfo{"T": {Columns: []source.ColumnInfo{{Name: "L", DataType: "BLOB", IsNullable: true}}}})
	w.columns["T"] = map[string]valueconv.OracleColumn{"L": {DataType: "BLOB", Nullable: true}}
	m.ExpectBegin()
	p := m.ExpectPrepare("EMPTY_BLOB")
	p.ExpectExec().WithArgs(sql.Named("p1", []any{(*goora.Blob)(nil), goora.Blob{Data: []byte{}}, goora.Blob{Data: []byte{0, 255}}}), sql.Named("p2", []any{0, 1, 0})).WillReturnResult(sqlmock.NewResult(0, 3))
	m.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"L"}, nil, [][]any{{nil}, {[]byte{}}, {[]byte{0, 255}}}))
	require.NoError(t, m.ExpectationsWereMet())
}
func crossSequenceFixture() source.SequenceInfo {
	return source.SequenceInfo{StartValueText: "12", Native: &source.NativeSequenceInfo{OracleCross: true, Schema: "dbo", SourceName: "Seq", Name: "SEQ", IncrementBy: "5", MinValue: "-100", MaxValue: "99999", CacheSize: "0"}}
}
func TestCrossOracleSequenceRejectsUnownedAndDrift(t *testing.T) {
	for _, id := range []int64{0, 123} {
		t.Run(string(rune('a'+id)), func(t *testing.T) {
			w, m := oracleMockWriter(t)
			s := crossSequenceFixture()
			s.Native.OracleObjectID = id
			m.ExpectQuery("SELECT o.OBJECT_ID").WithArgs("APP", "SEQ").WillReturnRows(sqlmock.NewRows([]string{"id", "step", "min", "max", "cycle", "order", "cache"}).AddRow(int64(123), "6", "-100", "99999", "N", "N", "0"))
			require.Error(t, w.createCrossOracleSequence(context.Background(), s))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
func TestCrossOracleSequencePersistsIdentityBeforeBinding(t *testing.T) {
	w, m := oracleMockWriter(t)
	s := crossSequenceFixture()
	s.Native.Bindings = []source.SequenceBinding{{Schema: "dbo", TableName: "T", ColumnName: "ID", Kind: "identity"}}
	m.ExpectQuery("SELECT o.OBJECT_ID").WillReturnRows(sqlmock.NewRows([]string{"id", "step", "min", "max", "cycle", "order", "cache"}))
	m.ExpectQuery("SELECT COUNT.*ALL_OBJECTS").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	m.ExpectQuery("SELECT TABLE_OWNER,TABLE_NAME,STATUS FROM ALL_TRIGGERS").WillReturnRows(sqlmock.NewRows([]string{"owner", "table", "status"}))
	ss := w.dia.SequenceStatements("APP", s, "")
	require.Len(t, ss, 2)
	m.ExpectExec("DECLARE n NUMBER").WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectQuery("SELECT OBJECT_ID FROM ALL_OBJECTS").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(321)))
	m.ExpectExec("CREATE OR REPLACE TRIGGER").WillReturnError(errors.New("trigger failure"))
	observed := false
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error {
		if e.SQL == ss[0].SQL && e.Status == "succeeded" {
			require.EqualValues(t, 321, s.Native.OracleObjectID)
			observed = true
		}
		return nil
	})
	require.ErrorContains(t, w.createCrossOracleSequence(ctx, s), "trigger failure")
	require.True(t, observed)
	require.EqualValues(t, 321, s.Native.OracleObjectID)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestCrossOracleCacheAndDescendingDDL(t *testing.T) {
	s := crossSequenceFixture()
	s.Native.CacheSize = "20"
	s.Native.IncrementBy = "-5"
	s.StartValueText = "-20"
	s.Native.Bindings = []source.SequenceBinding{{Schema: "dbo", TableName: "T", ColumnName: "ID", Kind: "identity"}}
	d := dialect.NewOracle()
	sql := dialect.JoinSQL(d.SequenceStatements("APP", s, ""))
	require.Contains(t, sql, "CACHE 20")
	require.Contains(t, sql, "MIN(")
	require.NotContains(t, sql, "DROP SEQUENCE")
}

func TestCrossOracleSequenceRepairRejectsChangedScopeAndProgram(t *testing.T) {
	for _, changed := range []string{"schema", "sql", "unknown"} {
		t.Run(changed, func(t *testing.T) {
			w, mock := oracleMockWriter(t)
			s := crossSequenceFixture()
			o := repair.Object{Kind: "sequence", Schema: "APP", TargetName: "SEQ", Sequence: &s}
			for _, stmt := range w.dia.SequenceStatements("APP", s, "") {
				o.Units = append(o.Units, repair.Unit{ID: "unit", SQL: stmt.SQL, OriginalSQL: stmt.SQL, Status: "failed", Selected: true})
			}
			units := append([]repair.Unit(nil), o.Units...)
			switch changed {
			case "schema":
				w.schema = "OTHER"
			case "sql":
				units[0].SQL = "DROP TABLE APP.T"
			case "unknown":
				units[0].Status = "unknown"
			}
			_, err := ExecuteRepair(context.Background(), w, o, units, nil)
			require.Error(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
