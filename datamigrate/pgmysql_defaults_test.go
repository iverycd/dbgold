package datamigrate

import (
	"context"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
)

// Reproduce the screenshot's five table blockers through the whole preflight,
// including PGColumnDefault and table DDL generation, not just the token parser.
func TestVastbaseQuotedLocalTimestampPreflight(t *testing.T) {
	for _, name := range []string{"clog_exchangeconfig", "clog_logdetailinfo", "clog_logdetailinfo_his", "clog_logpayloadinfo", "clog_logpayloadinfo_his"} {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			def := `"localtimestamp"(0)`
			col := source.ColumnInfo{Name: "createtime", DataType: "timestamp", DateTimePrecision: 6, Default: &def}
			r := &pgPlanReader{snapshot: &source.PGSnapshot{
				Profile: source.PGProfile{Product: "vastbase", Schema: "public"},
				Tables:  []source.PGTable{{Info: source.TableDDLInfo{TableName: name, Columns: []source.ColumnInfo{col}}}},
			}}
			w := &pgPlanWriter{db: db}
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 128)}, Config{Mode: "all", Content: "schema_only"})
			expectPGEmptyTarget(mock, "GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'")
			p := m.PreflightPGMySQL(context.Background())
			if !p.Ready {
				t.Fatalf("issues=%+v", p.Issues)
			}
			found := false
			for _, obj := range p.Objects {
				if obj.Stage == "table" && strings.Contains(dialect.JoinSQL(obj.Statements), "`createtime` DATETIME(6) NOT NULL DEFAULT (CAST(CURRENT_TIMESTAMP(0) AS DATETIME(6)))") {
					found = true
				}
			}
			if !found {
				t.Fatal("missing precision-preserving default DDL")
			}
			if *r.snapshot.Tables[0].Info.Columns[0].Default != def || p.tables[0].Info.Columns[0].DateTimePrecision != 6 {
				t.Fatal("source default or column precision was changed")
			}
			if w.mutations != 0 {
				t.Fatal("preflight mutated target")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
