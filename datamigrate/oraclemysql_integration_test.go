package datamigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// This target-only integration uses original Oracle metadata and values with a
// real MySQL writer. Run separately on dedicated MySQL 5.7 and 8.0 databases:
// DBGOLD_ORACLE_MYSQL_TEST_DSN and DBGOLD_ORACLE_MYSQL_TEST_ALLOW_DDL=1.
// Only uniquely named fixture tables are changed; no database is created and
// neither global nor session foreign-key checking is disabled.
func oracleMySQLLiveTarget(t *testing.T) (context.Context, *sql.DB, *target.MySQLWriter, string, string) {
	t.Helper()
	raw := os.Getenv("DBGOLD_ORACLE_MYSQL_TEST_DSN")
	if raw == "" || os.Getenv("DBGOLD_ORACLE_MYSQL_TEST_ALLOW_DDL") != "1" {
		t.Skip("requires dedicated DBGOLD_ORACLE_MYSQL_TEST_DSN and DBGOLD_ORACLE_MYSQL_TEST_ALLOW_DDL=1")
	}
	config, err := mysql.ParseDSN(raw)
	require.NoError(t, err, "invalid dedicated MySQL test DSN")
	require.True(t, strings.HasPrefix(config.DBName, "DBGOLD_") || strings.HasPrefix(config.DBName, "dbgold_"), "integration database must start with DBGOLD_ or dbgold_")
	if config.Params == nil {
		config.Params = map[string]string{}
	}
	config.Params["charset"] = "utf8mb4"
	config.ParseTime = true
	config.MultiStatements = false
	dsn := config.FormatDSN()
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	require.NoError(t, db.PingContext(ctx))
	var schema string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&schema))
	require.True(t, strings.HasPrefix(schema, "DBGOLD_") || strings.HasPrefix(schema, "dbgold_"), "actual integration database must start with DBGOLD_ or dbgold_")
	require.True(t, strings.EqualFold(config.DBName, schema), "actual database must match the dedicated test DSN")
	w, err := target.NewMySQL(dsn, schema, target.ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	version, err := w.OracleMySQLVersion(ctx)
	require.NoError(t, err)
	require.True(t, version.Major == 5 && version.Minor == 7 || version.Major == 8 && version.Minor == 0, "live regression requires MySQL 5.7 or 8.0")
	return ctx, db, w, schema, fmt.Sprintf("om_%x", time.Now().UnixNano())
}

func oracleMySQLLiveQuote(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func oracleMySQLLiveQualified(schema, table string) string {
	return oracleMySQLLiveQuote(schema) + "." + oracleMySQLLiveQuote(table)
}

func oracleMySQLLiveCleanup(t *testing.T, db *sql.DB, schema string, tables []string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Only foreign keys owned by these exact unique fixture tables are
		// removed, so cyclic fixtures can be cleaned with checking still on.
		for _, table := range tables {
			rows, err := db.QueryContext(ctx, `SELECT CONSTRAINT_NAME FROM information_schema.TABLE_CONSTRAINTS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_TYPE='FOREIGN KEY'`, schema, table)
			if err != nil {
				t.Errorf("read fixture cleanup constraints: %v", err)
				continue
			}
			var constraints []string
			for rows.Next() {
				var name string
				if err = rows.Scan(&name); err != nil {
					t.Errorf("read fixture cleanup constraint: %v", err)
					break
				}
				constraints = append(constraints, name)
			}
			if err = rows.Err(); err != nil {
				t.Errorf("read fixture cleanup constraints: %v", err)
			}
			_ = rows.Close()
			for _, constraint := range constraints {
				if _, err = db.ExecContext(ctx, "ALTER TABLE "+oracleMySQLLiveQualified(schema, table)+" DROP FOREIGN KEY "+oracleMySQLLiveQuote(constraint)); err != nil {
					t.Errorf("remove fixture constraint: %v", err)
				}
			}
		}
		for i := len(tables) - 1; i >= 0; i-- {
			if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+oracleMySQLLiveQualified(schema, tables[i])); err != nil {
				t.Errorf("drop fixture table: %v", err)
			}
		}
	})
}

type oracleMySQLLiveReader struct {
	source.Reader
	tables   []source.TableDDLInfo
	keys     []source.IndexInfo
	fks      []source.FKInfo
	data     map[string][][]any
	comments []source.CommentInfo
}

