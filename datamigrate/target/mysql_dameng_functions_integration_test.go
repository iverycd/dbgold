package target

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"gitee.com/chunanyong/dm"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Fixed business names require disposable schemas. Refuse to touch any of the
// fixture names if they already exist; cleanup is armed only after this check.
func TestMySQLDamengBusinessFunctionsIntegration(t *testing.T) {
	srcDSN, dstDSN, schema := os.Getenv("MYSQL_DM_TEST_SOURCE_DSN"), os.Getenv("MYSQL_DM_TEST_TARGET_DSN"), os.Getenv("MYSQL_DM_TEST_SCHEMA")
	if os.Getenv("MYSQL_DM_FUNCTION_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" || schema == "" {
		t.Skip("实库未验证：需要独立 MySQL/达梦测试 schema、MYSQL_DM_TEST_SOURCE_DSN / MYSQL_DM_TEST_TARGET_DSN / MYSQL_DM_TEST_SCHEMA 和 MYSQL_DM_FUNCTION_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg, err := mysql.ParseDSN(srcDSN)
	require.NoError(t, err)
	require.NotEmpty(t, cfg.DBName)
	src, err := sql.Open("mysql", srcDSN)
	require.NoError(t, err)
	defer src.Close()
	src.SetMaxOpenConns(1) // Keep the reference GROUP_CONCAT limit on one session.
	w, err := NewDaMeng(dstDSN, schema, ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	sq := func(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }
	functions := dmBusinessFunctions()
	tables := []struct{ name, columns string }{
		{"frame_ou", "ouguid VARCHAR(50), parentouguid VARCHAR(50), ouname VARCHAR(50)"},
		{"frame_user", "userguid VARCHAR(50), ouguid VARCHAR(50), displayname VARCHAR(50)"},
		{"code_main", "codeid INT, codename VARCHAR(50)"},
		{"code_items", "codeid INT, itemvalue VARCHAR(50), itemtext VARCHAR(50)"},
	}
	for _, table := range tables {
		var n int
		require.NoError(t, src.QueryRowContext(ctx, "SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?", cfg.DBName, table.name).Scan(&n))
		require.Zero(t, n, "source fixture table already exists: %s", table.name)
		require.NoError(t, w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=?", w.schema, strings.ToUpper(table.name)).Scan(&n))
		require.Zero(t, n, "target fixture name already exists: %s", table.name)
	}
	for _, spec := range functions {
		var n int
		if spec.name != "FUN_GETPY" { // This supplied DM routine has no MySQL reference.
			require.NoError(t, src.QueryRowContext(ctx, "SELECT COUNT(*) FROM INFORMATION_SCHEMA.ROUTINES WHERE ROUTINE_SCHEMA=? AND ROUTINE_NAME=?", cfg.DBName, strings.ToLower(spec.name)).Scan(&n))
			require.Zero(t, n, "source routine already exists: %s", spec.name)
		}
		require.NoError(t, w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=?", w.schema, spec.name).Scan(&n))
		require.Zero(t, n, "target routine already exists: %s", spec.name)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		for _, spec := range functions {
			if spec.name != "FUN_GETPY" {
				_, _ = src.ExecContext(cleanup, "DROP FUNCTION IF EXISTS "+sq(strings.ToLower(spec.name)))
			}
			_, _ = w.db.ExecContext(cleanup, "DROP FUNCTION "+w.qualifiedTable(spec.name))
		}
		for _, table := range tables {
			_, _ = src.ExecContext(cleanup, "DROP TABLE IF EXISTS "+sq(table.name))
			_, _ = w.db.ExecContext(cleanup, "DROP TABLE "+w.qualifiedTable(table.name))
		}
	}()
	exec := func(db *sql.DB, query string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, query, args...)
		require.NoError(t, err, query)
	}
	for _, table := range tables {
		exec(src, "CREATE TABLE "+sq(table.name)+" ("+table.columns+") CHARACTER SET utf8mb4")
		exec(w.db, "CREATE TABLE "+w.qualifiedTable(table.name)+" ("+strings.ToUpper(table.columns)+")")
	}
	insert := func(table string, args ...any) {
		t.Helper()
		marks := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
		exec(src, "INSERT INTO "+sq(table)+" VALUES ("+marks+")", args...)
		exec(w.db, "INSERT INTO "+w.qualifiedTable(table)+" VALUES ("+marks+")", args...)
	}
	insert("frame_ou", "root", nil, "总部")
	insert("frame_ou", "a", "root", "部门甲")
	insert("frame_ou", "b", "root", "部门乙")
	insert("frame_ou", "a1", "a", "小组甲")
	insert("frame_user", "u1", "a", "张三")
	insert("frame_user", "u2", "b", "李四")
	insert("code_main", 1, "涉及问题")
	insert("code_main", 2, "颜色")
	insert("code_items", 1, "q1", "问题甲")
	insert("code_items", 2, "red", "红色")

	// An unrelated same-name implementation must survive, even with another signature.
	exec(w.db, "CREATE FUNCTION "+w.qualifiedTable("GETITEMTEXT")+" RETURN VARCHAR(50) AS BEGIN RETURN 'existing marker'; END;")
	report, err := w.EnsureMySQLDamengBusinessFunctions(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 6, report.Created)
	require.Equal(t, 1, report.Reused)
	var marker string
	require.NoError(t, w.db.QueryRowContext(ctx, "SELECT "+w.qualifiedTable("GETITEMTEXT")+"() FROM DUAL").Scan(&marker))
	require.Equal(t, "existing marker", marker)
	exec(w.db, "DROP FUNCTION "+w.qualifiedTable("GETITEMTEXT"))
	report, err = w.EnsureMySQLDamengBusinessFunctions(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Created)
	require.Equal(t, 6, report.Reused)

	// Reference implementations preserve the screenshot's SELECT INTO behavior.
	lookup := func(query string) string {
		return "RETURNS VARCHAR(50) READS SQL DATA BEGIN DECLARE v_result VARCHAR(50); " + query + "; RETURN v_result; END"
	}
	refs := []struct{ name, params, body string }{
		{"getitemtext", "p_question VARCHAR(50), p_codename VARCHAR(50)", lookup("SELECT b.itemtext INTO v_result FROM code_main a JOIN code_items b ON a.codeid=b.codeid WHERE a.codename=p_codename AND b.itemvalue=p_question")},
		{"getounamebyuserguid", "p_userguid VARCHAR(50)", lookup("SELECT a.ouname INTO v_result FROM frame_ou a JOIN frame_user b ON a.ouguid=b.ouguid WHERE b.userguid=p_userguid")},
		{"getquestiontext", "p_question VARCHAR(50)", lookup("SELECT b.itemtext INTO v_result FROM code_main a JOIN code_items b ON a.codeid=b.codeid WHERE a.codename='涉及问题' AND b.itemvalue=p_question")},
		{"getusernamebyuserguid", "p_userguid VARCHAR(50)", lookup("SELECT displayname INTO v_result FROM frame_user WHERE userguid=p_userguid")},
		{"selectallusercount", "", "RETURNS VARCHAR(100) READS SQL DATA BEGIN DECLARE n INT; SELECT COUNT(*) INTO n FROM frame_user; RETURN CONCAT('mysql count :',n); END"},
		{"getchildoulist", "p_rootid VARCHAR(50)", `RETURNS LONGTEXT READS SQL DATA BEGIN
DECLARE v_result LONGTEXT; DECLARE v_frontier LONGTEXT;
SET v_result='$'; SET v_frontier=p_rootid;
WHILE v_frontier IS NOT NULL DO
SET v_result=CONCAT(v_result,',',v_frontier);
SELECT GROUP_CONCAT(ouguid) INTO v_frontier FROM frame_ou WHERE FIND_IN_SET(parentouguid,v_frontier)>0;
END WHILE;
RETURN SUBSTRING(v_result,3); END`},
	}
	for _, ref := range refs {
		exec(src, "CREATE FUNCTION "+sq(ref.name)+"("+ref.params+") "+ref.body)
	}
	exec(src, "SET SESSION group_concat_max_len=102400")
	read := func(db *sql.DB, query string, args ...any) sql.NullString {
		t.Helper()
		// Keep rows open while dereferencing a driver-backed CLOB locator.
		rows, err := db.QueryContext(ctx, query, args...)
		require.NoError(t, err)
		defer rows.Close()
		require.True(t, rows.Next())
		var raw any
		require.NoError(t, rows.Scan(&raw))
		var result sql.NullString
		if clob, ok := raw.(*dm.DmClob); ok {
			if clob != nil && clob.Valid {
				n, e := clob.GetLength()
				require.NoError(t, e)
				result.Valid = true
				if n > 0 {
					result.String, e = clob.ReadString(1, int(n))
					require.NoError(t, e)
				}
			}
		} else {
			require.NoError(t, result.Scan(raw))
		}
		require.False(t, rows.Next())
		require.NoError(t, rows.Err())
		return result
	}
	compare := func(name string, args ...any) sql.NullString {
		t.Helper()
		marks := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
		a := read(src, "SELECT "+sq(name)+"("+marks+")", args...)
		b := read(w.db, "SELECT "+w.qualifiedTable(name)+"("+marks+") FROM DUAL", args...)
		if name == "getchildoulist" {
			left, right := strings.Split(a.String, ","), strings.Split(b.String, ",")
			sort.Strings(left)
			sort.Strings(right)
			require.Equal(t, left, right) // MySQL deliberately has no sibling ordering.
		} else {
			require.Equal(t, a, b)
		}
		return b
	}
	require.Equal(t, "红色", compare("getitemtext", "red", "颜色").String)
	require.Equal(t, "问题甲", compare("getquestiontext", "q1").String)
	require.Equal(t, "部门甲", compare("getounamebyuserguid", "u1").String)
	require.Equal(t, "张三", compare("getusernamebyuserguid", "u1").String)
	require.Equal(t, "mysql count :2", compare("selectallusercount").String)
	for _, name := range []string{"getquestiontext", "getounamebyuserguid", "getusernamebyuserguid"} {
		require.False(t, compare(name, "missing").Valid)
		require.False(t, compare(name, nil).Valid)
	}
	require.False(t, compare("getitemtext", "missing", "颜色").Valid)
	require.False(t, compare("getitemtext", nil, nil).Valid)
	require.Equal(t, "root,a,b,a1", compare("getchildoulist", "root").String)
	compare("getchildoulist", "a,b")
	compare("getchildoulist", "root,a") // Overlapping roots are not a cycle.
	compare("getchildoulist", "missing")
	// Empty values follow DM's configured empty-string mode.
	for _, value := range []any{nil, ""} {
		got := read(w.db, "SELECT "+w.qualifiedTable("GETCHILDOULIST")+"(?) FROM DUAL", value)
		require.Empty(t, got.String)
	}
	insert("code_items", 1, "q1", "重复问题")
	for _, name := range []string{"getitemtext", "getquestiontext"} {
		args, marks := []any{"q1"}, "?"
		if name == "getitemtext" {
			args, marks = []any{"q1", "涉及问题"}, "?,?"
		}
		var value any
		require.Error(t, src.QueryRowContext(ctx, "SELECT "+sq(name)+"("+marks+")", args...).Scan(&value))
		require.Error(t, w.db.QueryRowContext(ctx, "SELECT "+w.qualifiedTable(name)+"("+marks+") FROM DUAL", args...).Scan(&value))
	}
	insert("frame_user", "u1", "a", "重复用户")
	for _, name := range []string{"getounamebyuserguid", "getusernamebyuserguid"} {
		var value any
		require.Error(t, src.QueryRowContext(ctx, "SELECT "+sq(name)+"('u1')").Scan(&value))
		require.Error(t, w.db.QueryRowContext(ctx, "SELECT "+w.qualifiedTable(name)+"('u1') FROM DUAL").Scan(&value))
	}
	// Both the output and a frontier exceed VARCHAR's 32KB limit. A grandchild
	// of the last sibling exercises membership beyond the first 32KB as well.
	var last string
	for i := 0; i < 1000; i++ {
		last = fmt.Sprintf("wide_%04d_%s", i, strings.Repeat("x", 30))
		insert("frame_ou", last, "wide", "长列表")
	}
	insert("frame_ou", "tail_child", last, "尾部子节点")
	large := compare("getchildoulist", "wide").String
	require.Greater(t, len(large), 32767)
	require.True(t, strings.HasSuffix(large, ",tail_child"))
	// Never execute the unbounded MySQL reference on cyclic input.
	insert("frame_ou", "cycle_a", "cycle_b", "环甲")
	insert("frame_ou", "cycle_b", "cycle_a", "环乙")
	var value any
	err = w.db.QueryRowContext(ctx, "SELECT "+w.qualifiedTable("GETCHILDOULIST")+"('cycle_a') FROM DUAL").Scan(&value)
	require.ErrorContains(t, err, "cycle detected")
	report, err = w.EnsureMySQLDamengBusinessFunctions(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 7, report.Reused)
	require.Zero(t, report.Created)
	for _, item := range report.Items {
		require.Equal(t, "已存在，跳过，未校验定义", item.DefinitionWarning)
	}
}
