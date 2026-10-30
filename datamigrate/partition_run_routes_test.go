package datamigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

type sqlServerPartitionRouteWriter struct {
	*partitionFlowWriter
	dia *dialect.SQLServerDialect
}

func (w *sqlServerPartitionRouteWriter) DBType() string                             { return "sqlserver" }
func (w *sqlServerPartitionRouteWriter) Dialect() dialect.Dialect                   { return w.dia }
func (w *sqlServerPartitionRouteWriter) CheckNames(context.Context, []string) error { return nil }
func (w *sqlServerPartitionRouteWriter) ValidateExisting(context.Context, *source.TableDDLInfo) error {
	return nil
}
func (w *sqlServerPartitionRouteWriter) RebuildDependencies(context.Context, map[string]bool) ([]dialect.Statement, error) {
	return nil, nil
}
func (w *sqlServerPartitionRouteWriter) DropDependencies(context.Context, []dialect.Statement) error {
	return nil
}
func (w *sqlServerPartitionRouteWriter) ObjectExists(context.Context, string) (bool, error) {
	return true, nil
}
func (w *sqlServerPartitionRouteWriter) CountPartitionRows(ctx context.Context, table string, spec *source.PartitionSpec) (map[string]int64, error) {
	counts, err := w.partitionFlowWriter.CountPartitionRows(ctx, table, spec)
	return map[string]int64{"1": counts["Dst.Low"], "2": counts["Dst.High"]}, err
}

func TestSQLServerPartitionRunUsesSerialRootStream(t *testing.T) {
	for _, scenario := range []string{"both", "data_only", "target_drift", "copy_error"} {
		t.Run(scenario, func(t *testing.T) {
			m, r, base := newPartitionFlow(t, "Root")
			r.specs["Root"] = partitionFlowSpec()
			base.profile = partition.Profile{Product: "sqlserver", Version: "16", Schema: "Dst", Mode: "filegroup:PRIMARY", Features: map[string]bool{"native_sqlserver": true, partition.FeatureRange: true}}
			w := &sqlServerPartitionRouteWriter{partitionFlowWriter: base, dia: dialect.NewSQLServer()}
			m.writer, m.cfg.TargetDBType = w, "sqlserver"
			if scenario == "data_only" {
				m.cfg.Content = "data_only"
				base.counts["Root"] = map[string]int64{"Dst.Low": 4, "Dst.High": 3}
			}
			if scenario == "target_drift" {
				base.actual = func(string) *source.PartitionSpec { return nil }
			}
			if scenario == "copy_error" {
				base.copyErr = errors.New("copy refused")
			}
			report := m.Run(context.Background())
			require.Zero(t, r.pageReads["Root"], "partition copying must not enter OFFSET paging")
			require.NotNil(t, report.PartitionMigration)
			if scenario == "target_drift" || scenario == "copy_error" {
				require.True(t, report.HasFailures())
				require.Empty(t, base.copied)
				require.False(t, m.partitionState.tables["Root"].dataExecuted)
				if scenario == "target_drift" {
					require.Empty(t, r.streamed)
					require.Contains(t, report.Data.Items[0].Error, "写入前分区核对失败")
				} else {
					require.Contains(t, report.Data.Items[0].Error, "copy refused")
				}
				return
			}
			require.False(t, report.HasFailures(), "tables=%+v data=%+v partitions=%+v", report.Tables, report.Data, report.PartitionMigration.Tables)
			require.Equal(t, []string{"Root"}, r.streamed)
			require.True(t, m.partitionState.tables["Root"].dataExecuted)
			require.Equal(t, "succeeded", report.PartitionMigration.Tables[0].Status)
			require.Equal(t, 1, report.Data.Success)
			require.Equal(t, int64(2), report.RowCounts[0].Dst)
		})
	}
}

type pgPartitionRouteReader struct{ *partitionFlowReader }

func (r *pgPartitionRouteReader) DBType() string { return "postgres" }
func (r *pgPartitionRouteReader) ReadPGSnapshot(context.Context) (*source.PGSnapshot, error) {
	return &source.PGSnapshot{Profile: source.PGProfile{Product: "postgres", Schema: "Src"}, Tables: []source.PGTable{{Info: *r.ddl["Root"]}}}, nil
}

type pgPartitionRouteWriter struct {
	*partitionFlowWriter
	db         *sql.DB
	countDelta int64
}

