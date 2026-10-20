package oracledameng

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestViewSemanticsFeaturesInspectTokensOnly(t *testing.T) {
	features, err := ViewSemanticsFeatures(`SELECT a || b AS combined, CONCAT(a,b) c, CAST(CAST(a AS TIMESTAMP(9)) AS DATE) d,
 LPAD(a,3,'x') p, LPAD(b,3,'x') p2, a/2 n, TRIM(a) t FROM T
 WHERE a='RPAD(x) / || CAST(x AS DATE)' /* SOUNDEX(x) */ -- LAST_DAY(x)
`)
	require.NoError(t, err)
	require.Equal(t, []string{"CONCAT", "LPAD", "TRIM", "cast_date", "integer_division", "operator_concat"}, features)
	features, err = ViewSemanticsFeatures(`SELECT CAST(a AS TIMESTAMP(0)) d, "LPAD" AS "CONCAT" FROM T`)
	require.NoError(t, err)
	require.Empty(t, features)
}

func TestViewSemanticsFeaturesFindNestedDateCastsAndFunctions(t *testing.T) {
	features, err := ViewSemanticsFeatures(`SELECT CAST(TO_CHAR(CAST(a AS DATE)) AS VARCHAR2(30)) x,
 SUBSTRB(a,1,2) b, LTRIM(a) l, RTRIM(a) r, REPLACE(a,'x') z, TRANSLATE(a,'ab','x') y,
 SOUNDEX(a) s, LAST_DAY(a) d, RPAD(a,2,'x') p FROM T`)
	require.NoError(t, err)
	require.Equal(t, []string{"LAST_DAY", "LTRIM", "REPLACE", "RPAD", "RTRIM", "SOUNDEX", "SUBSTRB", "TRANSLATE", "cast_date"}, features)
	_, err = ViewSemanticsFeatures(`SELECT 'unterminated FROM T`)
	require.Error(t, err)
}
