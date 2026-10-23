package target

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"gitee.com/chunanyong/dm"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func mockNativeDM(t *testing.T) (*DaMengNativeWriter, sqlmock.Sqlmock) {
	base, mock := mockDM(t)
	base.schema = "MixedOwner"
	base.srcType = "dameng"
	base.dia = &damengNativeDialect{dialect.NewDaMeng()}
	return &DaMengNativeWriter{base}, mock
}

func TestDamengNativeNamesAndNoRegeneration(t *testing.T) {
	w, _ := mockNativeDM(t)
	require.Equal(t, `"MixedOwner"."Some""Table"`, w.Dialect().QualifyTable("MixedOwner", `Some"Table`))
	_, err := w.Dialect().CreateTableStatements("MixedOwner", &source.TableDDLInfo{TableName: "T"}, "dameng", dialect.TypeOpt{}, func(s string) string { return s })
	require.ErrorContains(t, err, "原始表 DDL")
	require.Nil(t, w.Dialect().IndexStatements("MixedOwner", source.IndexInfo{}))
	require.Equal(t, "varchar(20)", w.Dialect().MapType(source.ColumnInfo{DataType: "varchar(20)"}, "dameng", dialect.TypeOpt{}))
}

func TestDamengNativeSQLScope(t *testing.T) {
	valid := []string{
		`CREATE TYPE "MixedOwner"."Name.With""Quote;,(" AS OBJECT ("Value" INT)`,
		`CREATE TABLE "MixedOwner"."T" ("c" VARCHAR(20) DEFAULT 'a;b')`,
		`CREATE UNIQUE INDEX "MixedOwner"."ix" ON "MixedOwner"."T" ("c")`,
		`CREATE OR REPLACE CLUSTER INDEX "MixedOwner"."ix" ON "MixedOwner"."T" ("c" DESC)`,
		`CREATE OR REPLACE CLUSTER UNIQUE INDEX "MixedOwner"."ix" ON "MixedOwner"."T" ("c" DESC)`,
		`CREATE NOT CLUSTER INDEX "MixedOwner"."ix" ON "MixedOwner"."T" ("c")`,
		`CREATE NOT PARTIAL INDEX "MixedOwner"."ix" ON "MixedOwner"."T" ("c")`,
		`ALTER TABLE "MixedOwner"."T" DISABLE CONSTRAINT "FK"`,
		`DROP VIEW "MixedOwner"."V"`,
		`TRUNCATE TABLE "MixedOwner"."T"`,
		`REFRESH MATERIALIZED VIEW "MixedOwner"."M" COMPLETE`,
		`COMMENT ON COLUMN "MixedOwner"."T"."c" IS '中文'`,
		`COMMENT ON COLUMN "T"."c" IS '中文'`,
		`GRANT SELECT ON "MixedOwner"."T" TO "Reader"`,
		"CREATE OR REPLACE PROCEDURE \"MixedOwner\".\"p\" AS BEGIN NULL; NULL; END;",
		"CREATE OR REPLACE PROCEDURE \"MixedOwner\".\"p\" AS BEGIN x:=q'[it's; ''quoted'']'; NULL; END;",
	}
	for _, query := range valid {
		require.NoError(t, validateDamengNativeSQL("MixedOwner", query), query)
	}
	invalid := []string{
		`CREATE TABLE "Other"."T" ("c" INT)`,
		`CREATE INDEX "MixedOwner"."ix" ON "Other"."T" ("c")`,
		`CREATE CLUSTER INDEX "MixedOwner"."ix" ON "Other"."T" ("c")`,
		`CREATE CLUSTER UNIQUE INDEX "MixedOwner"."ix" ON "Other"."T" ("c")`,
		`CREATE NOT CLUSTER INDEX "Other"."ix" ON "MixedOwner"."T" ("c")`,
		`CREATE CLUSTER INDEX "MixedOwner"."ix" ON "MixedOwner"."T" ("c"); DROP TABLE "MixedOwner"."T"`,
		`COMMENT ON COLUMN "Other"."T"."c" IS 'x'`,
		`CREATE TABLE "MixedOwner"."T" ("c" INT); DROP TABLE "Other"."T"`,
		`GRANT CREATE ANY TABLE TO "Reader"`,
		`DELETE FROM "MixedOwner"."T"`,
		`CREATE USER "Other" IDENTIFIED BY secret`,
		`REFRESH MATERIALIZED VIEW "Other"."M" COMPLETE`,
		`REFRESH MATERIALIZED VIEW "MixedOwner"."M" FAST`,
		`REFRESH MATERIALIZED VIEW "MixedOwner"."M" COMPLETE;DROP TABLE "MixedOwner"."T"`,
		`BEGIN EXECUTE IMMEDIATE 'DROP TABLE Other.T'; END;`,
	}
	for _, query := range invalid {
		require.Error(t, validateDamengNativeSQL("MixedOwner", query), query)
	}
}

