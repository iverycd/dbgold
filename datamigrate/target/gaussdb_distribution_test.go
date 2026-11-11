package target

import (
	"context"
	"regexp"
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
	mock.ExpectQuery(regexp.QuoteMeta(gaussColumnsSQL)).WithArgs(oid).WillReturnRows(sqlmock.NewRows([]string{"n", "name", "type"}).AddRow(1, "patient_id", "integer").AddRow(2, "auto_id", "bigint").AddRow(3, "Tenant", "integer")).RowsWillBeClosed()
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
		{"existing different distribution", "2", []string{"patient_id"}, true, true},
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
	mock.ExpectQuery(regexp.QuoteMeta(gaussIndexSQL)).WithArgs("App", "Patients", false, "auto_id").WillReturnRows(sqlmock.NewRows([]string{"oid", "kind", "method", "unique", "primary", "valid", "ready", "special", "keys", "options"}).AddRow(88, kind, "ubtree", true, false, valid, true, false, keys, "0"))
}
func gaussExpectGlobalVerification(mock sqlmock.Sqlmock) {
	gaussExpectIndex(mock, true, "G", "2")
	gaussExpectColumns(mock, 42)
	mock.ExpectQuery("SELECT relname FROM pg_catalog.pg_class WHERE oid=\\$1").WithArgs(88).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("auto_id"))
	mock.ExpectQuery(regexp.QuoteMeta(gaussDistributionSQL)).WithArgs("App", "auto_id").WillReturnRows(sqlmock.NewRows([]string{"oid", "mode", "keys", "kind", "persistence", "partition", "options"}).AddRow(88, "H", "1", "G", "p", "n", "{}"))
	mock.ExpectQuery(regexp.QuoteMeta(gaussColumnsSQL)).WithArgs(88).WillReturnRows(sqlmock.NewRows([]string{"n", "name", "type"}).AddRow(1, "auto_id", "bigint"))
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
		{"invalid", "G", "2", false}, {"ordinary same-name index", "i", "2", true}, {"wrong key", "G", "1", true},
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

func TestGaussDBLegacyDistributionCannotReplay(t *testing.T) {
	w, mock := gaussTestWriter(t)
	_, err := ExecuteRepair(context.Background(), w, repair.Object{Kind: "distribution"}, nil, nil)
	require.ErrorContains(t, err, "不支持原地修改")
	require.NoError(t, mock.ExpectationsWereMet())
}
