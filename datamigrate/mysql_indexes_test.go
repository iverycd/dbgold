package datamigrate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestMySQLPrefixIndexDialects(t *testing.T) {
	dialects := []dialect.Dialect{dialect.NewMySQL(), dialect.NewDaMeng(), dialect.NewOracle(), dialect.NewSQLServer(), dialect.NewOscar()}
	for _, db := range []string{"postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		dialects = append(dialects, dialect.NewPostgres(db))
	}
	for _, d := range dialects {
		t.Run(d.Name(), func(t *testing.T) {
			for _, unique := range []bool{false, true} {
				idx := source.IndexInfo{TableName: "audit_catalog", IndexName: "catalog_rowguid_index", IsUnique: unique,
					Method: "BTREE", Columns: []string{"RowGuid", "REPORT_SIGN", "Tenant"}, Descending: []bool{true, false, true}, MySQLPrefixLengths: []int{8, 0, 3}}
				sql := dialect.JoinSQL(d.IndexStatements("A1", idx))
				first, last := d.QuoteIdent("RowGuid"), d.QuoteIdent("Tenant")
				if d.Name() == "mysql" {
					first, last = first+"(8)", last+"(3)"
				}
				require.Contains(t, sql, "("+first+" DESC, "+d.QuoteIdent("REPORT_SIGN")+", "+last+" DESC)")
				require.Equal(t, unique, strings.Contains(sql, "UNIQUE"))
			}
		})
	}
}

func TestMySQLPGIndexNamesStableAcrossTableSelections(t *testing.T) {
	r := newMySQLDMIndexReader()
	w := &mysqlDMIndexWriter{targetType: "postgres"}
	m, _ := newTestMigrator(r, w)
	m.cfg.LowerCaseNames = true
	wide := strings.Repeat("索", 25)
	r.indexes = []source.IndexInfo{
		{TableName: "One", IndexName: "Mixed"}, {TableName: "Two", IndexName: "mixed"},
		{TableName: "Else", IndexName: "one_mixed"}, {TableName: "One", IndexName: wide},
	}
	seen := map[string]bool{}
	for _, filter := range []string{"*", "One"} {
		m.cfg.Mode, m.cfg.Filter = "include", filter
		indexes, err := m.sourceIndexes(context.Background())
		require.NoError(t, err)
		for _, idx := range indexes {
			idx.IndexName = m.objName(idx.IndexName)
			name := dialect.IndexName(w.Dialect(), idx)
			if filter == "*" {
				require.False(t, seen[name])
				seen[name] = true
			} else {
				require.True(t, seen[name])
			}
			require.LessOrEqual(t, len(name), 63)
			require.True(t, utf8.ValidString(name))
		}
	}
	require.True(t, seen["one_mixed"] && seen["two_mixed"])
}

func TestMySQLPrefixIndexKeyTooWideOnlyFailsThatIndex(t *testing.T) {
	r, _, m := sqlServerFixture()
	m.cfg.CaptureRepair = true
	r.ddl["Orders"].Columns = append(r.ddl["Orders"].Columns, source.ColumnInfo{Name: "WideCode", DataType: "varchar", Length: 500})
	r.indexes = append([]source.IndexInfo{{TableName: "Orders", IndexName: "BadPrefix", Columns: []string{"WideCode"},
		MySQLPrefixLengths: []int{8}, IsUnique: true}}, r.indexes...)
	report := m.Run(context.Background())
	require.Zero(t, report.Tables.Failed)
	require.Equal(t, 2, report.Data.Success)
	require.Equal(t, 2, report.Indexes.Success)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Contains(t, report.Indexes.Items[0].Error, "900")
	require.Contains(t, report.Indexes.Items[0].DDL, "UNIQUE NONCLUSTERED INDEX [BadPrefix] ON [dbo].[Orders] ([WideCode])")
	require.Len(t, report.Warnings, 1)
	require.Contains(t, report.Warnings[0].Reason, "WideCode(8)")
	for _, obj := range report.RepairManifest.Objects {
		if obj.SourceName == "BadPrefix" {
			require.Equal(t, "failed", obj.Status)
			require.Equal(t, report.Indexes.Items[0].DDL, obj.DDL)
			return
		}
	}
	t.Fatal("missing failed index in repair manifest")
}

func TestMySQLOraclePrefixOnLOBDoesNotBlockTableOrData(t *testing.T) {
	r, w, m := oracleMigrationFixture()
	r.ddl["Orders"].Columns = append(r.ddl["Orders"].Columns, source.ColumnInfo{Name: "Body", DataType: "text"})
	r.indexes = append(r.indexes, source.IndexInfo{TableName: "Orders", IndexName: "Wide", Columns: []string{"Body"}, MySQLPrefixLengths: []int{8}})
	// Simulate Oracle rejecting the full LOB key after the copy.
	w.indexCause = errors.New("LOB column cannot be indexed")
	report := m.Run(context.Background())
	require.Equal(t, 2, report.Tables.Success)
	require.Equal(t, 2, report.Data.Success)
	require.False(t, m.migrationTableBlocked("Orders"))
	for _, item := range report.Indexes.Items {
		if strings.Contains(item.DDL, `"BODY"`) {
			require.Equal(t, "execute", item.Stage)
			require.Contains(t, item.Error, "LOB")
			return
		}
	}
	t.Fatal("missing full-column DDL for rejected Oracle index")
}

func TestMySQLUniquePrefixOnPartitionedTable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		columns []string
		ok      bool
	}{
		{"includes partition key", []string{"RowGuid", "Id"}, true},
		{"missing partition key", []string{"RowGuid"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, r, w := newPartitionFlow(t, "Root")
			r.specs["Root"] = partitionFlowSpec()
			r.ddl["Root"].Columns = append(r.ddl["Root"].Columns, source.ColumnInfo{Name: "RowGuid", DataType: "varchar", Length: 36})
			r.indexes = []source.IndexInfo{{TableName: "Root", IndexName: "ByGuid", IsUnique: true, Method: "BTREE",
				Locality: "local", Columns: tc.columns, MySQLPrefixLengths: []int{8}}}
			m.cfg.Content = "schema_only"
			report := m.Run(context.Background())
			if tc.ok {
				require.False(t, report.HasFailures(), "%+v", report)
				require.Equal(t, 1, report.Indexes.Success)
				require.Len(t, report.Warnings, 1)
			} else {
				require.True(t, report.HasFailures())
				require.Contains(t, report.PartitionMigration.Tables[0].Error, "唯一")
				require.Empty(t, w.created)
			}
		})
	}
}
