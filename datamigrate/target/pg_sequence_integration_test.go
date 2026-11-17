package target

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// Opt-in DSNs must identify disposable test databases, including the affected
// legacy version when available. No business schema or sequence is modified.
func TestPGSequenceCompatibilityIntegration(t *testing.T) {
	for _, product := range pgSequenceProducts {
		t.Run(product, func(t *testing.T) {
			variable := "DBGOLD_TEST_" + strings.ToUpper(product) + "_DSN"
			dsn := os.Getenv(variable)
			if dsn == "" {
				t.Skip(variable + " is not set")
			}
			driver := "postgres"
			if product == "gaussdb" {
				driver = "opengauss"
			} else if product == "highgo" {
				driver = "highgo"
			}
			db, err := sql.Open(driver, dsn)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			db.SetMaxOpenConns(1)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			schema := fmt.Sprintf("dbgold_Seq_%d", time.Now().UnixNano())
			d := dialect.NewPostgres(product)
			exec := func(query string) { t.Helper(); _, err := db.ExecContext(ctx, query); require.NoError(t, err) }
			exec("CREATE SCHEMA " + d.QuoteIdent(schema))
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, err := db.ExecContext(cleanup, "DROP SCHEMA "+d.QuoteIdent(schema)+" CASCADE")
				require.NoError(t, err)
			})
			otherSchema := schema + "_other"
			exec("CREATE SCHEMA " + d.QuoteIdent(otherSchema))
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, err := db.ExecContext(cleanup, "DROP SCHEMA "+d.QuoteIdent(otherSchema)+" CASCADE")
				require.NoError(t, err)
			})
			w := pgSequenceWriter(db, schema, product)
			seq := source.SequenceInfo{TableName: "Orders", ColumnName: "ID", StartValue: 100}
			table := d.QualifyTable(schema, seq.TableName)
			sequence := d.QualifyTable(schema, dialect.SequenceName(d, seq))
			exec("CREATE TABLE " + table + ` ("ID" bigint, "OtherID" bigint)`)
			require.NoError(t, w.CreateSequence(ctx, seq, ""))
			assertState := func(qualified string, wantLast int64, wantCalled bool) {
				t.Helper()
				var last int64
				var called bool
				require.NoError(t, db.QueryRowContext(ctx, "SELECT last_value,is_called FROM "+qualified).Scan(&last, &called))
				require.Equal(t, wantLast, last)
				require.Equal(t, wantCalled, called)
			}
			assertState(sequence, 100, false)
			seq.StartValue = 1 // A retry's requested start must not reset an existing sequence.
			require.NoError(t, w.CreateSequence(ctx, seq, ""))
			assertState(sequence, 100, false)
			var value int64
			require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO "+table+` DEFAULT VALUES RETURNING "ID"`).Scan(&value))
			require.Equal(t, int64(100), value)
			exec("SELECT setval(" + repairLiteral(sequence) + ",500,true)")
			require.NoError(t, w.CreateSequence(ctx, seq, ""))
			assertState(sequence, 500, true)
			require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO "+table+` DEFAULT VALUES RETURNING "ID"`).Scan(&value))
			require.Equal(t, int64(501), value, "reusing an owned sequence must not rewind it")

			// A partial prior run may leave a sequence without OWNED BY.
			exec("ALTER SEQUENCE " + sequence + " OWNED BY NONE")
			// Retaining only this column's DEFAULT reference is safe as well.
			require.NoError(t, w.CreateSequence(ctx, seq, ""))
			assertState(sequence, 501, true)
			exec("ALTER SEQUENCE " + sequence + " OWNED BY NONE")
			exec("ALTER TABLE " + table + ` ALTER COLUMN "ID" DROP DEFAULT`)
			require.NoError(t, w.CreateSequence(ctx, seq, ""))
			require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO "+table+` DEFAULT VALUES RETURNING "ID"`).Scan(&value))
			require.Equal(t, int64(502), value)

			// Check catalog dependencies themselves, not only mocked query outcomes.
			other := d.QualifyTable(schema, "Other")
			exec("CREATE TABLE " + other + ` ("ID" bigint)`)
			crossSchema := d.QualifyTable(otherSchema, "Orders")
			exec("CREATE TABLE " + crossSchema + ` ("ID" bigint)`)
			// OWNED BY NONE does not mean unused: another column's default
			// can still reference this sequence (including across schemas).
			exec("ALTER SEQUENCE " + sequence + " OWNED BY NONE")
			exec("ALTER TABLE " + table + ` ALTER COLUMN "ID" DROP DEFAULT`)
			for _, binding := range []struct{ table, column string }{
				{table, "OtherID"}, {other, "ID"}, {crossSchema, "ID"},
			} {
				exec("ALTER TABLE " + binding.table + " ALTER COLUMN " + d.QuoteIdent(binding.column) + " SET DEFAULT nextval(" + repairLiteral(sequence) + ")")
				require.ErrorContains(t, w.CreateSequence(ctx, seq, ""), "其他表列")
				var def sql.NullString
				require.NoError(t, db.QueryRowContext(ctx, `SELECT column_default FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2 AND column_name=$3`, schema, "Orders", "ID").Scan(&def))
				require.False(t, def.Valid, "a shared sequence must not be bound to the intended column")
				assertState(sequence, 502, true)
				exec("ALTER TABLE " + binding.table + " ALTER COLUMN " + d.QuoteIdent(binding.column) + " DROP DEFAULT")
			}
			for _, binding := range []string{table + `."OtherID"`, other + `."ID"`} {
				exec("ALTER SEQUENCE " + sequence + " OWNED BY " + binding)
				exec("ALTER TABLE " + table + ` ALTER COLUMN "ID" DROP DEFAULT`)
				require.ErrorContains(t, w.CreateSequence(ctx, seq, ""), "已归属其他表列")
				var def sql.NullString
				require.NoError(t, db.QueryRowContext(ctx, `SELECT column_default FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2 AND column_name=$3`, schema, "Orders", "ID").Scan(&def))
				require.False(t, def.Valid, "a conflict must not change the intended column default")
			}
			exec("ALTER SEQUENCE " + sequence + " OWNED BY NONE")
			exec("DROP SEQUENCE " + sequence)
			exec("CREATE TABLE " + sequence + " (id integer)")
			require.ErrorContains(t, w.CreateSequence(ctx, seq, ""), "不是序列")

			// Real partial success: CREATE succeeds but the binding column is
			// missing. Adding the column and retrying must preserve both state fields.
			retry := source.SequenceInfo{TableName: "Retry", ColumnName: "ID", StartValue: 10}
			retryTable := d.QualifyTable(schema, retry.TableName)
			retrySequence := d.QualifyTable(schema, dialect.SequenceName(d, retry))
			exec("CREATE TABLE " + retryTable + " (other integer)")
			require.Error(t, w.CreateSequence(ctx, retry, ""))
			assertState(retrySequence, 10, false)
			exec("SELECT setval(" + repairLiteral(retrySequence) + ",900,false)")
			exec("ALTER TABLE " + retryTable + ` ADD COLUMN "ID" bigint`)
			retry.StartValue = 1
			require.NoError(t, w.CreateSequence(ctx, retry, ""))
			assertState(retrySequence, 900, false)
			require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO "+retryTable+` DEFAULT VALUES RETURNING "ID"`).Scan(&value))
			require.Equal(t, int64(900), value)
		})
	}
}
