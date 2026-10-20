package target

import (
	"context"
	"database/sql/driver"
	"math"
	"math/big"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/valueconv"
	"gitee.com/chunanyong/dm"
	"github.com/DATA-DOG/go-sqlmock"
	goora "github.com/sijms/go-ora/v2"
	"github.com/stretchr/testify/require"
)

type oracleDamengDecimalArgument string

func (a oracleDamengDecimalArgument) Match(v driver.Value) bool {
	decimal, ok := v.(*dm.DmDecimal)
	if !ok {
		return false
	}
	want, ok := new(big.Rat).SetString(string(a))
	if !ok {
		return false
	}
	actual, ok := new(big.Rat).SetString(decimal.String())
	return ok && actual.Cmp(want) == 0
}

type oracleDamengClobArgument string

func (a oracleDamengClobArgument) Match(v driver.Value) bool {
	clob, ok := v.(*dm.DmClob)
	if !ok {
		return false
	}
	actual, err := clob.ReadString(1, len([]rune(a)))
	return err == nil && actual == string(a)
}

type oracleDamengEmptyLOBArgument byte

func (a oracleDamengEmptyLOBArgument) Match(v driver.Value) bool {
	if a == 'C' {
		clob, ok := v.(*dm.DmClob)
		if !ok || !clob.Valid {
			return false
		}
		length, err := clob.GetLength()
		return err == nil && length == 0
	}
	blob, ok := v.(*dm.DmBlob)
	if !ok || !blob.Valid {
		return false
	}
	length, err := blob.GetLength()
	return err == nil && length == 0
}

func oracleDamengBindingWriter(t *testing.T) (*DaMengWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.ValueConverterOption(oracleArrayConverter{}))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		_ = db.Close()
	})
	w := &DaMengWriter{db: db, schema: "APP", srcType: "oracle", dia: dialect.NewDaMeng(), valueConv: valueconv.NewDaMeng(), identCache: map[string]bool{"T": false}}
	// Binding tests supply known effective target limits. Dedicated load tests
	// exercise the real catalog queries and character-set lookup.
	w.oracleDM.load = &oracleDMLoadCache{charset: 1, charsetKnown: true, tables: map[string]map[string]oracleDMLoadColumn{"T": {
		"RID": {kind: "BINARY", bytes: 18}, "URID": {kind: "VARBINARY", bytes: 4000},
		"V": {kind: "NVARCHAR", chars: 36, bytes: 144, units: "C"},
		"C": {kind: "CLOB"}, "B": {kind: "BLOB"}, "F": {kind: "BFILE", bytes: 512},
	}}}
	return w, mock
}

func TestOracleDamengCopyBindsActualTargetFloatFamily(t *testing.T) {
	for _, tc := range []struct {
		name, sourceType, targetType string
		value                        any
		argument                     driver.Value
		decimal                      bool
	}{
		{"binary32_rounding", "FLOAT(24)", "REAL", "16777217", float64(float32(16777217)), false},
		{"binary64_rounding", "FLOAT(126)", "DOUBLE", "9007199254740993", float64(9007199254740992), false},
		{"decimal_float", "FLOAT(126)", "DECIMAL", "12345678901234567890.123456789", oracleDamengDecimalArgument("12345678901234567890.123456789"), true},
		{"oracle_binary_float_to_decimal", "BINARY_FLOAT", "DECIMAL", float32(1.5), oracleDamengDecimalArgument("1.5"), true},
		{"oracle_binary_float_widening", "BINARY_FLOAT", "DOUBLE", float32(0.1), float64(float32(0.1)), false},
		{"oracle_binary_double", "BINARY_DOUBLE", "DOUBLE", float64(1.25), float64(1.25), false},
		{"null_float", "REAL", "REAL", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := oracleDamengBindingWriter(t)
			column := sqlmock.NewColumn("F").OfType(tc.targetType, float64(0))
			if tc.decimal {
				column.WithPrecisionAndScale(38, 12)
			}
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT "F" FROM "APP"."T" WHERE 1=0`)).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(column))
			mock.ExpectBegin()
			statement := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT /*+ MY_STRICT_TABLES(7) */ INTO "APP"."T" ("F") VALUES (?)`))
			statement.ExpectExec().WithArgs(tc.argument).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			require.NoError(t, w.CopyData(context.Background(), "T", []string{"F"}, []string{tc.sourceType}, [][]any{{tc.value}}))
		})
	}
}

