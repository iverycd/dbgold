package damengnative

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeReusedIndexIsFrozenBetweenBaseAndPrimaryKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	objects := []Object{
		{Kind: "constraint", Name: "Pk", Table: "T", ConstraintType: "P", ConstraintIndex: "OriginalIndex", Managed: true, Status: "ENABLED"},
		{Kind: "index", Name: "OriginalIndex", Table: "T", Managed: true},
	}
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: objects}}
	ddl := `CREATE TABLE "App"."T"("Id" INT NOT NULL,"Text" VARCHAR(80) DEFAULT 'CONSTRAINT Pk USING INDEX',CONSTRAINT "Pk" NOT CLUSTER PRIMARY KEY("Id")) STORAGE(ON "MAIN",CLUSTERBTR);`
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("CONSTRAINT", "Pk", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`ALTER TABLE "App"."T" ADD CONSTRAINT "Pk" NOT CLUSTER PRIMARY KEY("Id") ENABLE;`))
	indexDDL := `CREATE OR REPLACE UNIQUE INDEX "App"."OriginalIndex" ON "App"."T"("Id" ASC) STORAGE(ON "MAIN",CLUSTERBTR);`
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("INDEX", "OriginalIndex", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(indexDDL))
	base, foundation, delayed, err := r.freezeTableKeyAndValidationPlan("T", ddl)
	require.NoError(t, err)
	require.Empty(t, delayed)
	require.Contains(t, base, `DEFAULT 'CONSTRAINT Pk USING INDEX'`)
	require.NotContains(t, base, `CONSTRAINT "Pk"`)
	require.Len(t, foundation, 2)
	require.Equal(t, Unit{SQL: indexDDL, Phase: "before_data"}, foundation[0])
	require.Equal(t, `ALTER TABLE "App"."T" ADD CONSTRAINT "Pk" NOT CLUSTER PRIMARY KEY("Id") USING INDEX "App"."OriginalIndex" ENABLE `+r.frozenConstraintMarker(objects[0]), foundation[1].SQL)
	require.Equal(t, "before_data", foundation[1].Phase)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeNonFKNovalidateIsDeferredWithOriginalState(t *testing.T) {
	for _, generated := range []bool{false, true} {
		t.Run(map[bool]string{false: "named", true: "generated"}[generated], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			object := Object{Kind: "constraint", Name: "C", Table: "T", ConstraintType: "C", Status: "ENABLED", ConstraintNoValidate: true, Generated: generated}
			r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{object}}}
			clause := `CONSTRAINT "C" CHECK("Value">0)`
			definition := `ALTER TABLE "App"."T" ADD CONSTRAINT "C" CHECK("Value">0) ENABLE;`
			if generated {
				clause = `CHECK("Value">0)`
				definition = `ALTER TABLE "App"."T" ADD CONSTRAINT  CHECK("Value">0) ENABLE NOVALIDATE;`
			}
			mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("CONSTRAINT", "C", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(definition))
			base, foundation, delayed, err := r.freezeTableKeyAndValidationPlan("T", `CREATE TABLE "App"."T"("Value" INT,`+clause+`)`)
			require.NoError(t, err)
			require.Equal(t, `CREATE TABLE "App"."T"("Value" INT)`, base)
			require.Empty(t, foundation)
			require.Len(t, delayed, 1)
			require.Equal(t, "after_data", delayed[0].Phase)
			require.Contains(t, delayed[0].SQL, ` CHECK("Value">0) ENABLE NOVALIDATE`)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestNativeReusedIndexCatalogBindingDoesNotRelyOnDDLName(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{{Kind: "table", Name: "T", ID: "table"}, {Kind: "index", Name: "UserIdx"}, {Kind: "index", Name: "INDEX33554434"}}}, index: map[string]int{"index\x00UserIdx": 1, "index\x00INDEX33554434": 2}, constraints: map[string]string{}, raw: map[int64]catalogObject{33554433: {id: 33554433, sub: "INDEX", name: "UserIdx"}, 33554434: {id: 33554434, sub: "INDEX", name: "INDEX33554434"}}}
	mock.ExpectQuery(`SELECT CONSTRAINT_NAME,TABLE_NAME`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"name", "table", "kind", "index", "status", "validated"}).AddRow("PK", "T", "P", "UserIdx", "ENABLED", "VALIDATED"))
	mock.ExpectQuery(`SELECT i.NAME,t.NAME,x.FLAG,x.XTYPE`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"name", "table", "flag", "xtype"}).AddRow("UserIdx", "T", 5, 17).AddRow("INDEX33554434", "T", 1, 0))
	require.NoError(t, r.readConstraints())
	require.False(t, r.snapshot.Objects[1].Generated, "FLAG=5 is also a reused user index")
	require.True(t, r.snapshot.Objects[2].Generated, "exact native ID/name proves the disposable name")
	require.Equal(t, "UserIdx", r.snapshot.Objects[3].ConstraintIndex)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeConstraintIdentityUsesExactCatalogCase(t *testing.T) {
	got := repairGeneratedConstraintIdentity(`ALTER TABLE "App"."T" ADD CONSTRAINT MixedCase CHECK("Value">0) ENABLE NOVALIDATE;`, Object{Name: "MixedCase"})
	require.Contains(t, got, `ADD CONSTRAINT "MixedCase" CHECK`)
}

