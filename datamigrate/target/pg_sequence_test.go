package target

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	gausspq "gitee.com/opengauss/openGauss-connector-go-pq"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

var pgSequenceProducts = []string{"postgres", "gaussdb", "highgo", "kingbase", "vastbase", "gbase", "seabox"}

func pgSequenceWriter(db *sql.DB, schema, product string) Writer {
	d := dialect.NewPostgres(product)
	w := &PostgresWriter{db: db, schema: schema, dbType: product, dia: d}
	switch product {
	case "gaussdb":
		return &GaussDBWriter{db: db, schema: schema, dia: d}
	case "highgo":
		return &HighGoWriter{db: db, schema: schema, dia: d}
	case "kingbase":
		return &KingbaseWriter{db: db, schema: schema, dia: d}
	case "vastbase":
		return &VastbaseWriter{PostgresWriter: w}
	case "gbase":
		return &GBaseWriter{PostgresWriter: w}
	default:
		return w
	}
}

func expectPGSequenceReuse(m sqlmock.Sqlmock, schema string, seq source.SequenceInfo) *sqlmock.ExpectedQuery {
	return m.ExpectQuery(regexp.QuoteMeta(pgSequenceReuseSQL)).WithArgs(schema, "seq_"+seq.TableName+"_"+seq.ColumnName, seq.TableName, seq.ColumnName)
}
func pgSequenceReuseRows(kind string, reusable bool) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"relkind", "reusable"}).AddRow(kind, reusable)
}

