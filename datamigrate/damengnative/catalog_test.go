package damengnative

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func profileMock(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT SVR_VERSION,DB_VERSION,BUILD_VERSION,INSTANCE_NAME,HOST_NAME FROM V\$INSTANCE`).WillReturnRows(sqlmock.NewRows([]string{"version", "dbversion", "build", "instance", "host"}).AddRow("DM Database Server x64 V8", "0x7000d", "build1", "DM8", "host1"))
	mock.ExpectQuery(`SELECT CAST\(DB_MAGIC AS VARCHAR\(80\)\) FROM V\$RLOG`).WillReturnRows(sqlmock.NewRows([]string{"magic"}).AddRow("123"))
	mock.ExpectQuery(`SELECT PARA_NAME,PARA_VALUE FROM V\$DM_INI`).WillReturnRows(sqlmock.NewRows([]string{"name", "value"}).AddRow("COMPATIBLE_MODE", "2").AddRow("IFUN_DATETIME_MODE", "0"))
	mock.ExpectQuery(`SELECT CAST\(SF_GET_UNICODE_FLAG`).WillReturnRows(sqlmock.NewRows([]string{"charset", "case", "page"}).AddRow("1", "1", "8192"))
}

func TestResolveSchemaPreservesCatalogCase(t *testing.T) {
	for _, tc := range []struct {
		input string
		names []string
		want  string
		bad   bool
	}{{"App", []string{"APP", "App"}, "App", false}, {"app", []string{"APP"}, "APP", false}, {"app", []string{"APP", "App"}, "", true}, {"Missing", []string{"APP"}, "", true}} {
		t.Run(tc.input+tc.want, func(t *testing.T) {
			db, m, e := sqlmock.New()
			require.NoError(t, e)
			defer db.Close()
			rows := sqlmock.NewRows([]string{"name"})
			for _, n := range tc.names {
				rows.AddRow(n)
			}
			m.ExpectQuery(`SELECT NAME FROM SYS.SYSOBJECTS WHERE TYPE\$='SCH' ORDER BY NAME`).WillReturnRows(rows)
			got, e := ResolveSchema(context.Background(), db, tc.input)
			if tc.bad {
				require.Error(t, e)
			} else {
				require.NoError(t, e)
				require.Equal(t, tc.want, got)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestReadCatalogExtractsNativeDefinitionsAndBlocksUnknown(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	profileMock(m)
	m.ExpectQuery(`SELECT o.ID,o.NAME,o.TYPE\$`).WithArgs("MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "main", "sub", "pid", "valid", "i1", "i2", "i3"}).AddRow(1, "MixedTable", "SCHOBJ", "UTAB", -1, "Y", 0, 0, 0).AddRow(2, "INDEX1", "TABOBJ", "INDEX", 1, "Y", 0, 0, 0).AddRow(3, "MixedPkg", "SCHOBJ", "PKG", -1, "Y", 0, 0, 0).AddRow(4, "CustomOp", "SCHOBJ", "OPERATOR", -1, "Y", 0, 0, 0))
	m.ExpectQuery(`SELECT OBJECT_ID,OBJECT_NAME,OBJECT_TYPE,STATUS FROM ALL_OBJECTS`).WithArgs("MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind", "status"}).AddRow(1, "MixedTable", "TABLE", "VALID").AddRow(2, "INDEX1", "INDEX", "VALID").AddRow(3, "MixedPkg", "PACKAGE", "VALID").AddRow(3, "MixedPkg", "PACKAGE BODY", "VALID").AddRow(4, "CustomOp", "OPERATOR", "VALID"))
	m.ExpectQuery(`SELECT CONSTRAINT_NAME,TABLE_NAME`).WithArgs("MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"name", "table", "type", "index", "status"}))
	m.ExpectQuery(`SELECT i.NAME,t.NAME,x.FLAG,x.XTYPE FROM SYS.SYSINDEXES`).WithArgs("MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"index", "table", "flags", "xtype"}).AddRow("INDEX1", "MixedTable", 1, 0))
	m.ExpectQuery(`SELECT MASTER,LOG_TABLE FROM ALL_MVIEW_LOGS`).WithArgs("MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"master", "log"}))
	m.ExpectQuery(`SELECT d.ID,d.TYPE\$`).WithArgs("MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "refid", "refkind", "schema", "name"}).AddRow(3, "PACKAGE_BODY", 1, "TABLE", "MixedOwner", "MixedTable"))
	tableDDL := `CREATE TABLE "MixedOwner"."MixedTable" ("MixedCol" VARCHAR(12) DEFAULT 'KeepMiXeD', "Amount" DECIMAL(38,4)) STORAGE(ON "MAIN", CLUSTERBTR);`
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("TABLE", "MixedTable", "MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(tableDDL))
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("PKG_SPEC", "MixedPkg", "MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow("CREATE PACKAGE \"MixedOwner\".\"MixedPkg\" AS PROCEDURE P; END;\n/"))
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("PKG_BODY", "MixedPkg", "MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow("CREATE PACKAGE BODY \"MixedOwner\".\"MixedPkg\" AS PROCEDURE P AS BEGIN NULL; END; END;\n/"))
	m.ExpectQuery(`SELECT c.COLUMN_NAME,c.DATA_TYPE`).WithArgs("MixedOwner", "MixedTable").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "precision", "scale", "flags", "extra", "owner"}).AddRow("MixedCol", "VARCHAR", 0, 0, 0, 0, nil).AddRow("Amount", "DECIMAL", 38, 4, 0, 0, nil))
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "MixedOwner"."MixedTable"`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	m.ExpectQuery(`SELECT TVNAME,CAST\(NULL`).WithArgs("MixedOwner", "MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"table", "column", "text"}).AddRow("MixedTable", nil, "KeepMiXeD注释"))
	m.ExpectQuery(`SELECT g.OBJID,COUNT`).WithArgs("MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"id", "count"}))
	s, e := ReadCatalog(context.Background(), db, "MixedOwner", true)
	require.NoError(t, e)
	require.True(t, HasErrors(s.Issues))
	require.Contains(t, s.Issues[0].Reason, "operator")
	require.Len(t, s.Tables, 1)
	require.Equal(t, "DECIMAL(38,4)", s.Tables[0].Columns[1].SQLType)
	objects := map[string]Object{}
	for _, o := range s.Objects {
		objects[o.Kind+":"+o.Name] = o
	}
	require.Equal(t, tableDDL, objects["table:MixedTable"].Units[0].SQL)
	require.True(t, objects["index:INDEX1"].Managed)
	require.NotContains(t, objects["package_body:MixedPkg"].Units[0].SQL, "\n/")
	require.Contains(t, objects["package_body:MixedPkg"].Dependencies, objects["table:MixedTable"].ID)
	require.Contains(t, objects["comment:MixedTable"].Units[0].SQL, "KeepMiXeD注释")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeForeignKeyDeferralPreservesOtherSourceText(t *testing.T) {
	ddl := `CREATE TABLE "App"."Child" ("id" INT, "name" VARCHAR(20) DEFAULT 'REFERENCES AbC', CONSTRAINT "MiXeD_FK" FOREIGN KEY ("id") REFERENCES "App"."Parent"("id") ON UPDATE CASCADE ON DELETE SET NULL, CHECK ("name" <> 'Foreign Key')) STORAGE(ON "MAIN", CLUSTERBTR);`
	base, after, e := deferForeignKeys(ddl, "App", "Child")
	require.NoError(t, e)
	require.Len(t, after, 1)
	require.NotContains(t, base, `CONSTRAINT "MiXeD_FK"`)
	require.Contains(t, base, `DEFAULT 'REFERENCES AbC'`)
	require.Contains(t, base, `CHECK ("name" <> 'Foreign Key')`)
	require.Contains(t, base, `STORAGE(ON "MAIN", CLUSTERBTR)`)
	require.Equal(t, `ALTER TABLE "App"."Child" ADD CONSTRAINT "MiXeD_FK" FOREIGN KEY ("id") REFERENCES "App"."Parent"("id") ON UPDATE CASCADE ON DELETE SET NULL`, after[0])
	_, _, e = deferForeignKeys(`CREATE TABLE "X" ("id" INT DEFAULT 'bad)`, "A", "X")
	require.Error(t, e)
}

func TestNativeSequenceUsesReadonlyCacheBoundary(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{}}
	m.ExpectQuery(`SELECT CAST\(LAST_NUMBER`).WithArgs("App", "Seq").WillReturnRows(sqlmock.NewRows([]string{"last", "min", "max"}).AddRow("-900", "-9999", "0"))
	ddl, e := r.sequenceDDL(&Object{Name: "Seq", Kind: "sequence"}, `CREATE SEQUENCE "App"."Seq" START WITH -1000 INCREMENT BY -2 MINVALUE -9999 MAXVALUE 0 NOCYCLE CACHE 20`)
	require.NoError(t, e)
	require.Contains(t, ddl, "START WITH -900 INCREMENT BY -2")
	require.False(t, HasErrors(r.snapshot.Issues))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeDDLFailureIsBlocking(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	o := Object{Kind: "view", Name: "Broken", Schema: "App"}
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{o}}}
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("VIEW", "Broken", "App").WillReturnError(errors.New("permission denied"))
	r.extractDefinitions()
	require.True(t, HasErrors(r.snapshot.Issues))
	require.Empty(t, r.snapshot.Objects[0].Units)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedViewCopiesStoredSnapshotIntoPrebuiltStorage(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	mv := Object{ID: ObjectID("App", "materialized_view", "Mv"), Kind: "materialized_view", Name: "Mv", Schema: "App"}
	store := Object{ID: ObjectID("App", "table", "MTAB$_Mv"), Kind: "table", Name: "MTAB$_Mv", Schema: "App"}
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{mv, store}}, raw: map[int64]catalogObject{1: {id: 1, name: "Mv", sub: "VIEW", info2: 2}, 2: {id: 2, name: "MTAB$_Mv", sub: "UTAB"}}, byRaw: map[int64][]int{1: {0}, 2: {1}}, index: map[string]int{"materialized_view\x00Mv": 0, "table\x00MTAB$_Mv": 1}}
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("TABLE", "MTAB$_Mv", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TABLE "App"."MTAB$_Mv" ("id" INT) STORAGE(ON "MAIN", CLUSTERBTR)`))
	err := r.materializedSnapshot(0, `CREATE MATERIALIZED VIEW "App"."Mv" BUILD IMMEDIATE STORAGE(ON "MAIN") REFRESH COMPLETE ON DEMAND AS SELECT "id" FROM "App"."T"`)
	require.NoError(t, err)
	require.True(t, r.snapshot.Objects[1].Managed)
	require.Len(t, r.snapshot.Objects, 3)
	helper := r.snapshot.Objects[2]
	require.True(t, strings.HasPrefix(helper.Name, "DBGOLD_MV_"))
	require.Equal(t, "MTAB$_Mv", helper.Table)
	require.Contains(t, r.snapshot.Objects[0].Units[0].SQL, "FOR "+Qualified("App", helper.Name)+" ON PREBUILT TABLE NEVER REFRESH AS")
	require.Equal(t, Unit{SQL: `ALTER MATERIALIZED VIEW "App"."Mv" REFRESH COMPLETE ON DEMAND`, Phase: "mv_restore"}, r.snapshot.Objects[0].Units[1])
	require.NotContains(t, r.snapshot.Objects[0].Units[0].SQL, "BUILD IMMEDIATE")
	require.NotContains(t, helper.Units[0].SQL, `"MTAB$_Mv"`)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestSimpleGrantSplitRetainsQuotedSemicolons(t *testing.T) {
	got, e := splitSimpleDDL(`GRANT SELECT ON "App"."semi;colon" TO "Reader"; GRANT UPDATE("Mixed") ON "App"."semi;colon" TO "Writer" WITH GRANT OPTION;`)
	require.NoError(t, e)
	require.Len(t, got, 2)
	_, e = splitSimpleDDL(`GRANT SELECT ON T TO U; DROP TABLE T;`)
	require.Error(t, e)
}

func TestCatalogInventoryIncludesOverwriteDropWithoutReadingDDL(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	profileMock(m)
	m.ExpectQuery(`SELECT o.ID,o.NAME,o.TYPE\$`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "main", "sub", "pid", "valid", "i1", "i2", "i3"}).AddRow(1, "V", "SCHOBJ", "VIEW", -1, "Y", 0, 0, 0))
	m.ExpectQuery(`SELECT OBJECT_ID,OBJECT_NAME,OBJECT_TYPE,STATUS`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind", "status"}).AddRow(1, "V", "VIEW", "VALID"))
	m.ExpectQuery(`SELECT MVIEW_NAME FROM SYS.USER_MVIEWS`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"name"}))
	m.ExpectQuery(`SELECT CONSTRAINT_NAME,TABLE_NAME`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"name", "table", "type", "index", "status"}))
	m.ExpectQuery(`SELECT i.NAME,t.NAME,x.FLAG,x.XTYPE FROM SYS.SYSINDEXES`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"name", "table", "flags", "xtype"}))
	m.ExpectQuery(`SELECT MASTER,LOG_TABLE FROM ALL_MVIEW_LOGS`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"master", "log"}))
	m.ExpectQuery(`SELECT s.NAME,o.NAME,o.INFO1`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "flags"}))
	m.ExpectQuery(`SELECT d.ID,d.TYPE\$`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "refid", "refkind", "schema", "name"}))
	m.ExpectQuery(`SELECT TVNAME,CAST\(NULL`).WithArgs("App", "App").WillReturnRows(sqlmock.NewRows([]string{"table", "column", "text"}))
	m.ExpectQuery(`SELECT g.OBJID,COUNT`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"id", "count"}))
	s, e := ReadCatalog(context.Background(), db, "App", false)
	require.NoError(t, e)
	require.Len(t, s.Objects, 1)
	require.Equal(t, `DROP VIEW "App"."V"`, s.Objects[0].DropSQL)
	require.Empty(t, s.Objects[0].Units)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeDisabledConstraintAndGeneratedNames(t *testing.T) {
	o := Object{ID: "table", Kind: "table", Name: "T", Schema: "App"}
	r := &catalogReader{schema: "App", snapshot: &Snapshot{Objects: []Object{o, {Kind: "constraint", Name: "UserCheck", Table: "T", Status: "DISABLED"}, {Kind: "constraint", Name: "CONS134217729", Table: "T", Generated: true}}}}
	ddl := `CREATE TABLE "App"."T" ("id" INT,CONSTRAINT "CONS134217729" PRIMARY KEY ("id"),CONSTRAINT "UserCheck" CHECK ("id">0) DISABLE)`
	stripped := r.stripGeneratedConstraintNames("T", ddl)
	require.NotContains(t, stripped, `"CONS134217729"`)
	require.Contains(t, stripped, `PRIMARY KEY ("id")`)
	require.Contains(t, stripped, `"UserCheck" CHECK ("id">0) DISABLE`)
	r.restoreConstraintStates(&r.snapshot.Objects[0], ddl)
	require.Equal(t, `ALTER TABLE "App"."T" DISABLE CONSTRAINT "UserCheck"`, r.snapshot.Objects[0].Units[0].SQL)
}

func TestNativeTriggerStateAndDBLinkCredentials(t *testing.T) {
	for _, status := range []string{"N", "0", "DISABLED"} {
		t.Run(status, func(t *testing.T) {
			db, m, e := sqlmock.New()
			require.NoError(t, e)
			defer db.Close()
			r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{{Kind: "trigger", Name: "Tr", Schema: "App"}}}}
			m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("TRIGGER", "Tr", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TRIGGER "App"."Tr" BEFORE INSERT ON "App"."T" FOR EACH ROW BEGIN NULL; END;`))
			m.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs("App", "Tr").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(status))
			r.extractDefinitions()
			require.False(t, HasErrors(r.snapshot.Issues))
			require.Equal(t, "DISABLED", r.snapshot.Objects[0].Status)
			require.Equal(t, "restore", r.snapshot.Objects[0].Units[1].Phase)
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{{Kind: "db_link", Name: "Link", Schema: "App"}}}}
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("DB_LINK", "Link", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE LINK "App"."Link" CONNECT 'u' IDENTIFIED BY 'SecretPassword' USING 'remote'`))
	r.extractDefinitions()
	require.False(t, HasErrors(r.snapshot.Issues))
	require.Len(t, r.snapshot.Objects[0].Units, 1)
	require.NotContains(t, r.snapshot.Objects[0].Units[0].SQL, "SecretPassword")
	require.NotEmpty(t, r.snapshot.Objects[0].Units[0].SourceSQLHash)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeDBLinkMetadataErrorNeverEntersIssues(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{{Kind: "db_link", Name: "Link", Schema: "App"}}}}
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("DB_LINK", "Link", "App").WillReturnError(errors.New("GET_DDL failed while parsing IDENTIFIED BY 'SecretPassword' USING 'PWD=EndpointPassword'"))
	r.extractDefinitions()
	require.True(t, HasErrors(r.snapshot.Issues))
	require.Len(t, r.snapshot.Issues, 1)
	require.NotContains(t, r.snapshot.Issues[0].Reason, "SecretPassword")
	require.NotContains(t, r.snapshot.Issues[0].Reason, "EndpointPassword")
	require.Empty(t, r.snapshot.Objects[0].Units)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeLOBLimitFailsBeforeCopy(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{}}
	o := Object{Kind: "table", Name: "T", Schema: "App"}
	table := Table{Name: "T", Schema: "App", Columns: []Column{{Name: "Payload", DataType: "BLOB", Writable: true}}}
	m.ExpectQuery(`SELECT MAX\(DBMS_LOB.GETLENGTH\("Payload"\)\) FROM "App"."T"`).WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(2147483648)))
	r.validateLOBLengths(&o, &table, "T")
	require.True(t, HasErrors(r.snapshot.Issues))
	require.Contains(t, r.snapshot.Issues[0].Reason, "2147483647")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestDeclaredNativeUDTUsesDDLInsteadOfCLASSPlaceholder(t *testing.T) {
	table := Object{Kind: "table", Name: "T", Units: []Unit{{Phase: "before_data", SQL: `CREATE TABLE "App"."T" ("Value" "App"."MixedType", "Id" "App"."IdDomain")`}}}
	objects := []Object{{Kind: "type", Name: "MixedType", Schema: "App"}, {Kind: "domain", Name: "IdDomain", Schema: "App"}}
	schema, name, ok := declaredNativeType(table, "Value", "App", objects)
	require.True(t, ok)
	require.Equal(t, "App", schema)
	require.Equal(t, "MixedType", name)
	_, _, ok = declaredNativeType(table, "Id", "App", objects)
	require.False(t, ok, "DOMAIN uses its scalar codec while native DDL retains the domain reference")
}

func TestDisabledConstraintsAreInactiveBeforeRowsOrFKValidation(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	objects := []Object{{Kind: "table", Name: "T", Schema: "App"}, {Kind: "constraint", Name: "Check", Schema: "App", Table: "T", Status: "DISABLED", ConstraintType: "C", Managed: true}, {Kind: "constraint", Name: "FK", Schema: "App", Table: "T", Status: "DISABLED", ConstraintType: "R"}}
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: objects}, constraints: map[string]string{"Check": "C", "FK": "R"}}
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("TABLE", "T", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TABLE "App"."T" ("Id" INT,CONSTRAINT "Check" CHECK ("Id">0),CONSTRAINT "FK" FOREIGN KEY ("Id") REFERENCES "App"."P"("Id"))`))
	r.extractDefinitions()
	require.False(t, HasErrors(r.snapshot.Issues))
	units := r.snapshot.Objects[0].Units
	require.Len(t, units, 3)
	require.Equal(t, "before_data", units[0].Phase)
	require.Contains(t, units[0].SQL, `CHECK ("Id">0) DISABLE`)
	require.Equal(t, "after_data", units[1].Phase)
	require.True(t, strings.HasSuffix(units[1].SQL, " DISABLE"))
	require.Equal(t, "before_data", units[2].Phase)
	require.Contains(t, units[2].SQL, `DISABLE CONSTRAINT "Check"`)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestExistingIndexReferenceBlocksButInlineStorageIsAllowed(t *testing.T) {
	r := &catalogReader{schema: "App", snapshot: &Snapshot{}}
	o := Object{Kind: "table", Name: "T", Schema: "App"}
	r.checkExistingIndexReferences(&o, `CREATE TABLE "App"."T"("Id" INT,CONSTRAINT "Pk" PRIMARY KEY("Id") USING INDEX "App"."OldIndex")`)
	require.True(t, HasErrors(r.snapshot.Issues))
	r.snapshot.Issues = nil
	r.checkExistingIndexReferences(&o, `CREATE TABLE "App"."T"("Id" INT,CONSTRAINT "Pk" PRIMARY KEY("Id") USING INDEX (CREATE UNIQUE INDEX "App"."Idx" ON "App"."T"("Id")))`)
	require.Empty(t, r.snapshot.Issues)
	r.checkExistingIndexReferences(&o, `CREATE TABLE "App"."T"("Id" INT,CONSTRAINT "Pk" PRIMARY KEY("Id") USING INDEX STORAGE(ON "MAIN"))`)
	require.Empty(t, r.snapshot.Issues)
}

