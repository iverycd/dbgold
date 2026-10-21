package source

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const mysqlOscarViewsQuery = `SELECT TABLE_NAME, VIEW_DEFINITION FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME`

func TestMySQLOscarViewsPreserveQuotedNamesAndPrefixes(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &MySQLReader{db: db, dbName: "FinancePlatform_3.0"}
	raw := "select `FinancePlatform_3.0`.`Ta``ble.Name`.`MixedColumn`, `External.Db`.`OtherTable`.`ID`, 'Keep.MiXeD' AS `DisplayName` from `FinancePlatform_3.0`.`Ta``ble.Name`"
	mock.ExpectQuery(regexp.QuoteMeta(mysqlOscarViewsQuery)).WithArgs("FinancePlatform_3.0").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "VIEW_DEFINITION"}).AddRow("MixedView", raw)).RowsWillBeClosed()

	views, err := r.GetViewsForTarget(context.Background(), "oscar")
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.Equal(t, "MixedView", views[0].ViewName)
	require.Equal(t, raw, views[0].Definition)
	require.Empty(t, views[0].Columns)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMySQLOscarViewsKeepCompatibilityConversions(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &MySQLReader{db: db, dbName: "Db.0"}
	raw := "select ifnull(convert(`Db.0`.`Mixed`.`Name` using utf8mb4), 'Keep.MiXeD') AS `DisplayName`, isnull(`Mixed`.`ID`) AS `Missing`, date_format(`Mixed`.`Created`, '%Y-%m-%d') AS `Day` from (`Db.0`.`Mixed`)"
	want := "select coalesce(`Db.0`.`Mixed`.`Name` , 'Keep.MiXeD') AS `DisplayName`, (`Mixed`.`ID` IS NULL) AS `Missing`, to_char(`Mixed`.`Created`, 'YYYY-MM-DD') AS `Day` FROM `Db.0`.`Mixed`"
	mock.ExpectQuery(regexp.QuoteMeta(mysqlOscarViewsQuery)).WithArgs("Db.0").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "VIEW_DEFINITION"}).AddRow("MixedView", raw))

	views, err := r.GetViewsForTarget(context.Background(), "oscar")
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.Equal(t, want, views[0].Definition)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMySQLOscarViewsRejectUnreadableDefinitions(t *testing.T) {
	for _, tt := range []struct {
		name       string
		definition any
	}{{"null", nil}, {"empty", ""}, {"blank", " \n\t"}} {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			r := &MySQLReader{db: db, dbName: "Db.0"}
			mock.ExpectQuery(regexp.QuoteMeta(mysqlOscarViewsQuery)).WithArgs("Db.0").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "VIEW_DEFINITION"}).AddRow("UnreadableView", tt.definition)).RowsWillBeClosed()

			_, err = r.GetViewsForTarget(context.Background(), "oscar")
			require.ErrorContains(t, err, "视图 UnreadableView 定义不可读")
			require.ErrorContains(t, err, "SHOW VIEW")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestMySQLOscarViewsKeepBooleanCompatibilityFixes(t *testing.T) {
	for _, tt := range []struct {
		name, reference, value string
	}{
		{"view_portal_myitem", "portal_item.DISABLED", "0"},
		{"view_portal_myitem", "`portal_item`.`DISABLED`", "0"},
		{"view_portal_myitem", `"Portal_Item" . "Disabled"`, "0"},
		{"view_portal_myitem", "`Db.0`.`portal_item`.`DISABLED`", "0"},
		{"view_portal_myitem", " `Db``.0` \t. `portal_item` \n. `DISABLED` ", "0"},
		{"view_portal_myitem", "Db0.portal_item.DISABLED", "0"},
		{"view_cns_jxkh_rule_item", "b.ISUSED", "1"},
		{"view_cns_jxkh_rule_item", "`b`.`ISUSED`", "1"},
		{"view_cns_jxkh_rule_item", `"B" . "IsUsed"`, "1"},
		{"view_cns_jxkh_rule_item", "`Db.0`.`b`.`ISUSED`", "1"},
		{"view_cns_jxkh_rule_item", ` "Db"".0" . "B" . "IsUsed" `, "1"},
		{"view_cns_jxkh_rule_item", "Db0.b.ISUSED", "1"},
	} {
		t.Run(tt.name+"/"+tt.reference, func(t *testing.T) {
			query := "select * from `Db.0`.`Items` where (" + tt.reference + " = " + tt.value + ")"
			want := "select * from `Db.0`.`Items` where (" + tt.reference + " = '" + tt.value + "')"
			require.Equal(t, want, transformMySQLViewForOscar(tt.name, query))
			require.Equal(t, query, transformMySQLViewForOscar("AnotherView", query))
		})
	}
}

func TestMySQLOscarCharsetConvertCleanup(t *testing.T) {
	for _, tt := range []struct{ sql, want string }{
		{"select convert(`Name` using utf8mb4)", "select `Name` "},
		{"select CONVERT (concat(`First`, ')', `Last`) USING UTF8MB4)", "select concat(`First`, ')', `Last`) "},
		{"select convert(convert(`Name` using utf8mb4) using utf8mb4)", "select `Name`  "},
		{"select convert(`Name`, char(36))", "select convert(`Name`, char(36))"},
		{"select convert(`Name`, char -- using utf8mb4\n)", "select convert(`Name`, char -- using utf8mb4\n)"},
		{"select 'convert(Name using utf8mb4)', `convert(Name using utf8mb4)` /* convert(Name using utf8mb4) */ -- convert(Name using utf8mb4)\n", "select 'convert(Name using utf8mb4)', `convert(Name using utf8mb4)` /* convert(Name using utf8mb4) */ -- convert(Name using utf8mb4)\n"},
		{"select convert(`Name` using utf8mb4", "select convert(`Name` using utf8mb4"},
	} {
		t.Run(tt.sql, func(t *testing.T) {
			require.Equal(t, tt.want, stripMySQLUTF8MB4Convert(tt.sql))
		})
	}
	// The existing typed-CONVERT compatibility rewrite remains available.
	require.Equal(t, "select `Name`", transformMySQLViewForOscar("v", "select convert(`Name`, char(36))"))
}
