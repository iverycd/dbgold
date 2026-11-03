package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type oracleMySQLNormalizedIndexReader struct {
	*oracleMySQLTestReader
	normalized      []source.IndexInfo
	normalizedError error
	normalizedCalls int
	genericCalls    int
	genericError    error
}

func (r *oracleMySQLNormalizedIndexReader) GetIndexesForMySQL(context.Context) ([]source.IndexInfo, error) {
	r.normalizedCalls++
	return cloneOracleMySQLIndexes(r.normalized), r.normalizedError
}

func (r *oracleMySQLNormalizedIndexReader) GetIndexes(context.Context) ([]source.IndexInfo, error) {
	r.genericCalls++
	return cloneOracleMySQLIndexes(r.indexes), r.genericError
}

func oracleMySQLHiddenIndexObject(t *testing.T, report MigrationReport, sourceName string) repair.Object {
	t.Helper()
	require.NotNil(t, report.RepairManifest)
	// Exercise the durable metadata used by a subsequent repair, rather than
	// relying on slice aliases shared with the in-memory source inventory.
	data, err := json.Marshal(report.RepairManifest)
	require.NoError(t, err)
	var saved RepairManifest
	require.NoError(t, json.Unmarshal(data, &saved))
	for _, object := range saved.Objects {
		if object.Kind == "index" && object.SourceName == sourceName {
			return object
		}
	}
	t.Fatalf("repair manifest has no index with source name %s", sourceName)
	return repair.Object{}
}

func TestOracleMySQLHiddenIndexUsesNormalizedInventoryInBothModes(t *testing.T) {
	for _, objectOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("objects=%v", objectOnly), func(t *testing.T) {
			m, r, w := oracleMySQLIndexFixture(t, "P_USAGE_LOG")
			m.cfg.CaptureRepair, m.cfg.LowerCaseNames = true, true
			r.keys = nil
			r.ddl["P_USAGE_LOG"].Columns = append(r.ddl["P_USAGE_LOG"].Columns,
				source.ColumnInfo{Name: "ServerTS", DataType: "TIMESTAMP", Scale: 6})
			r.indexes = []source.IndexInfo{{TableName: "P_USAGE_LOG", IndexName: "IDX_P_USAGE_LOG_SERVERts", Columns: []string{"SYS_NC00013$"}}}
			normalized := &oracleMySQLNormalizedIndexReader{
				oracleMySQLTestReader: r,
				normalized:            []source.IndexInfo{{TableName: "P_USAGE_LOG", IndexName: "IDX_P_USAGE_LOG_SERVERts", Columns: []string{"ServerTS"}, Descending: []bool{true}}},
				genericError:          errors.New("the generic hidden-column inventory must not be used"),
			}
			m.reader = normalized
			if objectOnly {
				m.cfg.Objects, m.cfg.TableNames = []string{"indexes"}, []string{"P_USAGE_LOG"}
				w.tables["p_usage_log"] = target.MySQLIndexTable{Engine: "InnoDB", RowFormat: "DYNAMIC", Columns: []target.MySQLIndexColumn{
					{Name: "id", DataType: "int"}, {Name: "serverts", DataType: "datetime(6)"},
				}}
			}
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), "%+v", report)
			require.Equal(t, 1, report.Indexes.Success)
			require.Equal(t, 1, normalized.normalizedCalls)
			require.Zero(t, normalized.genericCalls)
			require.Equal(t, 1, w.versionCalls)
			require.Equal(t, []string{"ALTER TABLE `dst`.`p_usage_log` ADD INDEX `idx_p_usage_log_serverts` (`serverts` DESC)"}, w.indexSQL)
			object := oracleMySQLHiddenIndexObject(t, report, "IDX_P_USAGE_LOG_SERVERts")
			require.Equal(t, w.indexSQL[0], object.DDL)
			require.Len(t, object.Units, 1)
			require.Equal(t, object.DDL, object.Units[0].SQL)
			require.Equal(t, []bool{true}, object.Index.Descending)
			require.Equal(t, []string{"serverts"}, object.Index.Columns)
			require.Equal(t, []string{"ServerTS"}, normalized.normalized[0].Columns)
			require.Equal(t, []string{"SYS_NC00013$"}, r.indexes[0].Columns)
			if objectOnly {
				require.Empty(t, w.created)
				require.Equal(t, []string{"p_usage_log"}, w.tableCalls)
			} else {
				require.Len(t, w.created, 1)
				require.Empty(t, w.tableCalls)
			}
		})
	}
}

