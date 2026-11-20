package target

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestRepairMySQLCommentPreservesCompleteColumnDefinition(t *testing.T) {
	ddl := "CREATE TABLE `T` (\n `Id` bigint unsigned NOT NULL AUTO_INCREMENT COMMENT 'old',\n `Va``lue` varchar(120) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'a,b\\n\\\\path\\'x' COMMENT 'old\\'comment' /*!80023 INVISIBLE */,\n `computed` varchar(60) GENERATED ALWAYS AS (concat(`Va``lue`, ',COMMENT ', 'suffix')) STORED,\n PRIMARY KEY (`Id`)\n) ENGINE=InnoDB"
	got, err := repairMySQLColumnComment(ddl, "T", "Va`lue", "new '中文' \\path")
	require.NoError(t, err)
	require.Equal(t, "`Va``lue` varchar(120) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'a,b\n\\path''x' COMMENT 'new ''中文'' \\path' /*!80023 INVISIBLE */", got)
	got, err = repairMySQLColumnComment(ddl, "T", "Id", "identity")
	require.NoError(t, err)
	require.Equal(t, "`Id` bigint unsigned NOT NULL AUTO_INCREMENT COMMENT 'identity'", got)
	got, err = repairMySQLColumnComment(ddl, "T", "ID", "identity")
	require.NoError(t, err)
	require.Equal(t, "`Id` bigint unsigned NOT NULL AUTO_INCREMENT COMMENT 'identity'", got)
	got, err = repairMySQLColumnComment(ddl, "T", "computed", "generated")
	require.NoError(t, err)
	require.Equal(t, "`computed` varchar(60) GENERATED ALWAYS AS (concat(`Va``lue`, ',COMMENT ', 'suffix')) STORED COMMENT 'generated'", got)
}

func TestRepairMySQLCommentRejectsAmbiguousDefinitions(t *testing.T) {
	for _, ddl := range []string{
		"CREATE VIEW `T` AS SELECT 1",
		"CREATE TABLE `Other` (`v` int)",
		"CREATE TABLE `T` (`v` int, `v` int)",
		"CREATE TABLE `T` (`v` int, `V` int)",
		"CREATE TABLE `T` (`v` int COMMENT 'a' COMMENT 'b')",
		"CREATE TABLE `T` (`v` varchar(20) DEFAULT 'unclosed)",
		"CREATE TABLE `T` (`v` int COMMENT (SELECT 1))",
		"CREATE TABLE `T` (`v` int /* unknown extension */)",
		"CREATE TABLE `T` (`v` int); DROP TABLE `other`",
		"CREATE TABLE `T` (`v` varchar(10) DEFAULT '\\q')",
	} {
		t.Run(ddl, func(t *testing.T) { _, err := repairMySQLColumnComment(ddl, "T", "v", "new"); require.Error(t, err) })
	}
}

