package valueconv

import (
	"encoding/binary"
	"encoding/hex"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func spatialHeader(typ uint32) []byte {
	b := make([]byte, 5)
	b[0] = 1
	binary.LittleEndian.PutUint32(b[1:], typ)
	return b
}

func spatialCount(b []byte, n uint32) []byte {
	return binary.LittleEndian.AppendUint32(b, n)
}

func spatialCoordinates(b []byte, coordinates ...float64) []byte {
	for _, value := range coordinates {
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(value))
	}
	return b
}

func spatialRaw(srid uint32, wkb []byte) []byte {
	return append(binary.LittleEndian.AppendUint32(nil, srid), wkb...)
}

func TestMySQLSpatialHEXEWKBPreservesAllGeometryFamilies(t *testing.T) {
	point := spatialCoordinates(spatialHeader(1), 120.125123456789, -30.5)
	line := spatialCoordinates(spatialCount(spatialHeader(2), 2), 1, 2, -3, 4)
	polygon := spatialCoordinates(spatialCount(spatialCount(spatialHeader(3), 1), 4), 0, 0, 2, 0, 2, 2, 0, 0)
	cases := []struct {
		name string
		wkb  []byte
	}{
		{"POINT", point}, {"LINESTRING", line}, {"POLYGON", polygon},
		{"MULTIPOINT", append(spatialCount(spatialHeader(4), 1), point...)},
		{"MULTILINESTRING", append(spatialCount(spatialHeader(5), 1), line...)},
		{"MULTIPOLYGON", append(spatialCount(spatialHeader(6), 1), polygon...)},
		{"GEOMETRYCOLLECTION", append(append(spatialCount(spatialHeader(7), 2), point...), line...)},
		{"GEOMCOLLECTION", spatialCount(spatialHeader(7), 0)},
		{"GEOMETRY", append(spatialCount(spatialHeader(7), 1), spatialCount(spatialHeader(7), 0)...)},
	}
	for _, tc := range cases {
		for _, srid := range []uint32{0, 4326, 999999} {
			t.Run(tc.name, func(t *testing.T) {
				raw := spatialRaw(srid, tc.wkb)
				original := append([]byte(nil), raw...)
				value, err := NewPostgres().Convert(raw, "mysql", tc.name)
				require.NoError(t, err)
				encoded, err := hex.DecodeString(value.(string))
				require.NoError(t, err)
				require.Equal(t, binary.LittleEndian.Uint32(tc.wkb[1:5])|0x20000000, binary.LittleEndian.Uint32(encoded[1:5]))
				require.Equal(t, srid, binary.LittleEndian.Uint32(encoded[5:9]))
				require.Equal(t, tc.wkb[5:], encoded[9:], "coordinate payload must be byte-for-byte unchanged")
				require.Equal(t, original, raw, "input buffer must not be mutated")
			})
		}
	}
	got, err := NewPostgres().Convert(nil, "mysql", "POINT")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestMySQLSpatialHEXEWKBBigEndianAndKnownValue(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		wkb := make([]byte, 21)
		wkb[0] = 1
		if order == binary.BigEndian {
			wkb[0] = 0
		}
		order.PutUint32(wkb[1:5], 1)
		order.PutUint64(wkb[5:13], math.Float64bits(1))
		order.PutUint64(wkb[13:21], math.Float64bits(2))
		got, err := MySQLSpatialHEXEWKB(spatialRaw(4326, wkb), "POINT")
		require.NoError(t, err)
		if order == binary.LittleEndian {
			require.Equal(t, "0101000020e6100000000000000000f03f0000000000000040", got)
		} else {
			require.Equal(t, "0020000001000010e63ff00000000000004000000000000000", got)
		}
	}
}

func TestMySQLSpatialHEXEWKBRejectsMalformedValues(t *testing.T) {
	point := spatialCoordinates(spatialHeader(1), 1, 2)
	good := spatialRaw(4326, point)
	badOrder := append([]byte(nil), good...)
	badOrder[4] = 2
	nested := spatialCount(spatialHeader(7), 0)
	for range maxMySQLSpatialDepth {
		nested = append(spatialCount(spatialHeader(7), 1), nested...)
	}
	cases := [][]byte{
		{}, {0}, good[:4], good[:8], good[:len(good)-1], append(append([]byte(nil), good...), 0),
		badOrder, spatialRaw(1000000, point), spatialRaw(math.MaxUint32, point),
		spatialRaw(0, spatialHeader(8)), spatialRaw(0, spatialCount(spatialHeader(2), math.MaxUint32)),
		spatialRaw(0, append(spatialCount(spatialHeader(4), 1), spatialCount(spatialHeader(2), 0)...)),
		spatialRaw(0, nested), spatialRaw(0, spatialCoordinates(spatialHeader(1), math.Inf(1), 2)),
		spatialRaw(0, spatialCoordinates(spatialHeader(1), math.NaN(), 2)),
	}
	for i, raw := range cases {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			require.NotPanics(t, func() {
				_, err := NewPostgres().Convert(raw, "mysql", "GEOMETRY")
				require.Error(t, err)
			})
		})
	}
	_, err := NewPostgres().Convert("POINT(1 2)", "mysql", "POINT")
	require.Error(t, err)
	_, err = NewPostgres().Convert(good, "mysql", "POLYGON")
	require.ErrorContains(t, err, "不匹配")
}

func FuzzMySQLSpatialHEXEWKB(f *testing.F) {
	f.Add(spatialRaw(4326, spatialCoordinates(spatialHeader(1), 1, 2)))
	f.Add(spatialRaw(0, spatialCount(spatialHeader(7), 0)))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = MySQLSpatialHEXEWKB(raw, "GEOMETRY")
	})
}
