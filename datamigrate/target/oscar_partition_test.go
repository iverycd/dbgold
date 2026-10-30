package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/validation"
	"dbgold/internal/jdbcbridge"
	"github.com/stretchr/testify/require"
)

type fakeOscarPartitionBridge struct {
	fakeValidationOscarBridge
	results     map[string][][]any
	queryErrors map[string]error
	queries     []string
	cursors     map[uint64]string
	next        uint64
	counts      []string
	closeError  error
	incomplete  bool
	version     string
	noPrimary   bool
}

func (f *fakeOscarPartitionBridge) DescribeTable(ctx context.Context, session uint64, schema, table string) (jdbcbridge.TableMetadata, error) {
	meta, err := f.fakeValidationOscarBridge.DescribeTable(ctx, session, schema, table)
	if f.noPrimary {
		meta.PrimaryKey = nil
	}
	return meta, err
}

func (f *fakeOscarPartitionBridge) OpenCursor(_ context.Context, _ uint64, query string) (uint64, error) {
	f.queries = append(f.queries, query)
	if err := f.queryErrors[query]; err != nil {
		return 0, err
	}
	f.next++
	if f.cursors == nil {
		f.cursors = map[uint64]string{}
	}
	f.cursors[f.next] = query
	return f.next, nil
}
func (f *fakeOscarPartitionBridge) FetchCursor(_ context.Context, cursor uint64, _, _ int) ([][]any, bool, error) {
	query := f.cursors[cursor]
	if f.incomplete {
		return nil, false, nil
	}
	if rows, ok := f.results[query]; ok {
		// JDBC deserializes each fetch into fresh rows; validation streams may
		// release consumed entries without changing persisted database values.
		copied := make([][]any, len(rows))
		for i, row := range rows {
			copied[i] = slices.Clone(row)
		}
		return copied, true, nil
	}
	if query == `SELECT VERSION(),CURRENT_SCHEMA()` {
		return [][]any{{f.version, "Default"}}, true, nil
	}
	if strings.HasSuffix(query, " WHERE 1=0") {
		return nil, true, nil
	}
	return nil, false, fmt.Errorf("unexpected native catalog query: %s", query)
}
func (f *fakeOscarPartitionBridge) CloseCursor(_ context.Context, _ uint64) error {
	f.closes++
	return f.closeError
}
func (f *fakeOscarPartitionBridge) QueryInt64(_ context.Context, _ uint64, query string) (int64, error) {
	f.counts = append(f.counts, query)
	return int64(len(f.counts) - 1), nil
}

func newOscarPartitionFixture() (*OscarWriter, *fakeOscarPartitionBridge) {
	f := &fakeOscarPartitionBridge{version: "神通数据库7.0 (64bit)(build 20250117)", results: map[string][][]any{
		`SELECT c.OID,c.RELKIND FROM SYS_CLASS c JOIN SYS_NAMESPACE n ON n.OID=c.RELNAMESPACE WHERE n.NSPNAME='业务' AND c.RELNAME='Orders'`: {{int64(42), "f"}},
		`SELECT PARTTYPE1,PARTTYPE2,PARTKEYCOLS1,PARTKEYCOLS2,PARTCNT,INTERVAL_STR FROM V_SYS_PART_TABLES WHERE OID=42`:                    {{int64(1), int64(2), int64(1), int64(1), int64(1), nil}},
		`SELECT SYS_GET_TABLEDEF FROM V_SYS_TABLE WHERE SCHEMANAME='业务' AND TABLENAME='Orders'`:                                            {{`CREATE TABLE "业务"."Orders"("Id" BIGINT,"Original" CLOB) PARTITION BY RANGE("Id") SUBPARTITION BY LIST("Id") (PARTITION "P" VALUES LESS THAN(MAXVALUE)(SUBPARTITION "Leaf" VALUES(1),SUBPARTITION "Rest" VALUES(DEFAULT)))`}},
		`SELECT OID,PARTNAME,PART,PART_KEY_VALUES,PARTCNT FROM V_SYS_TAB_PARTITIONS WHERE BO=42 ORDER BY PART`:                             {{int64(43), "P", int64(0), "MAXVALUE", int64(2)}},
		`SELECT SUBPARTNAME,PART,PART_KEY_VALUES FROM V_SYS_TAB_SUBPARTITIONS WHERE BO=43 ORDER BY PART`:                                   {{"Leaf", int64(0), "(1)"}, {"Rest", int64(1), "DEFAULT"}},
	}}
	return newFakeOscarWriter(f), f
}