func (*oracleMySQLLiveReader) DBType() string          { return "oracle" }
func (*oracleMySQLLiveReader) MigrationSchema() string { return "APP" }
func (*oracleMySQLLiveReader) Close() error            { return nil }
func (r *oracleMySQLLiveReader) ListTables(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result []string
	for _, info := range r.tables {
		result = append(result, info.TableName)
	}
	return result, nil
}
func (r *oracleMySQLLiveReader) GetTableDDLInfo(ctx context.Context, table string) (*source.TableDDLInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, info := range r.tables {
		if info.TableName == table {
			return cloneSourceTableDDLInfo(&info), nil
		}
	}
	return nil, fmt.Errorf("unknown fixture table %s", table)
}
func (r *oracleMySQLLiveReader) GetPrimaryKey(_ context.Context, table string) ([]string, error) {
	for _, key := range r.keys {
		if key.TableName == table {
			return slices.Clone(key.Columns), nil
		}
	}
	return nil, nil
}
func (r *oracleMySQLLiveReader) GetPrimaryKeys(context.Context) ([]source.IndexInfo, error) {
	return slices.Clone(r.keys), nil
}
func (*oracleMySQLLiveReader) GetIndexes(context.Context) ([]source.IndexInfo, error) {
	return nil, nil
}
func (r *oracleMySQLLiveReader) GetForeignKeys(context.Context) ([]source.FKInfo, error) {
	return slices.Clone(r.fks), nil
}
func (*oracleMySQLLiveReader) GetSequences(context.Context) ([]source.SequenceInfo, error) {
	return nil, nil
}
func (*oracleMySQLLiveReader) GetViews(context.Context) ([]source.ViewInfo, error) { return nil, nil }
func (r *oracleMySQLLiveReader) GetComments(context.Context) ([]source.CommentInfo, error) {
	return slices.Clone(r.comments), nil
}
func (*oracleMySQLLiveReader) GetTriggerCount(context.Context) (int, error) { return 0, nil }
func (*oracleMySQLLiveReader) ListDatabases(context.Context) ([]string, error) {
	return []string{"APP"}, nil
}
func (r *oracleMySQLLiveReader) CountRows(_ context.Context, table string) (int64, error) {
	return int64(len(r.data[table])), nil
}
func (r *oracleMySQLLiveReader) ReadPage(ctx context.Context, table string, _ []string, offset, limit int64) ([]string, []string, [][]any, error) {
	info, err := r.GetTableDDLInfo(ctx, table)
	if err != nil {
		return nil, nil, nil, err
	}
	var columns, types []string
	for _, column := range info.Columns {
		columns = append(columns, column.Name)
		types = append(types, column.DataType)
	}
	if offset < 0 || limit <= 0 {
		return nil, nil, nil, fmt.Errorf("invalid fixture page")
	}
	data := r.data[table]
	if offset >= int64(len(data)) {
		return columns, types, nil, nil
	}
	return columns, types, data[offset:min(offset+limit, int64(len(data)))], nil
}

func oracleMySQLLiveRun(t *testing.T, ctx context.Context, w *target.MySQLWriter, schema string, r *oracleMySQLLiveReader, content string) MigrationReport {
	t.Helper()
	names, err := r.ListTables(ctx)
	require.NoError(t, err)
	job := &Job{LogCh: make(chan string, 4096)}
	m := NewMigrator(r, w, job, Config{
		TargetDBType: "mysql", TargetSchema: schema, Mode: "include", Filter: strings.Join(names, ","),
		Content: content, CaptureRepair: true, PageSize: 1, MaxParallel: 2,
	})
	report := m.Run(ctx)
	require.False(t, report.HasFailures(), "report=%+v logs=%v", report, drainLogs(job))
	return report
}