func TestOracleMySQLDescendingPrefixSQLMatchesReportAndRepair(t *testing.T) {
	for _, version := range []dialect.MySQLVersion{{Major: 8, Minor: 0, Patch: 30}, {Major: 8, Minor: 4, Patch: 1}, {Major: 5, Minor: 7, Patch: 44}} {
		for _, fails := range []bool{false, true} {
			t.Run(fmt.Sprintf("version=%v_fail=%v", version, fails), func(t *testing.T) {
				m, r, w := oracleMySQLIndexFixture(t, "T")
				m.cfg.CaptureRepair = true
				r.keys = nil
				r.ddl["T"].Columns = []source.ColumnInfo{{Name: "URL", DataType: "VARCHAR2", Length: 2000}}
				r.indexes = []source.IndexInfo{{TableName: "T", IndexName: "IX_URL", Columns: []string{"URL"}, Descending: []bool{true}}}
				w.version = version
				if fails {
					w.indexErrors["IX_URL"] = errors.New("index permission denied")
				}
				report := m.Run(context.Background())
				require.Len(t, w.indexSQL, 1)
				want := "ALTER TABLE `dst`.`T` ADD INDEX `IX_URL` (`URL`(768) DESC)"
				if version.Major == 5 {
					want = "ALTER TABLE `dst`.`T` ADD INDEX `IX_URL` (`URL`(768))"
				}
				require.Equal(t, want, w.indexSQL[0])
				require.Equal(t, []int{768}, w.indexes[0].PrefixLengths)
				require.Equal(t, 1, w.versionCalls)
				require.Equal(t, 1, w.profileCalls)
				require.Len(t, report.Warnings, 1, "prefix and direction warnings must not overwrite each other or duplicate the object")
				require.Contains(t, report.Warnings[0].Reason, "前缀")
				if version.Major == 5 {
					require.Contains(t, report.Warnings[0].Reason, "降序")
					require.Contains(t, report.Warnings[0].Reason, "查询性能")
				}
				object := oracleMySQLHiddenIndexObject(t, report, "IX_URL")
				require.Equal(t, want, object.DDL)
				require.Len(t, object.Units, 1)
				require.Equal(t, want, object.Units[0].SQL)
				if fails {
					require.Equal(t, 1, report.Indexes.Failed)
					require.Len(t, report.Indexes.Items, 1)
					require.Equal(t, want, report.Indexes.Items[0].DDL)
					require.Equal(t, "failed", object.Status)
				} else {
					require.False(t, report.HasFailures(), "%+v", report)
					require.Equal(t, "succeeded", object.Status)
				}
				require.Equal(t, []bool{true}, r.indexes[0].Descending, "target version must not mutate source directions")
				require.Empty(t, r.indexes[0].PrefixLengths)
			})
		}
	}
}

func TestOracleMySQLDescendingUniqueRemainsFullAndUsesVersionPolicy(t *testing.T) {
	for _, version := range []dialect.MySQLVersion{{Major: 8, Minor: 0, Patch: 30}, {Major: 5, Minor: 7, Patch: 44}} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			m, r, w := oracleMySQLIndexFixture(t, "T")
			m.cfg.CaptureRepair = true
			r.keys = nil
			r.ddl["T"].Columns = []source.ColumnInfo{{Name: "Code", DataType: "VARCHAR2", Length: 100}}
			r.indexes = []source.IndexInfo{{TableName: "T", IndexName: "UQ_Code", Columns: []string{"Code"}, IsUnique: true, Descending: []bool{true}}}
			w.version = version
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), "%+v", report)
			require.Len(t, w.indexSQL, 1)
			want := "ALTER TABLE `dst`.`T` ADD UNIQUE INDEX `UQ_Code` (`Code` DESC)"
			if version.Major == 5 {
				want = "ALTER TABLE `dst`.`T` ADD UNIQUE INDEX `UQ_Code` (`Code`)"
				require.Len(t, report.Warnings, 1)
				require.Contains(t, report.Warnings[0].Reason, "降序")
			} else {
				require.Empty(t, report.Warnings)
			}
			require.Equal(t, want, w.indexSQL[0])
			require.Empty(t, w.indexes[0].PrefixLengths)
			require.Zero(t, w.profileCalls, "unique indexes must never enter length-prefix planning")
			require.Equal(t, 1, w.versionCalls, "unique indexes must not bypass direction planning")
			require.Equal(t, want, oracleMySQLHiddenIndexObject(t, report, "UQ_Code").DDL)
		})
	}
}

