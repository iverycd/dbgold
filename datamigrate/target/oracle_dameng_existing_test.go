package target

import (
	"context"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func oracleDMExistingRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"name", "type", "chars", "bytes", "precision", "scale", "nullable", "units", "identity"})
}

func TestOracleDMExistingAcceptsCapacityCompatibleAliases(t *testing.T) {
	w, m := mockDM(t)
	m.ExpectQuery("SELECT c.COLUMN_NAME,c.DATA_TYPE").WithArgs("APP", "T").WillReturnRows(oracleDMExistingRows().
		AddRow("NC", "CHAR", 5, 20, 0, 0, "Y", "C", 0).
		AddRow("NV", "VARCHAR2", 5, 20, 0, 0, "Y", "C", 0).
		AddRow("L", "CLOB", 0, 0, 0, 0, "Y", "B", 0).
		AddRow("LR", "BLOB", 0, 0, 0, 0, "Y", "B", 0).
		AddRow("R", "BINARY", 0, 18, 0, 0, "Y", "B", 0).
		AddRow("UR", "VARBINARY", 0, 4000, 0, 0, "Y", "B", 0).
		AddRow("D", "TIMESTAMP", 0, 12, 0, 6, "Y", "B", 0))
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{
		{Name: "NC", DataType: "NCHAR", Length: 5, DataLength: 10, IsNullable: true},
		{Name: "NV", DataType: "NVARCHAR2", Length: 5, DataLength: 10, IsNullable: true},
		{Name: "L", DataType: "LONG", IsNullable: true},
		{Name: "LR", DataType: "LONG RAW", IsNullable: true},
		{Name: "R", DataType: "ROWID", IsNullable: true},
		{Name: "UR", DataType: "UROWID", IsNullable: true},
		{Name: "D", DataType: "DATE", IsNullable: true},
	}}
	require.NoError(t, w.ValidateOracleDamengExisting(context.Background(), info, dialect.TypeOpt{}))
}

func TestOracleDMExistingRejectsApproximateNumberTarget(t *testing.T) {
	w, m := mockDM(t)
	m.ExpectQuery("SELECT c.COLUMN_NAME,c.DATA_TYPE").WillReturnRows(oracleDMExistingRows().AddRow("N", "FLOAT", 0, 8, 53, 0, "Y", "B", 0))
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "N", DataType: "NUMBER", Precision: 38, Scale: 12}}}
	require.ErrorContains(t, w.ValidateOracleDamengExisting(context.Background(), info, dialect.TypeOpt{}), "不匹配")
}

func TestOracleDMExistingNationalColumnRequiresCharacterUnits(t *testing.T) {
	w, m := mockDM(t)
	m.ExpectQuery("SELECT c.COLUMN_NAME,c.DATA_TYPE").WillReturnRows(oracleDMExistingRows().AddRow("N", "NVARCHAR2", 10, 10, 0, 0, "Y", "B", 0))
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "N", DataType: "NVARCHAR2", Length: 10}}}
	require.ErrorContains(t, w.ValidateOracleDamengExisting(context.Background(), info, dialect.TypeOpt{}), "字符长度单位不足")
}

func TestOracleDMExistingRowIDRejectsFixedPadding(t *testing.T) {
	w, m := mockDM(t)
	m.ExpectQuery("SELECT c.COLUMN_NAME,c.DATA_TYPE").WillReturnRows(oracleDMExistingRows().AddRow("R", "BINARY", 0, 20, 0, 0, "Y", "B", 0))
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "R", DataType: "ROWID"}}}
	require.ErrorContains(t, w.ValidateOracleDamengExisting(context.Background(), info, dialect.TypeOpt{}), "BINARY(18)")
}

func TestOracleDMExistingFloatUsesActualColumnMetadata(t *testing.T) {
	w, m := mockDM(t)
	m.ExpectQuery("SELECT c.COLUMN_NAME,c.DATA_TYPE").WillReturnRows(oracleDMExistingRows().AddRow("F", "NUMBER", 0, 22, 0, 0, "Y", "B", 0))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT "F" FROM "APP"."T" WHERE 1=0`)).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("F").OfType("NUMBER", "").WithPrecisionAndScale(38, -1)))
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "F", DataType: "FLOAT", Precision: 126}}}
	require.NoError(t, w.ValidateOracleDamengExisting(context.Background(), info, dialect.TypeOpt{}))
	kind, err := w.OracleDamengColumnFloatKind(context.Background(), "t", "f")
	require.NoError(t, err)
	require.Equal(t, "decimal", kind)
}

func TestOracleDMExistingXMLRejectsUserTypeWithNativeName(t *testing.T) {
	w, m := mockDM(t)
	m.ExpectQuery("SELECT c.COLUMN_NAME,c.DATA_TYPE").WillReturnRows(oracleDMExistingRows().AddRow("X", "XMLTYPE", 0, 0, 0, 0, "Y", "B", 0))
	m.ExpectQuery("SELECT DATA_TYPE,NVL").WithArgs("APP", "T", "X").WillReturnRows(sqlmock.NewRows([]string{"type", "owner"}).AddRow("XMLTYPE", "APP"))
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "X", DataType: "XMLTYPE", TypeSchema: "SYS"}}}
	require.ErrorContains(t, w.ValidateOracleDamengExisting(context.Background(), info, dialect.TypeOpt{}), "APP.XMLTYPE")
}

func TestOracleDMExistingSpatialResolvesNativeClassOwner(t *testing.T) {
	w, m := mockDM(t)
	m.ExpectQuery("SELECT c.COLUMN_NAME,c.DATA_TYPE").WillReturnRows(oracleDMExistingRows().AddRow("G", "CLASS1234", 0, 0, 0, 0, "Y", "B", 0))
	m.ExpectQuery("SELECT DATA_TYPE,NVL").WithArgs("APP", "T", "G").WillReturnRows(sqlmock.NewRows([]string{"type", "owner"}).AddRow("CLASS1234", "SYSGEO2"))
	m.ExpectQuery("SELECT NAME,SF_GET_SCHEMA_NAME_BY_ID").WithArgs(int64(1234)).WillReturnRows(sqlmock.NewRows([]string{"type", "owner"}).AddRow("ST_GEOMETRY", "SYSGEO2"))
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "G", DataType: "SDO_GEOMETRY", TypeSchema: "MDSYS"}}}
	require.NoError(t, w.ValidateOracleDamengExisting(context.Background(), info, dialect.TypeOpt{}))
}

func TestOracleDMExistingLegacyLocalTimestampChecksUTCPath(t *testing.T) {
	for _, result := range []string{"2024-01-01 21:19:05.123456789 +00:00", "2024-01-02 03:04:05.123456789 +00:00"} {
		w, m := mockDM(t)
		m.ExpectQuery("SELECT c.COLUMN_NAME,c.DATA_TYPE").WillReturnRows(oracleDMExistingRows().AddRow("D", "TIMESTAMP WITH LOCAL TIME ZONE", 0, 12, 0, 9, "Y", "B", 0))
		m.ExpectQuery(`CAST\(TO_TIMESTAMP_TZ.*WITH LOCAL TIME ZONE`).WithArgs("2024-01-02 03:04:05.123456789 +05:45").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(result))
		info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "D", DataType: "TIMESTAMP(9) WITH LOCAL TIME ZONE"}}}
		err := w.ValidateOracleDamengExisting(context.Background(), info, dialect.TypeOpt{})
		if result == "2024-01-01 21:19:05.123456789 +00:00" {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, "UTC")
		}
	}
}
