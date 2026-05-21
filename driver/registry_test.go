package driver_test

import (
	"dbgold/driver"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDriver_SupportedTypes(t *testing.T) {
	types := []string{"mysql", "postgres", "oracle", "sqlserver"}
	for _, dbType := range types {
		d, err := driver.NewDriver(dbType)
		require.NoError(t, err, "dbType=%s", dbType)
		assert.NotNil(t, d)
	}
}

func TestNewDriver_UnsupportedType(t *testing.T) {
	_, err := driver.NewDriver("mongodb")
	assert.ErrorContains(t, err, "unsupported db type")
}
