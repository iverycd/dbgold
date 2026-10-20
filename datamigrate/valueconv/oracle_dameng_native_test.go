package valueconv

import (
	"math"
	"math/big"
	"testing"

	"gitee.com/chunanyong/dm"
	goora "github.com/sijms/go-ora/v2"
	"github.com/stretchr/testify/require"
)

func TestOracleDamengFloatingTargetRepresentations(t *testing.T) {
	v, err := OracleDamengFloat("1.23456789012345678901234567890123456789", "FLOAT", false, 64)
	require.NoError(t, err)
	require.Equal(t, float64(1.2345678901234567), v)
	v, err = OracleDamengFloat("1.23456789", "FLOAT", false, 32)
	require.NoError(t, err)
	require.Equal(t, float64(float32(1.23456789)), v)
	v, err = OracleDamengFloat(float32(0.1), "BINARY_FLOAT", false, 64)
	require.NoError(t, err)
	require.Equal(t, float64(float32(0.1)), v)
	v, err = OracleDamengFloat(float32(0.1), "BINARY_FLOAT", true, 64)
	require.NoError(t, err)
	actual, ok := new(big.Rat).SetString(v.(*dm.DmDecimal).String())
	require.True(t, ok)
	require.Zero(t, actual.Cmp(new(big.Rat).SetFloat64(float64(float32(0.1)))))
	v, err = OracleDamengFloat("1.23456789012345678901234567890123456789", "FLOAT", true, 64)
	require.NoError(t, err)
	require.IsType(t, &dm.DmDecimal{}, v)
	for _, raw := range []any{"NaN", "+Inf", "1e309", "1e-999", math.Inf(-1), "not-number"} {
		_, err := OracleDamengFloat(raw, "FLOAT", false, 64)
		require.Error(t, err, raw)
	}
	_, err = OracleDamengFloat("1e39", "FLOAT", false, 32)
	require.Error(t, err)
	_, err = OracleDamengFloat("1e-129", "FLOAT", true, 64)
	require.Error(t, err)
	_, err = OracleDameng("123456789012345678901234567890123456789", "NUMBER")
	require.Error(t, err, "NUMBER must not inherit floating rounding")
}

func TestOracleDamengIdentifiersAndNullLOBs(t *testing.T) {
	v, err := OracleDameng("AAAPecAAEAAAAFkAAA", "ROWID")
	require.NoError(t, err)
	require.Equal(t, []byte("AAAPecAAEAAAAFkAAA"), v)
	v, err = OracleDameng("0000000E.000A.0007", "ROWID")
	require.NoError(t, err)
	require.Equal(t, []byte("0000000E.000A.0007"), v)
	for _, discriminator := range []string{"A", "*", "-", "(", ")"} {
		v, err := OracleDameng(discriminator+"ABCD0123+/", "UROWID")
		require.NoError(t, err)
		require.Equal(t, []byte(discriminator+"ABCD0123+/"), v)
	}
	v, err = OracleDameng("*ABCD0123+/", "UROWID")
	require.NoError(t, err)
	require.Equal(t, []byte("*ABCD0123+/"), v)
	for _, tc := range []struct{ raw, kind string }{{"short", "ROWID"}, {"中文", "UROWID"}, {"a;drop", "UROWID"}} {
		_, err := OracleDameng(tc.raw, tc.kind)
		require.Error(t, err)
	}
	for _, kind := range []string{"CHAR", "VARCHAR2", "NCHAR", "NVARCHAR2"} {
		v, err := OracleDameng("", kind)
		require.NoError(t, err)
		require.Nil(t, v)
	}
	for _, kind := range []string{"CLOB", "NCLOB", "LONG"} {
		v, err := OracleDameng("", kind)
		require.NoError(t, err)
		require.IsType(t, &dm.DmClob{}, v)
		v, err = OracleDameng(nil, kind)
		require.NoError(t, err)
		require.Nil(t, v)
	}
	for _, kind := range []string{"BLOB", "LONG RAW"} {
		v, err := OracleDameng([]byte{}, kind)
		require.NoError(t, err)
		require.IsType(t, &dm.DmBlob{}, v)
	}
}

func TestOracleDamengNativeXMLAndSpatialCodecs(t *testing.T) {
	for _, raw := range []string{`<x xmlns="urn:test">汉字 &amp; AbC</x>`, `<a/><b/>`} {
		v, err := OracleDameng(raw, "SYS.XMLTYPE")
		require.NoError(t, err)
		require.IsType(t, &dm.DmClob{}, v)
	}
	for _, raw := range []string{"", "<x>", "<x>&unknown;</x>"} {
		_, err := OracleDameng(raw, "XMLTYPE")
		require.Error(t, err)
	}
	for _, raw := range []string{"SRID=4326;POINT (120.125 30.5)", "SRID=0;LINESTRING(0 0,1 2)", "SRID=4326;POLYGON((0 0,1 0,1 1,0 0))", "SRID=4326;GEOMETRYCOLLECTION(POINT(1 2),LINESTRING(0 0,1 2))", "SRID=4326;POINT EMPTY"} {
		v, err := OracleDameng(raw, "MDSYS.SDO_GEOMETRY")
		require.NoError(t, err, raw)
		require.Equal(t, raw, v)
	}
	for _, raw := range []string{"SRID=NULL;POINT(1 2)", "SRID=4326;POINT Z (1 2 3)", "SRID=4326;POINT(1 2 3)", "SRID=4326;CIRCULARSTRING(1 2,3 4,5 6)", "SRID=4326;POINT(1 2);DROP TABLE T", "SRID=4326;POINT(1e999 2)", "SRID=4326;POINT(1 2"} {
		_, err := OracleDameng(raw, "MDSYS.SDO_GEOMETRY")
		require.Error(t, err, raw)
	}
	require.Equal(t, "XMLTYPE(CAST(? AS CLOB))", OracleDamengPlaceholder("SYS.XMLTYPE"))
	require.Equal(t, "DMGEO2.ST_GEOMFROMEWKT(?)", OracleDamengPlaceholder("SDE.ST_GEOMETRY"))
}

func TestOracleDamengBFileIsReferenceOnly(t *testing.T) {
	file := oracleDMTestFile{"SourceDir", "AbC.xml"}
	v, err := OracleDameng(file, "BFILE")
	require.NoError(t, err)
	require.Equal(t, "SourceDir:AbC.xml", v)
	_, err = OracleDameng(oracleDMTestFile{"DIR", "bad:name"}, "BFILE")
	require.Error(t, err)
	v, err = OracleDameng(goora.CreateNullBFile(), "BFILE")
	require.NoError(t, err)
	require.Nil(t, v)
}

type oracleDMTestFile struct{ dir, file string }

func (f oracleDMTestFile) GetDirName() string  { return f.dir }
func (f oracleDMTestFile) GetFileName() string { return f.file }