func TestOscarPartitionProfileRequiresNativeVersionAndCatalogFields(t *testing.T) {
	w, f := newOscarPartitionFixture()
	p, err := w.InspectPartitionProfile(context.Background())
	require.NoError(t, err)
	require.Equal(t, "oscar", p.Product)
	require.Equal(t, "业务", p.Schema)
	require.Equal(t, "native-oscar", p.Mode)
	require.True(t, p.Features[partition.FeatureRange])
	require.True(t, p.Features[partition.FeatureDefaultList])
	require.True(t, p.Features[partition.FeatureSubpartition])
	require.True(t, p.Features["row_movement"])
	require.False(t, p.Features["row_movement_clause"])
	require.False(t, p.Features[partition.FeatureHash])
	require.False(t, p.Features[partition.FeatureInterval])
	require.Equal(t, len(f.queries), f.closes)
	for _, version := range []string{"PostgreSQL 16.6", "ShenTong KSTORE 7.0", "Oscar 8.0", ""} {
		f.version = version
		p, err = w.InspectPartitionProfile(context.Background())
		require.Error(t, err)
		require.Empty(t, p.Features)
	}
	f.version = "ShenTong 7.0"
	f.queryErrors = map[string]error{`SELECT p.OID,p.BO,p.PARTNAME,p.PART,p.PART_KEY_VALUES,p.PARTCNT FROM V_SYS_TAB_PARTITIONS p WHERE 1=0`: errors.New("permission denied")}
	p, err = w.InspectPartitionProfile(context.Background())
	require.ErrorContains(t, err, "permission denied")
	require.Empty(t, p.Features)
}

func TestOscarPartitionTableBindsNativeParentsAndKeepsSourceCase(t *testing.T) {
	w, f := newOscarPartitionFixture()
	spec, err := w.InspectPartitionTable(context.Background(), "Orders")
	require.NoError(t, err)
	require.Equal(t, "Id", spec.Keys[0].Column)
	require.Equal(t, "P", spec.Partitions[0].Name)
	require.Equal(t, "Leaf", spec.Partitions[0].Subpartition.Partitions[0].Name)
	require.Equal(t, 1, spec.Partitions[0].Subpartition.Partitions[0].Ordinal)
	require.Equal(t, len(f.queries), f.closes)
	for _, q := range f.queries {
		require.NotContains(t, q, "ALL_")
		require.NotContains(t, q, "lower(")
	}
	delete(f.results, `SELECT SUBPARTNAME,PART,PART_KEY_VALUES FROM V_SYS_TAB_SUBPARTITIONS WHERE BO=43 ORDER BY PART`)
	_, err = w.InspectPartitionTable(context.Background(), "Orders")
	require.Error(t, err, "unreadable child catalogs never become an ordinary table")
}

func TestOscarPartitionRowsUseVerifiedNativeLeavesIncludingEmptyLeaf(t *testing.T) {
	w, f := newOscarPartitionFixture()
	spec, err := w.InspectPartitionTable(context.Background(), "Orders")
	require.NoError(t, err)
	counts, err := w.CountPartitionRows(context.Background(), "Orders", spec)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"业务.Leaf": 0, "业务.Rest": 1}, counts)
	require.Equal(t, []string{`SELECT COUNT(*) FROM "业务"."Orders" SUBPARTITION ("Leaf")`, `SELECT COUNT(*) FROM "业务"."Orders" SUBPARTITION ("Rest")`}, f.counts)
	drift := source.ClonePartitionSpec(spec)
	drift.Partitions[0].Subpartition.Partitions[0].Name = "Changed"
	_, err = w.CountPartitionRows(context.Background(), "Orders", drift)
	require.ErrorContains(t, err, "漂移")
	require.Len(t, f.counts, 2)
}

