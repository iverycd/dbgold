package datamigrate

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
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/typemap"
)

// Opt-in tests use disposable databases only, on MySQL 5.7 and 8.0.
func TestPGMySQLTextComparisonIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_PGMYSQL_TARGET_DSN")
	if dsn == "" {
		t.Skip("set DBGOLD_PGMYSQL_TARGET_DSN for disposable MySQL fixtures")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ns := fmt.Sprintf("dbgold_text_%d", time.Now().UnixNano())
	qn := "`" + ns + "`"
	exec("CREATE DATABASE " + qn + " CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	defer db.ExecContext(context.Background(), "DROP DATABASE "+qn)
	w, err := target.NewMySQL(dsn, ns, target.ConnPoolConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	profile, err := w.InspectMySQL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	collations := []string{"utf8mb4_general_ci", "utf8mb4_bin", "utf8mb4_unicode_ci", "latin1_swedish_ci"}
	if profile.Parsed.Major == 8 {
		collations = append(collations, "utf8mb4_0900_ai_ci")
	}
	for _, collation := range collations {
		t.Run(collation, func(t *testing.T) {
			rules, err := target.ReadMySQLTextRules(ctx, db, collation)
			if err != nil {
				t.Fatal(err)
			}
			exec("ALTER DATABASE " + qn + " CHARACTER SET " + rules.Charset + " COLLATE " + rules.Collation)
			profile, err := w.InspectMySQL(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if profile.TextRules() != rules {
				t.Fatal(profile, rules)
			}
			for _, dataType := range []string{"varchar", "bpchar"} {
				t.Run(dataType, func(t *testing.T) {
					col := source.ColumnInfo{Name: "k", DataType: dataType, Length: 20, IsNullable: true}
					table := source.PGTable{Info: source.TableDDLInfo{TableName: "sample", Columns: []source.ColumnInfo{col, {Name: "n", DataType: "int4"}}}}
					ddl, _, err := dialect.NewMySQL().PGTableStatements(ns, table, profile.Parsed, func(s string) string { return s })
					if err != nil {
						t.Fatal(err)
					}
					for _, stmt := range ddl {
						exec(stmt.SQL)
					}
					var actualCollation string
					if err = db.QueryRowContext(ctx, `SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='sample' AND COLUMN_NAME='k'`, ns).Scan(&actualCollation); err != nil || actualCollation != collation {
						t.Fatal(actualCollation, err)
					}
					if strings.Contains(ddl[1].SQL, "COLLATE") || strings.Contains(ddl[1].SQL, "CHARSET") {
						t.Fatal(ddl)
					}
					pairs := [][2]string{{"a", "A"}, {"e", "é"}, {"x", "x "}, {"", " "}, {"ß", "ss"}, {"é", "é "}}
					if rules.Charset == "utf8mb4" {
						pairs = append(pairs, [2]string{"x", "x\u200b"}, [2]string{"e\u0301", "é"})
					}
					for _, pair := range pairs {
						exec("TRUNCATE TABLE " + qn + ".sample")
						exec("INSERT INTO "+qn+".sample(k,n) VALUES (?,1),(?,2)", pair[0], pair[1])
						var equal bool
						if err = db.QueryRowContext(ctx, "SELECT a.k=b.k FROM "+qn+".sample a JOIN "+qn+".sample b ON a.n=1 AND b.n=2").Scan(&equal); err != nil {
							t.Fatal(err)
						}
						values := [][]any{{pair[0], int64(1)}, {pair[1], int64(1)}}
						weighted, _, err := inspectPGTextPage(ctx, db, table, map[string]target.MySQLTextRules{"k": rules}, values, map[string]bool{"k": true}, profile.MaxAllowedPacket)
						if err != nil {
							t.Fatal(err)
						}
						a, _, _ := pgKey(weighted[0], table.Info.Columns, []string{"k", "n"})
						b, _, _ := pgKey(weighted[1], table.Info.Columns, []string{"k", "n"})
						if (a == b) != equal {
							t.Fatalf("pair=%q MySQL equal=%v weights equal=%v", pair, equal, a == b)
						}
					}
					// A NULL in a composite unique key remains exempt.
					weights, _, err := inspectPGTextPage(ctx, db, table, map[string]target.MySQLTextRules{"k": rules}, [][]any{{nil, int64(1)}}, map[string]bool{"k": true}, profile.MaxAllowedPacket)
					if err != nil {
						t.Fatal(err)
					}
					if _, valid, _ := pgKey(weights[0], table.Info.Columns, []string{"k", "n"}); valid {
						t.Fatal("NULL unique key considered comparable")
					}
					// Validate CHECK against actual stored-column comparison, including CHAR storage.
					if profile.Parsed.Checks() {
						table.Checks = []source.PGObject{{Name: "allowed", Definition: `CHECK ("k" = 'A')`}}
						m := &Migrator{reader: &pgPlanReader{}, writer: w, cfg: Config{LowerCaseNames: true}}
						q, err := m.pgRowCheckQuery(table, true, map[string]target.MySQLTextRules{"k": rules})
						if err != nil {
							t.Fatal(err)
						}
						for _, value := range []string{"a", "A", "A ", "á"} {
							exec("TRUNCATE TABLE " + qn + ".sample")
							exec("INSERT INTO "+qn+".sample VALUES (?,1)", value)
							var expected, actual bool
							if err = db.QueryRowContext(ctx, "SELECT (k='A') IS NOT FALSE FROM "+qn+".sample").Scan(&expected); err != nil {
								t.Fatal(err)
							}
							if err = db.QueryRowContext(ctx, q, value, int64(1)).Scan(&actual); err != nil {
								t.Fatal(err)
							}
							if actual != expected {
								t.Fatalf("CHECK %q expected=%v actual=%v", value, expected, actual)
							}
						}
					}
				})
			}
			// Data with characters outside the inherited charset must fail before writes.
			if rules.Charset == "latin1" {
				table := source.PGTable{Info: source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "k", DataType: "varchar", Length: 20}}}}
				_, _, err := inspectPGTextPage(ctx, db, table, map[string]target.MySQLTextRules{"k": rules}, [][]any{{"中文"}}, nil, profile.MaxAllowedPacket)
				if err == nil {
					t.Fatal("lossy character conversion accepted")
				}
			}
			// Foreign-key key generation must use the same server semantics as uniqueness.
			parent := source.PGTable{Info: source.TableDDLInfo{TableName: "parent", Columns: []source.ColumnInfo{{Name: "k", DataType: "varchar", Length: 20}}}, Indexes: []source.IndexInfo{{TableName: "parent", IndexName: "pk", Columns: []string{"k"}, IsUnique: true, IsPrimary: true}}}
			child := source.PGTable{Info: source.TableDDLInfo{TableName: "child", Columns: parent.Info.Columns}}
			p := &PGMySQLPreflight{Target: profile, tables: []source.PGTable{parent, child}, textRules: map[string]map[string]target.MySQLTextRules{"parent": {"k": rules}, "child": {"k": rules}}, validationFKs: []source.FKInfo{{TableName: "child", ConstraintName: "fk", Columns: []string{"k"}, RefTable: "parent", RefColumns: []string{"k"}}}}
			r := &pgTextFixtureReader{data: map[string][][]any{"parent": {{"A"}}, "child": {{"a"}}}}
			m := &Migrator{reader: r, cfg: Config{PageSize: 100}}
			err = m.validatePGMySQLData(ctx, p, db, false)
			var equal bool
			if e := db.QueryRowContext(ctx, "SELECT CONVERT('A' USING "+rules.Charset+") COLLATE "+rules.Collation+" = CONVERT('a' USING "+rules.Charset+") COLLATE "+rules.Collation).Scan(&equal); e != nil {
				t.Fatal(e)
			}
			if (err == nil) != equal {
				t.Fatalf("FK equal=%v error=%v", equal, err)
			}
			// Unique key rejection uses the same weights, including composite key delimiting.
			r.data["parent"] = [][]any{{"A"}, {"a"}}
			p.tables = []source.PGTable{parent}
			p.validationFKs = nil
			err = m.validatePGMySQLData(ctx, p, db, false)
			if (err != nil) != equal {
				t.Fatalf("unique equal=%v error=%v", equal, err)
			}
		})
	}
	if profile.Parsed.Checks() {
		t.Run("mixed_column_check", func(t *testing.T) {
			a, err := target.ReadMySQLTextRules(ctx, db, "utf8mb4_general_ci")
			if err != nil {
				t.Fatal(err)
			}
			b, err := target.ReadMySQLTextRules(ctx, db, "utf8mb4_bin")
			if err != nil {
				t.Fatal(err)
			}
			table := source.PGTable{Info: source.TableDDLInfo{TableName: "mixed", Columns: []source.ColumnInfo{{Name: "a", DataType: "varchar", Length: 20, IsNullable: true}, {Name: "b", DataType: "varchar", Length: 20, IsNullable: true}}}, Checks: []source.PGObject{{Name: "equal", Definition: `CHECK ("a" = "b")`}}}
			m := &Migrator{reader: &pgPlanReader{}, writer: w}
			query, err := m.pgRowCheckQuery(table, true, map[string]target.MySQLTextRules{"a": a, "b": b})
			if err != nil {
				t.Fatal(err)
			}
			exec("CREATE TABLE " + qn + ".mixed (a VARCHAR(20) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci, b VARCHAR(20) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin)")
			for _, pair := range [][2]any{{"a", "A"}, {"A", "A"}, {nil, "A"}, {"A", nil}, {nil, nil}} {
				exec("TRUNCATE TABLE " + qn + ".mixed")
				exec("INSERT INTO "+qn+".mixed VALUES (?,?)", pair[0], pair[1])
				var actual, want bool
				if err := db.QueryRowContext(ctx, "SELECT (a=b) IS NOT FALSE FROM "+qn+".mixed").Scan(&want); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRowContext(ctx, query, pair[0], pair[1]).Scan(&actual); err != nil {
					t.Fatal(err)
				}
				if actual != want {
					t.Fatalf("mixed CHECK %v got %v want %v", pair, actual, want)
				}
			}
		})
	}
	// Charset-specific capacity permits a varchar that utf8mb4 cannot hold.
	if _, err := typemap.PGToMySQLChecked(source.ColumnInfo{DataType: "varchar", Length: 20000}); err != nil {
		t.Fatal(err)
	}
}

type pgTextFixtureReader struct {
	source.Reader
	data map[string][][]any
}

func (r *pgTextFixtureReader) ReadPage(_ context.Context, table string, _ []string, offset, limit int64) ([]string, []string, [][]any, error) {
	data := r.data[table]
	if offset >= int64(len(data)) {
		return []string{"k"}, nil, nil, nil
	}
	return []string{"k"}, nil, data[offset:min(offset+limit, int64(len(data)))], nil
}