func (w *pgPartitionRouteWriter) DBType() string           { return "mysql" }
func (w *pgPartitionRouteWriter) Dialect() dialect.Dialect { return dialect.NewMySQL() }
func (w *pgPartitionRouteWriter) InspectionDB() *sql.DB    { return w.db }
func (w *pgPartitionRouteWriter) InspectMySQL(ctx context.Context) (target.MySQLProfile, error) {
	p, err := (&pgPlanWriter{db: w.db}).InspectMySQL(ctx)
	p.Database = "Dst"
	return p, err
}
func (w *pgPartitionRouteWriter) CountRows(ctx context.Context, table string) (int64, error) {
	n, err := w.partitionFlowWriter.CountRows(ctx, table)
	return n + w.countDelta, err
}

func newPGPartitionRoute(t *testing.T) (*Migrator, *pgPartitionRouteReader, *pgPartitionRouteWriter, sqlmock.Sqlmock) {
	t.Helper()
	m, baseReader, baseWriter := newPartitionFlow(t, "Root")
	baseReader.ddl["Root"].Columns[0].DataType = "int8"
	baseReader.rows["Root"] = [][]interface{}{{int64(1)}, {int64(11)}}
	baseReader.specs["Root"] = partitionFlowSpec()
	baseReader.specs["Root"].Method = "RANGE"
	baseReader.specs["Root"].Keys[0].DataType = "int8"
	baseWriter.profile = partition.Profile{Product: "mysql", Version: "8.0.36", Schema: "Dst", Features: map[string]bool{"inline_mysql": true, partition.FeatureRange: true}}
	baseWriter.counts["Root"] = map[string]int64{"Dst.Low": 4, "Dst.High": 3}
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mock.ExpectationsWereMet()); db.Close() })
	r, w := &pgPartitionRouteReader{baseReader}, &pgPartitionRouteWriter{partitionFlowWriter: baseWriter, db: db}
	m.reader, m.writer, m.cfg.TargetDBType, m.cfg.Content = r, w, "mysql", "data_only"
	return m, r, w, mock
}

func expectPGPartitionExistingTarget(mock sqlmock.Sqlmock, ordinary bool) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM information_schema.SCHEMATA`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectQuery(`SELECT TABLE_SCHEMA,TABLE_NAME,CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"s", "t", "c", "rs", "rt"}))
	mock.ExpectQuery(`SELECT ENGINE,TABLE_TYPE`).WillReturnRows(sqlmock.NewRows([]string{"e", "t", "r"}).AddRow("InnoDB", "BASE TABLE", "Dynamic"))
	mock.ExpectQuery(`SELECT COLUMN_NAME,COLUMN_TYPE,IS_NULLABLE`).WillReturnRows(sqlmock.NewRows([]string{"name", "type", "nullable", "extra", "collation", "default", "generated"}).AddRow("Id", "bigint", "NO", "", nil, nil, nil))
	if ordinary {
		mock.ExpectQuery(`SELECT 1 FROM`).WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	}
	mock.ExpectQuery(`SELECT INDEX_NAME,COLUMN_NAME,NON_UNIQUE`).WillReturnRows(sqlmock.NewRows([]string{"name", "col", "non", "prefix", "method"}))
	mock.ExpectQuery(`SELECT tc.CONSTRAINT_NAME,cc.CHECK_CLAUSE`).WillReturnRows(sqlmock.NewRows([]string{"name", "check", "enforced"}))
	mock.ExpectQuery(`SELECT k.CONSTRAINT_NAME,k.COLUMN_NAME`).WillReturnRows(sqlmock.NewRows([]string{"name", "col", "schema", "table", "refcol", "delete", "update"}))
	mock.ExpectQuery(`SELECT EVENT_OBJECT_TABLE,TRIGGER_NAME`).WillReturnRows(sqlmock.NewRows([]string{"table", "trigger"}))
}

