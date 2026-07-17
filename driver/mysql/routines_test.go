package mysql

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFindCreateColumn(t *testing.T) {
	assert.Equal(t, 2, findCreateColumn([]string{"Trigger", "sql_mode", "Create Trigger", "character_set_client"}))
	assert.Equal(t, 2, findCreateColumn([]string{"Trigger", "sql_mode", "SQL Original Statement", "character_set_client"}))
	assert.Equal(t, 1, findCreateColumn([]string{"Procedure", "CREATE PROCEDURE", "sql_mode"}))
	assert.Equal(t, -1, findCreateColumn([]string{"Trigger", "sql_mode"}))
}

func TestEscapeIdentifier(t *testing.T) {
	assert.Equal(t, "odd``name", escapeIdentifier("odd`name"))
}

func TestWrapMySQLDDL(t *testing.T) {
	assert.Equal(t,
		"DELIMITER $$\nCREATE TRIGGER orders_bi BEFORE INSERT ON orders FOR EACH ROW SET NEW.id = 1$$\nDELIMITER ;",
		wrapMySQLDDL("CREATE TRIGGER orders_bi BEFORE INSERT ON orders FOR EACH ROW SET NEW.id = 1;\n"),
	)
}
