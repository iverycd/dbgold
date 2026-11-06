package datamigrate

import (
	"context"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// A conservative preflight error is preferable to accepting SQL that returns
// different values. These cases stay within the small expression grammar.
func TestSQLServerPGViewReviewAvoidsSilentSemanticsChanges(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		check      func(*testing.T, string)
	}{
		{
			name: "isnull_untyped_null_uses_replacement_type",
			body: `SELECT ISNULL(NULL,1)+'2' AS Value`,
			check: func(t *testing.T, sql string) {
				// SQL Server returns integer 3, rather than the text "12".
				require.NotContains(t, sql, "||")
				require.Contains(t, sql, " + ")
			},
		},
		{
			name: "union_uses_common_output_type",
			body: `SELECT d.Value+'3' AS Value FROM (SELECT '1' AS Value UNION SELECT 2 AS Value) d`,
			check: func(t *testing.T, sql string) {
				// Both UNION rows have integer type, so their results are 4 and 5.
				require.NotContains(t, sql, "||")
				require.Contains(t, sql, " + ")
			},
		},
		{
			name: "len_excludes_trailing_spaces",
			body: `SELECT LEN(N'AbC  ') AS Value`,
			check: func(t *testing.T, sql string) {
				// SQL Server LEN returns 3; direct PostgreSQL length returns 5.
				require.Contains(t, sql, "rtrim(")
			},
		},
		{
			name: "integer_cast_truncates_positive_and_negative_numeric",
			body: `SELECT CAST(1.9 AS int) AS Positive, CAST(-1.9 AS int) AS Negative`,
			check: func(t *testing.T, sql string) {
				// SQL Server truncates toward zero; PostgreSQL numeric casts round.
				require.Contains(t, sql, "trunc(")
				require.NotContains(t, sql, "CAST(1.9 AS int)")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, _, w := pgViewTestMigrator()
			v, err := m.prepareView(context.Background(), source.ViewInfo{ViewName: "Review", Definition: tt.body})
			if err != nil {
				var issue *sqlServerPGViewError
				require.ErrorAs(t, err, &issue)
				require.Empty(t, w.views)
				return
			}
			tt.check(t, v.Definition)
		})
	}
}