func TestOracleMySQLDescendingVersionFailureIsLocalAndCached(t *testing.T) {
	m, r, w := oracleMySQLIndexFixture(t, "T")
	m.cfg.CaptureRepair = true
	r.keys = nil
	r.indexes = []source.IndexInfo{
		{TableName: "T", IndexName: "IX_Desc1", Columns: []string{"ID"}, Descending: []bool{true}},
		{TableName: "T", IndexName: "IX_Asc", Columns: []string{"ID"}},
		{TableName: "T", IndexName: "IX_Desc2", Columns: []string{"ID"}, Descending: []bool{true}},
	}
	w.versionError = errors.New("version query denied")
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Indexes.Success)
	require.Equal(t, 2, report.Indexes.Failed)
	require.Equal(t, 1, w.versionCalls)
	require.Len(t, w.indexes, 1)
	require.Equal(t, "IX_Asc", w.indexes[0].IndexName)
	require.Empty(t, report.Warnings)
	for _, item := range report.Indexes.Items {
		require.Equal(t, "unsupported", item.Stage)
		require.Contains(t, item.Error, "version query denied")
		require.Empty(t, item.DDL)
		object := oracleMySQLHiddenIndexObject(t, report, item.SourceName)
		require.Empty(t, object.DDL)
		require.Empty(t, object.Units)
	}
}

func TestOracleMySQLOnlySelectedDescendingIndexesProbeVersion(t *testing.T) {
	m, r, w := oracleMySQLIndexFixture(t, "Selected", "Ignored")
	m.cfg.Objects, m.cfg.TableNames = []string{"indexes"}, []string{"Selected"}
	m.cfg.ExcludedObjects = []ObjectRef{{Kind: "index", Table: "Selected", Name: "IX_Excluded"}}
	r.indexes = []source.IndexInfo{
		{TableName: "Selected", IndexName: "IX_Asc", Columns: []string{"ID"}},
		{TableName: "Selected", IndexName: "IX_Excluded", Columns: []string{"ID"}, Descending: []bool{true}},
		{TableName: "Ignored", IndexName: "IX_Other", Columns: []string{"ID"}, Descending: []bool{true}},
	}
	w.tables["Selected"] = target.MySQLIndexTable{Engine: "InnoDB", RowFormat: "DYNAMIC", Columns: []target.MySQLIndexColumn{{Name: "ID", DataType: "int"}}}
	w.versionError = errors.New("unused version query")
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, 1, report.Indexes.Success)
	require.Zero(t, w.versionCalls)
	require.Equal(t, []string{"Selected"}, w.tableCalls)
}

func TestOracleMySQLDescendingVersionCacheResetsBetweenRuns(t *testing.T) {
	m, r, w := oracleMySQLIndexFixture(t, "T")
	m.cfg.Objects = []string{"indexes"}
	r.indexes = []source.IndexInfo{
		{TableName: "T", IndexName: "IX_Desc1", Columns: []string{"ID"}, Descending: []bool{true}},
		{TableName: "T", IndexName: "IX_Desc2", Columns: []string{"ID"}, Descending: []bool{true}},
	}
	w.tables["T"] = target.MySQLIndexTable{Engine: "InnoDB", RowFormat: "DYNAMIC", Columns: []target.MySQLIndexColumn{{Name: "ID", DataType: "int"}}}
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, 1, w.versionCalls)
	require.Len(t, w.indexSQL, 2)
	for _, sql := range w.indexSQL {
		require.Contains(t, sql, "`ID` DESC")
	}
	w.version = dialect.MySQLVersion{Major: 5, Minor: 7, Patch: 44}
	report = m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, 2, w.versionCalls)
	require.Len(t, report.Warnings, 2)
	require.Len(t, w.indexSQL, 4)
	for _, sql := range w.indexSQL[2:] {
		require.NotContains(t, sql, " DESC")
	}
	require.Equal(t, []string{"T", "T"}, w.tableCalls)
}

