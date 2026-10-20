package target

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"gitee.com/chunanyong/dm"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOracleDMOrdinaryColumnsNeedNoINIPrivilege(t *testing.T) {
	w, _ := mockDM(t)
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{
		{Name: "N", DataType: "NUMBER", Precision: 38, Scale: 12},
		{Name: "S", DataType: "VARCHAR2", Length: 100, CharUsed: "C"},
		{Name: "D", DataType: "TIMESTAMP(6)"},
		{Name: "R", DataType: "RAW", DataLength: 20},
	}}
	require.NoError(t, w.PrepareOracleDameng(context.Background(), []*source.TableDDLInfo{info}, dialect.TypeOpt{}))
	require.False(t, w.OracleDamengUsesDate())
	require.Empty(t, w.OracleDamengProfile().Mode)
}

func TestOracleDMStrictLoadProbeRejectsPermissiveAndUnrelatedFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		pass bool
	}{
		{name: "conversion_error", err: &dm.DmError{ErrCode: -6111, ErrText: "字符串转换出错"}, pass: true},
		{name: "permissive_zero"},
		{name: "permission", err: &dm.DmError{ErrCode: -5515, ErrText: "无权限"}},
		{name: "driver_error", err: errors.New("client conversion failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := mockDM(t)
			q := m.ExpectQuery(regexp.QuoteMeta(oracleDMStrictLoadProbeSQL)).WithArgs("dbgold_invalid_number")
			if tc.err != nil {
				q.WillReturnError(tc.err)
			} else {
				q.WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
			}
			for n := 0; n < 2; n++ {
				err := w.PrepareOracleDamengLoad(context.Background())
				if tc.pass {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "禁止继续写入")
				}
			}
		})
	}
}

func TestOracleDMDateUsesObservedFullSecondCapability(t *testing.T) {
	for _, tc := range []struct {
		name, mode, output string
		modeErr, dateErr   error
		useDate            bool
	}{
		{name: "native", mode: "0"},
		{name: "mysql", mode: "4"},
		{name: "postgres", mode: "7"},
		{name: "no_ini_permission", modeErr: errors.New("permission denied")},
		{name: "oracle_full_time", mode: "2", output: "2024-01-02 03:04:05", useDate: true},
		{name: "oracle_date_only", mode: "2", output: "2024-01-02 00:00:00"},
		{name: "oracle_probe_unavailable", mode: "2", dateErr: errors.New("unsupported")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := mockDM(t)
			q := m.ExpectQuery("SELECT PARA_VALUE FROM V\\$DM_INI")
			if tc.modeErr != nil {
				q.WillReturnError(tc.modeErr)
			} else {
				q.WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow(tc.mode))
			}
			if tc.mode == "2" {
				q = m.ExpectQuery(`SELECT TO_CHAR\(CAST\(TO_TIMESTAMP`).WithArgs("2024-01-02 03:04:05")
				if tc.dateErr != nil {
					q.WillReturnError(tc.dateErr)
				} else {
					q.WillReturnRows(sqlmock.NewRows([]string{"date"}).AddRow(tc.output))
				}
			}
			infos := []*source.TableDDLInfo{{TableName: "T", Columns: []source.ColumnInfo{{Name: "D", DataType: "DATE"}}}}
			require.NoError(t, w.PrepareOracleDameng(context.Background(), infos, dialect.TypeOpt{}))
			require.Equal(t, tc.useDate, w.OracleDamengUsesDate())
			// The same writer caches the feature, including conservative fallback.
			require.NoError(t, w.PrepareOracleDameng(context.Background(), infos, dialect.TypeOpt{}))
		})
	}
}

func TestOracleDMTimestampProbesUseBindingAndCache(t *testing.T) {
	w, m := mockDM(t)
	const plain = "2024-01-02 03:04:05.123456700"
	const zoned = "2024-01-02 03:04:05.123456789 +05:45"
	m.ExpectQuery(`TO_TIMESTAMP\(\?,.*AS TIMESTAMP\(7\)`).WithArgs(plain).WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(plain))
	m.ExpectQuery(`TO_TIMESTAMP_TZ\(\?,.*AS TIMESTAMP\(9\) WITH TIME ZONE`).WithArgs(zoned).WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(zoned))
	infos := []*source.TableDDLInfo{{TableName: "T", Columns: []source.ColumnInfo{
		{Name: "P", DataType: "TIMESTAMP(7)"},
		{Name: "Z", DataType: "TIMESTAMP(9) WITH TIME ZONE"},
		{Name: "L", DataType: "TIMESTAMP(9) WITH LOCAL TIME ZONE"},
	}}}
	require.NoError(t, w.PrepareOracleDameng(context.Background(), infos, dialect.TypeOpt{}))
	require.NoError(t, w.PrepareOracleDameng(context.Background(), infos, dialect.TypeOpt{}))
}

func TestOracleDMNationalProbeChecksMultibyteData(t *testing.T) {
	for _, output := range []string{"汉A", "汉"} {
		w, m := mockDM(t)
		m.ExpectQuery(regexp.QuoteMeta("SELECT CAST(? AS NVARCHAR(2 CHAR)) FROM DUAL")).WithArgs("汉A").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(output))
		infos := []*source.TableDDLInfo{{TableName: "T", Columns: []source.ColumnInfo{{Name: "N", DataType: "VARCHAR2", Length: 2}}}}
		err := w.PrepareOracleDameng(context.Background(), infos, dialect.TypeOpt{UseNvarchar2: true})
		if output == "汉A" {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, "T.N")
		}
	}
}

func TestOracleDMFloatProfilesUseActualMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, dbType, want string
		p                  int64
		decimal            bool
	}{
		{name: "NUMBER_MODE_1", p: 126, dbType: "FLOAT", decimal: true, want: "decimal"},
		{name: "native_small", p: 24, dbType: "FLOAT", want: "real"},
		{name: "native_large", p: 126, dbType: "FLOAT", want: "double"},
		{name: "native_canonical", p: 63, dbType: "DOUBLE", want: "double"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := mockDM(t)
			definition := sqlmock.NewColumn("F").OfType(tc.dbType, float64(0))
			if tc.decimal {
				definition.WithPrecisionAndScale(38, -1)
			}
			m.ExpectQuery(regexp.QuoteMeta("SELECT CAST(NULL AS FLOAT(" + strconvInt(tc.p) + ")) FROM DUAL")).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(definition).AddRow(nil))
			infos := []*source.TableDDLInfo{{TableName: "Mixed", Columns: []source.ColumnInfo{{Name: "F", DataType: "FLOAT", Precision: tc.p}}}}
			require.NoError(t, w.PrepareOracleDameng(context.Background(), infos, dialect.TypeOpt{}))
			kind, err := w.OracleDamengColumnFloatKind(context.Background(), "MIXED", "f")
			require.NoError(t, err)
			require.Equal(t, tc.want, kind)
			p := w.OracleDamengProfile()
			p.FloatKinds[tc.p] = "bad external mutation"
			require.Equal(t, tc.want, w.OracleDamengProfile().FloatKinds[tc.p])
		})
	}
}

func strconvInt(p int64) string { return fmt.Sprintf("%d", p) }

func TestOracleDMExistingFloatFamilySurvivesLaterPreparation(t *testing.T) {
	w, m := mockDM(t)
	m.ExpectQuery(regexp.QuoteMeta(`SELECT "F" FROM "APP"."T" WHERE 1=0`)).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("F").OfType("NUMBER", "").WithPrecisionAndScale(38, -1)))
	got, err := w.OracleDamengColumnFloatKind(context.Background(), "T", "F")
	require.NoError(t, err)
	require.Equal(t, "decimal", got)
	m.ExpectQuery(regexp.QuoteMeta("SELECT CAST(NULL AS FLOAT(126)) FROM DUAL")).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("F").OfType("DOUBLE", float64(0))))
	require.NoError(t, w.PrepareOracleDameng(context.Background(), []*source.TableDDLInfo{{TableName: "T", Columns: []source.ColumnInfo{{Name: "F", DataType: "FLOAT"}}}}, dialect.TypeOpt{}))
	got, err = w.OracleDamengColumnFloatKind(context.Background(), "T", "F")
	require.NoError(t, err)
	require.Equal(t, "decimal", got)
}

type oracleDMProbeConverter struct{}

func (oracleDMProbeConverter) ConvertValue(v any) (driver.Value, error) {
	if clob, ok := v.(*dm.DmClob); ok {
		return clob.ReadString(1, 2048)
	}
	return driver.DefaultParameterConverter.ConvertValue(v)
}

func TestOracleDMNativeXMLBfileSpatialCapabilities(t *testing.T) {
	db, m, err := sqlmock.New(sqlmock.ValueConverterOption(oracleDMProbeConverter{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); db.Close() })
	w := &DaMengWriter{db: db, schema: "APP", dia: dialect.NewDaMeng()}
	const xml = `<r xmlns="urn:dbgold"><v>汉A</v></r>`
	m.ExpectQuery("SELECT CAST\\(XMLSERIALIZE").WithArgs(xml).WillReturnRows(sqlmock.NewRows([]string{"xml"}).AddRow(xml))
	m.ExpectQuery(regexp.QuoteMeta("SELECT BFILENAME(?,?) FROM DUAL WHERE 1=0")).WithArgs(nil, nil).WillReturnRows(sqlmock.NewRows([]string{"bfile"}))
	m.ExpectQuery("SELECT COUNT.*ALL_TYPES").WithArgs("SYSGEO2", "ST_GEOMETRY").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	m.ExpectQuery("SELECT DMGEO2.ST_ASTEXT").WithArgs("SRID=4326;POINT (1.25 -2.5)", "SRID=4326;POINT (1.25 -2.5)").WillReturnRows(sqlmock.NewRows([]string{"wkt", "srid"}).AddRow("POINT(1.250000 -2.500000)", 4326))
	infos := []*source.TableDDLInfo{{TableName: "T", Columns: []source.ColumnInfo{
		{Name: "X", DataType: "XMLTYPE", TypeSchema: "SYS"},
		{Name: "B", DataType: "BFILE"},
		{Name: "G", DataType: "SDO_GEOMETRY", TypeSchema: "MDSYS"},
	}}}
	require.NoError(t, w.PrepareOracleDameng(context.Background(), infos, dialect.TypeOpt{}))
	require.NoError(t, w.PrepareOracleDameng(context.Background(), infos, dialect.TypeOpt{}))
}

func TestOracleDMSpatialUnavailableFailsOnlyRequiredFamily(t *testing.T) {
	w, m := mockDM(t)
	m.ExpectQuery("SELECT COUNT.*ALL_TYPES").WithArgs("SYSGEO2", "ST_GEOMETRY").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	err := w.PrepareOracleDameng(context.Background(), []*source.TableDDLInfo{{TableName: "T", Columns: []source.ColumnInfo{{Name: "G", DataType: "SDO_GEOMETRY", TypeSchema: "MDSYS"}}}}, dialect.TypeOpt{})
	require.ErrorContains(t, err, "T.G")
	require.ErrorContains(t, err, "不会自动初始化")
}