func TestEventTriggerSourceFreezesDefinitionAndFinalState(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{{Kind: "trigger", Name: "Event", Schema: "App", EventTrigger: true}}}}
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("TRIGGER", "Event", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TRIGGER "App"."Event" AFTER DDL ON SCHEMA BEGIN NULL; END;`))
	m.ExpectQuery(`SELECT STATUS FROM ALL_TRIGGERS`).WithArgs("App", "Event").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("ENABLED"))
	r.extractDefinitions()
	require.Empty(t, r.snapshot.Issues)
	require.Equal(t, []Unit{{SQL: `CREATE TRIGGER "App"."Event" AFTER DDL ON SCHEMA BEGIN NULL; END;`, Phase: "event_create"}, {SQL: EventTriggerStateSQL("App", "Event", true), Phase: "event_activate"}}, r.snapshot.Objects[0].Units)
	require.NoError(t, m.ExpectationsWereMet())
	require.False(t, nativeTriggerIsEvent(`CREATE TRIGGER "Tr" AFTER UPDATE OF "DATABASE" ON "App"."T" BEGIN NULL; END;`))
}

func TestTimerTriggerSourceBlocksUnsafeImmediateScheduling(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{{Kind: "trigger", Name: "Timer", Schema: "App", EventTrigger: true}}}}
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("TRIGGER", "Timer", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TRIGGER "App"."Timer" AFTER TIMER ON DATABASE FOR EACH 1 MINUTE BEGIN NULL; END;`))
	r.extractDefinitions()
	require.True(t, HasErrors(r.snapshot.Issues))
	require.Contains(t, r.snapshot.Issues[0].Reason, "TIMER")
	require.Empty(t, r.snapshot.Objects[0].Units)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestTargetDatabaseEventHooksAreCheckedAcrossSchemas(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{}}
	m.ExpectQuery(`SELECT s.NAME,o.NAME,o.INFO1`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "flags", "status"}).AddRow("Other", "Global", 4, "Y").AddRow("Other", "Scoped", 4, "Y").AddRow("App", "Local", 4, "Y"))
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL\('TRIGGER',\?,\?\)`).WithArgs("Global", "Other").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TRIGGER "Other"."Global" AFTER LOGIN ON DATABASE BEGIN NULL; END;`))
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL\('TRIGGER',\?,\?\)`).WithArgs("Scoped", "Other").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TRIGGER "Other"."Scoped" AFTER DDL ON SCHEMA BEGIN NULL; END;`))
	require.NoError(t, r.checkActiveEventTriggers())
	require.Len(t, r.snapshot.Issues, 2)
	require.NoError(t, m.ExpectationsWereMet())
}
