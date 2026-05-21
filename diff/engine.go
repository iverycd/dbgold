package diff

import "dbgold/schema"

// Compare 计算从 src 到 dst 需要做的变更（dst 是目标状态）
func Compare(src, dst *schema.Schema) *Result {
	result := &Result{}

	srcMap := tableMap(src.Tables)
	dstMap := tableMap(dst.Tables)

	for name, dstTable := range dstMap {
		if _, exists := srcMap[name]; !exists {
			result.AddedTables = append(result.AddedTables, dstTable)
		}
	}
	for name, srcTable := range srcMap {
		if _, exists := dstMap[name]; !exists {
			result.DroppedTables = append(result.DroppedTables, srcTable)
		}
	}
	for name, srcTable := range srcMap {
		dstTable, exists := dstMap[name]
		if !exists {
			continue
		}
		td := compareTable(srcTable, dstTable)
		if td != nil {
			result.ModifiedTables = append(result.ModifiedTables, *td)
		}
	}
	return result
}

func tableMap(tables []schema.Table) map[string]schema.Table {
	m := make(map[string]schema.Table, len(tables))
	for _, t := range tables {
		m[t.Name] = t
	}
	return m
}

func compareTable(src, dst schema.Table) *TableDiff {
	td := &TableDiff{TableName: src.Name}
	changed := false

	srcCols := colMap(src.Columns)
	dstCols := colMap(dst.Columns)

	for name, dstCol := range dstCols {
		if _, exists := srcCols[name]; !exists {
			td.AddedColumns = append(td.AddedColumns, dstCol)
			changed = true
		}
	}
	for name, srcCol := range srcCols {
		if _, exists := dstCols[name]; !exists {
			td.DroppedColumns = append(td.DroppedColumns, srcCol)
			changed = true
		}
	}
	for name, srcCol := range srcCols {
		dstCol, exists := dstCols[name]
		if !exists {
			continue
		}
		cd := compareColumn(srcCol, dstCol)
		if cd != nil {
			td.ModifiedColumns = append(td.ModifiedColumns, *cd)
			changed = true
		}
	}

	srcIdx := indexMap(src.Indexes)
	dstIdx := indexMap(dst.Indexes)
	for name, dstI := range dstIdx {
		if _, exists := srcIdx[name]; !exists {
			td.AddedIndexes = append(td.AddedIndexes, dstI)
			changed = true
		}
	}
	for name, srcI := range srcIdx {
		if _, exists := dstIdx[name]; !exists {
			td.DroppedIndexes = append(td.DroppedIndexes, srcI)
			changed = true
		}
	}

	srcCon := constraintMap(src.Constraints)
	dstCon := constraintMap(dst.Constraints)
	for name, dstC := range dstCon {
		if _, exists := srcCon[name]; !exists {
			td.AddedConstraints = append(td.AddedConstraints, dstC)
			changed = true
		}
	}
	for name, srcC := range srcCon {
		if _, exists := dstCon[name]; !exists {
			td.DroppedConstraints = append(td.DroppedConstraints, srcC)
			changed = true
		}
	}

	srcFK := fkMap(src.ForeignKeys)
	dstFK := fkMap(dst.ForeignKeys)
	for name, dstF := range dstFK {
		if _, exists := srcFK[name]; !exists {
			td.AddedForeignKeys = append(td.AddedForeignKeys, dstF)
			changed = true
		}
	}
	for name, srcF := range srcFK {
		if _, exists := dstFK[name]; !exists {
			td.DroppedForeignKeys = append(td.DroppedForeignKeys, srcF)
			changed = true
		}
	}

	if !changed {
		return nil
	}
	return td
}

func compareColumn(src, dst schema.Column) *ColumnDiff {
	cd := &ColumnDiff{Column: dst, OldColumn: src}
	if src.Type != dst.Type {
		cd.TypeChanged = true
	}
	if src.Nullable != dst.Nullable {
		cd.NullableChanged = true
	}
	if defaultStr(src.Default) != defaultStr(dst.Default) {
		cd.DefaultChanged = true
	}
	if src.AutoIncrement != dst.AutoIncrement {
		cd.AutoIncrementChanged = true
	}
	if !cd.TypeChanged && !cd.NullableChanged && !cd.DefaultChanged && !cd.AutoIncrementChanged {
		return nil
	}
	return cd
}

func defaultStr(d *string) string {
	if d == nil {
		return ""
	}
	return *d
}

func colMap(cols []schema.Column) map[string]schema.Column {
	m := make(map[string]schema.Column, len(cols))
	for _, c := range cols {
		m[c.Name] = c
	}
	return m
}

func indexMap(indexes []schema.Index) map[string]schema.Index {
	m := make(map[string]schema.Index, len(indexes))
	for _, i := range indexes {
		m[i.Name] = i
	}
	return m
}

func constraintMap(constraints []schema.Constraint) map[string]schema.Constraint {
	m := make(map[string]schema.Constraint, len(constraints))
	for _, c := range constraints {
		m[c.Name] = c
	}
	return m
}

func fkMap(fks []schema.ForeignKey) map[string]schema.ForeignKey {
	m := make(map[string]schema.ForeignKey, len(fks))
	for _, f := range fks {
		m[f.Name] = f
	}
	return m
}
