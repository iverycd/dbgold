package target

import (
	"context"
	"database/sql"
	"encoding/hex"
	"regexp"
	"testing"

	"dbgold/datamigrate/valueconv"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestPGWritersSpatialCopyBindingsAndRollback(t *testing.T) {
	factories := map[string]func(*sql.DB) Writer{
		"postgres": func(db *sql.DB) Writer {
			return &PostgresWriter{db: db, schema: "App", srcType: "mysql", valueConv: valueconv.NewPostgres()}
		},
		"gaussdb": func(db *sql.DB) Writer {
			return &GaussDBWriter{db: db, schema: "App", srcType: "mysql", valueConv: valueconv.NewPostgres()}
		},
		"highgo": func(db *sql.DB) Writer {
			return &HighGoWriter{db: db, schema: "App", srcType: "mysql", valueConv: valueconv.NewPostgres()}
		},
		"kingbase": func(db *sql.DB) Writer {
			return &KingbaseWriter{db: db, schema: "App", srcType: "mysql", valueConv: valueconv.NewPostgres()}
		},
	}
	raw, err := hex.DecodeString("e61000000101000000000000000000f03f0000000000000040")
	require.NoError(t, err)
	const expected = "0101000020e6100000000000000000f03f0000000000000040"
	for name, factory := range factories {
		for _, valid := range []bool{true, false} {
			t.Run(name+map[bool]string{true: "/valid", false: "/invalid"}[valid], func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer db.Close()
				writer := factory(db)
				mock.ExpectBegin()
				stmt := mock.ExpectPrepare(regexp.QuoteMeta(`COPY "App"."Shapes" ("Shape") FROM STDIN`))
				rows := [][]any{{raw}, {nil}}
				if valid {
					stmt.ExpectExec().WithArgs(expected).WillReturnResult(sqlmock.NewResult(0, 1))
					stmt.ExpectExec().WithArgs(nil).WillReturnResult(sqlmock.NewResult(0, 1))
					stmt.ExpectExec().WithArgs().WillReturnResult(sqlmock.NewResult(0, 2))
					mock.ExpectCommit()
				} else {
					rows = [][]any{{raw[:8]}}
					mock.ExpectRollback()
				}
				err = writer.CopyData(context.Background(), "Shapes", []string{"Shape"}, []string{"GEOMETRY"}, rows)
				if valid {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "Shapes")
					require.ErrorContains(t, err, "第 1 列")
					require.ErrorContains(t, err, "Shape")
				}
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}