func TestOracleMySQLLiveForeignKeyRebuild(t *testing.T) {
	for _, shape := range []string{"parent_child", "cycle", "self_reference"} {
		t.Run(shape, func(t *testing.T) {
			ctx, db, w, schema, prefix := oracleMySQLLiveTarget(t)
			parent, child := prefix+"_a_parent", prefix+"_b_child"
			if shape == "self_reference" {
				child = parent
			}
			names := []string{parent}
			if child != parent {
				names = append(names, child)
			}
			oracleMySQLLiveCleanup(t, db, schema, names)
			r := &oracleMySQLLiveReader{data: map[string][][]any{}}
			for _, name := range names {
				_, err := db.ExecContext(ctx, "CREATE TABLE "+oracleMySQLLiveQualified(schema, name)+" (`ID` INT NOT NULL PRIMARY KEY, `PARENT_ID` INT NULL) ENGINE=InnoDB")
				require.NoError(t, err)
				r.tables = append(r.tables, source.TableDDLInfo{TableName: name, Columns: []source.ColumnInfo{
					{Name: "ID", DataType: "NUMBER", Precision: 9}, {Name: "PARENT_ID", DataType: "NUMBER", Precision: 9, IsNullable: true},
				}})
				r.keys = append(r.keys, source.IndexInfo{TableName: name, IndexName: "PRIMARY", Columns: []string{"ID"}, IsPrimary: true, IsUnique: true})
			}
			type edge struct{ child, parent, suffix string }
			edges := []edge{{child, parent, "child"}}
			if shape == "cycle" {
				edges = append(edges, edge{parent, child, "parent"})
			}
			for _, e := range edges {
				_, err := db.ExecContext(ctx, "ALTER TABLE "+oracleMySQLLiveQualified(schema, e.child)+" ADD CONSTRAINT "+oracleMySQLLiveQuote(prefix+"_old_"+e.suffix)+" FOREIGN KEY (`PARENT_ID`) REFERENCES "+oracleMySQLLiveQualified(schema, e.parent)+" (`ID`) ON DELETE CASCADE ON UPDATE RESTRICT")
				require.NoError(t, err)
				r.fks = append(r.fks, source.FKInfo{TableName: e.child, ConstraintName: prefix + "_new_" + e.suffix, Columns: []string{"PARENT_ID"}, RefSchema: "APP", RefTable: e.parent, RefColumns: []string{"ID"}, OnDelete: "CASCADE", OnUpdate: "NO ACTION"})
			}
			if shape != "self_reference" {
				_, err := db.ExecContext(ctx, "DROP TABLE "+oracleMySQLLiveQualified(schema, parent))
				var fkError *mysql.MySQLError
				require.True(t, errors.As(err, &fkError), "original target DROP must reproduce a MySQL foreign-key error")
				require.Contains(t, []uint16{1217, 1451, 3730}, fkError.Number)
			}
			report := oracleMySQLLiveRun(t, ctx, w, schema, r, "schema_only")
			require.Equal(t, len(names), report.Tables.Success)
			require.Equal(t, len(edges), report.Constraints.Success, "include mode must recreate source foreign keys")
			require.Len(t, report.TargetDependencies, len(edges))
			for _, result := range report.TargetDependencies {
				require.Equal(t, "succeeded", result.DropStatus)
			}
			actual, err := w.OracleMySQLForeignKeys(ctx)
			require.NoError(t, err)
			var fixtureFKs []target.MySQLForeignKey
			for _, fk := range actual {
				if fk.Schema == schema && slices.Contains(names, fk.Table) {
					fixtureFKs = append(fixtureFKs, fk)
					require.True(t, strings.HasPrefix(fk.ConstraintName, prefix+"_new_"))
				}
			}
			require.Len(t, fixtureFKs, len(edges))
			var checking int
			require.NoError(t, w.InspectionDB().QueryRowContext(ctx, `SELECT @@FOREIGN_KEY_CHECKS`).Scan(&checking))
			require.Equal(t, 1, checking)
		})
	}
}

