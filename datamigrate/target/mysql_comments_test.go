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

const mysqlCommentModeSQL = "SET SESSION sql_mode=CONCAT(@@SESSION.sql_mode, ',NO_BACKSLASH_ESCAPES')"

func mysqlCommentWriter(t *testing.T, srcType string) (*MySQLWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		_ = db.Close()
	})
	return &MySQLWriter{db: db, schema: "app", srcType: srcType, dia: dialect.NewMySQL()}, mock
}

func TestMySQLPrepareCommentPreservesActualColumnDefinition(t *testing.T) {
	w, mock := mysqlCommentWriter(t, "oracle")
	mock.ExpectExec(regexp.QuoteMeta("SET SESSION sql_mode='', sql_quote_show_create=1")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `app`.`Mixed`")).WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("Mixed", "CREATE TABLE `Mixed` (`Value` varchar(120) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'a,b\\n\\\\path\\'x' COMMENT 'old' /*!80023 INVISIBLE */, PRIMARY KEY (`Value`)) ENGINE=InnoDB"))
	mock.ExpectClose()
	var events []repair.Event
	ctx := repair.WithObserver(context.Background(), func(event repair.Event) error {
		events = append(events, event)
		return nil
	})
	statements, err := w.PrepareComment(ctx, source.CommentInfo{TableName: "Mixed", ColumnName: "Value", Comment: "新 '注释' \\path"})
	require.NoError(t, err)
	require.Equal(t, []dialect.Statement{{SQL: "ALTER TABLE `app`.`Mixed` MODIFY COLUMN `Value` varchar(120) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'a,b\n\\path''x' COMMENT '新 ''注释'' \\path' /*!80023 INVISIBLE */"}}, statements)
	require.Empty(t, events, "preparation must not execute DDL")
}

func TestMySQLPrepareCommentFailureDoesNotExecuteDDL(t *testing.T) {
	for _, tc := range []struct {
		name       string
		modeError  bool
		queryError bool
		ddl        string
	}{
		{name: "session mode failure", modeError: true},
		{name: "metadata read failure", queryError: true},
		{name: "target column missing", ddl: "CREATE TABLE `Mixed` (`Other` int)"},
		{name: "ambiguous target definition", ddl: "CREATE TABLE `Mixed` (`Value` int COMMENT 'old' COMMENT 'duplicate')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := mysqlCommentWriter(t, "oracle")
			mode := mock.ExpectExec(regexp.QuoteMeta("SET SESSION sql_mode='', sql_quote_show_create=1"))
			if tc.modeError {
				mode.WillReturnError(errors.New("mode denied"))
			} else {
				mode.WillReturnResult(sqlmock.NewResult(0, 0))
				query := mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `app`.`Mixed`"))
				if tc.queryError {
					query.WillReturnError(errors.New("metadata denied"))
				} else {
					query.WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("Mixed", tc.ddl))
				}
			}
			mock.ExpectClose()
			err := w.CreateComment(context.Background(), source.CommentInfo{TableName: "Mixed", ColumnName: "Value", Comment: "new"})
			require.Error(t, err)
		})
	}
}

func TestMySQLExecuteCommentStatementsObservesAndDiscardsSession(t *testing.T) {
	const query = "ALTER TABLE `app`.`Mixed` MODIFY COLUMN `Value` varchar(30) DEFAULT 'a\\b''c' COMMENT '注释\\路径''文本'"
	for _, tc := range []struct {
		name           string
		modeError      bool
		executionError bool
		observerError  bool
	}{
		{name: "success"},
		{name: "mode failure", modeError: true},
		{name: "database failure", executionError: true},
		{name: "durable state failure prevents DDL", observerError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := mysqlCommentWriter(t, "oracle")
			mode := mock.ExpectExec(regexp.QuoteMeta(mysqlCommentModeSQL))
			if tc.modeError {
				mode.WillReturnError(errors.New("mode denied"))
			} else {
				mode.WillReturnResult(sqlmock.NewResult(0, 0))
				if !tc.observerError {
					execution := mock.ExpectExec(regexp.QuoteMeta(query))
					if tc.executionError {
						execution.WillReturnError(errors.New("column locked"))
					} else {
						execution.WillReturnResult(sqlmock.NewResult(0, 0))
					}
				}
			}
			mock.ExpectClose()
			var events []repair.Event
			ctx := repair.WithObserver(context.Background(), func(event repair.Event) error {
				events = append(events, event)
				if tc.observerError {
					return errors.New("state unavailable")
				}
				return nil
			})
			err := w.ExecuteCommentStatements(ctx, []dialect.Statement{{SQL: query}})
			if tc.modeError || tc.executionError || tc.observerError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			switch {
			case tc.modeError:
				require.Empty(t, events)
			case tc.observerError:
				require.Equal(t, []repair.Event{{SQL: query, Status: "running"}}, events)
			case tc.executionError:
				require.Equal(t, []repair.Event{{SQL: query, Status: "running"}, {SQL: query, Status: "failed", Error: "column locked"}}, events)
			default:
				require.Equal(t, []repair.Event{{SQL: query, Status: "running"}, {SQL: query, Status: "succeeded"}}, events)
			}
		})
	}
	// No connection or SQL mode changes are needed without prepared statements.
	require.NoError(t, (&MySQLWriter{}).ExecuteCommentStatements(context.Background(), nil))
}

func TestMySQLCreateOracleCommentUsesPreparedStatements(t *testing.T) {
	for _, column := range []bool{false, true} {
		t.Run(map[bool]string{false: "table comment", true: "column comment"}[column], func(t *testing.T) {
			w, mock := mysqlCommentWriter(t, "oracle")
			cm := source.CommentInfo{TableName: "Mixed", Comment: "a\\b'中文"}
			query := "ALTER TABLE `app`.`Mixed` COMMENT = 'a\\b''中文'"
			if column {
				// Keep sqlmock's driver registered while the preparation session is
				// discarded before execution opens its separate physical session.
				keepAlive, err := w.db.Conn(context.Background())
				require.NoError(t, err)
				defer keepAlive.Close()
				cm.ColumnName = "Id"
				mock.ExpectExec(regexp.QuoteMeta("SET SESSION sql_mode='', sql_quote_show_create=1")).WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `app`.`Mixed`")).WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("Mixed", "CREATE TABLE `Mixed` (`Id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`Id`)) ENGINE=InnoDB"))
				mock.ExpectClose()
				query = "ALTER TABLE `app`.`Mixed` MODIFY COLUMN `Id` bigint unsigned NOT NULL AUTO_INCREMENT COMMENT 'a\\b''中文'"
			}
			mock.ExpectExec(regexp.QuoteMeta(mysqlCommentModeSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(regexp.QuoteMeta(query)).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectClose()
			require.NoError(t, w.CreateComment(context.Background(), cm))
		})
	}
}

func TestMySQLCreateCommentKeepsOtherSourceBehavior(t *testing.T) {
	w, mock := mysqlCommentWriter(t, "mysql")
	mock.ExpectExec(regexp.QuoteMeta("ALTER TABLE `app`.`Mixed` COMMENT = 'table'")).WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, w.CreateComment(context.Background(), source.CommentInfo{TableName: "Mixed", Comment: "table"}))
	require.NoError(t, w.CreateComment(context.Background(), source.CommentInfo{TableName: "Mixed", ColumnName: "Value", Comment: "column"}))
}
