package datamigrate

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"

	"github.com/stretchr/testify/require"
)

type oscarDamengRunReader struct {
	*oscarFixtureReader
	streamed []string
}

func (r *oscarDamengRunReader) MigrationSchema() string { return "SrcMixed" }

func (r *oscarDamengRunReader) PartitionRoots(context.Context) (map[string]string, error) {
	return map[string]string{"PhysicalLeaf": "Partitioned"}, nil
}

func (r *oscarDamengRunReader) PartitionMetadata(_ context.Context, table string) (*source.PartitionSpec, error) {
	if table == "Partitioned" {
		return &source.PartitionSpec{Method: "RANGE"}, nil
	}
	return nil, nil
}

func (r *oscarDamengRunReader) GetTableSourceDDL(_ context.Context, table string) (string, error) {
	return fmt.Sprintf(`CREATE TABLE "%s" ("Id" INT)`, table), nil
}

func (r *oscarDamengRunReader) StreamPartitionRows(context.Context, string, int, func([]string, []string, [][]any) error) error {
	return errors.New("分区不应进入数据复制")
}

func (r *oscarDamengRunReader) ReadPage(context.Context, string, []string, int64, int64) ([]string, []string, [][]any, error) {
	return nil, nil, nil, errors.New("神通必须使用固定游标")
}

func (r *oscarDamengRunReader) StreamOscarDamengTable(_ context.Context, table string, _ int, consume func([]string, []string, [][]any) error) error {
	r.streamed = append(r.streamed, table)
	for _, row := range r.rows[table] {
		if err := consume([]string{"Id"}, []string{"INT"}, [][]any{row}); err != nil {
			return err
		}
	}
	return nil
}

func (r *oscarDamengRunReader) CountRows(_ context.Context, table string) (int64, error) {
	return int64(len(r.rows[table])), nil
}

type oscarDamengRunWriter struct {
	partialMigrationWriter
	checked []string
	rows    map[string]int64
}

func (w *oscarDamengRunWriter) EffectiveSchema() string  { return "DST" }
func (w *oscarDamengRunWriter) Dialect() dialect.Dialect { return dialect.NewDaMeng() }

func (w *oscarDamengRunWriter) CreateTable(_ context.Context, statements []dialect.Statement) error {
	for _, statement := range statements {
		w.attempts = append(w.attempts, statement.SQL)
		if strings.Contains(statement.SQL, `"DST"."BROKEN"`) && strings.HasPrefix(statement.SQL, w.failStage+" TABLE ") {
			return w.failure
		}
	}
	w.created = append(w.created, dialect.JoinSQL(statements))
	return nil
}

func (w *oscarDamengRunWriter) ValidateOscarDamengExisting(_ context.Context, info *source.TableDDLInfo) error {
	if len(info.Columns) != 1 || info.Columns[0].Name != "ID" {
		return fmt.Errorf("目标列映射错误: %+v", info.Columns)
	}
	w.checked = append(w.checked, info.TableName)
	return nil
}

func (w *oscarDamengRunWriter) CopyData(_ context.Context, table string, cols, types []string, rows [][]any) error {
	if len(cols) != 1 || cols[0] != "ID" || len(types) != 1 || types[0] != "INT" {
		return fmt.Errorf("错误的数据投影: %v %v", cols, types)
	}
	w.copied = append(w.copied, table)
	w.rows[table] += int64(len(rows))
	return nil
}

func (w *oscarDamengRunWriter) CountRows(_ context.Context, table string) (int64, error) {
	return w.rows[table], nil
}

func oscarDamengRunFixture(failStage string, brokenFirst bool) (*oscarDamengRunReader, *oscarDamengRunWriter, *Migrator) {
	raw, _, _ := partialMigrationFixture("oscar", "dameng", "", brokenFirst)
	r := &oscarDamengRunReader{oscarFixtureReader: raw.oscarFixtureReader}
	r.foreign[0].RefSchema = r.MigrationSchema()
	w := &oscarDamengRunWriter{partialMigrationWriter: partialMigrationWriter{
		kind: "dameng", failStage: failStage, failure: errors.New("表 DDL 执行失败"),
	}, rows: make(map[string]int64)}
	m, _ := newTestMigrator(r, w)
	m.cfg.Content, m.cfg.MaxParallel = "both", 1
	return r, w, m
}

