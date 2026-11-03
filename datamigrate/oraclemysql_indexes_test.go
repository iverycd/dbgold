package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

type oracleMySQLIndexTestWriter struct {
	*oracleMySQLTestWriter
	profile      target.MySQLIndexProfile
	profileCalls int
	profileErr   error
	tables       map[string]target.MySQLIndexTable
	tableErrs    map[string]error
	tableCalls   []string
	indexes      []source.IndexInfo
	indexSQL     []string
	indexErrors  map[string]error
}

func (w *oracleMySQLIndexTestWriter) InspectMySQLIndexProfile(context.Context) (target.MySQLIndexProfile, error) {
	w.profileCalls++
	return w.profile, w.profileErr
}
func (w *oracleMySQLIndexTestWriter) InspectMySQLIndexTable(_ context.Context, table string) (target.MySQLIndexTable, error) {
	w.tableCalls = append(w.tableCalls, table)
	if err := w.tableErrs[table]; err != nil {
		return target.MySQLIndexTable{}, err
	}
	result, exists := w.tables[table]
	if !exists {
		return result, fmt.Errorf("target table %s missing", table)
	}
	result.Columns = slices.Clone(result.Columns)
	return result, nil
}
func (w *oracleMySQLIndexTestWriter) CreateIndex(ctx context.Context, index source.IndexInfo) error {
	w.indexes = append(w.indexes, index)
	sql := dialect.JoinSQL(w.Dialect().IndexStatements("dst", index))
	w.indexSQL = append(w.indexSQL, sql)
	if err := repair.Observe(ctx, sql, "running", nil); err != nil {
		return err
	}
	err := w.indexErrors[index.IndexName]
	status := "succeeded"
	if err != nil {
		status = "failed"
	}
	return errors.Join(err, repair.Observe(ctx, sql, status, err))
}

func oracleMySQLIndexFixture(t *testing.T, names ...string) (*Migrator, *oracleMySQLTestReader, *oracleMySQLIndexTestWriter) {
	t.Helper()
	m, r, base := oracleMySQLFixture(t, names...)
	w := &oracleMySQLIndexTestWriter{
		oracleMySQLTestWriter: base,
		profile:               target.MySQLIndexProfile{PageSize: 16384, LargePrefix: true},
		tables:                map[string]target.MySQLIndexTable{}, tableErrs: map[string]error{}, indexErrors: map[string]error{},
	}
	m.writer = w
	return m, r, w
}

func TestOracleMySQLIndexPrefixesShareExecutionReportAndRepair(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprint(capture), func(t *testing.T) {
			m, r, w := oracleMySQLIndexFixture(t, "Frame_Module")
			m.cfg.CaptureRepair, m.cfg.LowerCaseNames = capture, true
			r.ddl["Frame_Module"].Columns = append(r.ddl["Frame_Module"].Columns,
				source.ColumnInfo{Name: "ModuleURL", DataType: "VARCHAR2", Length: 2000})
			r.indexes = []source.IndexInfo{{TableName: "Frame_Module", IndexName: "IX_ModuleURL", Columns: []string{"ModuleURL"}}}
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), "%+v", report)
			require.Equal(t, 1, report.Indexes.Success)
			require.Contains(t, strings.Join(w.indexSQL, "\n"), "ADD INDEX `ix_moduleurl` (`moduleurl`(768))")
			require.Contains(t, w.created[0], "`moduleurl` varchar(2000)")
			require.Len(t, report.Warnings, 1)
			require.Contains(t, report.Warnings[0].Reason, "IX_ModuleURL")
			require.Contains(t, report.Warnings[0].Reason, "768")
			require.Equal(t, 1, w.profileCalls)
			require.Empty(t, w.tableCalls, "new-table planning must use frozen target declarations")
			require.Equal(t, int64(2000), r.ddl["Frame_Module"].Columns[1].Length)
			require.Equal(t, "VARCHAR2", r.ddl["Frame_Module"].Columns[1].DataType)
			require.Empty(t, r.indexes[0].PrefixLengths)
			require.Equal(t, []string{"ModuleURL"}, r.indexes[0].Columns)
			if capture {
				data, err := json.Marshal(report.RepairManifest)
				require.NoError(t, err)
				var saved RepairManifest
				require.NoError(t, json.Unmarshal(data, &saved))
				found := false
				for _, object := range saved.Objects {
					if object.Kind == "index" {
						found = true
						require.Equal(t, []int{768}, object.Index.PrefixLengths)
						require.Contains(t, w.indexSQL, object.DDL)
						require.Equal(t, object.DDL, object.Units[0].SQL)
						require.Equal(t, "succeeded", object.Status)
					}
				}
				require.True(t, found)
			}
		})
	}
}

