package cdc

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDamengIdentityNextConservativeSeedAndTypeBounds(t *testing.T) {
	for _, tc := range []struct {
		name, typ, seed, current, step, maximum, source, errorText string
	}{
		{"fresh empty", "INTEGER", "99", "99", "1", "", "100", "无法确认自增首值已消费"},
		{"explicit below seed", "INTEGER", "99", "99", "1", "1", "100", "保守下界 99"},
		{"seed present", "INTEGER", "99", "99", "1", "99", "100", ""},
		{"seed meets source", "BIGINT", "100", "100", "1", "", "100", ""},
		{"advanced then emptied", "BIGINT", "99", "100", "1", "", "101", ""},
		{"int exhausted", "INTEGER", "1", "2147483647", "1", "2147483647", "100", "超过 INTEGER 自增安全上限"},
		{"bigint exhausted", "BIGINT", "1", "9223372036854775806", "1", "", "100", "超过 BIGINT 自增安全上限"},
		{"bigint last available", "BIGINT", "1", "9223372036854775805", "1", "", "100", ""},
		{"int last available", "INT", "1", "2147483646", "1", "2147483646", "2147483647", ""},
		{"ambiguous at limit", "INT", "2147483647", "2147483647", "1", "", "1", "不能确认续号安全"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDamengIdentityNext("T", targetColumnMetadata{Name: "ID", DataType: tc.typ}, tc.seed, tc.current, tc.step,
				sql.NullString{String: tc.maximum, Valid: tc.maximum != ""}, sql.NullString{String: tc.source, Valid: tc.source != ""})
			if tc.errorText == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.errorText)
			}
		})
	}
}
