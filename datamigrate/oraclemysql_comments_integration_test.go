package datamigrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// Uses the same explicit, dedicated MySQL 5.7/8.0 opt-in as the other
// Oracle -> MySQL target-only live regressions. Source comments remain untouched.
func TestOracleMySQLLiveColumnCommentsMigration(t *testing.T) {
	for _, content := range []string{"schema_only", "both"} {
		t.Run(content, func(t *testing.T) {
			ctx, db, w, schema, prefix := oracleMySQLLiveTarget(t)
			table := prefix + "_comments"
			oracleMySQLLiveCleanup(t, db, schema, []string{table})
			quoted := "中文 'quoted' \\path\nnext line"
			long := strings.Repeat("中", 1023) + "尾末"
			r := &oracleMySQLLiveReader{
				tables: []source.TableDDLInfo{{TableName: table, Columns: []source.ColumnInfo{
					{Name: "Id", DataType: "NUMBER", Precision: 9},
					{Name: "Label", DataType: "VARCHAR2", Length: 80, IsNullable: true},
				}}},
				keys: []source.IndexInfo{{TableName: table, IndexName: "PRIMARY", Columns: []string{"Id"}, IsPrimary: true, IsUnique: true}},
				data: map[string][][]any{table: {{int64(1), "原始数据"}, {int64(2), nil}}},
				comments: []source.CommentInfo{
					{TableName: table, Comment: "表 '注释' \\path\nnext line"},
					{TableName: table, ColumnName: "Id", Comment: quoted},
					{TableName: table, ColumnName: "Label", Comment: long},
				},
			}
			report := oracleMySQLLiveRun(t, ctx, w, schema, r, content)
			require.Equal(t, 3, report.Comments.Total)
			require.Equal(t, 3, report.Comments.Success)
			require.Equal(t, 0, report.Comments.Failed)
			require.Equal(t, quoted, oracleMySQLLiveColumnComment(t, ctx, db, schema, table, "Id"))
			require.Equal(t, strings.Repeat("中", 1023)+"尾", oracleMySQLLiveColumnComment(t, ctx, db, schema, table, "Label"))
			require.Equal(t, long, r.comments[2].Comment, "target truncation must not mutate source metadata")
			var tableComment string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT TABLE_COMMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?`, schema, table).Scan(&tableComment))
			require.Equal(t, r.comments[0].Comment, tableComment)
			var count int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+oracleMySQLLiveQualified(schema, table)).Scan(&count))
			if content == "both" {
				require.Equal(t, 2, count)
				var label string
				require.NoError(t, db.QueryRowContext(ctx, "SELECT `Label` FROM "+oracleMySQLLiveQualified(schema, table)+" WHERE `Id`=1").Scan(&label))
				require.Equal(t, "原始数据", label)
			} else {
				require.Zero(t, count)
			}
			truncations := 0
			for _, adjustment := range report.AutoRepairs {
				if adjustment.Type == "oracle_mysql_comment_truncate" {
					truncations++
					require.Equal(t, table, adjustment.Table)
					require.Equal(t, "Label", adjustment.Source)
					require.Equal(t, "Label", adjustment.Target)
					require.Equal(t, "applied", adjustment.Status)
				}
			}
			require.Equal(t, 1, truncations)
			require.NotNil(t, report.RepairManifest)
			comments := 0
			for _, object := range report.RepairManifest.Objects {
				if object.Kind != "comment" {
					continue
				}
				comments++
				require.Equal(t, "succeeded", object.Status)
				require.Len(t, object.Units, 1)
				require.Equal(t, "succeeded", object.Units[0].Status)
				require.NotEmpty(t, object.Units[0].SQL)
				if object.Comment.ColumnName == "Label" {
					require.Equal(t, strings.Repeat("中", 1023)+"尾", object.Comment.Comment)
				}
			}
			require.Equal(t, 3, comments)
		})
	}
}

func TestOracleMySQLLiveColumnCommentsOnlyPreservesColumnsAndData(t *testing.T) {
	ctx, db, w, schema, prefix := oracleMySQLLiveTarget(t)
	table := prefix + "_existing_comments"
	oracleMySQLLiveCleanup(t, db, schema, []string{table})
	qualified := oracleMySQLLiveQualified(schema, table)
	_, err := db.ExecContext(ctx, "CREATE TABLE "+qualified+" ("+
		"`Id` bigint unsigned NOT NULL AUTO_INCREMENT COMMENT 'old identity',"+
		"`Label` varchar(80) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'a,b' COMMENT 'old label',"+
		"`Optional` varchar(30) NULL DEFAULT NULL COMMENT 'keep me',"+
		"`CreatedAt` timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT 'old time',"+
		"`Computed` varchar(100) GENERATED ALWAYS AS (concat(`Label`, ',tail')) STORED COMMENT 'old computed',"+
		"PRIMARY KEY (`Id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	require.NoError(t, err)
	value := "原始值 ' \\ \n"
	_, err = db.ExecContext(ctx, "INSERT INTO "+qualified+" (`Label`,`Optional`,`CreatedAt`) VALUES (?,NULL,'2025-01-02 03:04:05')", value)
	require.NoError(t, err)
	beforeColumns := oracleMySQLLiveColumnAttributes(t, ctx, db, schema, table)
	beforeData := oracleMySQLLiveCommentFixtureData(t, ctx, db, qualified)
	long := strings.Repeat("列", 1025)
	r := &oracleMySQLLiveReader{
		tables: []source.TableDDLInfo{{TableName: table, Columns: []source.ColumnInfo{
			{Name: "Id", DataType: "NUMBER", Precision: 18},
			{Name: "Label", DataType: "VARCHAR2", Length: 80},
			{Name: "Optional", DataType: "VARCHAR2", Length: 30, IsNullable: true},
			{Name: "CreatedAt", DataType: "TIMESTAMP"},
			{Name: "Computed", DataType: "VARCHAR2", Length: 100},
		}}},
		comments: []source.CommentInfo{
			{TableName: table, ColumnName: "Id", Comment: "新自增注释"},
			{TableName: table, ColumnName: "Label", Comment: "新 '引号' \\path\n换行"},
			{TableName: table, ColumnName: "CreatedAt", Comment: "更新时间"},
			{TableName: table, ColumnName: "Computed", Comment: long},
		},
	}
	job := &Job{LogCh: make(chan string, 4096)}
	m := NewMigrator(r, w, job, Config{TargetDBType: "mysql", TargetSchema: schema, Objects: []string{"comments"}, TableNames: []string{table}, CaptureRepair: true})
	report := m.Run(ctx)
	require.False(t, report.HasFailures(), "report=%+v logs=%v", report, drainLogs(job))
	require.Equal(t, 4, report.Comments.Success)
	require.Equal(t, beforeColumns, oracleMySQLLiveColumnAttributes(t, ctx, db, schema, table), "only comments may change")
	require.Equal(t, beforeData, oracleMySQLLiveCommentFixtureData(t, ctx, db, qualified), "existing rows must remain unchanged")
	for _, comment := range r.comments {
		expected := comment.Comment
		if comment.ColumnName == "Computed" {
			expected = strings.Repeat("列", 1024)
		}
		require.Equal(t, expected, oracleMySQLLiveColumnComment(t, ctx, db, schema, table, comment.ColumnName))
	}
	require.Equal(t, "keep me", oracleMySQLLiveColumnComment(t, ctx, db, schema, table, "Optional"), "missing source comments must not clear existing comments")
	_, err = db.ExecContext(ctx, "INSERT INTO "+qualified+" (`Label`) VALUES ('next')")
	require.NoError(t, err, "AUTO_INCREMENT and defaults must remain operational")
	var id uint64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT `Id` FROM "+qualified+" WHERE `Label`='next'").Scan(&id))
	require.Equal(t, uint64(2), id)
}

func oracleMySQLLiveColumnComment(t *testing.T, ctx context.Context, db *sql.DB, schema, table, column string) string {
	t.Helper()
	var comment string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COLUMN_COMMENT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME=?`, schema, table, column).Scan(&comment))
	return comment
}

func oracleMySQLLiveColumnAttributes(t *testing.T, ctx context.Context, db *sql.DB, schema, table string) [][]sql.NullString {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT COLUMN_NAME,COLUMN_TYPE,IS_NULLABLE,COLUMN_DEFAULT,EXTRA,CHARACTER_SET_NAME,COLLATION_NAME,GENERATION_EXPRESSION,ORDINAL_POSITION FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION`, schema, table)
	require.NoError(t, err)
	defer rows.Close()
	var columns [][]sql.NullString
	for rows.Next() {
		values := make([]sql.NullString, 9)
		args := make([]any, len(values))
		for i := range values {
			args[i] = &values[i]
		}
		require.NoError(t, rows.Scan(args...))
		columns = append(columns, values)
	}
	require.NoError(t, rows.Err())
	return columns
}

func oracleMySQLLiveCommentFixtureData(t *testing.T, ctx context.Context, db *sql.DB, qualified string) []sql.NullString {
	t.Helper()
	values := make([]sql.NullString, 5)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT CAST(`Id` AS CHAR),`Label`,`Optional`,DATE_FORMAT(`CreatedAt`,'%Y-%m-%d %H:%i:%s'),`Computed` FROM "+qualified+" WHERE `Id`=1").Scan(&values[0], &values[1], &values[2], &values[3], &values[4]))
	return values
}
