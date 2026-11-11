package target

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func gaussTestWriter(t *testing.T) (*GaussDBWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return &GaussDBWriter{db: db, schema: "App", dia: dialect.NewPostgres("gaussdb")}, mock
}
func gaussExpectTable(mock sqlmock.Sqlmock, oid int, distribution string) {
	mock.ExpectQuery(regexp.QuoteMeta(gaussDistributionSQL)).WithArgs("App", "Patients").WillReturnRows(sqlmock.NewRows([]string{"oid", "mode", "keys", "kind", "persistence", "partition", "options"}).AddRow(oid, "H", distribution, "r", "p", "n", "{orientation=row,storage_type=USTORE}"))
	gaussExpectColumns(mock, oid)
}
func gaussExpectColumns(mock sqlmock.Sqlmock, oid int) {
	mock.ExpectQuery(regexp.QuoteMeta(gaussColumnsSQL)).WithArgs(oid).WillReturnRows(sqlmock.NewRows([]string{"n", "name", "type"}).AddRow(1, "patient_id", "integer").AddRow(3, "Tenant", "integer").AddRow(24, "auto_id", "bigint")).RowsWillBeClosed()
}
func gaussExpectGTM(mock sqlmock.Sqlmock, version, free, option string) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version()")).WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(version))
	if !gauss507.MatchString(version) {
		return
	}
	mock.ExpectQuery("SHOW enable_gtm_free").WillReturnRows(sqlmock.NewRows([]string{"free"}).AddRow(free))
	mock.ExpectQuery("SHOW gtm_option").WillReturnRows(sqlmock.NewRows([]string{"option"}).AddRow(option))
}
func gaussUnique() source.IndexInfo {
	return source.IndexInfo{TableName: "Patients", IndexName: "auto_id", Columns: []string{"auto_id"}, IsUnique: true, Method: "BTREE"}
}

const gaussTestVersion = "gaussdb (GaussDB Kernel 507.0.0 build 530a0cd2)"

