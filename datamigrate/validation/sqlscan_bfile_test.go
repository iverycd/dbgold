package validation

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBFileLocatorClassificationIsSpecificToDamengDriver(t *testing.T) {
	for _, family := range []string{"dameng", "oracle", "postgres"} {
		column := Column{DatabaseType: "BFILE"}
		ClassifyColumn(family, &column)
		if family == "dameng" {
			require.Equal(t, Text, column.Kind)
		} else {
			require.Equal(t, Unsupported, column.Kind)
		}
	}
}
