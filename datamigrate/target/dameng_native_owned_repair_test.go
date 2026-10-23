package target

import (
	"context"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeOwnedIndexUnknownReconcilesChildDefinition(t *testing.T) {
	for _, variant := range []string{"absent", "same", "other_table", "different"} {
		t.Run(variant, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			w := &DaMengNativeWriter{DaMengWriter: &DaMengWriter{db: db, schema: "App"}}
			statement := `CREATE OR REPLACE UNIQUE INDEX "App"."Idx" ON "App"."T"("Id")`
			table := damengnative.Object{Kind: "table", Name: "T", Units: []damengnative.Unit{{SQL: statement}}}
			snapshot := &damengnative.Snapshot{}
			if variant != "absent" {
				parent := "T"
				if variant == "other_table" {
					parent = "Other"
				}
				snapshot.Objects = append(snapshot.Objects, damengnative.Object{Kind: "index", Name: "Idx", Table: parent})
			}
			if variant == "same" || variant == "different" {
				ddl := statement
				if variant == "different" {
					ddl = `CREATE INDEX "App"."Idx" ON "App"."T"("Other")`
				}
				mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("Idx", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(ddl))
			}
			handled, done, retry, err := w.reconcileNativeOwnedUnit(context.Background(), table, 0, snapshot)
			require.NoError(t, err)
			require.True(t, handled)
			require.Equal(t, variant == "same", done)
			require.Equal(t, variant == "absent", retry)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestNativeOwnedPrimaryKeyProofRequiresOriginalIndexBinding(t *testing.T) {
	for _, binding := range []string{"Idx", "Other"} {
		t.Run(binding, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			w := &DaMengNativeWriter{DaMengWriter: &DaMengWriter{db: db, schema: "App"}}
			table := damengnative.Object{Kind: "table", Name: "T", Units: []damengnative.Unit{{SQL: `ALTER TABLE "App"."T" ADD CONSTRAINT "Pk" NOT CLUSTER PRIMARY KEY("Id") USING INDEX "App"."Idx" ENABLE`}}}
			snapshot := &damengnative.Snapshot{Objects: []damengnative.Object{{Kind: "constraint", Name: "Pk", Table: "T", ConstraintType: "P", Status: "ENABLED", ConstraintIndex: binding}}}
			if binding == "Idx" {
				// DM8 metadata omits USING INDEX; the catalog binding proves it.
				mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("Pk", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`ALTER TABLE "App"."T" ADD CONSTRAINT "Pk" NOT CLUSTER PRIMARY KEY("Id") ENABLE;`))
			}
			handled, done, retry, err := w.reconcileNativeOwnedUnit(context.Background(), table, 0, snapshot)
			require.NoError(t, err)
			require.True(t, handled)
			require.Equal(t, binding == "Idx", done)
			require.False(t, retry)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestNativeOwnedNovalidateProofDoesNotBlindlyReplayGeneratedConstraint(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[existing], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			w := &DaMengNativeWriter{DaMengWriter: &DaMengWriter{db: db, schema: "App"}}
			statement := `ALTER TABLE "App"."T" ADD CHECK("Value">0) ENABLE NOVALIDATE`
			table := damengnative.Object{Kind: "table", Name: "T", Units: []damengnative.Unit{{SQL: statement}, {SQL: statement}}}
			snapshot := &damengnative.Snapshot{}
			if existing {
				snapshot.Objects = append(snapshot.Objects, damengnative.Object{Kind: "constraint", Name: "CONS134218000", Table: "T", ConstraintType: "C", Generated: true, Status: "ENABLED", ConstraintNoValidate: true})
				for i := 0; i < 2; i++ {
					mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("CONS134218000", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`ALTER TABLE "App"."T" ADD CONSTRAINT CHECK("Value">0) ENABLE NOVALIDATE;`))
				}
			}
			for unit := 0; unit < 2; unit++ {
				handled, done, retry, err := w.reconcileNativeOwnedUnit(context.Background(), table, unit, snapshot)
				require.NoError(t, err)
				require.True(t, handled)
				require.Equal(t, existing && unit == 0, done)
				require.Equal(t, !existing || unit == 1, retry)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestNativeOwnedConstraintMissingNameStillRespectsOtherTableAndState(t *testing.T) {
	w := &DaMengNativeWriter{DaMengWriter: &DaMengWriter{schema: "App"}}
	table := damengnative.Object{Kind: "table", Name: "T", Units: []damengnative.Unit{{SQL: `ALTER TABLE "App"."T" ADD CONSTRAINT "C" CHECK("Id">0) ENABLE NOVALIDATE`}}}
	for _, object := range []damengnative.Object{
		{Kind: "constraint", Name: "C", Table: "Other", ConstraintType: "C", Status: "ENABLED", ConstraintNoValidate: true},
		{Kind: "constraint", Name: "C", Table: "T", ConstraintType: "C", Status: "DISABLED"},
	} {
		handled, done, retry, err := w.reconcileNativeOwnedUnit(context.Background(), table, 0, &damengnative.Snapshot{Objects: []damengnative.Object{object}})
		require.NoError(t, err)
		require.True(t, handled)
		require.False(t, done)
		require.False(t, retry)
	}
}

func TestNativeOwnedUnnamedProofDoesNotCountNamedCheck(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	w := &DaMengNativeWriter{DaMengWriter: &DaMengWriter{db: db, schema: "App"}}
	statement := `ALTER TABLE "App"."T" ADD CHECK("Value">0) ENABLE NOVALIDATE`
	table := damengnative.Object{Kind: "table", Name: "T", Units: []damengnative.Unit{{SQL: statement}, {SQL: statement}}}
	snapshot := &damengnative.Snapshot{Objects: []damengnative.Object{
		{Kind: "constraint", Name: "Named", Table: "T", ConstraintType: "C", Status: "ENABLED", ConstraintNoValidate: true},
		{Kind: "constraint", Name: "CONS134218000", Table: "T", ConstraintType: "C", Generated: true, Status: "ENABLED", ConstraintNoValidate: true},
	}}
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("CONS134218000", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`ALTER TABLE "App"."T" ADD CONSTRAINT CHECK("Value">0) ENABLE NOVALIDATE;`))
	handled, done, retry, err := w.reconcileNativeOwnedUnit(context.Background(), table, 1, snapshot)
	require.NoError(t, err)
	require.True(t, handled)
	require.False(t, done)
	require.True(t, retry, "只有一个无名约束完成，命名同body约束不能充抵第二个")
	require.NoError(t, mock.ExpectationsWereMet())
}
