package target

import (
	"context"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOracleDamengCapabilitiesFailBeforeDDL(t *testing.T) {
	for _, mode := range []string{"0", "1"} {
		w, m := mockDM(t)
		w.srcType = "oracle"
		m.ExpectQuery("SELECT PARA_NAME,PARA_VALUE").WillReturnRows(sqlmock.NewRows([]string{"PARA_NAME", "PARA_VALUE"}).AddRow("COMPATIBLE_MODE", "2").AddRow("IFUN_DATETIME_MODE", mode))
		e := w.ValidateOracleDamengCapabilities(context.Background(), []*source.TableDDLInfo{{TableName: "T", Columns: []source.ColumnInfo{{Name: "TS", DataType: "TIMESTAMP(9)"}}}})
		if mode == "0" {
			require.ErrorContains(t, e, "IFUN_DATETIME_MODE=1")
		} else {
			require.NoError(t, e)
		}
	}
}

func TestOracleDamengExistingRejectsDateTimeLoss(t *testing.T) {
	w, m := mockDM(t)
	w.srcType = "oracle"
	m.ExpectQuery("SELECT c.COLUMN_NAME,c.DATA_TYPE").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "chars", "bytes", "precision", "scale", "nullable", "units", "identity"}).AddRow("D", "DATE", 0, 3, 0, 0, "Y", "B", 0))
	e := w.ValidateOracleDamengExisting(context.Background(), &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "D", DataType: "DATE", IsNullable: true}}}, dialect.TypeOpt{})
	require.ErrorContains(t, e, "TIMESTAMP(0)")
}

func oracleDMTestSequence() source.SequenceInfo {
	return source.SequenceInfo{StartValueText: "100", Native: &source.NativeSequenceInfo{Name: "SEQ_T", IncrementBy: "1", MinValue: "1", MaxValue: "9223372036854775807", CacheSize: "20", Bindings: []source.SequenceBinding{{TableName: "T", ColumnName: "ID", Kind: "trigger", TriggerName: "TRG_T", OnlyWhenNull: true}}}}
}

func TestOracleDamengNativeSequenceCollisionDoesNotMutate(t *testing.T) {
	w, m := mockDM(t)
	w.srcType = "oracle"
	m.ExpectQuery("SELECT COUNT.*ALL_OBJECTS").WithArgs("APP", "SEQ_T").WillReturnRows(sqlmock.NewRows([]string{"COUNT"}).AddRow(1))
	require.ErrorContains(t, w.CreateSequence(context.Background(), oracleDMTestSequence(), ""), "禁止覆盖")
	s := oracleDMTestSequence()
	s.Native.MaxValue = "9999999999999999999999999999"
	require.ErrorContains(t, dialect.ValidateOracleDamengSequence(s), "int64")
	s = oracleDMTestSequence()
	s.Native.Cycle = true
	require.ErrorContains(t, dialect.ValidateOracleDamengSequence(s), "循环")
}

func TestOracleDamengCopyUsesExplicitTemporalConversions(t *testing.T) {
	w, m := mockDM(t)
	w.srcType = "oracle"
	w.identCache["T"] = false
	m.ExpectBegin()
	p := m.ExpectPrepare(`INSERT INTO .*TO_TIMESTAMP\(\?, 'YYYY-MM-DD HH24:MI:SS.FF9'\).*TO_TIMESTAMP_TZ`)
	p.ExpectExec().WithArgs("2024-01-02 03:04:05.123456789", "2024-01-02 03:04:05.987654321 -05:00").WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"TS", "TZ"}, []string{"TIMESTAMP(9)", "TIMESTAMP(9) WITH TIME ZONE"}, [][]interface{}{{"2024-01-02 03:04:05.123456789", "2024-01-02 03:04:05.987654321 -05:00"}}))
}
