package damengnative

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeFKStateFreezesNovalidateAndGeneratedIdentity(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	fk := Object{Kind: "constraint", Name: "CONS134218001", Table: "Child", ConstraintType: "R", Status: "ENABLED", Generated: true, ConstraintNoValidate: true}
	r := &catalogReader{ctx: context.Background(), db: db, schema: "MiXeD", snapshot: &Snapshot{Objects: []Object{fk}}}
	// Generated names and NOVALIDATE may both be absent in TABLE metadata;
	// the native CONSTRAINT definition supplies complete reference semantics.
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("CONSTRAINT", fk.Name, "MiXeD").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`ALTER TABLE "MiXeD"."Child" ADD CONSTRAINT "CONS134218001" FOREIGN KEY("Pid") REFERENCES "MiXeD"."ENABLE"("Id") ON DELETE CASCADE ENABLE;`))
	units, err := r.freezeTableForeignKeys("Child", 1)
	require.NoError(t, err)
	require.Len(t, units, 1)
	require.True(t, strings.HasSuffix(units[0], " ENABLE NOVALIDATE"))
	unit := r.stripGeneratedConstraintNames("Child", units[0])
	require.NotContains(t, unit, "CONS134218001")
	require.Contains(t, unit, `REFERENCES "MiXeD"."ENABLE"("Id") ON DELETE CASCADE`)
	require.Contains(t, unit, " ENABLE NOVALIDATE")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeFKFrozenStateReplacesOnlyTopLevelStateKeywords(t *testing.T) {
	ddl := `ALTER TABLE "App"."Child" ADD CONSTRAINT "NOVALIDATE" FOREIGN KEY("ENABLE") REFERENCES "App"."DISABLE"("VALIDATE") ENABLE NOVALIDATE;`
	got, err := constraintFrozenState(ddl, Object{Status: "DISABLED"})
	require.NoError(t, err)
	require.Contains(t, got, `CONSTRAINT "NOVALIDATE" FOREIGN KEY("ENABLE") REFERENCES "App"."DISABLE"("VALIDATE")`)
	require.True(t, strings.HasSuffix(got, " DISABLE"))
	require.NotContains(t, got, " ENABLE NOVALIDATE")
}

func TestNativeFKInventoryMismatchStopsBeforeDefinitionRead(t *testing.T) {
	r := &catalogReader{snapshot: &Snapshot{}}
	_, err := r.freezeTableForeignKeys("Child", 1)
	require.ErrorContains(t, err, "数量不同")
}