func TestOracleMySQLDescendingIndexSharesWideTableVersionProbe(t *testing.T) {
	m, r, w := oracleMySQLIndexFixture(t, "Wide")
	m.cfg.CaptureRepair = true
	for i := 0; i < 10; i++ {
		r.ddl["Wide"].Columns = append(r.ddl["Wide"].Columns,
			source.ColumnInfo{Name: fmt.Sprintf("C%d", i), DataType: "VARCHAR2", Length: 2000, IsNullable: true})
	}
	r.indexes = []source.IndexInfo{{TableName: "Wide", IndexName: "IX_Desc", Columns: []string{"C0"}, Descending: []bool{true}}}
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, 1, w.versionCalls, "table and descending-index planning must share the target version")
	require.Contains(t, w.created[0], "`C0` varchar(2000)")
	require.Contains(t, w.created[0], "`C1` longtext")
	object := oracleMySQLHiddenIndexObject(t, report, "IX_Desc")
	require.Contains(t, object.DDL, "`C0`(768) DESC")
	require.Contains(t, w.indexSQL, object.DDL)
}

func TestOracleMySQLUnsupportedExpressionKeepsNoExecutableUnits(t *testing.T) {
	m, r, w := oracleMySQLIndexFixture(t, "T")
	m.cfg.CaptureRepair = true
	r.keys = nil
	reason := `Oracle 表达式 UPPER("ID") 暂不支持自动转换`
	r.indexes = []source.IndexInfo{
		{TableName: "T", IndexName: "IX_Expression", Columns: []string{"SYS_NC00013$"}, Unsupported: reason},
		{TableName: "T", IndexName: "IX_Ordinary", Columns: []string{"ID"}},
	}
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Indexes.Success)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Len(t, w.indexes, 1)
	require.Equal(t, "IX_Ordinary", w.indexes[0].IndexName)
	require.Zero(t, w.versionCalls)
	require.Equal(t, "unsupported", report.Indexes.Items[0].Stage)
	require.Equal(t, reason, report.Indexes.Items[0].Error)
	require.Empty(t, report.Indexes.Items[0].DDL)
	object := oracleMySQLHiddenIndexObject(t, report, "IX_Expression")
	require.Empty(t, object.DDL)
	require.Empty(t, object.Units)
	require.Equal(t, "failed", object.Status)
}

func TestOracleMySQLDescendingExistingNonInnoDBIsUnsupported(t *testing.T) {
	m, r, w := oracleMySQLIndexFixture(t, "T")
	m.cfg.Objects, m.cfg.CaptureRepair = []string{"indexes"}, true
	r.indexes = []source.IndexInfo{
		{TableName: "T", IndexName: "IX_Desc", Columns: []string{"ID"}, Descending: []bool{true}},
		{TableName: "T", IndexName: "IX_Asc", Columns: []string{"ID"}},
	}
	w.tables["T"] = target.MySQLIndexTable{Engine: "MyISAM", RowFormat: "DYNAMIC"}
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Indexes.Success)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Equal(t, "IX_Asc", w.indexes[0].IndexName)
	require.Contains(t, report.Indexes.Items[0].Error, "MyISAM")
	require.Contains(t, report.Indexes.Items[0].Error, "降序")
	require.Empty(t, oracleMySQLHiddenIndexObject(t, report, "IX_Desc").Units)
}

func TestOracleMySQLNormalizedInventoryIsNotUsedForOtherTargets(t *testing.T) {
	m, r, _ := oracleMySQLIndexFixture(t, "T")
	r.indexes = []source.IndexInfo{{TableName: "T", IndexName: "IX_Generic", Columns: []string{"ID"}}}
	normalized := &oracleMySQLNormalizedIndexReader{
		oracleMySQLTestReader: r,
		normalizedError:       errors.New("MySQL-only catalog must not be read"),
	}
	m.reader, m.writer = normalized, &mockWriter{}
	indexes, err := m.sourceIndexes(context.Background())
	require.NoError(t, err)
	require.Equal(t, r.indexes, indexes)
	require.Equal(t, 1, normalized.genericCalls)
	require.Zero(t, normalized.normalizedCalls)
}

func TestOracleMySQLNormalizedInventoryFailureNeverFallsBackToHiddenColumns(t *testing.T) {
	m, r, _ := oracleMySQLIndexFixture(t, "T")
	r.indexes = []source.IndexInfo{{TableName: "T", IndexName: "IX_Hidden", Columns: []string{"SYS_NC00013$"}}}
	normalized := &oracleMySQLNormalizedIndexReader{
		oracleMySQLTestReader: r,
		normalizedError:       errors.New("Oracle index catalog interrupted"),
	}
	m.reader = normalized
	indexes, err := m.sourceIndexes(context.Background())
	require.ErrorContains(t, err, "Oracle index catalog interrupted")
	require.Empty(t, indexes)
	require.Equal(t, 1, normalized.normalizedCalls)
	require.Zero(t, normalized.genericCalls)
}
