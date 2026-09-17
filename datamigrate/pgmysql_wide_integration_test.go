package datamigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/target"
	"github.com/go-sql-driver/mysql"
)

// Target-only integration using the supplied full DDL, in a disposable database.
func TestPGMySQLWideTableIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_PGMYSQL_TARGET_DSN")
	if dsn == "" {
		t.Skip("set DBGOLD_PGMYSQL_TARGET_DSN for disposable MySQL fixtures")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ns := fmt.Sprintf("dbgold_wide_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, "CREATE DATABASE `"+ns+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(context.Background(), "DROP DATABASE `"+ns+"`")
	w, err := target.NewMySQL(dsn, ns, target.ConnPoolConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	profile, err := w.InspectMySQL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Parsed.Checks() {
		t.Skip("requires enforced MySQL CHECK constraints")
	}
	for _, name := range []string{"kgyjs_sqzb", "projectbiaoduan"} {
		t.Run(name, func(t *testing.T) {
			table := wideFixture(t, name)
			snap := &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{table}}
			p := &PGMySQLPreflight{Target: profile, Source: snap.Profile, snapshot: snap}
			m := NewMigrator(&pgPlanReader{snapshot: snap}, w, &Job{}, Config{Content: "data_only"})
			original, _, err := dialect.NewMySQL().PGTableStatements(ns, table, profile.Parsed, m.objName)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.ExecContext(ctx, original[1].SQL)
			var myerr *mysql.MySQLError
			if !errors.As(err, &myerr) || myerr.Number != 1118 {
				t.Fatalf("expected actual MySQL row-width failure, got %v", err)
			}
			table.Info.Columns = append([]source.ColumnInfo(nil), table.Info.Columns...)
			m.fitPGMySQLRow(p, &table)
			stmts, _, err := dialect.NewMySQL().PGTableStatements(ns, table, profile.Parsed, m.objName)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, stmts[1].SQL); err != nil {
				t.Fatal(err)
			}
			// A fresh data-only plan starts with untouched source metadata and must
			// rediscover LONGTEXT and its actual enforced length CHECK.
			p.tables = []source.PGTable{snap.Tables[0]}
			p.tables[0].Info.Columns = append([]source.ColumnInfo(nil), snap.Tables[0].Info.Columns...)
			if err := m.inspectPGMySQLTarget(ctx, p, db, true); err != nil {
				t.Fatal(err)
			}
			m.checkPGMySQLTextStructure(p)
			p.finish()
			if !p.Ready {
				t.Fatalf("data-only issues: %+v", p.Issues)
			}
			for _, c := range p.tables[0].Info.Columns {
				if !c.PGMySQLLongText {
					continue
				}
				qn := "`" + ns + "`.`" + name + "`"
				insert := "INSERT INTO " + qn + " (`rowguid`," + sqlsyntax.QuoteIdent(c.Name) + ") VALUES (?,?)"
				val := strings.Repeat("中😀", int(c.Length)/2)
				if _, err := db.ExecContext(ctx, insert, "good", val); err != nil {
					t.Fatal(err)
				}
				var back string
				if err := db.QueryRowContext(ctx, "SELECT "+sqlsyntax.QuoteIdent(c.Name)+" FROM "+qn+" WHERE rowguid='good'").Scan(&back); err != nil || back != val {
					t.Fatalf("round trip: err=%v same=%v", err, back == val)
				}
				_, err := db.ExecContext(ctx, insert, "bad", val+"中")
				if !errors.As(err, &myerr) || myerr.Number != 3819 {
					t.Fatalf("length CHECK failed to reject: %v", err)
				}
				if _, err := db.ExecContext(ctx, insert, "null", nil); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	// Actual Vastbase default: second-precision function on a microsecond column.
	local := `"localtimestamp"(0)`
	def := "'中文'::varchar"
	table := source.PGTable{Info: source.TableDDLInfo{TableName: "defaults", Columns: []source.ColumnInfo{
		{Name: "created", DataType: "timestamp", DateTimePrecision: 6, Default: &local},
		{Name: "body", DataType: "varchar", Length: 4000, PGMySQLLongText: true, Default: &def},
	}}}
	stmts, _, err := dialect.NewMySQL().PGTableStatements(ns, table, profile.Parsed, func(s string) string { return s })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, stmts[1].SQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO `"+ns+"`.`defaults` () VALUES ()"); err != nil {
		t.Fatal(err)
	}
	var present bool
	var body string
	if err := db.QueryRowContext(ctx, "SELECT created IS NOT NULL,body FROM `"+ns+"`.`defaults`").Scan(&present, &body); err != nil || !present || body != "中文" {
		t.Fatal(present, body, err)
	}
	var micros int
	if err := db.QueryRowContext(ctx, "SELECT MICROSECOND(created) FROM `"+ns+"`.`defaults`").Scan(&micros); err != nil || micros != 0 {
		t.Fatalf("default lost second precision: micros=%d err=%v", micros, err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO `"+ns+"`.`defaults` (created,body) VALUES ('2026-09-17 12:34:56.123456','explicit')"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT MICROSECOND(created) FROM `"+ns+"`.`defaults` WHERE body='explicit'").Scan(&micros); err != nil || micros != 123456 {
		t.Fatalf("column precision narrowed: micros=%d err=%v", micros, err)
	}
}