func TestDamengNativeValuesPreserveExactData(t *testing.T) {
	decimal := damengnative.Column{Name: "Amount", DataType: "DECIMAL", SQLType: "DECIMAL(38,19)", Writable: true}
	raw := "-1234567890123456789.1234567890123456789"
	value, _, err := nativeCopyValue(decimal, raw, "MixedOwner")
	require.NoError(t, err)
	require.Equal(t, raw, value)
	placeholder, err := nativePlaceholder(decimal)
	require.NoError(t, err)
	require.Equal(t, "CAST(CAST(? AS VARCHAR(256)) AS DECIMAL(38,19))", placeholder)
	_, _, err = nativeCopyValue(decimal, float64(1.234), "MixedOwner")
	require.Error(t, err)
	for _, invalid := range []string{"NaN", "1;DROP TABLE X", "1e999999999999999999999", " 1"} {
		_, err = nativeDecimalText(invalid)
		require.Error(t, err)
	}
	text := damengnative.Column{Name: "Name", DataType: "VARCHAR", Writable: true}
	value, _, err = nativeCopyValue(text, "AbC\x00中文😀", "MixedOwner")
	require.NoError(t, err)
	require.Equal(t, "AbC\x00中文😀", value)
	for _, kind := range []string{"CLOB", "BLOB"} {
		c := damengnative.Column{Name: "Lob", DataType: kind, Writable: true}
		v, _, err := nativeCopyValue(c, nil, "MixedOwner")
		require.NoError(t, err)
		require.Nil(t, v)
		var empty any = ""
		if kind == "BLOB" {
			empty = []byte{}
		}
		v, _, err = nativeCopyValue(c, empty, "MixedOwner")
		require.NoError(t, err)
		if kind == "CLOB" {
			require.IsType(t, &dm.DmClob{}, v)
		} else {
			require.IsType(t, &dm.DmBlob{}, v)
		}
	}
	stamp := time.Date(2024, 2, 29, 12, 34, 56, 123456789, time.FixedZone("", -5*3600))
	value, _, err = nativeCopyValue(damengnative.Column{Name: "At", DataType: "TIMESTAMP(9) WITH TIME ZONE", Writable: true}, stamp, "MixedOwner")
	require.NoError(t, err)
	require.Equal(t, stamp, value)
	require.Error(t, ValidateDamengNativeColumn(damengnative.Column{Name: "Unknown", DataType: "MYSTERY_TYPE", Writable: true}))
}

