package dm

// VisitBFileLocators visits only non-NULL BFILE attributes identified by the
// server's type descriptor. It does not read LOB contents, execute SQL, or infer
// a BFILE from a string's contents. The callback sees the original locator.
func (ds *DmStruct) VisitBFileLocators(visit func(string) error) error {
	if visit == nil {
		return ECGO_INVALID_PARAMETER_VALUE.throw()
	}
	if ds == nil || !ds.Valid {
		return nil
	}
	if ds.m_strctDesc == nil {
		return ECGO_INVALID_COLUMN_TYPE.throw()
	}
	return visitBFileValue(ds, ds.m_strctDesc.m_typeDesc, visit, make(map[interface{}]bool))
}

// VisitBFileLocators also supports named collections, including collections of
// objects, nested collections, and BFILE scalars.
func (da *DmArray) VisitBFileLocators(visit func(string) error) error {
	if visit == nil {
		return ECGO_INVALID_PARAMETER_VALUE.throw()
	}
	if da == nil || !da.Valid {
		return nil
	}
	if da.m_arrDesc == nil {
		return ECGO_INVALID_COLUMN_TYPE.throw()
	}
	return visitBFileValue(da, da.m_arrDesc.m_typeDesc, visit, make(map[interface{}]bool))
}

func visitBFileValue(value interface{}, desc *TypeDescriptor, visit func(string) error, active map[interface{}]bool) error {
	if value == nil {
		return nil
	}
	if desc == nil || desc.column == nil {
		return ECGO_INVALID_COLUMN_TYPE.throw()
	}
	if len(active) >= 1024 {
		return ECGO_INVALID_COLUMN_TYPE.throw()
	}
	if desc.getDType() == ARRAY || desc.getDType() == SARRAY || desc.getDType() == CLASS && desc.getObjId() == 4 {
		array, ok := value.(*DmArray)
		if !ok {
			return ECGO_INVALID_COLUMN_TYPE.throw()
		}
		if array == nil || !array.Valid {
			return nil
		}
		if desc.m_arrObj == nil || active[array] {
			return ECGO_INVALID_COLUMN_TYPE.throw()
		}
		active[array] = true
		defer delete(active, array)
		for i := range array.m_arrData {
			if err := visitBFileValue(array.m_arrData[i].m_dumyData, desc.m_arrObj, visit, active); err != nil {
				return err
			}
		}
		return nil
	}
	if desc.getDType() == CLASS || desc.getDType() == PLTYPE_RECORD {
		object, ok := value.(*DmStruct)
		if !ok {
			return ECGO_INVALID_COLUMN_TYPE.throw()
		}
		if object == nil || !object.Valid {
			return nil
		}
		if len(object.m_attribs) != len(desc.m_fieldsObj) || active[object] {
			return ECGO_STRUCT_MEM_NOT_MATCH.throw()
		}
		active[object] = true
		defer delete(active, object)
		for i := range object.m_attribs {
			if err := visitBFileValue(object.m_attribs[i].m_dumyData, &desc.m_fieldsObj[i], visit, active); err != nil {
				return err
			}
		}
		return nil
	}
	// This is the same server descriptor evidence used to set MASK_BFILE on
	// ordinary result columns. VARCHAR payloads without the flag stay opaque.
	if desc.column.colType != VARCHAR || desc.column.prec != BFILE_PREC || desc.column.scale != BFILE_SCALE {
		return nil
	}
	locator, ok := value.(string)
	if !ok {
		return ECGO_DATA_CONVERTION_ERROR.throw()
	}
	return visit(locator)
}