func TestGaussDBPlanSelectsLocalOrGlobalFromActualDistribution(t *testing.T) {
	for _, tc := range []struct {
		name, dist      string
		columns         []string
		primary, global bool
	}{
		{"local primary", "1", []string{"patient_id"}, true, false},
		{"disjoint unique", "1", []string{"auto_id"}, false, true},
		{"existing different distribution", "24", []string{"patient_id"}, true, true},
		{"composite distribution included", "1 3", []string{"Tenant", "patient_id", "auto_id"}, false, false},
		{"composite distribution incomplete", "1 3", []string{"patient_id"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := gaussTestWriter(t)
			gaussExpectTable(mock, 42, tc.dist)
			if tc.global {
				gaussExpectGTM(mock, gaussTestVersion, "off", "1")
			}
			idx := gaussUnique()
			idx.IsPrimary = tc.primary
			idx.Columns = tc.columns
			plan, err := w.PlanGaussDBIndex(context.Background(), idx)
			require.NoError(t, err)
			require.Equal(t, tc.global, plan.GaussDB.Global)
			sql := dialect.JoinSQL(w.dia.IndexStatements("App", plan))
			if tc.global && tc.primary {
				require.Contains(t, sql, "BY GLOBAL INDEX")
			} else if tc.global {
				require.Contains(t, sql, "CREATE GLOBAL UNIQUE INDEX")
			} else {
				require.NotContains(t, sql, "GLOBAL")
			}
			require.NotContains(t, sql, "IF NOT EXISTS")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestGaussDBGSIRejectsUnverifiedCapabilitiesWithoutDDL(t *testing.T) {
	for _, tc := range []struct{ name, version, free, option, want string }{
		{"older kernel", "GaussDB Kernel 505.2.1", "off", "1", "尚未验证"},
		{"GTM free", gaussTestVersion, "on", "2", "GTM-LITE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := gaussTestWriter(t)
			gaussExpectTable(mock, 42, "1")
			gaussExpectGTM(mock, tc.version, tc.free, tc.option)
			_, err := w.PlanGaussDBIndex(context.Background(), gaussUnique())
			require.ErrorContains(t, err, tc.want)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestGaussDBRepairRejectsTableRecreationBeforeDDL(t *testing.T) {
	w, mock := gaussTestWriter(t)
	gaussExpectTable(mock, 42, "1")
	gaussExpectGTM(mock, gaussTestVersion, "off", "1")
	idx, err := w.PlanGaussDBIndex(context.Background(), gaussUnique())
	require.NoError(t, err)
	gaussExpectTable(mock, 43, "1")
	gaussExpectGTM(mock, gaussTestVersion, "off", "1")
	require.ErrorContains(t, w.CreateIndex(context.Background(), idx), "已变化")
	require.NoError(t, mock.ExpectationsWereMet())
}

func gaussExpectIndex(mock sqlmock.Sqlmock, valid bool, kind, keys string) {
	mock.ExpectQuery(regexp.QuoteMeta(gaussIndexSQL)).WithArgs("App", "Patients", false, "auto_id").WillReturnRows(gaussIndexRows(valid, kind, len(strings.Fields(keys)), 1, keys, "0"))
}
func gaussIndexRows(valid bool, kind string, totalColumns, keyColumns int, keys, options string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"oid", "kind", "method", "unique", "primary", "valid", "ready", "special", "keys", "options", "indnatts", "indnkeyatts"}).AddRow(88, kind, "ubtree", true, false, valid, true, false, keys, options, totalColumns, keyColumns)
}
func gaussExpectGlobalVerification(mock sqlmock.Sqlmock) {
	// GaussDB 507 reports one unique key plus four appended system attributes.
	gaussExpectIndex(mock, true, "G", "24 -11 -1 -3 -5")
	gaussExpectColumns(mock, 42)
	gaussExpectGlobalDistribution(mock, "1")
}
func gaussExpectGlobalDistribution(mock sqlmock.Sqlmock, distribution string) {
	mock.ExpectQuery("SELECT relname FROM pg_catalog.pg_class WHERE oid=\\$1").WithArgs(88).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("auto_id"))
	mock.ExpectQuery(regexp.QuoteMeta(gaussDistributionSQL)).WithArgs("App", "auto_id").WillReturnRows(sqlmock.NewRows([]string{"oid", "mode", "keys", "kind", "persistence", "partition", "options"}).AddRow(88, "H", distribution, "G", "p", "n", "{}"))
	mock.ExpectQuery(regexp.QuoteMeta(gaussColumnsSQL)).WithArgs(88).WillReturnRows(sqlmock.NewRows([]string{"n", "name", "type"}).AddRow(1, "auto_id", "bigint").AddRow(2, "Tenant", "integer")).RowsWillBeClosed()
}

func TestGaussDBUniqueIndexCreationAndDuplicateVerification(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{true: "matching duplicate", false: "new index"}[duplicate], func(t *testing.T) {
			w, mock := gaussTestWriter(t)
			gaussExpectTable(mock, 42, "1")
			gaussExpectGTM(mock, gaussTestVersion, "off", "1")
			idx, err := w.PlanGaussDBIndex(context.Background(), gaussUnique())
			require.NoError(t, err)
			gaussExpectTable(mock, 42, "1")
			gaussExpectGTM(mock, gaussTestVersion, "off", "1")
			exec := mock.ExpectExec(regexp.QuoteMeta(dialect.JoinSQL(w.dia.IndexStatements("App", idx))))
			if duplicate {
				exec.WillReturnError(&pq.Error{Code: "42P07"})
				gaussExpectGlobalVerification(mock)
			} else {
				exec.WillReturnResult(sqlmock.NewResult(0, 0))
			}
			gaussExpectGlobalVerification(mock)
			require.NoError(t, w.CreateIndex(context.Background(), idx))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestGaussDBIndexVerificationRejectsWrongDefinition(t *testing.T) {
	for _, tc := range []struct {
		name, kind, keys string
		valid            bool
	}{
		{"invalid", "G", "24", false}, {"ordinary same-name index", "i", "24", true}, {"wrong key", "G", "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := gaussTestWriter(t)
			gaussExpectTable(mock, 42, "1")
			gaussExpectGTM(mock, gaussTestVersion, "off", "1")
			idx, err := w.PlanGaussDBIndex(context.Background(), gaussUnique())
			require.NoError(t, err)
			gaussExpectIndex(mock, tc.valid, tc.kind, tc.keys)
			if tc.name == "wrong key" {
				gaussExpectColumns(mock, 42)
			}
			require.Error(t, w.verifyGaussDBIndex(context.Background(), idx))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestGaussDBIndexVerificationSeparatesKeysFromIncludedAttributes(t *testing.T) {
	for _, tc := range []struct {
		name, kind, keys, options string
		totalColumns, keyColumns  int
		columns                   []string
		descending                []bool
		want                      string
	}{
		{name: "reported GSI catalog", keys: "24 -11 -1 -3 -5", options: "0", totalColumns: 5, keyColumns: 1},
		{name: "GSI without included attributes", keys: "24", options: "0", totalColumns: 1, keyColumns: 1},
		{name: "GSI containing user and system attributes", keys: "24 1 3 -11 -1 -3 -5", options: "0", totalColumns: 7, keyColumns: 1},
		{name: "composite GSI with descending key", keys: "24 3 -11 -1 -3 -5", options: "0 3", totalColumns: 6, keyColumns: 2, columns: []string{"auto_id", "Tenant"}, descending: []bool{false, true}},
		{name: "ordinary unique index", kind: "i", keys: "1", options: "0", totalColumns: 1, keyColumns: 1, columns: []string{"patient_id"}},
		{name: "ordinary composite unique index", kind: "i", keys: "1 3", options: "0 1", totalColumns: 2, keyColumns: 2, columns: []string{"patient_id", "Tenant"}, descending: []bool{false, true}},
		{name: "wrong key with correct count", keys: "3 -11 -1 -3 -5", options: "0", totalColumns: 5, keyColumns: 1, want: "目标索引键列或顺序不匹配"},
		{name: "wrong composite key order", keys: "3 24 -11 -1 -3 -5", options: "0 3", totalColumns: 6, keyColumns: 2, columns: []string{"auto_id", "Tenant"}, descending: []bool{false, true}, want: "目标索引键列或顺序不匹配"},
		{name: "wrong descending direction", keys: "24 -11 -1 -3 -5", options: "1", totalColumns: 5, keyColumns: 1, want: "目标索引键列或顺序不匹配"},
		{name: "extra unique key is not an included attribute", keys: "24 3 -11 -1 -3 -5", options: "0 0", totalColumns: 6, keyColumns: 2, want: "目标索引键数量不匹配: 预期=1, indnkeyatts=2, indnatts=6"},
		{name: "missing composite unique key", keys: "24 -11 -1 -3 -5", options: "0", totalColumns: 5, keyColumns: 1, columns: []string{"auto_id", "Tenant"}, want: "目标索引键数量不匹配: 预期=2, indnkeyatts=1, indnatts=5"},
		{name: "no key columns", keys: "-11 -1 -3 -5", totalColumns: 4, keyColumns: 0, want: "目标索引目录元数据异常"},
		{name: "negative key count", keys: "24", options: "0", totalColumns: 1, keyColumns: -1, want: "目标索引目录元数据异常"},
		{name: "key count exceeds total", keys: "24", options: "0 0", totalColumns: 1, keyColumns: 2, want: "目标索引目录元数据异常"},
		{name: "indkey shorter than total", keys: "24 -11 -1 -3 -5", options: "0", totalColumns: 6, keyColumns: 1, want: "目标索引目录元数据异常"},
		{name: "indkey longer than total", keys: "24 -11 -1 -3 -5", options: "0", totalColumns: 4, keyColumns: 1, want: "目标索引目录元数据异常"},
		{name: "missing key direction", keys: "24 3 -11 -1 -3 -5", options: "0", totalColumns: 6, keyColumns: 2, columns: []string{"auto_id", "Tenant"}, want: "目标索引目录元数据异常"},
		{name: "extra key direction", keys: "24 -11 -1 -3 -5", options: "0 0", totalColumns: 5, keyColumns: 1, want: "目标索引目录元数据异常"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := gaussTestWriter(t)
			idx := gaussUnique()
			if tc.columns != nil {
				idx.Columns = tc.columns
			}
			idx.Descending = tc.descending
			kind := tc.kind
			if kind == "" {
				kind = "G"
			}
			idx.GaussDB = &source.GaussDBIndexPlan{
				Base:   source.GaussDBDistribution{Schema: "App", Table: "Patients", OID: 42, Mode: "H", Columns: []string{"patient_id"}},
				Global: kind == "G", Columns: []string{"auto_id"},
			}
			mock.ExpectQuery(regexp.QuoteMeta(gaussIndexSQL)).WithArgs("App", "Patients", false, "auto_id").WillReturnRows(gaussIndexRows(true, kind, tc.totalColumns, tc.keyColumns, tc.keys, tc.options))
			gaussExpectColumns(mock, 42)
			if kind == "G" && tc.want == "" {
				gaussExpectGlobalDistribution(mock, "1")
			}
			err := w.verifyGaussDBIndex(context.Background(), idx)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
				if tc.want == "目标索引目录元数据异常" {
					require.ErrorContains(t, err, "预期键列=")
					require.ErrorContains(t, err, "indnatts=")
					require.ErrorContains(t, err, "indnkeyatts=")
					require.ErrorContains(t, err, "indkey列数=")
					require.ErrorContains(t, err, "indoption列数=")
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestGaussDBDuplicateIndexRejectsDifferentUniqueKeys(t *testing.T) {
	w, mock := gaussTestWriter(t)
	gaussExpectTable(mock, 42, "1")
	gaussExpectGTM(mock, gaussTestVersion, "off", "1")
	idx, err := w.PlanGaussDBIndex(context.Background(), gaussUnique())
	require.NoError(t, err)
	gaussExpectTable(mock, 42, "1")
	gaussExpectGTM(mock, gaussTestVersion, "off", "1")
	mock.ExpectExec(regexp.QuoteMeta(dialect.JoinSQL(w.dia.IndexStatements("App", idx)))).WillReturnError(&pq.Error{Code: "42P07"})
	mock.ExpectQuery(regexp.QuoteMeta(gaussIndexSQL)).WithArgs("App", "Patients", false, "auto_id").WillReturnRows(gaussIndexRows(true, "G", 6, 2, "24 3 -11 -1 -3 -5", "0 0"))
	gaussExpectColumns(mock, 42)
	require.ErrorContains(t, w.CreateIndex(context.Background(), idx), "目标索引键数量不匹配: 预期=1, indnkeyatts=2, indnatts=6")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInspectRepairGaussDBGSIUsesKeyCountAndChecksDistribution(t *testing.T) {
	for _, tc := range []struct{ name, distribution, status string }{
		{"matching GSI", "1", "succeeded"},
		{"different GSI distribution", "2", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := gaussTestWriter(t)
			gaussExpectTable(mock, 42, "1")
			gaussExpectGTM(mock, gaussTestVersion, "off", "1")
			idx, err := w.PlanGaussDBIndex(context.Background(), gaussUnique())
			require.NoError(t, err)
			gaussExpectTable(mock, 42, "1")
			gaussExpectGTM(mock, gaussTestVersion, "off", "1")
			gaussExpectIndex(mock, true, "G", "24 -11 -1 -3 -5")
			gaussExpectColumns(mock, 42)
			gaussExpectGlobalDistribution(mock, tc.distribution)
			v, err := InspectRepair(context.Background(), w, repair.Object{Kind: "index", Schema: "App", Table: "Patients", TargetName: "auto_id", Index: &idx})
			require.NoError(t, err)
			require.Equal(t, tc.status, v.Status)
			if tc.status == "failed" {
				require.Contains(t, v.Message, "目标 GSI 分布列不匹配")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestGaussDBLegacyDistributionCannotReplay(t *testing.T) {
	w, mock := gaussTestWriter(t)
	_, err := ExecuteRepair(context.Background(), w, repair.Object{Kind: "distribution"}, nil, nil)
	require.ErrorContains(t, err, "不支持原地修改")
	require.NoError(t, mock.ExpectationsWereMet())
}
