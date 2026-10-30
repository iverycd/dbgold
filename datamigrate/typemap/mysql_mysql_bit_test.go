package typemap

import (
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestMySQLToMySQLBitRetainsNativeWidthWithoutCharacterLength(t *testing.T) {
	for _, tc := range []struct {
		col  source.ColumnInfo
		want string
	}{
		{source.ColumnInfo{DataType: "bit", FormattedType: "bit(12)"}, "bit(12)"},
		{source.ColumnInfo{DataType: "bit", FormattedType: "BIT(64)", Precision: 1}, "bit(64)"},
		{source.ColumnInfo{DataType: "bit", Precision: 48}, "bit(48)"},
		{source.ColumnInfo{DataType: "bit", Length: 8}, "bit(8)"},
		{source.ColumnInfo{DataType: "bit", FormattedType: "bit(12); DROP TABLE t", Precision: 12}, "bit(12)"},
		{source.ColumnInfo{DataType: "bit"}, "bit"},
	} {
		require.Equal(t, tc.want, MySQLToMySQL(tc.col, false, false))
	}
}