func TestOracleMySQLIndexFailureShowsPlannedDDL(t *testing.T) {
	m, r, w := oracleMySQLIndexFixture(t, "T")
	m.cfg.CaptureRepair = true
	r.ddl["T"].Columns = append(r.ddl["T"].Columns, source.ColumnInfo{Name: "URL", DataType: "VARCHAR2", Length: 2000})
	r.indexes = []source.IndexInfo{{TableName: "T", IndexName: "IX_URL", Columns: []string{"URL"}}}
	w.indexErrors["IX_URL"] = errors.New("create index permission denied")
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Indexes.Failed)
	require.Len(t, report.Indexes.Items, 1)
	require.Contains(t, report.Indexes.Items[0].DDL, "`URL`(768)")
	require.Contains(t, w.indexSQL, report.Indexes.Items[0].DDL)
	for _, object := range report.RepairManifest.Objects {
		if object.Kind == "index" {
			require.Equal(t, report.Indexes.Items[0].DDL, object.DDL)
			require.Equal(t, "failed", object.Status)
		}
	}
}

func TestOracleMySQLObjectIndexUsesActualTargetAndCachesPerTable(t *testing.T) {
	m, r, w := oracleMySQLIndexFixture(t, "MixedTable", "Unselected")
	m.cfg.Objects, m.cfg.TableNames = []string{"indexes"}, []string{"MixedTable"}
	m.cfg.LowerCaseNames, m.cfg.CaptureRepair = true, true
	r.ddl["MixedTable"].Columns = []source.ColumnInfo{{Name: "URL", DataType: "VARCHAR2", Length: 20}}
	r.indexes = []source.IndexInfo{
		{TableName: "MixedTable", IndexName: "IxOne", Columns: []string{"URL"}},
		{TableName: "MixedTable", IndexName: "IxTwo", Columns: []string{"URL"}},
		{TableName: "Unselected", IndexName: "IxOther", Columns: []string{"URL"}},
	}
	w.tables["mixedtable"] = target.MySQLIndexTable{Engine: "InnoDB", RowFormat: "Dynamic", Columns: []target.MySQLIndexColumn{
		{Name: "url", DataType: "varchar(3000)", Length: 3000, MaxBytes: 3},
	}}
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, 2, report.Indexes.Success)
	require.Equal(t, []string{"mixedtable"}, w.tableCalls)
	require.Equal(t, 1, w.profileCalls)
	require.Empty(t, w.created)
	require.Len(t, report.Warnings, 2)
	for _, index := range w.indexes {
		require.Equal(t, []int{1024}, index.PrefixLengths)
		require.Equal(t, []string{"url"}, index.Columns)
	}
}

