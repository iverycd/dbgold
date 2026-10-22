package dm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/big"
	"testing"
)

func exactTestDecimal(t *testing.T, raw string) *DmDecimal {
	t.Helper()
	value, ok := new(big.Float).SetPrec(512).SetString(raw)
	if !ok {
		t.Fatal("invalid fixture")
	}
	d, err := NewDecimalFromBigFloat(value)
	if err != nil {
		t.Fatal(err)
	}
	want, ok := new(big.Rat).SetString(raw)
	if !ok {
		t.Fatal("invalid rational fixture")
	}
	got, ok := new(big.Rat).SetString(d.String())
	if !ok || want.Cmp(got) != 0 {
		t.Fatalf("constructor lost digits: %s", d.String())
	}
	return d
}

func TestDBGoldDecimalFullNegativeMantissa(t *testing.T) {
	for _, raw := range []string{
		"-1234567890123456789.1234567890123456789",
		"1234567890123456789.1234567890123456789",
		"-0.12345678901234567890123456789012345678",
		"-99999999999999999999999999999999999999",
		"-1e124", "1e125", "-1e-128", "1e-128", "0",
	} {
		t.Run(raw, func(t *testing.T) {
			d := exactTestDecimal(t, raw)
			wire, err := d.encodeDecimal()
			if err != nil {
				t.Fatal(err)
			}
			if len(wire) > XDEC_SIZE {
				t.Fatalf("wire too wide: %x", wire)
			}
			decoded, err := decodeDecimal(wire, -1, -1)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := new(big.Rat).SetString(raw)
			got, ok := new(big.Rat).SetString(decoded.String())
			if !ok || want.Cmp(got) != 0 {
				t.Fatalf("wire roundtrip lost digits: %s => %x => %s", raw, wire, decoded.String())
			}
			if raw == "-1234567890123456789.1234567890123456789" {
				if len(wire) != 21 || wire[0] != 0x35 || wire[20] != 0x0b {
					t.Fatalf("negative 20-digit base100 payload truncated: %x", wire)
				}
			}
		})
	}
	tooWide := DmDecimal{sign: 1, weight: 0, digits: "123456789012345678901234567890123456789012", Valid: true}
	if _, err := tooWide.encodeDecimal(); err == nil {
		t.Fatal("must reject overlong mantissa instead of truncating")
	}
}

func TestDBGoldUDTLOBLengthAndPayload(t *testing.T) {
	for _, payload := range [][]byte{{}, {0}, {0, 1, 2, 0, 255}, bytes.Repeat([]byte{0, 1, 255}, 1024)} {
		t.Run("blob", func(t *testing.T) {
			wire, err := TypeDataSV.convertLobToBytes(*NewBlob(payload), BLOB, ENCODING_UTF8)
			if err != nil {
				t.Fatal(err)
			}
			if len(wire) != 4+len(payload) || binary.LittleEndian.Uint32(wire[:4]) != uint32(len(payload)) || !bytes.Equal(wire[4:], payload) {
				t.Fatalf("blob length/payload corrupt: %x", wire)
			}
		})
	}
	for _, text := range []string{"", "A", "中文😀\x00AbC"} {
		t.Run("clob", func(t *testing.T) {
			wire, err := TypeDataSV.convertLobToBytes(*NewClob(text), CLOB, ENCODING_UTF8)
			if err != nil {
				t.Fatal(err)
			}
			if len(wire) != 4+len([]byte(text)) || binary.LittleEndian.Uint32(wire[:4]) != uint32(len([]byte(text))) || !bytes.Equal(wire[4:], []byte(text)) {
				t.Fatalf("clob byte length/payload corrupt: %x", wire)
			}
		})
	}
	for _, fixture := range []struct{ encoding, text string }{{ENCODING_GB18030, "中文😀\x00AbC"}, {ENCODING_EUCKR, "한글\x00AbC"}} {
		t.Run(fixture.encoding, func(t *testing.T) {
			want := Dm_build_943.Dm_build_1159(fixture.text, fixture.encoding, nil)
			wire, err := TypeDataSV.convertLobToBytes(*NewClob(fixture.text), CLOB, fixture.encoding)
			if err != nil {
				t.Fatal(err)
			}
			if len(wire) != 4+len(want) || binary.LittleEndian.Uint32(wire[:4]) != uint32(len(want)) || !bytes.Equal(wire[4:], want) {
				t.Fatalf("encoded CLOB length/payload corrupt: %x", wire)
			}
		})
	}
}

func codecDescriptor(kind int32, conn *DmConnection) TypeDescriptor {
	return TypeDescriptor{column: &column{colType: kind, prec: 38, scale: 19}, m_conn: conn, m_serverEncoding: ENCODING_UTF8}
}

