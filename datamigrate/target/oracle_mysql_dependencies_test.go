package target

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"strconv"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func oracleMySQLDependencyMock(t *testing.T, schema string) (*MySQLWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &MySQLWriter{db: db, schema: schema, dia: dialect.NewMySQL()}, mock
}

func mysqlFKCatalogRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "CONSTRAINT_NAME", "REFERENCED_TABLE_SCHEMA", "REFERENCED_TABLE_NAME", "COLUMN_NAME", "REFERENCED_COLUMN_NAME", "ORDINAL_POSITION", "DELETE_RULE", "UPDATE_RULE"})
}

func TestOracleMySQLVersionCapabilities(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		checks bool
		bad    bool
	}{
		{"5.7.44-log", false, false},
		{"8.0.15", false, false},
		{"8.0.16-commercial", true, false},
		{"8.4.0", true, false},
		{"9.0.1", true, false},
		{" 8.0.36-ubuntu ", true, false},
		{"5.6.51", false, true},
		{"5.5.5-10.11.6-MariaDB", false, true},
		{"MySQL unknown", false, true},
		{"999999999999999999999.0.0", false, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			w, mock := oracleMySQLDependencyMock(t, "app")
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT VERSION()`)).WillReturnRows(sqlmock.NewRows([]string{"VERSION()"}).AddRow(tc.raw))
			v, err := w.OracleMySQLVersion(context.Background())
			if tc.bad {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.checks, v.Checks())
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
	w, mock := oracleMySQLDependencyMock(t, "app")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT VERSION()`)).WillReturnError(errors.New("version denied"))
	_, err := w.OracleMySQLVersion(context.Background())
	require.ErrorContains(t, err, "version denied")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOracleMySQLSchemaResolutionIsReadOnly(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		w, mock := oracleMySQLDependencyMock(t, "App`")
		schema, err := w.OracleMySQLSchema(context.Background())
		require.NoError(t, err)
		require.Equal(t, "App`", schema)
		require.NoError(t, mock.ExpectationsWereMet())
	})
	for _, value := range []driver.Value{"FromDSN", nil, ""} {
		t.Run("default", func(t *testing.T) {
			w, mock := oracleMySQLDependencyMock(t, "")
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT DATABASE()`)).WillReturnRows(sqlmock.NewRows([]string{"DATABASE()"}).AddRow(value))
			schema, err := w.OracleMySQLSchema(context.Background())
			if value == nil || value == "" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, value, schema)
			}
			require.Empty(t, w.schema, "resolving the default does not mutate the writer")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
	t.Run("canceled", func(t *testing.T) {
		w, mock := oracleMySQLDependencyMock(t, "app")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := w.OracleMySQLSchema(ctx)
		require.ErrorIs(t, err, context.Canceled)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestOracleMySQLLowerCaseTableNamesReadOnly(t *testing.T) {
	for _, mode := range []int{-1, 0, 1, 2, 3} {
		t.Run(strconv.Itoa(mode), func(t *testing.T) {
			w, mock := oracleMySQLDependencyMock(t, "app")
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT @@lower_case_table_names`)).WillReturnRows(sqlmock.NewRows([]string{"@@lower_case_table_names"}).AddRow(mode))
			got, err := w.OracleMySQLLowerCaseTableNames(context.Background())
			if mode < 0 || mode > 2 {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, mode, got)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
	w, mock := oracleMySQLDependencyMock(t, "app")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT @@lower_case_table_names`)).WillReturnError(errors.New("settings denied"))
	_, err := w.OracleMySQLLowerCaseTableNames(context.Background())
	require.ErrorContains(t, err, "settings denied")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOracleMySQLForeignKeyInventoryAndRestoreDDL(t *testing.T) {
	w, mock := oracleMySQLDependencyMock(t, "app")
	mock.ExpectQuery(regexp.QuoteMeta(oracleMySQLForeignKeyQuery)).WithArgs("app", "app").WillReturnRows(mysqlFKCatalogRows().
		AddRow("app", "Child`", "fk`", "app", "Parent`", "B`", "ID2", 1, "CASCADE", "SET NULL").
		AddRow("app", "Child`", "fk`", "app", "Parent`", "A", "ID1", 2, "CASCADE", "SET NULL").
		AddRow("app", "Self", "self_fk", "app", "Self", "ParentId", "Id", 1, "NO ACTION", "RESTRICT").
		AddRow("outside", "Retained", "foreign_fk", "app", "Parent`", "ParentId", "Id", 1, "RESTRICT", "CASCADE"))
	fks, err := w.OracleMySQLForeignKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, fks, 3)
	require.Equal(t, []string{"B`", "A"}, fks[0].Columns)
	require.Equal(t, []string{"ID2", "ID1"}, fks[0].RefColumns)
	require.Equal(t, "ALTER TABLE `app`.`Child``` DROP FOREIGN KEY `fk```", fks[0].DropSQL)
	require.Equal(t, "ALTER TABLE `app`.`Child``` ADD CONSTRAINT `fk``` FOREIGN KEY (`B```, `A`) REFERENCES `app`.`Parent``` (`ID2`, `ID1`) ON DELETE CASCADE ON UPDATE SET NULL", fks[0].RestoreSQL)
	require.Equal(t, fks[1].Table, fks[1].RefTable)
	require.Equal(t, "outside", fks[2].Schema)
	require.Contains(t, fks[2].RestoreSQL, "REFERENCES `app`.`Parent```")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOracleMySQLForeignKeyInventoryResolvesDefaultDatabase(t *testing.T) {
	w, mock := oracleMySQLDependencyMock(t, "")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT DATABASE()`)).WillReturnRows(sqlmock.NewRows([]string{"DATABASE()"}).AddRow("app"))
	mock.ExpectQuery(regexp.QuoteMeta(oracleMySQLForeignKeyQuery)).WithArgs("app", "app").WillReturnRows(mysqlFKCatalogRows())
	fks, err := w.OracleMySQLForeignKeys(context.Background())
	require.NoError(t, err)
	require.NotNil(t, fks)
	require.Empty(t, fks)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOracleMySQLForeignKeyInventoryRejectsPartialMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows *sqlmock.Rows
	}{
		{"missing referential catalog row", mysqlFKCatalogRows().AddRow("app", "child", "fk", "app", "parent", "pid", "id", 1, nil, nil)},
		{"missing name", mysqlFKCatalogRows().AddRow("app", "child", "", "app", "parent", "pid", "id", 1, "RESTRICT", "RESTRICT")},
		{"missing initial column", mysqlFKCatalogRows().AddRow("app", "child", "fk", "app", "parent", "pid", "id", 2, "RESTRICT", "RESTRICT")},
		{"invalid action", mysqlFKCatalogRows().AddRow("app", "child", "fk", "app", "parent", "pid", "id", 1, "SET DEFAULT", "RESTRICT")},
		{"inconsistent target", mysqlFKCatalogRows().
			AddRow("app", "child", "fk", "app", "parent", "pid", "id", 1, "RESTRICT", "RESTRICT").
			AddRow("app", "child", "fk", "app", "different", "pid2", "id2", 2, "RESTRICT", "RESTRICT")},
		{"row read failure", mysqlFKCatalogRows().AddRow("app", "child", "fk", "app", "parent", "pid", "id", 1, "RESTRICT", "RESTRICT").RowError(0, errors.New("catalog interrupted"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := oracleMySQLDependencyMock(t, "app")
			mock.ExpectQuery(regexp.QuoteMeta(oracleMySQLForeignKeyQuery)).WithArgs("app", "app").WillReturnRows(tc.rows)
			fks, err := w.OracleMySQLForeignKeys(context.Background())
			require.Error(t, err)
			require.Nil(t, fks)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
	w, mock := oracleMySQLDependencyMock(t, "app")
	mock.ExpectQuery(regexp.QuoteMeta(oracleMySQLForeignKeyQuery)).WithArgs("app", "app").WillReturnError(errors.New("denied"))
	_, err := w.OracleMySQLForeignKeys(context.Background())
	require.ErrorContains(t, err, "denied")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOracleMySQLDependencyExecutionUsesDurableObserver(t *testing.T) {
	const query = "ALTER TABLE `app`.`child` DROP FOREIGN KEY `fk`"
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "database failure"}[fail], func(t *testing.T) {
			w, mock := oracleMySQLDependencyMock(t, "app")
			expect := mock.ExpectExec(regexp.QuoteMeta(query))
			if fail {
				expect.WillReturnError(errors.New("1217 blocked"))
			} else {
				expect.WillReturnResult(sqlmock.NewResult(0, 0))
			}
			var events []repair.Event
			ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { events = append(events, e); return nil })
			err := w.ExecuteOracleMySQLDependency(ctx, query)
			if fail {
				require.ErrorContains(t, err, query)
				require.ErrorContains(t, err, "1217")
			} else {
				require.NoError(t, err)
			}
			require.Len(t, events, 2)
			require.Equal(t, "running", events[0].Status)
			require.Equal(t, map[bool]string{false: "succeeded", true: "failed"}[fail], events[1].Status)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
	t.Run("save failure prevents SQL", func(t *testing.T) {
		w, mock := oracleMySQLDependencyMock(t, "app")
		ctx := repair.WithObserver(context.Background(), func(repair.Event) error { return errors.New("disk full") })
		err := w.ExecuteOracleMySQLDependency(ctx, query)
		require.ErrorContains(t, err, "disk full")
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