func TestOracleMySQLIndexPlanResetAndLegacyLimits(t *testing.T) {
	m, r, w := oracleMySQLIndexFixture(t, "T")
	m.cfg.Objects = []string{"indexes"}
	r.ddl["T"].Columns = []source.ColumnInfo{{Name: "URL", DataType: "VARCHAR2", Length: 2000}}
	r.indexes = []source.IndexInfo{{TableName: "T", IndexName: "IX", Columns: []string{"URL"}}}
	w.tables["T"] = target.MySQLIndexTable{Engine: "InnoDB", RowFormat: "COMPACT", Columns: []target.MySQLIndexColumn{{Name: "URL", DataType: "varchar(2000)", MaxBytes: 4}}}
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, []int{191}, w.indexes[0].PrefixLengths)
	w.tables["T"] = target.MySQLIndexTable{Engine: "InnoDB", RowFormat: "DYNAMIC", Columns: []target.MySQLIndexColumn{{Name: "URL", DataType: "varchar(2000)", MaxBytes: 4}}}
	w.profile.PageSize = 8192
	report = m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, []int{384}, w.indexes[1].PrefixLengths)
	require.Equal(t, 2, w.profileCalls)
	require.Equal(t, []string{"T", "T"}, w.tableCalls)
	require.Len(t, report.Warnings, 1)
}

func TestOracleMySQLIndexProbeFailuresAreLocal(t *testing.T) {
	m, r, w := oracleMySQLIndexFixture(t, "Good", "Bad", "Ignored")
	m.cfg.Objects, m.cfg.CaptureRepair = []string{"indexes"}, true
	m.cfg.Filter = "Good,Bad"
	for _, table := range []string{"Good", "Bad", "Ignored"} {
		r.ddl[table].Columns = []source.ColumnInfo{{Name: "URL", DataType: "VARCHAR2", Length: 2000}}
		r.indexes = append(r.indexes, source.IndexInfo{TableName: table, IndexName: "IX_" + table, Columns: []string{"URL"}})
	}
	w.tables["Good"] = target.MySQLIndexTable{Engine: "InnoDB", RowFormat: "DYNAMIC", Columns: []target.MySQLIndexColumn{{Name: "URL", DataType: "varchar(2000)", MaxBytes: 4}}}
	w.tableErrs["Bad"] = errors.New("catalog permission denied")
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Indexes.Success)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Equal(t, []string{"Good", "Bad"}, w.tableCalls)
	require.Len(t, w.indexes, 1)
	require.Contains(t, report.Indexes.Items[0].Error, "catalog permission denied")
	require.Empty(t, report.Indexes.Items[0].DDL)
	require.Len(t, report.Warnings, 1)
}

func TestOracleMySQLExcludedObjectIndexesAreNotInspectedOrExecuted(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprint(capture), func(t *testing.T) {
			m, r, w := oracleMySQLIndexFixture(t, "T")
			m.cfg.Objects, m.cfg.CaptureRepair = []string{"indexes"}, capture
			m.cfg.LowerCaseNames = true
			m.cfg.ExcludedObjects = []ObjectRef{{Kind: "index", Table: "T", Name: "ix_skip"}}
			r.ddl["T"].Columns = []source.ColumnInfo{{Name: "URL", DataType: "VARCHAR2", Length: 2000}}
			r.indexes = []source.IndexInfo{
				{TableName: "T", IndexName: "IX_SKIP", Columns: []string{"URL"}},
				{TableName: "T", IndexName: "IX_KEEP", Columns: []string{"URL"}},
			}
			w.tables["t"] = target.MySQLIndexTable{Engine: "InnoDB", RowFormat: "DYNAMIC", Columns: []target.MySQLIndexColumn{{Name: "url", DataType: "varchar(2000)", MaxBytes: 4}}}
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), "%+v", report)
			require.Equal(t, 1, report.Indexes.Total)
			require.Len(t, w.indexes, 1)
			require.Equal(t, "ix_keep", w.indexes[0].IndexName)
			require.Len(t, report.Warnings, 1)
			if capture {
				for _, object := range report.RepairManifest.Objects {
					require.NotEqual(t, "ix_skip", object.TargetName)
				}
			}
		})
	}
}

