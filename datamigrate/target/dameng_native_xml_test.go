package target

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNestedNativeXMLDefinitionRequiresActualColumnAttribute(t *testing.T) {
	for _, xml := range []bool{true, false} {
		t.Run(map[bool]string{true: "xml", false: "ordinary_text"}[xml], func(t *testing.T) {
			conn, m := historyEmptyTestConn(t)
			ddl := `CREATE TABLE "Dst"."T"("X" TEXT,"Plain" TEXT DEFAULT 'TEXT')`
			rows := sqlmock.NewRows([]string{"column", "type", "scale"})
			if xml {
				rows.AddRow("X", "TEXT", 8)
			} else {
				rows.AddRow("X", "TEXT", 0).AddRow("Plain", "TEXT", 0)
			}
			m.ExpectQuery(`SELECT COLUMN_NAME,DATA_TYPE,NVL\(DATA_SCALE,0\) FROM ALL_TAB_COLUMNS`).WithArgs("Dst", "T").WillReturnRows(rows)
			got, err := nativeRestoreXMLDefinition(context.Background(), conn, "Dst", "T", ddl)
			require.NoError(t, err)
			if xml {
				require.Equal(t, `CREATE TABLE "Dst"."T"("X" XMLTYPE,"Plain" TEXT DEFAULT 'TEXT')`, got)
			} else {
				require.Equal(t, ddl, got)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestNestedNativeXMLDefinitionRejectsMismatchedDeclaration(t *testing.T) {
	conn, m := historyEmptyTestConn(t)
	m.ExpectQuery(`SELECT COLUMN_NAME,DATA_TYPE,NVL\(DATA_SCALE,0\)`).WithArgs("Dst", "T").WillReturnRows(sqlmock.NewRows([]string{"column", "type", "scale"}).AddRow("X", "TEXT", 8))
	_, err := nativeRestoreXMLDefinition(context.Background(), conn, "Dst", "T", `CREATE TABLE "Dst"."T"("X" VARCHAR(40),"Other" CLOB)`)
	require.ErrorContains(t, err, "XMLTYPE")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNestedNativeXMLDefinitionLeavesLiteralAndQuotedWordsAlone(t *testing.T) {
	conn, m := historyEmptyTestConn(t)
	ddl := `CREATE TABLE "Dst"."T"("TEXT" VARCHAR(40) DEFAULT 'CLOB TEXT')`
	got, err := nativeRestoreXMLDefinition(context.Background(), conn, "Dst", "T", ddl)
	require.NoError(t, err)
	require.Equal(t, ddl, got)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPhysicalPartitionXMLAttributesFollowOnlyNativeParentChain(t *testing.T) {
	conn, m := historyEmptyTestConn(t)
	ddl := `CREATE TABLE "Dst"."ArbitraryPhysicalName"("X" TEXT)`
	m.ExpectQuery(`SELECT COLUMN_NAME,DATA_TYPE,NVL\(DATA_SCALE,0\)`).WithArgs("Dst", "ArbitraryPhysicalName").WillReturnRows(sqlmock.NewRows([]string{"column", "type", "scale"}))
	m.ExpectQuery(`SELECT b.NAME FROM SYS.SYSOBJECTS p JOIN SYS.SYSHPARTTABLEINFO`).WithArgs("Dst", "ArbitraryPhysicalName").WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Logical"))
	m.ExpectQuery(`SELECT COLUMN_NAME,DATA_TYPE,NVL\(DATA_SCALE,0\)`).WithArgs("Dst", "Logical").WillReturnRows(sqlmock.NewRows([]string{"column", "type", "scale"}).AddRow("X", "TEXT", 8))
	got, err := nativeRestoreXMLDefinition(context.Background(), conn, "Dst", "ArbitraryPhysicalName", ddl)
	require.NoError(t, err)
	require.Equal(t, `CREATE TABLE "Dst"."ArbitraryPhysicalName"("X" XMLTYPE)`, got)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestPhysicalPartitionXMLInheritanceSupportsMultipleLevelsAndRejectsCycles(t *testing.T) {
	for _, cycle := range []bool{false, true} {
		t.Run(map[bool]string{false: "multiple_levels", true: "cycle_rejected"}[cycle], func(t *testing.T) {
			conn, m := historyEmptyTestConn(t)
			metadata := `SELECT COLUMN_NAME,DATA_TYPE,NVL\(DATA_SCALE,0\)`
			parent := `SELECT b.NAME .*b.SCHID=p.SCHID.*s.NAME=\? AND p.NAME=\?`
			for _, name := range []string{"Leaf", "Intermediate"} {
				m.ExpectQuery(metadata).WithArgs("Dst", name).WillReturnRows(sqlmock.NewRows([]string{"column", "type", "scale"}))
				next := "Intermediate"
				if name == "Intermediate" {
					next = "Root"
					if cycle {
						next = "Leaf"
					}
				}
				m.ExpectQuery(parent).WithArgs("Dst", name).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow(next))
			}
			if !cycle {
				m.ExpectQuery(metadata).WithArgs("Dst", "Root").WillReturnRows(sqlmock.NewRows([]string{"column", "type", "scale"}).AddRow("X", "TEXT", 8))
			}
			ddl := `CREATE TABLE "Dst"."Leaf"("X" TEXT)`
			got, err := nativeRestoreXMLDefinition(context.Background(), conn, "Dst", "Leaf", ddl)
			if cycle {
				require.ErrorContains(t, err, "循环")
			} else {
				require.NoError(t, err)
				require.Equal(t, `CREATE TABLE "Dst"."Leaf"("X" XMLTYPE)`, got)
			}
			require.NoError(t, m.ExpectationsWereMet(), "继承解析只读，循环也不能执行任何写入")
		})
	}
}
