package schema_test

import (
	"dbgold/schema"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleDDL = `
CREATE TABLE users (
    id INT NOT NULL AUTO_INCREMENT,
    name VARCHAR(100) NOT NULL,
    email VARCHAR(255),
    PRIMARY KEY (id)
);

CREATE INDEX idx_email ON users (email);

CREATE VIEW active_users AS SELECT * FROM users WHERE email IS NOT NULL;
`

func TestParseDDL_Tables(t *testing.T) {
	s, err := schema.ParseDDL(sampleDDL)
	require.NoError(t, err)
	require.Len(t, s.Tables, 1)
	assert.Equal(t, "users", s.Tables[0].Name)
}

func TestParseDDL_Views(t *testing.T) {
	s, err := schema.ParseDDL(sampleDDL)
	require.NoError(t, err)
	require.Len(t, s.Views, 1)
	assert.Equal(t, "active_users", s.Views[0].Name)
}

func TestParseDDL_Empty(t *testing.T) {
	s, err := schema.ParseDDL("")
	require.NoError(t, err)
	assert.Empty(t, s.Tables)
}
