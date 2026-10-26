package dm

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

func TestDBGoldNativeArrayDescriptorRecursesIntoElement(t *testing.T) {
	for _, kind := range []int32{ARRAY, SARRAY} {
		file := TypeDescriptor{column: &column{colType: VARCHAR, prec: BFILE_PREC, scale: BFILE_SCALE}}
		array := TypeDescriptor{column: &column{colType: kind}, m_length: 5, m_arrObj: &file}
		got, err := array.getClassDescChkInfo()
		want := make([]byte, 12)
		binary.LittleEndian.PutUint16(want, ARRAY)
		binary.LittleEndian.PutUint32(want[2:], 5)
		binary.LittleEndian.PutUint16(want[6:], VARCHAR)
		binary.LittleEndian.PutUint16(want[8:], BFILE_PREC)
		binary.LittleEndian.PutUint16(want[10:], BFILE_SCALE)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("native array checksum descriptor: got=%x want=%x err=%v", got, want, err)
		}
	}
}

func TestDBGoldShortStaticArrayPaddingPreservesFollowingAttribute(t *testing.T) {
	conn := &DmConnection{dmConnector: &DmConnector{}, serverEncoding: ENCODING_UTF8}
	file := codecDescriptor(VARCHAR, conn)
	file.column.prec, file.column.scale = BFILE_PREC, BFILE_SCALE
	array := codecDescriptor(SARRAY, conn)
	array.m_length, array.m_arrObj = 5, &file
	text := codecDescriptor(VARCHAR, conn)
	text.column.prec, text.column.scale = 100, 0
	outer := codecDescriptor(CLASS, conn)
	outer.m_size, outer.m_fieldsObj = 2, []TypeDescriptor{array, text}
	object := NewDmStruct("Offline", []interface{}{[]interface{}{"Dir:first"}, "after"})
	object, err := object.createByStructDescriptor(newStructDescriptorByTypeDescriptor(&outer), conn)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := TypeDataSV.structToBytes(object, &outer)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := TypeDataSV.bytesToObj(wire, nil, &outer)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := decoded.(*DmStruct).GetAttributes()
	if err != nil {
		t.Fatal(err)
	}
	values, err := fields[0].(*DmArray).GetArray()
	want := []interface{}{"Dir:first", nil, nil, nil, nil}
	if err != nil || !reflect.DeepEqual(values, want) || fields[1] != "after" {
		t.Fatalf("static NULL padding corrupted a sibling attribute: values=%#v fields=%#v err=%v", values, fields, err)
	}
}