func TestOracleMySQLLiveDefaultsAndWideRows(t *testing.T) {
	ctx, db, w, schema, prefix := oracleMySQLLiveTarget(t)
	class, wide, bad := prefix+"_class", prefix+"_wide", prefix+"_bad_default"
	oracleMySQLLiveCleanup(t, db, schema, []string{class, wide, bad})
	_, err := db.ExecContext(ctx, "CREATE TABLE "+oracleMySQLLiveQualified(schema, bad)+" (`appclassguid` varchar(50) NOT NULL, `appclassname` varchar(50), `ordernumber` double NOT NULL DEFAULT NULL, `tenantguid` varchar(50)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	var originalError *mysql.MySQLError
	require.True(t, errors.As(err, &originalError), "original NOT NULL DEFAULT NULL must fail on MySQL")
	require.Equal(t, uint16(1067), originalError.Number)
	nullDefault := "NULL"
	r := &oracleMySQLLiveReader{tables: []source.TableDDLInfo{{TableName: class, Columns: []source.ColumnInfo{
		{Name: "appclassguid", DataType: "VARCHAR2", Length: 50},
		{Name: "appclassname", DataType: "VARCHAR2", Length: 50, IsNullable: true},
		{Name: "ordernumber", DataType: "NUMBER", Default: &nullDefault},
		{Name: "tenantguid", DataType: "VARCHAR2", Length: 50, IsNullable: true},
	}}}, data: map[string][][]any{class: {{"one", "名称", float64(1), "tenant"}, {"two", nil, float64(-2.5), nil}}}}
	wideInfo := source.TableDDLInfo{TableName: wide, Columns: []source.ColumnInfo{{Name: "ID", DataType: "NUMBER", Precision: 9}}}
	value := strings.Repeat("中😀", 1000) // Exactly 2000 source characters.
	first, second := []any{int64(1)}, []any{int64(2)}
	for i := 0; i < 12; i++ {
		wideInfo.Columns = append(wideInfo.Columns, source.ColumnInfo{Name: fmt.Sprintf("payload_%02d", i), DataType: "VARCHAR2", Length: 2000, IsNullable: true})
		first, second = append(first, value), append(second, nil)
	}
	originalDDL, err := dialect.NewMySQL().CreateTableStatements(schema, &wideInfo, "oracle", dialect.TypeOpt{}, func(s string) string { return s })
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, originalDDL[1].SQL)
	require.True(t, errors.As(err, &originalError), "original wide VARCHAR declaration must fail on MySQL")
	require.Equal(t, uint16(1118), originalError.Number)
	r.tables = append(r.tables, wideInfo)
	r.keys = []source.IndexInfo{
		{TableName: class, IndexName: "PRIMARY", Columns: []string{"appclassguid"}, IsPrimary: true, IsUnique: true},
		{TableName: wide, IndexName: "PRIMARY", Columns: []string{"ID"}, IsPrimary: true, IsUnique: true},
	}
	r.data[wide] = [][]any{first, second}
	report := oracleMySQLLiveRun(t, ctx, w, schema, r, "both")
	require.Equal(t, 2, report.Data.Success)
	require.Len(t, report.RowCounts, 2)
	for _, count := range report.RowCounts {
		require.True(t, count.Match)
		require.Equal(t, int64(2), count.Dst)
	}
	var nullable, dataType string
	var def sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `SELECT DATA_TYPE,IS_NULLABLE,COLUMN_DEFAULT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME='ordernumber'`, schema, class).Scan(&dataType, &nullable, &def))
	require.Equal(t, "double", dataType)
	require.Equal(t, "NO", nullable)
	require.False(t, def.Valid)
	var number float64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT `ordernumber` FROM "+oracleMySQLLiveQualified(schema, class)+" WHERE `appclassguid`='two'").Scan(&number))
	require.Equal(t, -2.5, number)
	var columns []string
	for _, column := range wideInfo.Columns[1:] {
		columns = append(columns, oracleMySQLLiveQuote(column.Name))
	}
	back := make([]sql.NullString, len(columns))
	args := make([]any, len(columns))
	for i := range back {
		args[i] = &back[i]
	}
	require.NoError(t, db.QueryRowContext(ctx, "SELECT "+strings.Join(columns, ",")+" FROM "+oracleMySQLLiveQualified(schema, wide)+" WHERE `ID`=1").Scan(args...))
	for _, cell := range back {
		require.True(t, cell.Valid)
		require.Equal(t, value, cell.String)
	}
	require.NoError(t, db.QueryRowContext(ctx, "SELECT "+strings.Join(columns, ",")+" FROM "+oracleMySQLLiveQualified(schema, wide)+" WHERE `ID`=2").Scan(args...))
	for _, cell := range back {
		require.False(t, cell.Valid)
	}
	rows, err := db.QueryContext(ctx, `SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND DATA_TYPE='longtext' ORDER BY ORDINAL_POSITION`, schema, wide)
	require.NoError(t, err)
	var promoted []string
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		promoted = append(promoted, column)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.NotEmpty(t, promoted)
	require.Less(t, len(promoted), len(columns))
	require.NotEmpty(t, report.AutoRepairs)
	require.NotNil(t, report.RepairManifest)
	for _, object := range report.RepairManifest.Objects {
		if object.Kind != "table" || object.Table != wide {
			continue
		}
		for _, column := range object.Columns {
			if slices.Contains(promoted, column.Name) {
				require.Equal(t, "longtext", strings.ToLower(column.DataType))
			}
		}
	}
	version, err := w.OracleMySQLVersion(ctx)
	require.NoError(t, err)
	insert := "INSERT INTO " + oracleMySQLLiveQualified(schema, wide) + " (`ID`," + oracleMySQLLiveQuote(promoted[0]) + ") VALUES (?,?)"
	_, err = db.ExecContext(ctx, insert, 3, value+"中")
	if version.Checks() {
		var checkError *mysql.MySQLError
		require.True(t, errors.As(err, &checkError))
		require.Equal(t, uint16(3819), checkError.Number)
	} else {
		require.NoError(t, err, "older MySQL deliberately relaxes the promoted length limit")
		relaxed := false
		for _, warning := range report.Warnings {
			relaxed = relaxed || strings.Contains(warning.Reason, "放宽")
		}
		require.True(t, relaxed, "relaxing a source length limit must be reported")
		var longer string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT "+oracleMySQLLiveQuote(promoted[0])+" FROM "+oracleMySQLLiveQualified(schema, wide)+" WHERE `ID`=3").Scan(&longer))
		require.Equal(t, value+"中", longer)
	}
}