func TestOracleMySQLObjectIndexUnknownTypesAndEnginesKeepFullDDL(t *testing.T) {
	for _, engine := range []string{"MyISAM", "InnoDB"} {
		t.Run(engine, func(t *testing.T) {
			m, r, w := oracleMySQLIndexFixture(t, "T")
			m.cfg.Objects = []string{"indexes"}
			r.ddl["T"].Columns = []source.ColumnInfo{{Name: "Value", DataType: "VARCHAR2", Length: 2000}}
			r.indexes = []source.IndexInfo{{TableName: "T", IndexName: "IX", Columns: []string{"Value"}}}
			w.tables["T"] = target.MySQLIndexTable{Engine: engine, RowFormat: "DYNAMIC", Columns: []target.MySQLIndexColumn{{Name: "Value", DataType: "custom_type"}}}
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), "%+v", report)
			require.Len(t, w.indexSQL, 1)
			require.Contains(t, w.indexSQL[0], "(`Value`)")
			require.Empty(t, w.indexes[0].PrefixLengths)
			require.Zero(t, w.profileCalls)
			require.Empty(t, report.Warnings)
		})
	}
}

func TestOracleMySQLIndexProfileFailureKeepsShortIndex(t *testing.T) {
	m, r, w := oracleMySQLIndexFixture(t, "T")
	r.ddl["T"].Columns = append(r.ddl["T"].Columns,
		source.ColumnInfo{Name: "URL", DataType: "VARCHAR2", Length: 2000},
		source.ColumnInfo{Name: "Code", DataType: "VARCHAR2", Length: 10})
	r.indexes = []source.IndexInfo{
		{TableName: "T", IndexName: "IX_Long", Columns: []string{"URL"}},
		{TableName: "T", IndexName: "IX_Short", Columns: []string{"Code"}},
	}
	w.profileErr = errors.New("page size query denied")
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Indexes.Success)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Equal(t, 1, w.profileCalls)
	require.Contains(t, report.Indexes.Items[0].Error, "page size query denied")
	require.Empty(t, report.Warnings)
}

func TestOracleMySQLUniqueKeysStayFullWithExplicit1071Diagnosis(t *testing.T) {
	m, r, w := oracleMySQLIndexFixture(t, "T")
	r.ddl["T"].Columns = []source.ColumnInfo{{Name: "URL", DataType: "VARCHAR2", Length: 2000}}
	r.keys = []source.IndexInfo{{TableName: "T", IndexName: "PK_URL", Columns: []string{"URL"}, IsPrimary: true, IsUnique: true}}
	r.indexes = []source.IndexInfo{{TableName: "T", IndexName: "UQ_URL", Columns: []string{"URL"}, IsUnique: true}}
	w.indexErrors["PK_URL"] = &mysql.MySQLError{Number: 1071, Message: "Specified key was too long"}
	w.indexErrors["UQ_URL"] = w.indexErrors["PK_URL"]
	report := m.Run(context.Background())
	require.Equal(t, 1, report.PrimaryKeys.Failed)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Contains(t, report.PrimaryKeys.Items[0].Error, "不能自动改为前缀")
	require.Contains(t, report.Indexes.Items[0].Error, "不能自动改为前缀")
	require.Zero(t, w.profileCalls)
	require.Empty(t, w.tableCalls)
	for _, index := range w.indexes {
		require.Empty(t, index.PrefixLengths)
	}
	for _, sql := range w.indexSQL {
		require.Contains(t, sql, "(`URL`)")
	}
}

func TestOracleMySQLPrefixSurvivesRecoveryColumnRename(t *testing.T) {
	_, _, w := oracleMySQLIndexFixture(t, "T")
	table := repair.Object{Kind: "table", Table: "T", ColumnMapping: map[string]string{"URL": "url"}}
	object := repair.Object{Kind: "index", Table: "T", Schema: "dst", Index: &source.IndexInfo{
		TableName: "T", IndexName: "IX", Columns: []string{"url"}, PrefixLengths: []int{768},
	}}
	got := regenerateRepairCandidate(w, Config{}, table, object, map[string]string{"URL": "renamed_url"})
	require.Equal(t, []int{768}, got.Index.PrefixLengths)
	require.Equal(t, []string{"renamed_url"}, got.Index.Columns)
	require.Contains(t, got.SuggestedDDL, "`renamed_url`(768)")
	require.Equal(t, []string{"url"}, object.Index.Columns)
}
