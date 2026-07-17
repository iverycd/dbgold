package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWrapPLSQLSource(t *testing.T) {
	assert.Equal(t,
		"CREATE OR REPLACE TRIGGER orders_bi BEFORE INSERT ON orders BEGIN NULL; END;\n/",
		wrapPLSQLSource("TRIGGER orders_bi BEFORE INSERT ON orders BEGIN NULL; END;\n/\n"),
	)
}