func TestDBGoldNestedObjectArrayActualCodecRoundtrip(t *testing.T) {
	for _, collection := range []bool{false, true} {
		t.Run(fmt.Sprintf("named_collection_%t", collection), func(t *testing.T) {
			conn := &DmConnection{dmConnector: &DmConnector{}, serverEncoding: ENCODING_UTF8}
			blobDesc, clobDesc, decimalDesc := codecDescriptor(BLOB, conn), codecDescriptor(CLOB, conn), codecDescriptor(DECIMAL, conn)
			innerDesc := codecDescriptor(CLASS, conn)
			innerDesc.m_size = 3
			innerDesc.m_fieldsObj = []TypeDescriptor{blobDesc, clobDesc, decimalDesc}
			arrayDesc := codecDescriptor(ARRAY, conn)
			if collection {
				arrayDesc.column.colType = CLASS
				arrayDesc.m_objId = 4
				arrayDesc.m_cltnType = CLTN_TYPE_VARRAY
			}
			arrayDesc.m_arrObj = &innerDesc
			outerDesc := codecDescriptor(CLASS, conn)
			outerDesc.m_size = 4
			outerDesc.m_fieldsObj = []TypeDescriptor{blobDesc, clobDesc, decimalDesc, arrayDesc}
			negative := exactTestDecimal(t, "-1234567890123456789.1234567890123456789")
			payload := []byte{0, 1, 2, 0, 255}
			text := "保留AbC中文😀\x00"
			inner := []interface{}{*NewBlob(payload), *NewClob(text), *negative}
			empty := []interface{}{*NewBlob([]byte{}), *NewClob(""), nil}
			values := []interface{}{*NewBlob(payload), *NewClob(text), *negative, []interface{}{inner, nil, empty}}
			obj := NewDmStruct("UnusedOfflineType", values)
			obj, err := obj.createByStructDescriptor(newStructDescriptorByTypeDescriptor(&outerDesc), conn)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := TypeDataSV.structToBytes(obj, &outerDesc)
			if err != nil {
				t.Fatal(err)
			}
			decodedValue, err := TypeDataSV.bytesToObj(wire, nil, &outerDesc)
			if err != nil {
				t.Fatal(err)
			}
			decoded := decodedValue.(*DmStruct)
			attributes, err := decoded.GetAttributes()
			if err != nil {
				t.Fatal(err)
			}
			check := func(attrs []interface{}, empty bool) {
				t.Helper()
				b := attrs[0].(*DmBlob)
				c := attrs[1].(*DmClob)
				bl, err := b.GetLength()
				if err != nil {
					t.Fatal(err)
				}
				cl, err := c.GetLength()
				if err != nil {
					t.Fatal(err)
				}
				if empty {
					if bl != 0 || cl != 0 || !b.Valid || !c.Valid || attrs[2] != nil {
						t.Fatalf("empty LOB/NULL changed: %#v", attrs)
					}
					return
				}
				got, err := b.getBytes(1, int32(bl))
				if err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("nested blob changed: %x %v", got, err)
				}
				gotText, err := c.getSubString(1, int32(cl))
				if err != nil || gotText != text {
					t.Fatalf("nested clob changed: %q %v", gotText, err)
				}
				if attrs[2].(*DmDecimal).String() != negative.String() {
					t.Fatalf("nested decimal changed: %s", attrs[2].(*DmDecimal).String())
				}
			}
			check(attributes, false)
			array := attributes[3].(*DmArray)
			elements, err := array.GetArray()
			if err != nil {
				t.Fatal(err)
			}
			list := elements.([]interface{})
			if len(list) != 3 || list[1] != nil {
				t.Fatalf("array NULL member changed: %#v", list)
			}
			first, err := list[0].(*DmStruct).GetAttributes()
			if err != nil {
				t.Fatal(err)
			}
			check(first, false)
			last, err := list[2].(*DmStruct).GetAttributes()
			if err != nil {
				t.Fatal(err)
			}
			check(last, true)
		})
	}
}

func TestDBGoldCollectionsNestedInCollectionsUseTheirElementDescriptors(t *testing.T) {
	conn := &DmConnection{dmConnector: &DmConnector{}, serverEncoding: ENCODING_UTF8}
	decimalDesc := codecDescriptor(DECIMAL, conn)
	inner := codecDescriptor(CLASS, conn)
	inner.m_objId, inner.m_cltnType, inner.m_arrObj = 4, CLTN_TYPE_VARRAY, &decimalDesc
	outer := codecDescriptor(CLASS, conn)
	outer.m_objId, outer.m_cltnType, outer.m_arrObj = 4, CLTN_TYPE_NST_TABLE, &inner
	root := codecDescriptor(CLASS, conn)
	root.m_size, root.m_fieldsObj = 1, []TypeDescriptor{outer}
	decimal := exactTestDecimal(t, "-1234567890123456789.1234567890123456789")
	value := NewDmStruct("UnusedOfflineType", []interface{}{[]interface{}{[]interface{}{*decimal, nil}, nil, []interface{}{}}})
	value, err := value.createByStructDescriptor(newStructDescriptorByTypeDescriptor(&root), conn)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := TypeDataSV.structToBytes(value, &root)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := TypeDataSV.bytesToObj(wire, nil, &root)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := decoded.(*DmStruct).GetAttributes()
	if err != nil {
		t.Fatal(err)
	}
	arrays, err := fields[0].(*DmArray).GetArray()
	if err != nil {
		t.Fatal(err)
	}
	list := arrays.([]interface{})
	if len(list) != 3 || list[1] != nil {
		t.Fatalf("nested collection multiplicity/null changed: %#v", list)
	}
	first, err := list[0].(*DmArray).GetArray()
	if err != nil {
		t.Fatal(err)
	}
	items := first.([]interface{})
	if len(items) != 2 || items[1] != nil || items[0].(*DmDecimal).String() != decimal.String() {
		t.Fatalf("nested decimal/null changed: %#v", items)
	}
	last, err := list[2].(*DmArray).GetArray()
	if err != nil {
		t.Fatal(err)
	}
	if last != nil && len(last.([]interface{})) != 0 {
		t.Fatal("empty nested collection changed")
	}
}

func TestDBGoldUDTLOBErrorsPropagate(t *testing.T) {
	conn := &DmConnection{dmConnector: &DmConnector{}, serverEncoding: ENCODING_UTF8}
	for _, kind := range []int32{BLOB, CLOB} {
		desc := codecDescriptor(kind, conn)
		if _, err := TypeDataSV.typeDataToBytes(newTypeData("wrong value type", nil), &desc); err == nil {
			t.Fatalf("LOB codec error silently discarded for %d", kind)
		}
	}
}
