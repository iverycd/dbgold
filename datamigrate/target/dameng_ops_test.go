package target

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func mockDM(t *testing.T) (*DaMengWriter, sqlmock.Sqlmock) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); db.Close() })
	return &DaMengWriter{db: db, schema: "APP", srcType: "sqlserver", dia: dialect.NewDaMeng(), valueConv: valueconv.NewDaMeng(), identCache: map[string]bool{}}, m
}

func TestDamengColumnMappingValidation(t *testing.T) {
	for _, cols := range [][]string{{"ROW_ID_2", "ROW_ID"}, {"ROW_ID", "OLD_NAME"}, {"ROW_ID"}, {}} {
		w, mock := mockDM(t)
		rows := sqlmock.NewRows([]string{"COLUMN_NAME"})
		for _, c := range cols {
			rows.AddRow(c)
		}
		mock.ExpectQuery("SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS").WithArgs("APP", "T").WillReturnRows(rows)
		err := w.ValidateDamengColumns(context.Background(), &source.TableDDLInfo{TableName: "t", Columns: []source.ColumnInfo{{Name: "ROW_ID_2"}, {Name: "ROW_ID"}}})
		if len(cols) == 2 && cols[0] == "ROW_ID_2" {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
}
func TestDamengIdentityDetectionFailsClosed(t *testing.T) {
	w, m := mockDM(t)
	m.ExpectQuery("SELECT COUNT").WithArgs("APP", "MIXED").WillReturnError(errors.New("permission denied"))
	e := w.CopyData(context.Background(), "Mixed", []string{"Id"}, []string{"INT"}, [][]interface{}{{int64(1)}})
	require.ErrorContains(t, e, "permission denied")
	require.Empty(t, w.identCache)
}
func TestDamengIdentityBatchLifecycle(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "insert failure"}[fail], func(t *testing.T) {
			w, m := mockDM(t)
			m.ExpectQuery("SELECT COUNT").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
			m.ExpectBegin()
			m.ExpectExec(regexp.QuoteMeta(`SET IDENTITY_INSERT "APP"."T" ON`)).WillReturnResult(sqlmock.NewResult(0, 0))
			p := m.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO "APP"."T" ("ID") VALUES (?)`))
			if fail {
				p.ExpectExec().WithArgs(int64(1)).WillReturnError(errors.New("write failed"))
				m.ExpectRollback()
				m.ExpectExec(regexp.QuoteMeta(`SET IDENTITY_INSERT "APP"."T" OFF`)).WillReturnResult(sqlmock.NewResult(0, 0))
			} else {
				p.ExpectExec().WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectExec(regexp.QuoteMeta(`SET IDENTITY_INSERT "APP"."T" OFF`)).WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectCommit()
			}
			e := w.CopyData(context.Background(), "T", []string{"Id"}, []string{"INT"}, [][]interface{}{{int64(1)}})
			if fail {
				require.ErrorContains(t, e, "write failed")
			} else {
				require.NoError(t, e)
			}
		})
	}
}
func TestDamengIdentityCleanupFailure(t *testing.T) {
	w, m := mockDM(t)
	w.identCache["T"] = true
	m.ExpectBegin()
	m.ExpectExec("SET IDENTITY_INSERT.*ON").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectPrepare("INSERT INTO").ExpectExec().WillReturnError(errors.New("insert failed"))
	m.ExpectRollback()
	m.ExpectExec("SET IDENTITY_INSERT.*OFF").WillReturnError(errors.New("cleanup failed"))
	e := w.CopyData(context.Background(), "T", []string{"Id"}, []string{"INT"}, [][]interface{}{{1}})
	require.ErrorContains(t, e, "cleanup failed")
	require.ErrorContains(t, e, "insert failed")
}
func TestDamengSequenceFailureReported(t *testing.T) {
	w, m := mockDM(t)
	seq := source.SequenceInfo{TableName: "T", ColumnName: "Id", StartValue: 55, IdentityInfo: &source.IdentityInfo{Increment: 5}}
	m.ExpectExec(regexp.QuoteMeta(`ALTER TABLE "APP"."T" DROP IDENTITY`)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(regexp.QuoteMeta(`ALTER TABLE "APP"."T" ADD COLUMN "ID" IDENTITY(55, 5)`)).WillReturnError(errors.New("cannot reseed"))
	require.ErrorContains(t, w.CreateSequence(context.Background(), seq, ""), "cannot reseed")
}
func TestDamengExistingSourcesStillConvert(t *testing.T) {
	w, _ := mockDM(t)
	w.srcType = "mysql"
	require.Equal(t, []interface{}{int64(5), []byte{0, 255}}, w.convertRow([]interface{}{[]byte{5}, []byte{0, 255}}, []string{"BIT", "BLOB"}))
	w.srcType = "postgres"
	require.Equal(t, []interface{}{1, nil}, w.convertRow([]interface{}{true, nil}, []string{"BOOL", "BYTEA"}))
}

func TestDamengCancelledWriteCleansIdentity(t *testing.T) {
	w, m := mockDM(t)
	w.identCache["T"] = true
	m.ExpectBegin()
	m.ExpectExec("SET IDENTITY_INSERT.*ON").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectPrepare("INSERT INTO").ExpectExec().WillReturnError(context.Canceled)
	m.ExpectRollback()
	m.ExpectExec("SET IDENTITY_INSERT.*OFF").WillReturnResult(sqlmock.NewResult(0, 0))
	err := w.CopyData(context.Background(), "T", []string{"Id"}, []string{"INT"}, [][]interface{}{{int64(1)}})
	require.ErrorIs(t, err, context.Canceled)
}

func TestDamengExistingTargetRejectsNarrowing(t *testing.T) {
	w, m := mockDM(t)
	m.ExpectQuery("SELECT c.COLUMN_NAME,c.DATA_TYPE").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "chars", "bytes", "precision", "scale", "nullable", "units", "identity"}).AddRow("VALUE", "DECIMAL", 0, 22, 10, 2, "Y", "B", 0))
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "Value", DataType: "decimal", Precision: 38, Scale: 18, IsNullable: true}}}
	require.ErrorContains(t, w.ValidateSQLServerDamengExisting(context.Background(), info, dialect.TypeOpt{}), "数值精度不足")
}

func TestDamengExistingTargetAcceptsCompatibleColumns(t *testing.T) {
	w, m := mockDM(t)
	m.ExpectQuery("SELECT c.COLUMN_NAME,c.DATA_TYPE").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "chars", "bytes", "precision", "scale", "nullable", "units", "identity"}).AddRow("VALUE", "DECIMAL", 0, 22, 38, 18, "Y", "B", 0).AddRow("UID", "VARCHAR", 36, 36, 0, 0, "Y", "B", 0))
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "Value", DataType: "decimal", Precision: 38, Scale: 18, IsNullable: true}, {Name: "Uid", DataType: "uniqueidentifier", IsNullable: true}}}
	require.NoError(t, w.ValidateSQLServerDamengExisting(context.Background(), info, dialect.TypeOpt{CharInLength: true}))
}
