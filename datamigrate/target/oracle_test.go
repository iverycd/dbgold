package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	"github.com/DATA-DOG/go-sqlmock"
	goora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/converters"
	"github.com/sijms/go-ora/v2/network"
	"github.com/stretchr/testify/require"
)

type oracleArrayConverter struct{}

func (oracleArrayConverter) ConvertValue(v interface{}) (driver.Value, error) { return v, nil }

type oracleArrayMatch struct {
	n     int
	check func([]interface{}) bool
}

func (a oracleArrayMatch) Match(v driver.Value) bool {
	x, ok := v.([]interface{})
	return ok && len(x) == a.n && (a.check == nil || a.check(x))
}

func oracleBoundNull(v interface{}) bool {
	return v != nil && reflect.ValueOf(v).Kind() == reflect.Ptr && reflect.ValueOf(v).IsNil()
}
func oracleMockWriter(t *testing.T) (*OracleWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, m, e := sqlmock.New(sqlmock.ValueConverterOption(oracleArrayConverter{}))
	require.NoError(t, e)
	t.Cleanup(func() { db.Close() })
	w := newOracleWriter(db, "APP")
	w.username = "APP"
	return w, m
}
func TestOracleCopyBatchAndCommittedWarnings(t *testing.T) {
	w, m := oracleMockWriter(t)
	w.columns["T"] = map[string]valueconv.OracleColumn{"ID": {Name: "ID", DataType: "NUMBER", Precision: 20}, "TXT": {Name: "TXT", DataType: "VARCHAR2", Nullable: true}}
	rows := make([][]interface{}, 501)
	for i := range rows {
		rows[i] = []interface{}{i + 1, ""}
	}
	m.ExpectBegin()
	p := m.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO "APP"."T" ("ID", "TXT") VALUES (:p1, :p2)`))
	p.ExpectExec().WithArgs(oracleArrayMatch{n: 500, check: func(a []interface{}) bool { n, e := a[499].(*goora.Number).Int64(); return e == nil && n == 500 }}, oracleArrayMatch{n: 500, check: func(a []interface{}) bool { return oracleBoundNull(a[0]) && oracleBoundNull(a[499]) }}).WillReturnResult(sqlmock.NewResult(0, 500))
	p.ExpectExec().WithArgs(oracleArrayMatch{n: 1}, oracleArrayMatch{n: 1}).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"ID", "TXT"}, []string{"INT", "VARCHAR"}, rows))
	require.Equal(t, []valueconv.MigrationWarning{{Table: "T", Column: "TXT", Reason: "空字符串转 NULL", Count: 501}}, w.MigrationWarnings())
	require.NoError(t, m.ExpectationsWereMet())
}
func TestOracleCopyRollsBackEntirePage(t *testing.T) {
	for _, commitFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "second-array", true: "commit"}[commitFailure], func(t *testing.T) {
			w, m := oracleMockWriter(t)
			w.columns["T"] = map[string]valueconv.OracleColumn{"TXT": {DataType: "VARCHAR2", Nullable: true}}
			rows := make([][]interface{}, 501)
			for i := range rows {
				rows[i] = []interface{}{""}
			}
			m.ExpectBegin()
			p := m.ExpectPrepare("INSERT INTO")
			p.ExpectExec().WithArgs(oracleArrayMatch{n: 500}).WillReturnResult(sqlmock.NewResult(0, 500))
			if commitFailure {
				p.ExpectExec().WithArgs(oracleArrayMatch{n: 1}).WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectCommit().WillReturnError(errors.New("commit failed"))
			} else {
				p.ExpectExec().WithArgs(oracleArrayMatch{n: 1}).WillReturnError(errors.New("array failed"))
				m.ExpectRollback()
			}
			require.Error(t, w.CopyData(context.Background(), "T", []string{"TXT"}, []string{"VARCHAR"}, rows))
			require.Empty(t, w.MigrationWarnings())
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
func TestOracleLOBNullEmptyAndMixed(t *testing.T) {
	w, m := oracleMockWriter(t)
	w.columns["T"] = map[string]valueconv.OracleColumn{"LOB": {DataType: "BLOB", Nullable: true}}
	m.ExpectBegin()
	p := m.ExpectPrepare(`CASE WHEN :p2 = 1 THEN EMPTY_BLOB\(\) ELSE :p1 END`)
	p.ExpectExec().WithArgs(
		sql.Named("p1", []interface{}{(*goora.Blob)(nil), goora.Blob{Data: []byte{}}, goora.Blob{Data: []byte{0, 1}}}),
		sql.Named("p2", []interface{}{0, 1, 0}),
	).WillReturnResult(sqlmock.NewResult(0, 3))
	m.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"LOB"}, []string{"BLOB"}, [][]interface{}{{nil}, {[]byte{}}, {[]byte{0, 1}}}))
	require.NoError(t, m.ExpectationsWereMet())
}
func TestOracleNonNullConversionErrorAndCancellation(t *testing.T) {
	w, m := oracleMockWriter(t)
	w.columns["T"] = map[string]valueconv.OracleColumn{"TXT": {DataType: "VARCHAR2"}}
	m.ExpectBegin()
	m.ExpectPrepare("INSERT INTO")
	m.ExpectRollback()
	err := w.CopyData(context.Background(), "T", []string{"TXT"}, []string{"VARCHAR"}, [][]interface{}{{""}})
	require.ErrorContains(t, err, "表 T 列 TXT 页内第 1 行")
	require.Empty(t, w.MigrationWarnings())
	require.NoError(t, m.ExpectationsWereMet())
	w, m = oracleMockWriter(t)
	w.columns["T"] = map[string]valueconv.OracleColumn{"TXT": {DataType: "VARCHAR2"}}
	m.ExpectBegin()
	p := m.ExpectPrepare("INSERT INTO")
	p.ExpectExec().WillDelayFor(time.Second).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectRollback()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	require.Error(t, w.CopyData(ctx, "T", []string{"TXT"}, []string{"VARCHAR"}, [][]interface{}{{"x"}}))
	require.Eventually(t, func() bool { return m.ExpectationsWereMet() == nil }, time.Second, time.Millisecond)
	require.Empty(t, w.MigrationWarnings())
}
func TestOracleDropOnlyIgnoresMissingTable(t *testing.T) {
	for _, code := range []int{942, 1031} {
		t.Run(map[int]string{942: "missing", 1031: "permission"}[code], func(t *testing.T) {
			w, m := oracleMockWriter(t)
			m.ExpectExec("DROP TABLE").WillReturnError(&network.OracleError{ErrCode: code})
			if code == 942 {
				m.ExpectExec("CREATE TABLE").WillReturnResult(sqlmock.NewResult(0, 0))
			}
			e := w.CreateTable(context.Background(), []dialect.Statement{{SQL: `DROP TABLE "APP"."T"`}, {SQL: `CREATE TABLE "APP"."T" ("ID" NUMBER)`}})
			if code == 942 {
				require.NoError(t, e)
			} else {
				require.Error(t, e)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
func TestOracleSequenceProvenance(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign", true: "owned"}[owned], func(t *testing.T) {
			w, m := oracleMockWriter(t)
			s := source.SequenceInfo{TableName: "T", ColumnName: "ID"}
			name, trig := w.dia.SequenceName(s), w.dia.TriggerName(s)
			m.ExpectQuery("SELECT OBJECT_TYPE FROM ALL_OBJECTS").WithArgs("APP", name).WillReturnRows(sqlmock.NewRows([]string{"OBJECT_TYPE"}).AddRow("SEQUENCE"))
			m.ExpectQuery("SELECT TABLE_OWNER,TABLE_NAME").WithArgs("APP", trig).WillReturnRows(sqlmock.NewRows([]string{"TABLE_OWNER", "TABLE_NAME", "TRIGGERING_EVENT", "TRIGGER_TYPE"}).AddRow("APP", "T", "INSERT", "BEFORE EACH ROW"))
			text := "user trigger"
			if owned {
				text = w.dia.SequenceStatements("APP", source.SequenceInfo{TableName: "T", ColumnName: "ID", StartValue: 1}, "")[1].SQL
			}
			m.ExpectQuery("SELECT TEXT FROM ALL_SOURCE").WithArgs("APP", trig).WillReturnRows(sqlmock.NewRows([]string{"TEXT"}).AddRow(text))
			e := w.inspectSequence(context.Background(), s)
			if owned {
				require.NoError(t, e)
				require.True(t, w.dia.OwnedSequences[name])
			} else {
				require.ErrorContains(t, e, "归属标记")
				require.False(t, w.dia.OwnedSequences[name])
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
func TestOraclePreflightPrivilegeAndExistingObjects(t *testing.T) {
	w, m := oracleMockWriter(t)
	m.ExpectQuery("SELECT PRIVILEGE FROM SESSION_PRIVS").WillReturnRows(sqlmock.NewRows([]string{"PRIVILEGE"}).AddRow("DROP ANY TABLE"))
	err := w.PreflightOracle(context.Background(), map[string]*source.TableDDLInfo{"T": {}}, []OracleObject{{Kind: "TABLE", Name: "T", Table: "T"}}, nil, true, false)
	require.ErrorContains(t, err, "CREATE TABLE")
	require.NoError(t, m.ExpectationsWereMet())
	w, m = oracleMockWriter(t)
	m.ExpectQuery("SELECT PRIVILEGE FROM SESSION_PRIVS").WillReturnRows(sqlmock.NewRows([]string{"PRIVILEGE"}).AddRow("CREATE VIEW"))
	m.ExpectQuery("SELECT OBJECT_TYPE FROM ALL_OBJECTS").WithArgs("APP", "V").WillReturnRows(sqlmock.NewRows([]string{"OBJECT_TYPE"}).AddRow("TABLE"))
	err = w.PreflightOracle(context.Background(), nil, []OracleObject{{Kind: "VIEW", Name: "V"}}, nil, false, false)
	require.ErrorContains(t, err, "TABLE")
	require.NoError(t, m.ExpectationsWereMet())
}
func TestOracleReadTargetMetadataAndFloatBinding(t *testing.T) {
	w, m := oracleMockWriter(t)
	m.ExpectQuery("SELECT COLUMN_NAME, DATA_TYPE").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "NULLABLE", "DATA_PRECISION", "DATA_SCALE", "DATA_LENGTH", "CHAR_LENGTH", "CHAR_USED"}).AddRow("N", "BINARY_DOUBLE", "Y", nil, nil, 8, 0, nil))
	m.ExpectBegin()
	p := m.ExpectPrepare(regexp.QuoteMeta("TO_BINARY_DOUBLE(:p1, '9.9999999999999999EEEE', 'NLS_NUMERIC_CHARACTERS=''.,''')"))
	p.ExpectExec().WithArgs(oracleArrayMatch{n: 2, check: func(a []interface{}) bool { return oracleBoundNull(a[0]) && strings.HasSuffix(a[1].(string), "e+308") }}).WillReturnResult(sqlmock.NewResult(0, 2))
	m.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"N"}, []string{"DOUBLE"}, [][]interface{}{{nil}, {"1e308"}}))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOraclePromotedVarcharEmptyIsNull(t *testing.T) {
	w, m := oracleMockWriter(t)
	w.columns["T"] = map[string]valueconv.OracleColumn{"TXT": {DataType: "CLOB", Nullable: true}}
	m.ExpectBegin()
	p := m.ExpectPrepare("INSERT INTO")
	p.ExpectExec().WithArgs(oracleArrayMatch{n: 1, check: func(a []interface{}) bool { return oracleBoundNull(a[0]) }}, oracleArrayMatch{n: 1, check: func(a []interface{}) bool { return a[0] == 0 }}).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"TXT"}, []string{"VARCHAR"}, [][]interface{}{{""}}))
	require.Equal(t, int64(1), w.MigrationWarnings()[0].Count)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOraclePreflightChecksColumnCharset(t *testing.T) {
	w, m := oracleMockWriter(t)
	w.codec = converters.NewStringConverter(1) // US7ASCII
	err := w.PreflightOracle(context.Background(), map[string]*source.TableDDLInfo{"T": {Columns: []source.ColumnInfo{{Name: "中文"}}}}, nil, nil, true, false)
	require.ErrorContains(t, err, "表 T")
	require.ErrorContains(t, err, "目标字符集")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleAllNullArrayKeepsTargetTypes(t *testing.T) {
	w, m := oracleMockWriter(t)
	cols := []string{"N", "D", "C", "NC", "B", "NV"}
	types := []string{"NUMBER", "TIMESTAMP(6)", "CLOB", "NCLOB", "BLOB", "NVARCHAR2"}
	w.columns["T"] = map[string]valueconv.OracleColumn{}
	for i, name := range cols {
		w.columns["T"][name] = valueconv.OracleColumn{DataType: types[i], Nullable: true}
	}
	expect := func(v interface{}) oracleArrayMatch {
		return oracleArrayMatch{n: 2, check: func(a []interface{}) bool {
			return oracleBoundNull(a[0]) && oracleBoundNull(a[1]) && reflect.TypeOf(v) == reflect.TypeOf(a[0]) && reflect.TypeOf(v) == reflect.TypeOf(a[1])
		}}
	}
	flag := oracleArrayMatch{n: 2, check: func(a []interface{}) bool { return a[0] == 0 && a[1] == 0 }}
	m.ExpectBegin()
	p := m.ExpectPrepare("INSERT INTO")
	p.ExpectExec().WithArgs(expect((*goora.Number)(nil)), expect((*goora.TimeStamp)(nil)), expect((*goora.Clob)(nil)), flag, expect((*goora.NClob)(nil)), flag, expect((*goora.Blob)(nil)), flag, expect((*goora.NVarChar)(nil))).WillReturnResult(sqlmock.NewResult(0, 2))
	m.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "T", cols, nil, [][]interface{}{make([]interface{}, 6), make([]interface{}, 6)}))
	require.Empty(t, w.MigrationWarnings())
	require.NoError(t, m.ExpectationsWereMet())
}
