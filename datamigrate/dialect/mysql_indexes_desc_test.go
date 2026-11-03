package dialect

import (
	"reflect"
	"slices"
	"testing"

	"dbgold/datamigrate/source"
)

func TestMySQLIndexDescendingKeys(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index source.IndexInfo
		want  string
	}{
		{
			name: "mixed directions and escaped identifiers",
			index: source.IndexInfo{
				TableName: "Ta`ble", IndexName: "Ix`Name",
				Columns: []string{"Up`Key", "DownKey", "TailKey"}, Descending: []bool{false, true, false},
			},
			want: "ALTER TABLE `Db``Name`.`Ta``ble` ADD INDEX `Ix``Name` (`Up``Key`, `DownKey` DESC, `TailKey`)",
		},
		{
			name: "prefix before direction and short direction list",
			index: source.IndexInfo{
				TableName: "T", IndexName: "Ix", Columns: []string{"URL", "ID"},
				PrefixLengths: []int{768}, Descending: []bool{true},
			},
			want: "ALTER TABLE `Db``Name`.`T` ADD INDEX `Ix` (`URL`(768) DESC, `ID`)",
		},
		{
			name: "unique keys retain full fields",
			index: source.IndexInfo{
				TableName: "T", IndexName: "UQ", IsUnique: true, Columns: []string{"URL", "ID"},
				PrefixLengths: []int{768, 1}, Descending: []bool{true, false},
			},
			want: "ALTER TABLE `Db``Name`.`T` ADD UNIQUE INDEX `UQ` (`URL` DESC, `ID`)",
		},
		{
			name: "primary keys retain full fields",
			index: source.IndexInfo{
				TableName: "T", IsPrimary: true, Columns: []string{"URL", "ID"},
				PrefixLengths: []int{768, 1}, Descending: []bool{false, true},
			},
			want: "ALTER TABLE `Db``Name`.`T` ADD PRIMARY KEY (`URL`, `ID` DESC)",
		},
		{
			name: "omitted directions preserve ascending sql",
			index: source.IndexInfo{
				TableName: "T", IndexName: "Ix", Columns: []string{"ID", "TS"},
			},
			want: "ALTER TABLE `Db``Name`.`T` ADD INDEX `Ix` (`ID`, `TS`)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.index
			before.Columns = slices.Clone(tc.index.Columns)
			before.PrefixLengths = slices.Clone(tc.index.PrefixLengths)
			before.Descending = slices.Clone(tc.index.Descending)
			stmts := NewMySQL().IndexStatements("Db`Name", tc.index)
			if len(stmts) != 1 || stmts[0].SQL != tc.want {
				t.Fatalf("IndexStatements() = %#v, want %q", stmts, tc.want)
			}
			if !reflect.DeepEqual(before, tc.index) {
				t.Fatalf("IndexStatements mutated source metadata: before=%#v after=%#v", before, tc.index)
			}
		})
	}
}