func TestNativePartitionConstraintUsesActualPartitionObjectID(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{{ID: "base", Kind: "table", Name: "T"}, {ID: "actual-partition", Kind: "table_partition", Name: "Leaf"}}}, index: map[string]int{}, constraints: map[string]string{}, raw: map[int64]catalogObject{}}
	mock.ExpectQuery(`SELECT CONSTRAINT_NAME,TABLE_NAME`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"name", "table", "kind", "index", "status", "validated"}).AddRow("ChildPk", "Leaf", "P", nil, "ENABLED", "VALIDATED"))
	mock.ExpectQuery(`SELECT i.NAME,t.NAME,x.FLAG,x.XTYPE`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"name", "table", "flag", "xtype"}))
	require.NoError(t, r.readConstraints())
	require.Equal(t, "actual-partition", r.snapshot.Objects[2].ParentID)
	require.Contains(t, r.snapshot.Objects[2].Dependencies, "actual-partition")
}

func TestNativeUserClusterIndexRetainsNameAndDescendingDefinition(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{{Kind: "index", Name: "UserCluster", Table: "T", Managed: true}}}}
	definition := `CREATE CLUSTER INDEX "App"."UserCluster" ON "App"."T"("Id" DESC)`
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("INDEX", "UserCluster", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(definition))
	base, foundation, delayed, err := r.freezeTableKeyAndValidationPlan("T", `CREATE TABLE "App"."T"("Id" INT,CLUSTER KEY("Id")) STORAGE(ON "MAIN",CLUSTERBTR)`)
	require.NoError(t, err)
	require.Equal(t, `CREATE TABLE "App"."T"("Id" INT) STORAGE(ON "MAIN",CLUSTERBTR)`, base)
	require.Equal(t, []Unit{{SQL: definition, Phase: "before_data"}}, foundation)
	require.Empty(t, delayed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeConstraintSplicingIgnoresQuotedPunctuation(t *testing.T) {
	ddl := `CREATE TABLE "App"."T"("(" INT,")" VARCHAR(20) DEFAULT ')',"Id" INT,CONSTRAINT "P" NOT CLUSTER PRIMARY KEY("Id"),CONSTRAINT "F" FOREIGN KEY("(") REFERENCES "App"."Parent"("Id"))`
	base, err := removeNativeTableConstraint(ddl, Object{Name: "P", Table: "T"}, `ALTER TABLE "App"."T" ADD CONSTRAINT "P" NOT CLUSTER PRIMARY KEY("Id")`)
	require.NoError(t, err)
	require.Contains(t, base, `"(" INT,")" VARCHAR(20) DEFAULT ')'`)
	require.NotContains(t, base, `CONSTRAINT "P"`)
	base, fks, err := deferForeignKeys(base, "App", "T")
	require.NoError(t, err)
	require.Len(t, fks, 1)
	require.Contains(t, fks[0], `FOREIGN KEY("(") REFERENCES`)
	require.Contains(t, base, `DEFAULT ')'`)
	require.NotContains(t, base, `CONSTRAINT "F"`)
	cluster, err := removeNativeTableClusterKey(`CREATE TABLE "App"."T"("(" INT,"Id" INT DEFAULT ')',CLUSTER KEY("Id"))`, "App", "T")
	require.NoError(t, err)
	require.Equal(t, `CREATE TABLE "App"."T"("(" INT,"Id" INT DEFAULT ')')`, cluster)
}

func TestNativeGeneratedDisabledConstraintHasNoNameBlocker(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	objects := []Object{{Kind: "table", Name: "T", Schema: "App"}, {Kind: "constraint", Name: "CONS134218000", Table: "T", ConstraintType: "C", Status: "DISABLED", Generated: true, Managed: true}}
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: objects}}
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("TABLE", "T", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TABLE "App"."T"("Id" INT,CHECK("Id">0) DISABLE)`))
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("CONSTRAINT", "CONS134218000", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`ALTER TABLE "App"."T" ADD CONSTRAINT CHECK("Id">0) DISABLE;`))
	r.extractDefinitions()
	require.Empty(t, r.snapshot.Issues)
	require.Len(t, r.snapshot.Objects[0].Units, 2)
	require.Equal(t, `CREATE TABLE "App"."T"("Id" INT)`, r.snapshot.Objects[0].Units[0].SQL)
	require.Contains(t, r.snapshot.Objects[0].Units[1].SQL, `ADD  CHECK("Id">0) DISABLE /* DBGOLD_CONSTRAINT`)
	require.NotContains(t, r.snapshot.Objects[0].Units[1].SQL, "CONS134218000")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeIdenticalGeneratedConstraintsKeepDistinctFrozenUnits(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	objects := []Object{
		{ID: "first", Kind: "constraint", Name: "CONS134218001", Table: "T", ConstraintType: "C", Status: "ENABLED", ConstraintNoValidate: true, Generated: true},
		{ID: "second", Kind: "constraint", Name: "CONS134218002", Table: "T", ConstraintType: "C", Status: "ENABLED", ConstraintNoValidate: true, Generated: true},
	}
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: objects}}
	for _, object := range objects {
		mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("CONSTRAINT", object.Name, "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`ALTER TABLE "App"."T" ADD CONSTRAINT CHECK("Id">0) ENABLE NOVALIDATE;`))
	}
	base, _, delayed, err := r.freezeTableKeyAndValidationPlan("T", `CREATE TABLE "App"."T"("Id" INT,CHECK("Id">0),CHECK("Id">0))`)
	require.NoError(t, err)
	require.Equal(t, `CREATE TABLE "App"."T"("Id" INT)`, base)
	require.Len(t, delayed, 2)
	require.NotEqual(t, delayed[0].SQL, delayed[1].SQL)
	require.Contains(t, delayed[0].SQL, "DBGOLD_CONSTRAINT first")
	require.Contains(t, delayed[1].SQL, "DBGOLD_CONSTRAINT second")
	require.NoError(t, mock.ExpectationsWereMet())
}
