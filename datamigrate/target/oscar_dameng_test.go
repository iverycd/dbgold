package target

import (
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"gitee.com/chunanyong/dm"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOscarDamengDropOnlyIgnoresMissingTable(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"missing", &dm.DmError{ErrCode: -2106}},
		{"permission", errors.New("DROP denied")},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, mock := mockDM(t)
			w.SetSourceType("oscar")
			const drop = `DROP TABLE "APP"."T" CASCADE CONSTRAINTS`
			const create = `CREATE TABLE "APP"."T" ("ID" INT)`
			mock.ExpectExec(regexp.QuoteMeta(drop)).WillReturnError(test.err)
			if test.name == "missing" {
				mock.ExpectExec(regexp.QuoteMeta(create)).WillReturnResult(sqlmock.NewResult(0, 0))
				require.NoError(t, w.CreateTable(t.Context(), []dialect.Statement{{SQL: drop}, {SQL: create}}))
			} else {
				require.ErrorContains(t, w.CreateTable(t.Context(), []dialect.Statement{{SQL: drop}, {SQL: create}}), "DROP denied")
			}
		})
	}
}

func TestOscarDamengWriterBindsExactDataAndRollsBackInvalidPage(t *testing.T) {
	for _, bad := range []bool{false, true} {
		name := "mapped values"
		if bad {
			name = "invalid bit string"
		}
		t.Run(name, func(t *testing.T) {
			w, mock := oracleDamengBindingWriter(t)
			w.SetSourceType("oscar")
			mock.ExpectQuery(`SELECT TO_CHAR\(CAST`).WithArgs("03:04:05.123456 +05:45").WillReturnRows(sqlmock.NewRows([]string{"clock"}).AddRow("03:04:05.123456 +05:45"))
			mock.ExpectBegin()
			const insert = `INSERT /*+ MY_STRICT_TABLES(7) */ INTO "APP"."T" ("BITS", "OK", "AMOUNT", "DOCUMENT", "CLOCK") VALUES (?, ?, ?, XMLTYPE(CAST(? AS CLOB)), CAST(CAST(? AS VARCHAR(64)) AS TIME(6) WITH TIME ZONE))`
			stmt := mock.ExpectPrepare(regexp.QuoteMeta(insert))
			const amount = "1234567890123456789.1234567890123456789"
			const doc = `<doc>中文</doc>`
			row := []any{"010", true, amount, doc, "15:04:05.000001+08"}
			stmt.ExpectExec().WithArgs([]byte{0, 1, 0}, int64(1), oracleDamengDecimalArgument(amount), oracleDamengClobArgument(doc), "15:04:05.000001 +08:00").WillReturnResult(sqlmock.NewResult(0, 1))
			rows := [][]any{row}
			if bad {
				rows = append(rows, []any{"02", true, amount, doc, "15:04:05+08"})
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}
			err := w.CopyData(t.Context(), "T", []string{"Bits", "Ok", "Amount", "Document", "Clock"}, []string{"VARBIT", "BOOL", "NUMERIC", "XMLTYPE", "TIMETZ"}, rows)
			if bad {
				require.ErrorContains(t, err, "T.Bits")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestOscarDamengWriteRejectsSilentValueChanges(t *testing.T) {
	t.Run("Oracle-compatible empty text", func(t *testing.T) {
		w, mock := mockDM(t)
		w.SetSourceType("oscar")
		mock.ExpectQuery(`SELECT CASE WHEN '' IS NULL`).WithArgs("").WillReturnRows(sqlmock.NewRows([]string{"nullified"}).AddRow(1))
		err := w.CopyData(t.Context(), "T", []string{"Label"}, []string{"VARCHAR"}, [][]any{{nil}, {""}})
		require.ErrorContains(t, err, "T.Label")
		require.ErrorContains(t, err, "空字符串转换为 NULL")
	})
	t.Run("nanosecond timestamp import", func(t *testing.T) {
		w, mock := mockDM(t)
		w.SetSourceType("oscar")
		mock.ExpectQuery(`SELECT TO_CHAR\(CAST\(TO_TIMESTAMP`).WithArgs("2024-01-02 03:04:05.123456789").WillReturnRows(sqlmock.NewRows([]string{"timestamp"}).AddRow("2024-01-02 03:04:05.123456000"))
		info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "SeenAt", DataType: "TIMESTAMP", DateTimePrecision: 9}}}
		err := w.PrepareOscarDameng(t.Context(), info)
		require.ErrorContains(t, err, "T.SeenAt")
		require.ErrorContains(t, err, "改变小数秒")
		err = w.CopyData(t.Context(), "T", []string{"SeenAt"}, []string{"TIMESTAMP"}, [][]any{{"2026-11-19 03:04:05.123456789"}})
		require.ErrorContains(t, err, "改变小数秒")
	})
	t.Run("incompatible repertoire", func(t *testing.T) {
		w, mock := mockDM(t)
		w.SetSourceType("oscar")
		mock.ExpectQuery(regexp.QuoteMeta(oracleDMLoadCharsetSQL)).WillReturnRows(sqlmock.NewRows([]string{"charset"}).AddRow(2))
		err := w.CopyData(t.Context(), "T", []string{"Label"}, []string{"VARCHAR"}, [][]any{{"💾"}})
		require.ErrorContains(t, err, "目标字符集无损编码")
	})
}

func TestOscarDamengAppendChecksTargetMetadata(t *testing.T) {
	for _, chars := range []int64{4, 9} {
		w, mock := mockDM(t)
		rows := sqlmock.NewRows([]string{"column", "type", "chars", "bytes", "precision", "scale", "nullable", "units", "identity"}).
			AddRow("TITLE", "VARCHAR2", chars, chars*4, 0, 0, "Y", "C", 0).
			AddRow("VAL", "DECIMAL", 0, 0, 18, 3, "N", "B", 0)
		mock.ExpectQuery(regexp.QuoteMeta(damengExistingColumnsSQL)).WithArgs("APP", "T").WillReturnRows(rows)
		info := &source.TableDDLInfo{TableName: "t", Columns: []source.ColumnInfo{
			{Name: "Title", DataType: "VARCHAR", Length: 9, IsNullable: true},
			{Name: "Val", DataType: "NUMERIC", Precision: 18, Scale: 3},
		}}
		err := w.ValidateOscarDamengExisting(t.Context(), info)
		if chars < 9 {
			require.ErrorContains(t, err, "t.Title")
		} else {
			require.NoError(t, err)
		}
	}
}

func TestOscarDamengIdentityChecksProgressionWithoutWriting(t *testing.T) {
	for _, current := range []string{"31", "32", "29"} {
		w, mock := mockDM(t)
		w.SetSourceType("oscar")
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM SYS.SYSCOLUMNS`).WithArgs("APP", "T", "ID").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		mock.ExpectQuery(`SELECT IDENT_INCR`).WithArgs(`"APP"."T"`, `"APP"."T"`).WillReturnRows(sqlmock.NewRows([]string{"step", "current"}).AddRow("2", current))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT MAX("ID") FROM "APP"."T"`)).WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow("31"))
		err := w.CreateSequence(t.Context(), source.SequenceInfo{TableName: "t", ColumnName: "id", StartValue: 33, IdentityInfo: &source.IdentityInfo{Increment: 2}}, "")
		if current != "31" {
			require.ErrorContains(t, err, "IDENTITY 下一值")
		} else {
			require.NoError(t, err)
		}
	}
}
