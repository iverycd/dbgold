package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func TestDamengIdentityCutoverBoundsIntegration(t *testing.T) {
	mysqlDSN, dmDSN := os.Getenv("CDC_MYSQL_DSN"), os.Getenv("CDC_DAMENG_DSN")
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" || mysqlDSN == "" || dmDSN == "" {
		t.Skip("requires disposable MySQL and DM instances")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	sourceCfg, err := mysqlDriver.ParseDSN(mysqlDSN)
	require.NoError(t, err)
	sourceCfg.DBName = ""
	src, err := sql.Open("mysql", sourceCfg.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, src.Close()) })
	database := fmt.Sprintf("cdc_ident_%d", time.Now().UnixNano())
	_, err = src.ExecContext(ctx, "CREATE DATABASE `"+database+"`")
	require.NoError(t, err)
	t.Cleanup(func() { _, e := src.Exec("DROP DATABASE `" + database + "`"); require.NoError(t, e) })
	dst, err := sql.Open("dm", dmDSN)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dst.Close()) })
	schema := fmt.Sprintf("CDC_IDENT_%d", time.Now().UnixNano())
	_, err = dst.ExecContext(ctx, "CREATE SCHEMA "+quoteIdent(schema)+" AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	t.Cleanup(func() { _, e := dst.Exec("DROP SCHEMA " + quoteIdent(schema) + " CASCADE"); require.NoError(t, e) })
	cfg := Config{JobID: "identity", TargetDBType: "dameng", TargetDSN: dmDSN, TargetSchema: schema, SourceDSN: sourceCfg.FormatDSN(), SourceDatabase: database}
	a, err := newDamengApplier(ctx, cfg)
	require.NoError(t, err)
	defer func() { require.NoError(t, a.Close()) }()
	for _, tc := range []struct {
		name, typ, seed, current, actual, errorText string
	}{
		{"INITIAL", "INT", "99", "99", "99", "保守下界 99"},
		{"DELETEALL", "INT", "99", "99", "100", "保守下界 99"},
		{"LOWER", "INT", "99", "99", "99", "保守下界 99"},
		{"EQUAL", "INT", "99", "99", "100", ""},
		{"ADVANCED", "INT", "99", "100", "101", ""},
		{"ROLLEDBACK", "INT", "99", "99", "100", "保守下界 99"},
		{"TRUNCATED", "INT", "99", "99", "99", "保守下界 99"},
		{"INTLIMIT", "INT", "1", "2147483647", "", "超过"},
		{"BIGLIMIT", "BIGINT", "1", "9223372036854775807", "", "超过"},
		{"BIGEMPTY", "BIGINT", "1", "9223372036854775807", "", "超过"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := src.ExecContext(ctx, "CREATE TABLE `"+database+"`.`"+tc.name+"` (ID BIGINT AUTO_INCREMENT PRIMARY KEY) AUTO_INCREMENT=100")
			require.NoError(t, err)
			q := quoteIdent(schema) + "." + quoteIdent(tc.name)
			exec := func(query string) { t.Helper(); _, e := dst.ExecContext(ctx, query); require.NoError(t, e) }
			exec("CREATE TABLE " + q + " (ID " + tc.typ + " IDENTITY(" + tc.seed + ",1),NOTE INT)")
			switch tc.name {
			case "DELETEALL", "ADVANCED", "TRUNCATED":
				exec("INSERT INTO " + q + "(NOTE) VALUES(1)")
				if tc.name == "ADVANCED" {
					exec("INSERT INTO " + q + "(NOTE) VALUES(1)")
				}
				if tc.name == "TRUNCATED" {
					exec("TRUNCATE TABLE " + q)
				} else {
					exec("DELETE FROM " + q)
				}
			case "LOWER", "EQUAL", "INTLIMIT", "BIGLIMIT", "BIGEMPTY":
				conn, e := dst.Conn(ctx)
				require.NoError(t, e)
				value := tc.current
				if tc.name == "LOWER" {
					value = "1"
				}
				_, e = conn.ExecContext(ctx, "SET IDENTITY_INSERT "+q+" ON")
				require.NoError(t, e)
				_, e = conn.ExecContext(ctx, "INSERT INTO "+q+"(ID,NOTE) VALUES("+value+",1)")
				require.NoError(t, e)
				_, e = conn.ExecContext(ctx, "SET IDENTITY_INSERT "+q+" OFF")
				require.NoError(t, e)
				require.NoError(t, conn.Close())
				if tc.name == "BIGEMPTY" {
					exec("DELETE FROM " + q)
				}
			case "ROLLEDBACK":
				tx, e := dst.BeginTx(ctx, nil)
				require.NoError(t, e)
				_, e = tx.ExecContext(ctx, "INSERT INTO "+q+"(NOTE) VALUES(1)")
				require.NoError(t, e)
				require.NoError(t, tx.Rollback())
			}
			var beforeCurrent string
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT IDENT_CURRENT(?) FROM DUAL", q).Scan(&beforeCurrent))
			info := TableInfo{Name: tc.name, Columns: []string{"ID"}, ColumnTypes: []string{"BIGINT"}, AutoIncrement: []int{0}}
			for _, restart := range []bool{false, true} {
				if restart {
					require.NoError(t, a.Close())
					a, err = newDamengApplier(ctx, cfg)
					require.NoError(t, err)
				}
				checkErr := a.SyncSequences(ctx, []TableInfo{info})
				if tc.errorText == "" {
					require.NoError(t, checkErr)
				} else {
					require.ErrorContains(t, checkErr, tc.errorText)
				}
				var current string
				require.NoError(t, dst.QueryRowContext(ctx, "SELECT IDENT_CURRENT(?) FROM DUAL", q).Scan(&current))
				require.Equal(t, beforeCurrent, current, "read-only cutover check must not consume or reseed identity")
			}
			_, err = dst.ExecContext(ctx, "INSERT INTO "+q+"(NOTE) VALUES(2)")
			if tc.actual == "" {
				require.Error(t, err, "exhausted identity cannot generate another value")
			} else {
				require.NoError(t, err)
				var actual string
				require.NoError(t, dst.QueryRowContext(ctx, "SELECT ID FROM "+q+" WHERE NOTE=2").Scan(&actual))
				require.Equal(t, tc.actual, actual)
			}
		})
	}
}