func TestPGSequenceWritersCreateAndRepeat(t *testing.T) {
	for _, product := range pgSequenceProducts {
		t.Run(product, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			db.SetMaxOpenConns(1)
			w := pgSequenceWriter(db, "App", product)
			seq := source.SequenceInfo{TableName: "Orders", ColumnName: "ID", StartValue: 100}
			ss := w.Dialect().SequenceStatements("App", seq, "App")
			for _, duplicate := range []bool{false, true} {
				create := m.ExpectExec(regexp.QuoteMeta(ss[0].SQL))
				if duplicate {
					if product == "gaussdb" {
						create.WillReturnError(&gausspq.Error{Code: "42P07", Message: "already exists"})
					} else {
						create.WillReturnError(&pq.Error{Code: "42P07", Message: "already exists"})
					}
					expectPGSequenceReuse(m, "App", seq).WillReturnRows(pgSequenceReuseRows("S", true))
				} else {
					create.WillReturnResult(sqlmock.NewResult(0, 0))
				}
				for _, s := range ss[1:] {
					m.ExpectExec(regexp.QuoteMeta(s.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
				}
				var events []repair.Event
				ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { events = append(events, e); return nil })
				require.NoError(t, w.CreateSequence(ctx, seq, "App"))
				require.Len(t, events, 2*len(ss))
				for i, s := range ss {
					require.Equal(t, repair.Event{SQL: s.SQL, Status: "running"}, events[2*i])
					require.Equal(t, repair.Event{SQL: s.SQL, Status: "succeeded"}, events[2*i+1])
				}
			}
			// Only CREATE and the planned bindings may execute, never DROP/setval/RESTART.
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestPGSequenceDuplicateInspection(t *testing.T) {
	for _, tc := range []struct {
		name, state, kind, want string
		reusable, noRows        bool
		queryErr                error
	}{
		{name: "unowned or same column", state: "42710", kind: "S", reusable: true},
		{name: "same name table", state: "42P07", kind: "r", reusable: true, want: "不是序列"},
		{name: "other table or column", state: "42P07", kind: "S", want: "已归属其他表列"},
		{name: "missing exact name", state: "42P07", noRows: true, want: "核验同名序列"},
		{name: "catalog denied", state: "42P07", queryErr: errors.New("catalog denied"), want: "catalog denied"},
		{name: "create denied", state: "42501", want: "create failed"},
		{name: "syntax error", state: "42601", want: "create failed"},
		{name: "connection lost", state: "08006", want: "create failed"},
		{name: "unique violation", state: "23505", want: "create failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			db.SetMaxOpenConns(1)
			w := pgSequenceWriter(db, "App", "postgres")
			seq := source.SequenceInfo{TableName: "Orders", ColumnName: "ID", StartValue: 100}
			ss := w.Dialect().SequenceStatements("App", seq, "")
			m.ExpectExec(regexp.QuoteMeta(ss[0].SQL)).WillReturnError(&pq.Error{Code: pq.ErrorCode(tc.state), Message: "create failed"})
			if tc.state == "42P07" || tc.state == "42710" {
				q := expectPGSequenceReuse(m, "App", seq)
				switch {
				case tc.queryErr != nil:
					q.WillReturnError(tc.queryErr)
				case tc.noRows:
					q.WillReturnRows(sqlmock.NewRows([]string{"relkind", "reusable"}))
				default:
					q.WillReturnRows(pgSequenceReuseRows(tc.kind, tc.reusable))
				}
			}
			if tc.want == "" {
				for _, s := range ss[1:] {
					m.ExpectExec(regexp.QuoteMeta(s.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
				}
			}
			var events []repair.Event
			ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { events = append(events, e); return nil })
			err = w.CreateSequence(ctx, seq, "")
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
				var pgErr *pq.Error
				require.ErrorAs(t, err, &pgErr)
				require.Equal(t, pq.ErrorCode(tc.state), pgErr.Code)
				require.Len(t, events, 2)
				require.NotEqual(t, "succeeded", events[1].Status)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestPGSequenceUnqualifiedSchema(t *testing.T) {
	for _, mode := range []string{"resolved", "null", "empty", "error"} {
		t.Run(mode, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			w := pgSequenceWriter(db, "", "postgres")
			seq := source.SequenceInfo{TableName: "Orders", ColumnName: "ID", StartValue: 1}
			ss := w.Dialect().SequenceStatements("", seq, "")
			m.ExpectExec(regexp.QuoteMeta(ss[0].SQL)).WillReturnError(&pq.Error{Code: "42P07"})
			q := m.ExpectQuery(regexp.QuoteMeta("SELECT current_schema()"))
			switch mode {
			case "resolved":
				q.WillReturnRows(sqlmock.NewRows([]string{"schema"}).AddRow("MixedSchema"))
			case "null":
				q.WillReturnRows(sqlmock.NewRows([]string{"schema"}).AddRow(nil))
			case "empty":
				q.WillReturnRows(sqlmock.NewRows([]string{"schema"}).AddRow(""))
			case "error":
				q.WillReturnError(errors.New("schema unavailable"))
			}
			if mode == "resolved" {
				expectPGSequenceReuse(m, "MixedSchema", seq).WillReturnRows(pgSequenceReuseRows("S", true))
				for _, s := range ss[1:] {
					m.ExpectExec(regexp.QuoteMeta(s.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
				}
			}
			err = w.CreateSequence(context.Background(), seq, "")
			if mode == "resolved" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestPGSequenceBindingFailureCanRetry(t *testing.T) {
	for _, failedStep := range []int{1, 2, 3} {
		t.Run(string(rune('0'+failedStep)), func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			w := pgSequenceWriter(db, "App", "postgres")
			seq := source.SequenceInfo{TableName: "Orders", ColumnName: "ID", StartValue: 100}
			ss := w.Dialect().SequenceStatements("App", seq, "App")
			for i, s := range ss[:failedStep+1] {
				ex := m.ExpectExec(regexp.QuoteMeta(s.SQL))
				if i == failedStep {
					ex.WillReturnError(errors.New("binding denied"))
				} else {
					ex.WillReturnResult(sqlmock.NewResult(0, 0))
				}
			}
			require.ErrorContains(t, w.CreateSequence(context.Background(), seq, "App"), "binding denied")
			m.ExpectExec(regexp.QuoteMeta(ss[0].SQL)).WillReturnError(&pq.Error{Code: "42P07"})
			expectPGSequenceReuse(m, "App", seq).WillReturnRows(pgSequenceReuseRows("S", true))
			for _, s := range ss[1:] {
				m.ExpectExec(regexp.QuoteMeta(s.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			require.NoError(t, w.CreateSequence(context.Background(), seq, "App"))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestPGSequenceObserverFailureIsNotIgnored(t *testing.T) {
	for _, status := range []string{"running", "succeeded"} {
		t.Run(status, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			w := pgSequenceWriter(db, "App", "postgres")
			seq := source.SequenceInfo{TableName: "Orders", ColumnName: "ID", StartValue: 1}
			if status == "succeeded" {
				ss := w.Dialect().SequenceStatements("App", seq, "")
				m.ExpectExec(regexp.QuoteMeta(ss[0].SQL)).WillReturnError(&pq.Error{Code: "42P07"})
				expectPGSequenceReuse(m, "App", seq).WillReturnRows(pgSequenceReuseRows("S", true))
			}
			persistErr := &pq.Error{Code: "42P07", Message: "ledger unavailable"}
			ctx := repair.WithObserver(context.Background(), func(e repair.Event) error {
				if e.Status == status {
					return persistErr
				}
				return nil
			})
			require.ErrorIs(t, w.CreateSequence(ctx, seq, ""), persistErr)
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