func TestDamengNativeBatchIdentityAndGeneratedColumns(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			w, mock := mockNativeDM(t)
			table := damengnative.Table{Name: "MixedTable", Schema: "MixedOwner", Columns: []damengnative.Column{{Name: "Id", DataType: "BIGINT", Writable: true, Identity: true}, {Name: "Amount", DataType: "DECIMAL", SQLType: "DECIMAL(38,19)", Writable: true}, {Name: "Calculated", DataType: "BIGINT"}}}
			mock.ExpectBegin()
			mock.ExpectExec(regexp.QuoteMeta(`SET IDENTITY_INSERT "MixedOwner"."MixedTable" ON`)).WillReturnResult(sqlmock.NewResult(0, 0))
			stmt := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO "MixedOwner"."MixedTable" ("Id", "Amount") VALUES (?, CAST(CAST(? AS VARCHAR(256)) AS DECIMAL(38,19)))`))
			exec := stmt.ExpectExec().WithArgs(int64(42), "-1234567890123456789.1234567890123456789")
			if fail {
				exec.WillReturnError(errors.New("write failed"))
				mock.ExpectRollback()
				mock.ExpectExec("SET IDENTITY_INSERT.*OFF").WillReturnResult(sqlmock.NewResult(0, 0))
			} else {
				exec.WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("SET IDENTITY_INSERT.*OFF").WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectCommit()
			}
			err := w.CopyDamengNativeBatch(context.Background(), table, [][]any{{int64(42), "-1234567890123456789.1234567890123456789"}})
			if fail {
				require.ErrorContains(t, err, "write failed")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDamengNativeBatchRejectsBeforeMutation(t *testing.T) {
	w, _ := mockNativeDM(t)
	table := damengnative.Table{Name: "T", Schema: "MixedOwner", Columns: []damengnative.Column{{Name: "Text", DataType: "VARCHAR", Writable: true}}}
	tooLarge := strings.Repeat("x", NativeBatchBytes/2+1)
	require.ErrorContains(t, w.CopyDamengNativeBatch(context.Background(), table, [][]any{{tooLarge}, {tooLarge}}), "缩小批次")
	table.Columns[0].DataType = "UNKNOWN"
	require.ErrorContains(t, w.CopyDamengNativeBatch(context.Background(), table, [][]any{{"value"}}), "无损写入器")
}

func TestDamengNativeBatchDistinguishesNullAndEmptyLOB(t *testing.T) {
	w, mock := mockNativeDM(t)
	table := damengnative.Table{Name: "T", Schema: "MixedOwner", Columns: []damengnative.Column{{Name: "Body", DataType: "CLOB", Writable: true}, {Name: "Payload", DataType: "BLOB", Writable: true}}}
	mock.ExpectBegin()
	empty := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO "MixedOwner"."T" ("Body", "Payload") VALUES (EMPTY_CLOB(), EMPTY_BLOB())`))
	empty.ExpectExec().WithArgs().WillReturnResult(sqlmock.NewResult(0, 1))
	bound := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO "MixedOwner"."T" ("Body", "Payload") VALUES (?, ?)`))
	bound.ExpectExec().WithArgs(nil, nil).WillReturnResult(sqlmock.NewResult(0, 1))
	empty.ExpectExec().WithArgs().WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err := w.CopyDamengNativeBatch(context.Background(), table, [][]any{{"", []byte{}}, {nil, nil}, {"", []byte{}}})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDamengNativeWholeProgramExecutionObserved(t *testing.T) {
	w, mock := mockNativeDM(t)
	query := `CREATE OR REPLACE PROCEDURE "MixedOwner"."p" AS BEGIN NULL; NULL; END;`
	mock.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "MixedOwner"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(query)).WillReturnResult(sqlmock.NewResult(0, 0))
	var events []repair.Event
	ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { events = append(events, e); return nil })
	require.NoError(t, w.ExecuteDamengNative(ctx, query))
	require.Len(t, events, 2)
	require.Equal(t, "running", events[0].Status)
	require.Equal(t, "succeeded", events[1].Status)
	require.Equal(t, query, events[1].SQL)
}

func TestDamengNativeRepairRejectsEditedOrReplayedUnits(t *testing.T) {
	w, _ := mockNativeDM(t)
	definition := damengnative.Object{ID: "object", Kind: "PROCEDURE", Name: "p", Schema: "MixedOwner", Units: []damengnative.Unit{{SQL: `CREATE PROCEDURE "MixedOwner"."p" AS BEGIN NULL; END;`, Phase: "after_data"}}}
	obj := repair.Object{Native: &definition, NativeFingerprint: damengnative.Fingerprint(&definition), Schema: "MixedOwner", TargetName: "p", Units: []repair.Unit{{SQL: definition.Units[0].SQL, OriginalSQL: definition.Units[0].SQL, Status: "succeeded", Selected: true}}}
	_, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
	require.ErrorContains(t, err, "未选择")
	units := append([]repair.Unit{}, obj.Units...)
	units[0].Status = "failed"
	units[0].SQL += " ALTER TABLE x DROP COLUMN y"
	_, err = ExecuteRepair(context.Background(), w, obj, units, nil)
	require.ErrorContains(t, err, "计划已改变")
}

func TestDamengNativeRecoveryPreservesDisabledState(t *testing.T) {
	w, mock := mockNativeDM(t)
	mock.ExpectQuery("SELECT OWNER,TRIGGER_NAME,STATUS FROM ALL_TRIGGERS").WithArgs("MixedOwner", "T").WillReturnRows(sqlmock.NewRows([]string{"owner", "trigger", "status"}).AddRow("MixedOwner", "Active", "Y").AddRow("MixedOwner", "Inactive", "N"))
	mock.ExpectQuery("SELECT c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME,c.STATUS").WithArgs("MixedOwner", "T", "MixedOwner", "T").WillReturnRows(sqlmock.NewRows([]string{"owner", "table", "constraint", "status", "validated"}).AddRow("MixedOwner", "Child", "FK", "ENABLED", "VALIDATED").AddRow("MixedOwner", "Child", "OldFK", "DISABLED", "NOT VALIDATED"))
	disable, restore, err := w.NativeRecoveryState(context.Background(), "T")
	require.NoError(t, err)
	require.Len(t, disable, 2)
	require.Len(t, restore, 2)
	require.Equal(t, `ALTER TRIGGER "MixedOwner"."Active" DISABLE`, disable[0].SQL)
	require.Equal(t, `ALTER TABLE "MixedOwner"."Child" ENABLE CONSTRAINT "FK"`, restore[0].SQL)
	require.Equal(t, `ALTER TRIGGER "MixedOwner"."Active" ENABLE`, restore[1].SQL)
}

func TestDamengNativeRecoveryRejectsExternalInboundForeignKey(t *testing.T) {
	w, mock := mockNativeDM(t)
	mock.ExpectQuery("SELECT OWNER,TRIGGER_NAME,STATUS FROM ALL_TRIGGERS").WillReturnRows(sqlmock.NewRows([]string{"owner", "trigger", "status"}))
	mock.ExpectQuery("SELECT c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME,c.STATUS").WillReturnRows(sqlmock.NewRows([]string{"owner", "table", "constraint", "status", "validated"}).AddRow("ExternalOwner", "Child", "FK", "ENABLED", "VALIDATED"))
	disable, restore, err := w.NativeRecoveryState(context.Background(), "T")
	require.ErrorContains(t, err, "入向外键")
	require.Empty(t, disable)
	require.Empty(t, restore)
}

func TestDamengNativeRecoveryPreservesNovalidate(t *testing.T) {
	w, mock := mockNativeDM(t)
	mock.ExpectQuery("SELECT OWNER,TRIGGER_NAME,STATUS FROM ALL_TRIGGERS").WillReturnRows(sqlmock.NewRows([]string{"owner", "trigger", "status"}))
	mock.ExpectQuery("SELECT c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME,c.STATUS").WillReturnRows(sqlmock.NewRows([]string{"owner", "table", "constraint", "status", "validated"}).AddRow("MixedOwner", "Child", "FK", "ENABLED", "NOT VALIDATED"))
	disable, restore, err := w.NativeRecoveryState(context.Background(), "T")
	require.NoError(t, err)
	require.Len(t, disable, 1)
	require.Equal(t, `ALTER TABLE "MixedOwner"."Child" ENABLE NOVALIDATE CONSTRAINT "FK"`, restore[0].SQL)
}

func TestDamengNativeValueBudgetIncludesNestedLOBs(t *testing.T) {
	for _, value := range []any{dm.NewBlob([]byte{0, 1, 2}), dm.NewClob("中文")} {
		size, err := DamengNativeValueSize(value)
		require.NoError(t, err)
		require.Greater(t, size, 0)
		_, err = nativeComplexValue(value, "Source", "Target")
		require.NoError(t, err)
	}
	require.Equal(t, `"Target"."LocalType"`, nativeComplexTypeName("Source", "Target", `"Source"."LocalType"`))
	require.Equal(t, `"External"."OtherType"`, nativeComplexTypeName("Source", "Target", `"External"."OtherType"`))
	decimal, err := dm.NewDecimalFromString("42.25")
	require.NoError(t, err)
	bound, err := nativeComplexValueAt(decimal, "Source", "Target", false)
	require.NoError(t, err)
	require.IsType(t, dm.DmDecimal{}, bound)
	bound, err = nativeComplexValueAt(dm.NewBlob([]byte{1, 2}), "Source", "Target", false)
	require.NoError(t, err)
	require.IsType(t, dm.DmBlob{}, bound)
	negative := "-1234567890123456789.1234567890123456789"
	exact, err := nativeExactUDTDecimal(negative)
	require.NoError(t, err)
	bound, err = nativeComplexValueAt(exact, "Source", "Target", false)
	require.NoError(t, err)
	require.Equal(t, negative, bound.(dm.DmDecimal).String())
}

func TestDamengNativeDefinitionComparisonPreservesSemantics(t *testing.T) {
	require.True(t, nativeDefinitionsEqual(`CREATE TABLE "S"."T" ("id" BIGINT IDENTITY(-9,2),"x" INT DEFAULT 4);`, `create table "S"."T" ("id" bigint identity(21,2),"x" int default 4)`))
	require.False(t, nativeDefinitionsEqual(`CREATE TABLE "S"."T" ("x" INT DEFAULT 4)`, `CREATE TABLE "S"."T" ("x" INT DEFAULT 5)`))
	require.False(t, nativeDefinitionsEqual(`CREATE VIEW "S"."V" AS SELECT 'AbC' FROM DUAL`, `CREATE VIEW "S"."V" AS SELECT 'abc' FROM DUAL`))
}

func TestDamengNativePreflightRejectsCreatePermissionBeforeDDL(t *testing.T) {
	w, mock := mockNativeDM(t)
	mock.ExpectQuery("SELECT USER FROM DUAL").WillReturnRows(sqlmock.NewRows([]string{"user"}).AddRow("MixedOwner"))
	mock.ExpectQuery("SELECT PRIVILEGE FROM SESSION_PRIVS").WillReturnRows(sqlmock.NewRows([]string{"privilege"}).AddRow("DROP ANY TABLE"))
	issues := w.ValidateDamengNativeObjects(context.Background(), []damengnative.Object{{Kind: "TABLE", Name: "T", Schema: "MixedOwner", Units: []damengnative.Unit{{SQL: `CREATE TABLE "MixedOwner"."T" ("x" INT)`}}}})
	require.Len(t, issues, 1)
	require.Contains(t, issues[0].Reason, "CREATE TABLE")
}

func TestDamengNativeExternalReferenceMustExist(t *testing.T) {
	w, mock := mockNativeDM(t)
	mock.ExpectQuery("SELECT COUNT.*ALL_OBJECTS").WithArgs("External", "Lookup").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	require.ErrorContains(t, w.CheckDamengNativeReference(context.Background(), damengnative.Reference{Schema: "External", Name: "Lookup", Kind: "TABLE"}), "不存在")
}

func TestDamengNativeStorageTablespacesAndExternalDropGuard(t *testing.T) {
	tokens, err := nativeSQLTokens(`CREATE TABLE "MixedOwner"."T" ("x" INT) STORAGE(ON "MAIN",LOB ON "LOB_SPACE") TABLESPACE "MAIN"`)
	require.NoError(t, err)
	require.Equal(t, []string{"MAIN", "LOB_SPACE"}, nativeTablespaces(tokens))
	w, mock := mockNativeDM(t)
	mock.ExpectQuery("SELECT USER FROM DUAL").WillReturnRows(sqlmock.NewRows([]string{"user"}).AddRow("SYSDBA"))
	mock.ExpectQuery("SELECT PRIVILEGE FROM SESSION_PRIVS").WillReturnRows(sqlmock.NewRows([]string{"privilege"}))
	mock.ExpectQuery("SELECT c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME FROM ALL_CONSTRAINTS").WithArgs("MixedOwner", "T", "MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"owner", "table", "constraint"}).AddRow("External", "Child", "FK"))
	mock.ExpectQuery("SELECT DISTINCT s.NAME,dep.NAME FROM SYS.SYSDEPENDENCIES").WithArgs("MixedOwner", "T", "MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"owner", "object"}))
	issues := w.ValidateDamengNativeObjects(context.Background(), []damengnative.Object{{Kind: "table", Name: "T", Schema: "MixedOwner", Units: []damengnative.Unit{{SQL: `DROP TABLE "MixedOwner"."T" CASCADE CONSTRAINTS`, Phase: "drop"}}}})
	require.Len(t, issues, 1)
	require.Contains(t, issues[0].Reason, "禁止级联删除")
}