func TestGenerateRepairMySQLCommentReadsActualDefinition(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	w := &MySQLWriter{db: db, schema: "app", dia: dialect.NewMySQL()}
	m.ExpectExec(regexp.QuoteMeta("SET SESSION sql_mode='', sql_quote_show_create=1")).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `app`.`T`")).WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("T", "CREATE TABLE `T` (`Id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`Id`)) ENGINE=InnoDB"))
	m.ExpectClose()
	units, err := GenerateRepairComment(context.Background(), w, source.CommentInfo{TableName: "T", ColumnName: "Id", Comment: "key"})
	require.NoError(t, err)
	require.Equal(t, []dialect.Statement{{SQL: "ALTER TABLE `app`.`T` MODIFY COLUMN `Id` bigint unsigned NOT NULL AUTO_INCREMENT COMMENT 'key'"}}, units)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestGenerateRepairMySQLCommentChecksTargetTableCaseRules(t *testing.T) {
	for _, tc := range []struct {
		name       string
		shown      string
		caseMode   int
		probeError bool
		wantError  bool
	}{
		{name: "case mode 1 permits folded table", shown: "orders", caseMode: 1},
		{name: "case mode 2 permits folded table", shown: "orders", caseMode: 2},
		{name: "case mode 0 rejects folded table", shown: "orders", caseMode: 0, wantError: true},
		{name: "invalid case mode rejects folded table", shown: "orders", caseMode: 3, wantError: true},
		{name: "nonmatching table rejected", shown: "other", caseMode: 1, wantError: true},
		{name: "case rule unavailable", shown: "orders", probeError: true, wantError: true},
		{name: "exact table needs no case mode query", shown: "Orders"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			w := &MySQLWriter{db: db, schema: "app", dia: dialect.NewMySQL()}
			mock.ExpectExec(regexp.QuoteMeta("SET SESSION sql_mode='', sql_quote_show_create=1")).WillReturnResult(sqlmock.NewResult(0, 0))
			ddl := "CREATE TABLE `" + tc.shown + "` (`Id` bigint NOT NULL, PRIMARY KEY (`Id`)) ENGINE=InnoDB"
			mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `app`.`Orders`")).WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow(tc.shown, ddl))
			if tc.shown != "Orders" {
				query := mock.ExpectQuery(regexp.QuoteMeta("SELECT @@lower_case_table_names"))
				if tc.probeError {
					query.WillReturnError(errors.New("case mode unavailable"))
				} else {
					query.WillReturnRows(sqlmock.NewRows([]string{"lower_case_table_names"}).AddRow(tc.caseMode))
				}
			}
			mock.ExpectClose()
			units, err := GenerateRepairComment(context.Background(), w, source.CommentInfo{TableName: "Orders", ColumnName: "ID", Comment: "key"})
			if tc.wantError {
				require.Error(t, err)
				require.Nil(t, units)
			} else {
				require.NoError(t, err)
				require.Equal(t, []dialect.Statement{{SQL: "ALTER TABLE `app`.`Orders` MODIFY COLUMN `Id` bigint NOT NULL COMMENT 'key'"}}, units)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestGenerateRepairCommentDoesNotExecuteDDL(t *testing.T) {
	w, m := repairPGMock(t)
	units, err := GenerateRepairComment(context.Background(), w, source.CommentInfo{TableName: "T", ColumnName: "ID", Comment: "a'b"})
	require.NoError(t, err)
	require.Len(t, units, 1)
	require.Contains(t, units[0].SQL, `COMMENT ON COLUMN "app"."T"."ID"`)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestInspectRepairCommentComparesActualText(t *testing.T) {
	for _, tc := range []struct {
		name, actual, status string
		edited               bool
	}{
		{"equal", "expected", "succeeded", false},
		{"wrong", "wrong", "failed", false},
		{"edited expectation", "changed", "unknown", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			w := &MySQLWriter{db: db, schema: "app", dia: dialect.NewMySQL()}
			m.ExpectQuery("SELECT COLUMN_COMMENT").WithArgs("app", "T", "Id").WillReturnRows(sqlmock.NewRows([]string{"comment"}).AddRow(tc.actual))
			obj := repair.Object{Kind: "comment", Schema: "app", Table: "T", Comment: &source.CommentInfo{TableName: "T", ColumnName: "Id", Comment: "expected"}}
			if tc.edited {
				obj.Units = []repair.Unit{{SQL: "changed", OriginalSQL: "original"}}
			}
			v, err := InspectRepair(context.Background(), w, obj)
			require.NoError(t, err)
			require.Equal(t, tc.status, v.Status)
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestExecuteRepairMySQLCommentPinsStringEscaping(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	w := &MySQLWriter{db: db, schema: "app", dia: dialect.NewMySQL()}
	m.ExpectExec(regexp.QuoteMeta("USE `app`")).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(regexp.QuoteMeta("SET SESSION sql_mode=CONCAT(@@SESSION.sql_mode, ',NO_BACKSLASH_ESCAPES')")).WillReturnResult(sqlmock.NewResult(0, 0))
	q := "ALTER TABLE `app`.`T` COMMENT = 'a\\b''c'"
	m.ExpectExec(regexp.QuoteMeta(q)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectClose()
	units, err := ExecuteRepair(context.Background(), w, repair.Object{Kind: "comment", Schema: "app"}, []repair.Unit{{SQL: q, Selected: true}}, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", units[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestDamengCommentRepairReadsSubmittedTextOnTheSameSession(t *testing.T) {
	const ddl = `COMMENT ON TABLE "APP"."Mixed.Table" IS 'old'`
	const edited = `COMMENT ON TABLE "APP"."Mixed.Table" IS 'new''s; text'`
	for _, tc := range []struct {
		name, actual, status string
		readErr              error
	}{
		{"edited text", "new's; text", "succeeded", nil},
		{"original text still present", "old", "failed", nil},
		{"catalog unavailable after DDL", "", "unknown", errors.New("catalog unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			db.SetMaxOpenConns(1)
			obj := repair.Object{Kind: "comment", Table: "mixed.table", Schema: "app", DDL: ddl,
				Units: []repair.Unit{{SQL: edited, OriginalSQL: ddl, Selected: true}}}
			w := &DaMengWriter{db: db, schema: "APP", dia: dialect.NewDaMeng()}
			mock.ExpectExec(`SET SCHEMA "APP"`).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(regexp.QuoteMeta(edited)).WillReturnResult(sqlmock.NewResult(0, 0))
			read := mock.ExpectQuery("SELECT COMMENTS FROM ALL_TAB_COMMENTS").WithArgs("APP", "Mixed.Table")
			if tc.readErr != nil {
				read.WillReturnError(tc.readErr)
			} else {
				read.WillReturnRows(sqlmock.NewRows([]string{"COMMENTS"}).AddRow(tc.actual))
			}
			mock.ExpectClose()
			units, err := ExecuteRepair(t.Context(), w, obj, obj.Units, nil)
			if tc.status == "succeeded" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, tc.status, units[0].Status)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestInspectSavedDamengComment(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	const ddl = `COMMENT ON COLUMN "App"."Mixed"."Col" IS 'old'`
	obj := repair.Object{Kind: "comment", Schema: "App", DDL: ddl, Units: []repair.Unit{
		{SQL: `COMMENT ON COLUMN "Mixed"."Col" IS 'edited'`, OriginalSQL: ddl, Status: "succeeded"},
		{SQL: `COMMENT ON COLUMN "Mixed"."Col" IS 'unsent'`, Status: "pending"},
	}}
	w := &DaMengWriter{db: db, schema: "APP", dia: dialect.NewDaMeng()}
	v, err := InspectRepair(t.Context(), w, obj)
	require.NoError(t, err)
	require.Equal(t, "failed", v.Status, "a different quoted schema must not be inspected")
	w.schema = "App"
	mock.ExpectQuery("SELECT COMMENTS FROM ALL_COL_COMMENTS").WithArgs("App", "Mixed", "Col").
		WillReturnRows(sqlmock.NewRows([]string{"COMMENTS"}).AddRow("edited"))
	v, err = InspectRepair(t.Context(), w, obj)
	require.NoError(t, err)
	require.Equal(t, "succeeded", v.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}
