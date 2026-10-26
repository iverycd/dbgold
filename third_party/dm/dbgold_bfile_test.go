package dm

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestDBGoldBFileDescriptorVisitorActualCodecRoundtrip(t *testing.T) {
	for _, kind := range []int32{ARRAY, SARRAY, CLASS} {
		t.Run(fmt.Sprintf("kind_%d", kind), func(t *testing.T) {
			conn := &DmConnection{dmConnector: &DmConnector{}, serverEncoding: ENCODING_UTF8}
			file := codecDescriptor(VARCHAR, conn)
			file.column.prec, file.column.scale = BFILE_PREC, BFILE_SCALE
			ordinary := codecDescriptor(VARCHAR, conn)
			ordinary.column.prec, ordinary.column.scale = BFILE_PREC, 0
			object := codecDescriptor(CLASS, conn)
			object.m_size, object.m_fieldsObj = 3, []TypeDescriptor{file, ordinary, codecDescriptor(CLOB, conn)}
			array := codecDescriptor(kind, conn)
			array.m_length = 3
			if kind == CLASS {
				array.m_objId, array.m_cltnType = 4, CLTN_TYPE_VARRAY
			}
			array.m_arrObj = &object
			files := codecDescriptor(CLASS, conn)
			files.m_objId, files.m_cltnType, files.m_arrObj = 4, CLTN_TYPE_NST_TABLE, &file
			outer := codecDescriptor(CLASS, conn)
			outer.m_size, outer.m_fieldsObj = 3, []TypeDescriptor{object, array, files}
			first := []interface{}{"MiX.Dir:File 中文.bin", "not:a:file", *NewClob("ordinary:lob")}
			last := []interface{}{"MiX.Dir:Trailing ", "OtherDir:missing", *NewClob("")}
			value := NewDmStruct("Offline", []interface{}{first, []interface{}{first, nil, last}, []interface{}{"Another:文件", nil, "MiX.Dir:File 中文.bin"}})
			value, err := value.createByStructDescriptor(newStructDescriptorByTypeDescriptor(&outer), conn)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := TypeDataSV.structToBytes(value, &outer)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := TypeDataSV.bytesToObj(wire, nil, &outer)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			err = decoded.(*DmStruct).VisitBFileLocators(func(raw string) error { got = append(got, raw); return nil })
			want := []string{"MiX.Dir:File 中文.bin", "MiX.Dir:File 中文.bin", "MiX.Dir:Trailing ", "Another:文件", "MiX.Dir:File 中文.bin"}
			if err != nil || !reflect.DeepEqual(want, got) {
				t.Fatalf("BFILE descriptor roundtrip: got %q, want %q, err=%v", got, want, err)
			}
			fields, err := decoded.(*DmStruct).GetAttributes()
			if err != nil {
				t.Fatal(err)
			}
			got = nil
			if err := fields[2].(*DmArray).VisitBFileLocators(func(raw string) error { got = append(got, raw); return nil }); err != nil || !reflect.DeepEqual(want[3:], got) {
				t.Fatalf("top-level collection: got %q, err=%v", got, err)
			}
		})
	}
}

func TestDBGoldBFileVisitorNullsUnmaterializedAndErrors(t *testing.T) {
	visit := func(string) error { t.Fatal("unexpected locator"); return nil }
	for _, object := range []*DmStruct{nil, {}, {Valid: false}} {
		if err := object.VisitBFileLocators(visit); err != nil {
			t.Fatal(err)
		}
	}
	for _, array := range []*DmArray{nil, {}, {Valid: false}} {
		if err := array.VisitBFileLocators(visit); err != nil {
			t.Fatal(err)
		}
	}
	if NewDmStruct("unbound", []interface{}{"Dir:file"}).VisitBFileLocators(visit) == nil || NewDmArray("unbound", []interface{}{"Dir:file"}).VisitBFileLocators(visit) == nil {
		t.Fatal("an unmaterialized constructor has no server type evidence")
	}
	if (*DmStruct)(nil).VisitBFileLocators(nil) == nil || (*DmArray)(nil).VisitBFileLocators(nil) == nil {
		t.Fatal("nil callbacks must fail explicitly")
	}
	file := TypeDescriptor{column: &column{colType: VARCHAR, prec: BFILE_PREC, scale: BFILE_SCALE}}
	objectDesc := TypeDescriptor{column: &column{colType: CLASS}, m_size: 1, m_fieldsObj: []TypeDescriptor{file}}
	object := newDmStructByTypeData([]TypeData{*newTypeData("Dir:file", nil)}, &objectDesc)
	want := errors.New("callback failed")
	if err := object.VisitBFileLocators(func(string) error { return want }); !errors.Is(err, want) {
		t.Fatalf("callback error was lost: %v", err)
	}
	object.m_attribs[0].m_dumyData = 17
	if object.VisitBFileLocators(visit) == nil {
		t.Fatal("invalid typed locator must not be silently ignored")
	}
	object.m_attribs = nil
	if object.VisitBFileLocators(visit) == nil {
		t.Fatal("a descriptor/value shape mismatch must fail")
	}
	if _, err := object.GetAttributes(); err == nil {
		t.Fatal("a missing decoded attribute list must not masquerade as an empty object")
	}
	arrayDesc := TypeDescriptor{column: &column{colType: ARRAY}}
	arrayDesc.m_arrObj = &arrayDesc
	array := newDmArrayByTypeData(nil, &arrayDesc)
	array.m_arrData = []TypeData{*newTypeData(array, nil)}
	if array.VisitBFileLocators(visit) == nil {
		t.Fatal("cyclic object data must not recurse forever")
	}
}

func TestDBGoldComplexAccessorsRejectMissingServerDescriptors(t *testing.T) {
	for _, object := range []*DmStruct{nil, {}, NewDmStruct("Unmaterialized", []interface{}{"Dir:file"})} {
		if _, err := object.GetAttributes(); err == nil {
			t.Fatal("attributes without a materialized descriptor must fail")
		}
		if _, err := object.GetSQLTypeName(); err == nil {
			t.Fatal("SQL type without a materialized descriptor must fail")
		}
	}
	for _, array := range []*DmArray{nil, {}, NewDmArray("Unmaterialized", []interface{}{"Dir:file"})} {
		if _, err := array.GetArray(); err == nil {
			t.Fatal("array without a materialized descriptor must fail")
		}
		if _, err := array.GetBaseTypeName(); err == nil {
			t.Fatal("base type without a materialized descriptor must fail")
		}
	}
}