func TestOscarPartitionTableRejectsIncompleteChildCounts(t *testing.T) {
	for _, count := range []any{int64(1), int64(3), int64(-1), nil} {
		w, f := newOscarPartitionFixture()
		f.results[`SELECT OID,PARTNAME,PART,PART_KEY_VALUES,PARTCNT FROM V_SYS_TAB_PARTITIONS WHERE BO=42 ORDER BY PART`][0][4] = count
		_, err := w.InspectPartitionTable(context.Background(), "Orders")
		require.Error(t, err, "native parent count %v cannot certify two child rows", count)
		require.Equal(t, len(f.queries), f.closes)
	}
	w, f := newOscarPartitionFixture()
	query := `SELECT SUBPARTNAME,PART,PART_KEY_VALUES FROM V_SYS_TAB_SUBPARTITIONS WHERE BO=43 ORDER BY PART`
	f.results[query] = f.results[query][:1]
	_, err := w.InspectPartitionTable(context.Background(), "Orders")
	require.ErrorContains(t, err, "PARTCNT")
	require.Equal(t, len(f.queries), f.closes)
}

func TestOscarPartitionValidationRejectsWrongScopeAndReadsRawNativeLeaf(t *testing.T) {
	w, f := newOscarPartitionFixture()
	meta, err := w.DescribeValidationTable(context.Background(), "Orders")
	require.NoError(t, err)
	location := validation.PartitionLocation{Schema: "业务", Name: "Leaf", Ordinal: 1, Subpartition: true}
	query := `SELECT "Id","Original" FROM "业务"."Orders" SUBPARTITION ("Leaf")`
	f.results[query] = [][]any{{int64(1), "x\x00y "}}
	stream, err := w.OpenPartitionValidationScan(context.Background(), "Orders", location, meta.Columns)
	require.NoError(t, err)
	row, err := stream.Next(context.Background())
	require.NoError(t, err)
	require.Equal(t, "x\x00y ", row[1])
	require.NoError(t, stream.Close())
	for _, bad := range []validation.PartitionLocation{
		{Schema: "other", Name: "Leaf", Subpartition: true},
		{Name: "Leaf", Subpartition: false},
		{Name: "Leaf", Subpartition: true, Ordinal: 2},
		{Name: "unrelated", Subpartition: true},
		{Name: "Leaf", Subpartition: true, Native: true},
	} {
		_, err := w.OpenPartitionValidationScan(context.Background(), "Orders", bad, meta.Columns)
		require.Error(t, err)
	}
}

func TestOscarPartitionQueriesCloseOnFailureAndQuoteCatalogLiterals(t *testing.T) {
	w, f := newOscarPartitionFixture()
	f.incomplete = true
	_, err := w.InspectPartitionProfile(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, f.closes)
	f.incomplete = false
	f.closeError = errors.New("cursor close failed")
	_, err = w.InspectPartitionProfile(context.Background())
	require.ErrorContains(t, err, "cursor close failed")
	quoted, err := oscarPartitionLiteral("Ow'ner")
	require.NoError(t, err)
	require.Equal(t, "'Ow''ner'", quoted)
	_, err = oscarPartitionLiteral("ambiguous\\name")
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	previous := len(f.queries)
	_, err = w.InspectPartitionProfile(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, f.queries, previous)
}

func TestOscarPartitionUsesResolvedSchemaAndExactQuotedIdentity(t *testing.T) {
	w, f := newOscarPartitionFixture()
	w.schema = ""
	f.results[`SELECT CURRENT_SCHEMA()`] = [][]any{{"Ow'ner"}}
	q := `SELECT c.OID,c.RELKIND FROM SYS_CLASS c JOIN SYS_NAMESPACE n ON n.OID=c.RELNAMESPACE WHERE n.NSPNAME='Ow''ner' AND c.RELNAME='Ta''ble'`
	f.results[q] = [][]any{{int64(123), "r"}}
	spec, err := w.InspectPartitionTable(context.Background(), "Ta'ble")
	require.NoError(t, err)
	require.Nil(t, spec)
	require.Equal(t, q, f.queries[len(f.queries)-1])
	f.results[q] = nil
	_, err = w.InspectPartitionTable(context.Background(), "Ta'ble")
	require.ErrorContains(t, err, "唯一")
	// Reusing a familiar table name from a different schema must not help.
	f.results[q] = [][]any{{int64(1), "r"}, {int64(2), "f"}}
	_, err = w.InspectPartitionTable(context.Background(), "Ta'ble")
	require.ErrorContains(t, err, "唯一")
}