func TestOracleDamengFloatFailureRejectsEntirePageBeforeWrite(t *testing.T) {
	w, mock := oracleDamengBindingWriter(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT "F" FROM "APP"."T" WHERE 1=0`)).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("F").OfType("DOUBLE", float64(0))))
	err := w.CopyData(context.Background(), "T", []string{"F"}, []string{"BINARY_DOUBLE"}, [][]any{{float64(1.25)}, {math.Inf(1)}})
	require.ErrorContains(t, err, "T.F")
}

func TestOracleDamengNumberKeepsExactDecimalBinding(t *testing.T) {
	w, mock := oracleDamengBindingWriter(t)
	mock.ExpectBegin()
	statement := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT /*+ MY_STRICT_TABLES(7) */ INTO "APP"."T" ("N") VALUES (?)`))
	statement.ExpectExec().WithArgs(oracleDamengDecimalArgument("12345678901234567890.123456789012345678")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"N"}, []string{"NUMBER"}, [][]any{{"12345678901234567890.123456789012345678"}}))
}

func TestOracleDamengUnknownFloatTargetFailsBeforeWrite(t *testing.T) {
	w, mock := oracleDamengBindingWriter(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT "F" FROM "APP"."T" WHERE 1=0`)).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("F").OfType("VARCHAR", "")))
	err := w.CopyData(context.Background(), "T", []string{"F"}, []string{"FLOAT"}, [][]any{{"1.25"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "T.F")
}

func TestOracleDamengCopyNeedsActualCapacityBeforeTransaction(t *testing.T) {
	w, mock := mockDM(t)
	w.srcType = "oracle"
	mock.ExpectQuery(regexp.QuoteMeta(oracleDMLoadColumnsSQL)).WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "chars", "bytes", "units"}))
	mock.ExpectQuery(regexp.QuoteMeta(oracleDMLoadCharsetSQL)).WillReturnRows(sqlmock.NewRows([]string{"charset"}).AddRow(1))
	err := w.CopyData(context.Background(), "T", []string{"V"}, []string{"VARCHAR2"}, [][]any{{"text"}})
	require.ErrorContains(t, err, "T.V 缺少写入容量元数据")
}

func TestOracleDamengStructuredValuesStayParameterized(t *testing.T) {
	w, mock := oracleDamengBindingWriter(t)
	mock.ExpectBegin()
	statement := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT /*+ MY_STRICT_TABLES(7) */ INTO "APP"."T" ("RID", "URID", "DOC", "G") VALUES (?, ?, XMLTYPE(CAST(? AS CLOB)), DMGEO2.ST_GEOMFROMEWKT(?))`))
	statement.ExpectExec().WithArgs([]byte("AAAAABAAAAAAAABAAB"), []byte("AAAAABAAAAAAAABAAB"), oracleDamengClobArgument(`<r key="AbC">中文</r>`), "SRID=4326;POINT (1 2)").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"RID", "URID", "DOC", "G"}, []string{"ROWID", "UROWID", "SYS.XMLTYPE", "MDSYS.SDO_GEOMETRY"}, [][]any{{"AAAAABAAAAAAAABAAB", "AAAAABAAAAAAAABAAB", `<r key="AbC">中文</r>`, "SRID=4326;POINT (1 2)"}}))
}

func TestOracleDamengCopyDistinguishesNullCharacterAndEmptyLOB(t *testing.T) {
	w, mock := oracleDamengBindingWriter(t)
	mock.ExpectBegin()
	statement := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT /*+ MY_STRICT_TABLES(7) */ INTO "APP"."T" ("V", "C", "B", "F") VALUES (?, ?, ?, ?)`))
	statement.ExpectExec().WithArgs(nil, oracleDamengEmptyLOBArgument('C'), oracleDamengEmptyLOBArgument('B'), nil).WillReturnResult(sqlmock.NewResult(0, 1))
	statement.ExpectExec().WithArgs(nil, nil, nil, nil).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"V", "C", "B", "F"}, []string{"VARCHAR2", "CLOB", "BLOB", "BFILE"}, [][]any{{"", "", []byte{}, goora.CreateNullBFile()}, {nil, nil, nil, (*goora.BFile)(nil)}}))
}

func TestOracleDamengCopyBindsBFileLocatorWithoutOpeningFile(t *testing.T) {
	w, mock := oracleDamengBindingWriter(t)
	mock.ExpectExec(regexp.QuoteMeta("SELECT :1 FROM DUAL")).WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	file, err := goora.CreateBFile(w.db, "MixedDirectory", "source's-file.txt")
	require.NoError(t, err)
	mock.ExpectBegin()
	statement := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT /*+ MY_STRICT_TABLES(7) */ INTO "APP"."T" ("F") VALUES (?)`))
	statement.ExpectExec().WithArgs("MixedDirectory:source's-file.txt").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"F"}, []string{"BFILE"}, [][]any{{file}}))
}