func TestOscarDamengContinuesAfterLocalTableFailures(t *testing.T) {
	for _, stage := range []string{"generate", "DROP", "CREATE"} {
		t.Run(stage, func(t *testing.T) {
			r, w, m := oscarDamengRunFixture(stage, stage != "CREATE")
			if stage == "generate" {
				r.ddl["Broken"].Columns[0].DataType = "unsupported_custom_type"
			}
			m.cfg.CaptureRepair = true
			report := m.Run(t.Context())
			require.Equal(t, 3, report.Tables.Total)
			require.Equal(t, 2, report.Tables.Success)
			require.Equal(t, 2, report.Data.Success)
			require.Equal(t, []string{"A", "Z"}, r.streamed)
			require.Equal(t, []string{"A", "Z"}, w.copied)
			require.Len(t, report.Tables.Items, 1)
			failure := report.Tables.Items[0]
			require.Equal(t, "Broken", failure.Name)
			require.Contains(t, failure.SourceDDL, `"Broken"`)
			if stage == "generate" {
				require.Equal(t, "preflight", failure.Stage)
			} else {
				require.Equal(t, "execute", failure.Stage)
				require.Contains(t, failure.DDL, `CREATE TABLE "DST"."BROKEN"`)
			}
			if stage != "CREATE" {
				for _, stmt := range w.attempts {
					require.NotContains(t, stmt, `CREATE TABLE "DST"."BROKEN"`)
				}
			}
			require.Equal(t, 2, report.PrimaryKeys.Success)
			require.Equal(t, 2, report.Indexes.Success)
			require.Equal(t, 2, report.Comments.Success)
			require.Len(t, w.fks, 1)
			require.Equal(t, "HEALTHY_FK", w.fks[0].ConstraintName)
			require.Equal(t, "dependency", report.Constraints.Items[0].Stage)
			require.Contains(t, report.Constraints.Items[0].Error, "Broken")
			require.Empty(t, w.views)
			require.Contains(t, report.Warnings[0].SourceDDL, "SELECT")
			require.Equal(t, "dependency", report.Views.Items[0].Stage)
			require.NotNil(t, report.RepairManifest)
			require.True(t, report.HasFailures())
		})
	}
}

func TestOscarDamengConnectionFailureStopsWork(t *testing.T) {
	r, w, m := oscarDamengRunFixture("DROP", true)
	w.failure = driver.ErrBadConn
	report := m.Run(t.Context())
	require.Empty(t, r.streamed)
	require.Empty(t, w.created)
	require.Equal(t, "task", report.Tables.Items[0].Stage)
}

func TestOscarDamengSkipsPartitionsAndChecksAppendDelta(t *testing.T) {
	r, w, m := oscarDamengRunFixture("", false)
	r.tables = []string{"A", "Partitioned", "PhysicalLeaf"}
	r.rows["A"] = [][]any{{int64(7)}, {int64(7)}}
	w.rows["A"] = 5
	m.cfg.Content = "data_only"
	preflight := m.PreflightPartitions(t.Context())
	require.False(t, preflight.Ready)
	require.True(t, preflight.CanStart)
	require.Len(t, preflight.Issues, 1)
	require.Equal(t, "Partitioned", preflight.Issues[0].Object.Name)
	report := m.Run(t.Context())
	require.NotNil(t, report.PartitionMigration)
	require.Equal(t, []string{"A"}, w.checked)
	require.Equal(t, []string{"A"}, r.streamed)
	require.Equal(t, 1, report.Data.Success)
	require.Len(t, report.RowCounts, 1)
	require.Equal(t, int64(7), w.rows["A"])
	require.Equal(t, int64(2), report.RowCounts[0].Dst)
	require.True(t, report.RowCounts[0].Match)
	require.Empty(t, w.attempts)
}

func TestOscarDamengRecoveryUsesSameCursorAndFreshIdentity(t *testing.T) {
	r, w, m := oscarDamengRunFixture("", false)
	saved := []source.ColumnInfo{{Name: "Id", DataType: "INT", Extra: "auto_increment", IdentityInfo: &source.IdentityInfo{Seed: 12, Increment: 1}}}
	r.ddl["A"].Columns = []source.ColumnInfo{{Name: "Id", DataType: "INT", Extra: "auto_increment", IdentityInfo: &source.IdentityInfo{Seed: 27, Increment: 1}}}
	r.rows["A"] = [][]any{{int64(15)}, {int64(15)}}
	w.rows["A"] = 1
	ops := recoveryOps{
		schema: func(context.Context, target.Writer) (string, error) { return "DST", nil },
		inspect: func(context.Context, target.Writer, string) ([]source.ColumnInfo, error) {
			return []source.ColumnInfo{{Name: "ID", DataType: "INT", Extra: "auto_increment"}}, nil
		},
		prepare:  func(context.Context, target.Writer, string, []source.ColumnInfo) error { return nil },
		truncate: func(context.Context, target.Writer, string) error { w.rows["A"] = 0; return nil },
		calibrate: func(_ context.Context, _ target.Writer, table string, seqs []source.SequenceInfo) error {
			require.Equal(t, "A", table)
			require.Len(t, seqs, 1)
			require.Equal(t, int64(27), seqs[0].StartValue)
			return nil
		},
	}
	obj := repair.Object{Kind: "table", SourceTable: "A", Table: "A", SourceColumns: saved}
	preview, err := previewRecovery(t.Context(), r, w, m.cfg, obj, nil, nil, ops)
	require.NoError(t, err)
	result, err := runRecovery(t.Context(), r, w, m.cfg, preview, true, nil, ops)
	require.NoError(t, err)
	require.Equal(t, []string{"A"}, r.streamed)
	require.Equal(t, int64(2), result.CommittedRows)
	require.Equal(t, "succeeded", result.Status)
}