func TestOscarPartitionNativeSelectorsEscapeQuotedLeafNames(t *testing.T) {
	w, f := newOscarPartitionFixture()
	f.results[`SELECT SUBPARTNAME,PART,PART_KEY_VALUES FROM V_SYS_TAB_SUBPARTITIONS WHERE BO=43 ORDER BY PART`][0][0] = `Leaf"Quote`
	spec, err := w.InspectPartitionTable(context.Background(), "Orders")
	require.NoError(t, err)
	_, err = w.CountPartitionRows(context.Background(), "Orders", spec)
	require.NoError(t, err)
	require.Equal(t, `SELECT COUNT(*) FROM "业务"."Orders" SUBPARTITION ("Leaf""Quote")`, f.counts[0])
	meta, err := w.DescribeValidationTable(context.Background(), "Orders")
	require.NoError(t, err)
	query := `SELECT "Id","Original" FROM "业务"."Orders" SUBPARTITION ("Leaf""Quote")`
	f.results[query] = nil
	stream, err := w.OpenPartitionValidationScan(context.Background(), "Orders", validation.PartitionLocation{Name: `Leaf"Quote`, Subpartition: true}, meta.Columns)
	require.NoError(t, err)
	require.Equal(t, query, f.queries[len(f.queries)-1])
	require.NoError(t, stream.Close())
}

type oscarPartitionAppendScanner struct {
	*OscarWriter
	leaf *validation.PartitionLocation
}

func (s oscarPartitionAppendScanner) OpenValidationScan(ctx context.Context, table string, columns []validation.Column) (validation.RowStream, error) {
	if s.leaf != nil {
		return s.OpenPartitionValidationScan(ctx, table, *s.leaf, columns)
	}
	return s.OscarWriter.OpenValidationScan(ctx, table, columns)
}

func TestOscarPartitionAppendValidationPreservesOldValuesAndDuplicates(t *testing.T) {
	for _, scope := range []string{"root", "leaf", "empty_leaf"} {
		for _, corrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/corrupt=%v", scope, corrupt), func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("TMPDIR", dir)
				ctx := context.Background()
				src, sf := newOscarPartitionFixture()
				dst, df := newOscarPartitionFixture()
				sf.noPrimary, df.noPrimary = true, true
				rootQuery := `SELECT "Id","Original" FROM "业务"."Orders"`
				query := rootQuery
				scanner := oscarPartitionAppendScanner{OscarWriter: dst}
				if scope != "root" {
					scanner.leaf = &validation.PartitionLocation{Schema: "业务", Name: "Leaf", Ordinal: 1, Subpartition: true}
					query += ` SUBPARTITION ("Leaf")`
				}
				meta, err := src.DescribeValidationTable(ctx, "Orders")
				require.NoError(t, err)
				plan := validation.Plan{SourceTable: "Orders", TargetTable: "Orders", SourceColumns: meta.Columns, TargetColumns: meta.Columns}
				sf.results[rootQuery] = [][]any{{int64(1), "new\x00 "}, {int64(1), "new\x00 "}}
				if scope != "empty_leaf" {
					df.results[query] = [][]any{{int64(1), "old\x00 "}}
				} else {
					df.results[query] = nil
				}
				baseline, err := validation.CaptureAppendBaseline(ctx, scanner, plan)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, baseline.Close()) })
				df.results[query] = append(df.results[query], []any{int64(1), "new\x00 "}, []any{int64(1), "new\x00 "})
				if corrupt {
					// Keep all row counts unchanged while losing one trailing space.
					row := df.results[query][0]
					row[1] = strings.TrimSuffix(row[1].(string), " ")
				}
				result := validation.ValidateAppend(ctx, src, scanner, plan, baseline)
				require.Empty(t, result.Error)
				require.Equal(t, int64(2), result.SourceRows)
				require.Equal(t, int64(2), result.TargetRows)
				if corrupt {
					require.Equal(t, validation.StatusMismatch, result.Status)
				} else {
					require.Equal(t, validation.StatusPassed, result.Status)
				}
				require.Equal(t, len(sf.queries), sf.closes)
				require.Equal(t, len(df.queries), df.closes)
				require.NoError(t, baseline.Close())
				entries, err := os.ReadDir(dir)
				require.NoError(t, err)
				require.Empty(t, entries)
			})
		}
	}
}
