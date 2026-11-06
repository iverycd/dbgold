package target

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	"github.com/stretchr/testify/require"
)

// The DSNs explicitly opt into DDL in a disposable test database. Each test
// creates its own schema; no existing application objects are touched.
func TestSQLServerNumericCOPYIntegration(t *testing.T) {
	for _, database := range []struct{ name, variable string }{
		{"postgres", "DBGOLD_TEST_POSTGRES_DSN"},
		{"kingbase", "DBGOLD_TEST_KINGBASE_DSN"},
	} {
		t.Run(database.name, func(t *testing.T) {
			dsn := os.Getenv(database.variable)
			if dsn == "" {
				t.Skip(database.variable + " is not set")
			}
			db, err := sql.Open("postgres", dsn)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			require.NoError(t, db.PingContext(ctx))
			schema := fmt.Sprintf("dbgold_numeric_copy_%d", time.Now().UnixNano())
			_, err = db.ExecContext(ctx, "CREATE SCHEMA "+source.QuotePGIdent(schema))
			require.NoError(t, err)
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_, err := db.ExecContext(cleanup, "DROP SCHEMA "+source.QuotePGIdent(schema)+" CASCADE")
				require.NoError(t, err)
			})
			qualified := source.QuotePGIdent(schema) + `."values"`
			_, err = db.ExecContext(ctx, "CREATE TABLE "+qualified+` ("id" integer PRIMARY KEY, "amount" decimal(38,18), "exact_int" numeric(38,0), "cash" decimal(19,4), "small_cash" decimal(10,4), "payload" bytea)`)
			require.NoError(t, err)
			writer := &KingbaseWriter{db: db, schema: schema, valueConv: valueconv.NewPostgres()}
			writer.SetSourceType("sqlserver")
			const decimal = "12345678901234567890.123456789012345678"
			const numeric = "-12345678901234567890123456789012345678"
			payload := []byte{0, 255, 0}
			require.NoError(t, writer.CopyData(ctx, "values", []string{"id", "amount", "exact_int", "cash", "small_cash", "payload"}, []string{"INT", "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY", "VARBINARY"}, [][]any{
				{int64(1), []byte(decimal), []byte(numeric), []byte("-922337203685477.5808"), []byte("214748.3647"), payload},
				{int64(2), []byte("0"), []byte("0"), []byte("0.0000"), []byte("0.0000"), []byte{}},
				{int64(3), nil, nil, nil, nil, nil},
			}))
			var amount, integer, cash, smallCash string
			var gotPayload []byte
			require.NoError(t, db.QueryRowContext(ctx, `SELECT "amount"::text, "exact_int"::text, "cash"::text, "small_cash"::text, "payload" FROM `+qualified+` WHERE "id"=1`).Scan(&amount, &integer, &cash, &smallCash, &gotPayload))
			require.Equal(t, decimal, amount)
			require.Equal(t, numeric, integer)
			require.Equal(t, "-922337203685477.5808", cash)
			require.Equal(t, "214748.3647", smallCash)
			require.Equal(t, payload, gotPayload)
			require.NoError(t, db.QueryRowContext(ctx, `SELECT "amount"::text, "exact_int"::text, "cash"::text, "small_cash"::text, "payload" FROM `+qualified+` WHERE "id"=2`).Scan(&amount, &integer, &cash, &smallCash, &gotPayload))
			require.Equal(t, "0.000000000000000000", amount)
			require.Equal(t, "0", integer)
			require.Equal(t, "0.0000", cash)
			require.Equal(t, "0.0000", smallCash)
			require.NotNil(t, gotPayload)
			require.Empty(t, gotPayload)
			var nullRows int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM `+qualified+` WHERE "id"=3 AND "amount" IS NULL AND "exact_int" IS NULL AND "cash" IS NULL AND "small_cash" IS NULL AND "payload" IS NULL`).Scan(&nullRows))
			require.Equal(t, 1, nullRows)
		})
	}
}