func TestPGMySQLPartitionAppendRunChecksBaselineAndTarget(t *testing.T) {
	for _, scenario := range []string{"both", "schema_only", "append", "count_mismatch", "target_drift", "copy_error", "missing_baseline", "ordinary_nonempty"} {
		t.Run(scenario, func(t *testing.T) {
			m, r, w, mock := newPGPartitionRoute(t)
			if scenario == "both" || scenario == "schema_only" {
				m.cfg.Content = scenario
				w.counts["Root"] = map[string]int64{"Dst.Low": 0, "Dst.High": 0}
			}
			if scenario == "copy_error" {
				w.copyErr = errors.New("copy refused")
			}
			if scenario == "count_mismatch" {
				w.countDelta = 1
			}
			if scenario == "ordinary_nonempty" {
				delete(r.specs, "Root")
			}
			if scenario == "missing_baseline" {
				delete(w.counts["Root"], "Dst.High")
			}
			if scenario == "target_drift" {
				original := w.actual
				calls := 0
				w.actual = func(table string) *source.PartitionSpec {
					calls++
					if calls > 1 {
						return nil
					}
					return original(table)
				}
			}
			if scenario == "missing_baseline" {
				// The common partition preflight rejects incomplete baselines before
				// entering any MySQL table inspection or write path.
			} else if m.cfg.Content == "data_only" {
				expectPGPartitionExistingTarget(mock, scenario == "ordinary_nonempty")
			} else {
				expectPGEmptyTarget(mock, "GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'")
			}
			if scenario != "missing_baseline" && scenario != "ordinary_nonempty" {
				if m.cfg.Content == "data_only" {
					mock.ExpectQuery(`SHOW GRANTS`).WillReturnRows(sqlmock.NewRows([]string{"grant"}).AddRow("GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'"))
				}
				expectPGTextRecheck(mock)
				if m.cfg.Content == "data_only" {
					mock.ExpectQuery(`SELECT COLUMN_NAME,COLUMN_TYPE,IS_NULLABLE`).WillReturnRows(sqlmock.NewRows([]string{"name", "type", "nullable", "extra", "collation", "default", "generated"}).AddRow("Id", "bigint", "NO", "", nil, nil, nil))
				}
			}
			report := m.Run(context.Background())
			require.Zero(t, r.pageReads["Root"])
			if scenario == "append" || scenario == "both" || scenario == "schema_only" {
				require.False(t, report.HasFailures(), "preflight=%+v tables=%+v data=%+v", report.Preflight, report.Tables, report.Data)
				require.Equal(t, "succeeded", report.PartitionMigration.Tables[0].Status)
				if scenario == "schema_only" {
					require.Empty(t, r.streamed)
					require.Empty(t, w.copied)
					return
				}
				require.Equal(t, []string{"Root"}, r.streamed)
				require.Equal(t, 1, report.Data.Success)
				require.Equal(t, int64(2), report.RowCounts[0].Dst)
				if scenario == "append" {
					require.Equal(t, map[string]int64{"Dst.Low": 4, "Dst.High": 3}, report.PartitionMigration.Tables[0].Baseline)
				}
				return
			}
			require.True(t, report.HasFailures(), "%+v", report)
			if scenario == "count_mismatch" {
				require.Equal(t, 1, report.Data.Failed)
				require.False(t, report.RowCounts[0].Match)
			} else {
				if scenario != "copy_error" {
					require.Empty(t, r.streamed)
				}
				require.Empty(t, w.copied)
			}
			switch scenario {
			case "target_drift":
				require.Contains(t, report.Data.Items[0].Error, "写入前分区核对失败")
			case "copy_error":
				require.Contains(t, report.Data.Items[0].Error, "copy refused")
			case "missing_baseline", "ordinary_nonempty":
				message := "追加基线"
				var issues []CompatibilityIssue
				if scenario == "ordinary_nonempty" {
					message = "要求空目标表"
					issues = report.Preflight.Issues
				} else {
					issues = report.PartitionMigration.Issues
				}
				found := false
				for _, issue := range issues {
					if strings.Contains(issue.Reason, message) {
						found = true
					}
				}
				require.True(t, found, "preflight=%+v", report.Preflight)
			}
		})
	}
}

func TestPGMySQLPartitionWideRowPreflightHasCompleteSnapshot(t *testing.T) {
	m, r, w, _ := newPGPartitionRoute(t)
	m.cfg.Content = "schema_only"
	for i := 0; i < 5; i++ {
		r.ddl["Root"].Columns = append(r.ddl["Root"].Columns, source.ColumnInfo{
			Name: fmt.Sprintf("Wide%d", i), DataType: "varchar", Length: 4000, IsNullable: true,
		})
	}
	// The preliminary partition plan must give row fitting the same dependency
	// snapshot as the full PG plan. Width above 65535 enters its FK traversal.
	p := m.PreflightPartitions(context.Background())
	require.True(t, p.Ready, "%+v", p.Issues)
	require.Len(t, p.Tables, 1)
	require.NotNil(t, p.Tables[0].Plan)
	ddl := dialect.JoinSQL(p.Tables[0].Plan.Statements)
	require.Contains(t, ddl, "`Wide0` LONGTEXT")
	require.Contains(t, ddl, "CHECK (CHAR_LENGTH(`Wide0`) <= 4000)")
	require.Contains(t, ddl, "PARTITION BY RANGE")
	for _, column := range r.ddl["Root"].Columns {
		require.False(t, column.PGMySQLLongText, "preflight must not mutate source metadata")
	}
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
}
