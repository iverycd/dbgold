package sqlsyntax

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestLexTSQLPreservesIdentifiersAndLiteralsAcrossNestedComments(t *testing.T) {
	tokens, err := LexTSQL(`/* outer /* inner */ [unterminated */ SELECT [Mi]]xed], 'a[b]''c' -- [ignored
FROM [dbo].[T]`)
	require.NoError(t, err)
	require.Equal(t, "Mi]xed", tokens[1].Value)
	require.Equal(t, byte('q'), tokens[1].Kind)
	require.Equal(t, "a[b]'c", tokens[3].Value)
	require.Equal(t, "T", tokens[len(tokens)-1].Value)
	for _, invalid := range []string{`SELECT [bad`, `SELECT 'bad`, `/* outer /* inner */`} {
		_, err := LexTSQL(invalid)
		require.Error(t, err)
	}
}
