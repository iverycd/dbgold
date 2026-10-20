package target

import (
	"context"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/failure"
	"dbgold/datamigrate/source"
	"gitee.com/chunanyong/dm"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOracleDMExistingFeatureConnectionErrorsKeepCause(t *testing.T) {
	for _, tc := range []struct {
		name, sourceType, actualType, query string
		precision, scale                    int64
	}{
		{name: "float_metadata", sourceType: "FLOAT", actualType: "FLOAT", precision: 126, query: `SELECT "C" FROM`},
		{name: "local_timestamp", sourceType: "TIMESTAMP(6) WITH LOCAL TIME ZONE", actualType: "TIMESTAMP WITH LOCAL TIME ZONE", scale: 6, query: `SELECT TO_CHAR\(\(CAST\(TO_TIMESTAMP_TZ`},
		{name: "xml_native_type", sourceType: "XMLTYPE", actualType: "XMLTYPE", query: `SELECT DATA_TYPE,NVL\(DATA_TYPE_OWNER`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := mockDM(t)
			cause := &dm.DmError{ErrCode: 6001, ErrText: "communication error"}
			mock.ExpectQuery("SELECT c.COLUMN_NAME,c.DATA_TYPE").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "chars", "bytes", "precision", "scale", "nullable", "units", "identity"}).AddRow("C", tc.actualType, 0, 8, tc.precision, tc.scale, "Y", "B", 0))
			mock.ExpectQuery(tc.query).WillReturnError(cause)
			info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "C", DataType: tc.sourceType, Precision: tc.precision, IsNullable: true}}}
			err := w.ValidateOracleDamengExisting(context.Background(), info, dialect.TypeOpt{})
			require.ErrorIs(t, err, cause)
			require.True(t, failure.IsTaskError(err))
		})
	}
}
