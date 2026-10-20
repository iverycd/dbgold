package target

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOracleDMViewSemanticsProbeOnlyUsedFeaturesAndCache(t *testing.T) {
	w, mock := mockDM(t)
	q, _, err := oracleDMViewSentinel("operator_concat", false)
	require.NoError(t, err)
	mock.ExpectQuery("^" + regexp.QuoteMeta(q) + "$").WillReturnRows(sqlmock.NewRows([]string{"ok"}).AddRow(1))
	for range 2 {
		require.NoError(t, w.ValidateOracleDamengView(context.Background(), `SELECT a || b c FROM T`))
	}
	require.NoError(t, w.ValidateOracleDamengView(context.Background(), `SELECT 'TRIM(x) / ||' c FROM T /* SOUNDEX(x) */`))
}

func TestOracleDMViewSemanticsFailureIsExplicitAndCached(t *testing.T) {
	for _, err := range []error{nil, errors.New("function unavailable")} {
		w, mock := mockDM(t)
		query, _, e := oracleDMViewSentinel("TRIM", false)
		require.NoError(t, e)
		expect := mock.ExpectQuery("^" + regexp.QuoteMeta(query) + "$")
		if err == nil {
			expect.WillReturnRows(sqlmock.NewRows([]string{"ok"}).AddRow(0))
		} else {
			expect.WillReturnError(err)
		}
		for range 2 {
			e = w.ValidateOracleDamengView(context.Background(), `SELECT TRIM(a) c FROM T`)
			require.ErrorContains(t, e, "TRIM")
			require.ErrorContains(t, e, "未执行该视图 DDL")
		}
	}
}

func TestOracleDMViewDateCastUsesCurrentDialectProfile(t *testing.T) {
	w, mock := mockDM(t)
	for _, useDate := range []bool{false, true} {
		w.dia.(*dialect.DaMengDialect).SetOracleDamengDate(useDate)
		query, _, err := oracleDMViewSentinel("cast_date", useDate)
		require.NoError(t, err)
		mock.ExpectQuery("^"+regexp.QuoteMeta(query)+"$").WithArgs("2024-01-02 03:04:05.900000000", "2024-01-02 03:04:05").WillReturnRows(sqlmock.NewRows([]string{"ok"}).AddRow(1))
		require.NoError(t, w.ValidateOracleDamengView(context.Background(), `SELECT CAST(a AS DATE) c FROM T`))
	}
}

func TestOracleDMViewSentinelsCoverTheFiniteSupportedSubset(t *testing.T) {
	w, mock := mockDM(t)
	for _, name := range []string{"CONCAT", "LPAD", "RPAD", "SOUNDEX", "LAST_DAY", "TRIM", "LTRIM", "RTRIM", "REPLACE", "TRANSLATE", "SUBSTR"} {
		query, args, err := oracleDMViewSentinel(name, false)
		require.NoError(t, err)
		expect := mock.ExpectQuery("^" + regexp.QuoteMeta(query) + "$")
		if len(args) == 3 {
			expect.WithArgs(args[0], args[1], args[2])
		} else if len(args) > 0 {
			expect.WithArgs(args[0], args[1])
		}
		expect.WillReturnRows(sqlmock.NewRows([]string{"ok"}).AddRow(1))
		require.NoError(t, w.ValidateOracleDamengView(context.Background(), "SELECT "+name+"(a) c FROM T"))
	}
	query, _, err := oracleDMViewSentinel("integer_division", false)
	require.NoError(t, err)
	mock.ExpectQuery("^" + regexp.QuoteMeta(query) + "$").WillReturnRows(sqlmock.NewRows([]string{"ok"}).AddRow(1))
	require.NoError(t, w.ValidateOracleDamengView(context.Background(), `SELECT a/2 c FROM T`))
	require.ErrorContains(t, w.ValidateOracleDamengView(context.Background(), `SELECT SUBSTRB(a,1,2) c FROM T`), "字符集")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, w.ValidateOracleDamengView(ctx, `SELECT a FROM T`), context.Canceled)
}
