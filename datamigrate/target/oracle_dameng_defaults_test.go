package target

import (
	"context"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestOracleDamengFloatDefaultsUseActualTargetRange(t *testing.T) {
	for _, tc := range []struct {
		family, value string
		valid         bool
	}{
		{"real", "1e100", false}, {"double", "1e100", true}, {"decimal", "1e125", true}, {"decimal", "1e200", false}, {"real", "((1.2345678901234567))", true}, {"real", "NULL", true}, {"double", "CAST(1e100 AS FLOAT(24))", false},
	} {
		w := &DaMengWriter{}
		w.cacheOracleDMFloatColumn("T", "F", tc.family)
		info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "F", DataType: "FLOAT", Precision: 24, Default: &tc.value}}}
		err := w.ValidateOracleDamengDefaults(context.Background(), []*source.TableDDLInfo{info}, dialect.TypeOpt{})
		if tc.valid {
			require.NoError(t, err, tc)
		} else {
			require.Error(t, err, tc)
		}
	}
}
