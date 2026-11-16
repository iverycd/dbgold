package source

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestPGOracleIndexesRetainSourceSemantics(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "MiXeD", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{"pg_index.indnkeyatts": true, "pg_index.indnullsnotdistinct": true}}}
	mock.ExpectQuery(`SELECT t.relname,i.relname,.*pg_get_expr.*pg_collation`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"table", "index", "keys", "unique", "primary", "method", "expr", "pred", "nkeys", "natts", "nulls", "valid", "immediate", "options", "def", "oid", "expressions", "predicate", "constraint", "constraint_type", "deferrable", "deferred", "validated", "collation"}).
		AddRow("Orders", "MiXeDIndex", "0 2 1", true, false, "btree", true, true, 2, 3, true, true, false, "3 0", `CREATE UNIQUE INDEX "MiXeDIndex" ON "Orders" ((lower("Code")) DESC, "Id") INCLUDE ("Code") WHERE "Id">0`, 42, `lower("Code")`, `("Id" > 0)`, "UniqueRule", "u", true, true, false, true))
	mock.ExpectQuery(`SELECT c.relname,a.attnum,a.attname`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"table", "number", "column"}).AddRow("Orders", 1, "Code").AddRow("Orders", 2, "Id"))
	mock.ExpectQuery(`SELECT pg_get_indexdef\(\$1::oid,\$2,true\)`).WithArgs(int64(42), 1).WillReturnRows(sqlmock.NewRows([]string{"key"}).AddRow(`lower("Code")`))
	mock.ExpectQuery(`SELECT pg_get_indexdef\(\$1::oid,\$2,true\)`).WithArgs(int64(42), 2).WillReturnRows(sqlmock.NewRows([]string{"key"}).AddRow(`"Id"`))
	indexes, err := r.pgIndexesForTarget(context.Background(), true)
	require.NoError(t, err)
	require.Len(t, indexes, 1)
	idx := indexes[0]
	require.Equal(t, "MiXeDIndex", idx.IndexName)
	require.Empty(t, idx.Unsupported)
	require.Equal(t, []string{"Id", "Code"}, idx.Columns)
	require.Equal(t, []bool{true, false}, idx.Descending)
	require.Equal(t, []int{0, 2, 1}, idx.Attnums)
	require.Equal(t, []string{`lower("Code")`, `"Id"`}, idx.PG.KeyDefinitions)
	require.Equal(t, 2, idx.PG.KeyCount)
	require.True(t, idx.PG.NullsNotDistinct)
	require.True(t, idx.PG.NonDefaultCollation)
	require.Equal(t, "UniqueRule", idx.PG.ConstraintName)
	require.True(t, idx.PG.Deferrable && idx.PG.InitiallyDeferred)
	require.False(t, idx.PG.Validated)
	require.False(t, idx.PG.Immediate)

	mock.ExpectQuery(`SELECT t.relname,i.relname FROM`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"table", "index"}).AddRow("Orders", "MiXeDIndex"))
	table := &PGTable{Indexes: indexes}
	require.NoError(t, r.markPGOperatorClassesForTarget(context.Background(), map[string]*PGTable{"Orders": table}, true))
	require.True(t, table.Indexes[0].PG.NonDefaultOpClass)
	require.Empty(t, table.Indexes[0].Unsupported)
}

func TestPGOracleForeignKeysPreserveDeferralAndColumnOrder(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "MiXeD", profile: PGProfile{Catalog: "pg_catalog"}}
	mock.ExpectQuery(`SELECT c.conname,t.relname,.*c.condeferred,c.convalidated,pg_get_constraintdef`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"name", "table", "refschema", "reftable", "columns", "refcolumns", "delete", "update", "deferrable", "match", "oid", "deferred", "valid", "definition"}).
		AddRow("FkMiXeD", "Child", "OtherSchema", "Parent", "{2,1}", "{4,3}", "n", "c", true, "f", int64(42), true, false, `FOREIGN KEY ("B", "A") REFERENCES "OtherSchema"."Parent"("Y", "X") MATCH FULL DEFERRABLE INITIALLY DEFERRED NOT VALID`))
	mock.ExpectQuery(`SELECT c.relname,a.attnum,a.attname`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"table", "number", "column"}).AddRow("Child", 1, "A").AddRow("Child", 2, "B"))
	mock.ExpectQuery(`SELECT attnum,attname`).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"number", "column"}).AddRow(3, "X").AddRow(4, "Y"))
	fks, err := r.pgForeignKeysForTarget(context.Background(), true)
	require.NoError(t, err)
	require.Len(t, fks, 1)
	fk := fks[0]
	require.Empty(t, fk.Unsupported)
	require.Equal(t, "FkMiXeD", fk.ConstraintName)
	require.Equal(t, "OtherSchema", fk.RefSchema)
	require.Equal(t, []string{"B", "A"}, fk.Columns)
	require.Equal(t, []string{"Y", "X"}, fk.RefColumns)
	require.Equal(t, "SET NULL", fk.OnDelete)
	require.Equal(t, "CASCADE", fk.OnUpdate)
	require.Equal(t, "f", fk.PG.Match)
	require.True(t, fk.PG.Deferrable && fk.PG.InitiallyDeferred)
	require.False(t, fk.PG.Validated)
}

func TestPGOracleSnapshotKeepsRawChecksAndHealthyTables(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "MiXeD", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{"pg_constraint.connoinherit": true}}}
	empty := func(pattern string) {
		mock.ExpectQuery(pattern).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"unused"})).RowsWillBeClosed()
	}
	mock.ExpectQuery(`SELECT c.oid,c.relname,c.relkind`).WithArgs("MiXeD").WillReturnRows(pgSnapshotIdentityRows().AddRow(1, "Unreadable", "r", false, nil, false, false).AddRow(2, "Orders", "r", false, "Comment", true, false))
	empty(`SELECT t.relname,i.relname,`)
	empty(`SELECT c.relname,a.attnum,a.attname`)
	empty(`SELECT c.conname,t.relname`)
	empty(`SELECT c.relname,a.attnum,a.attname`)
	mock.ExpectQuery(`SELECT a.attnum,a.attname,t.typname`).WithArgs("MiXeD", "Orders").WillReturnRows(sqlmock.NewRows([]string{"ordinal", "name", "type", "kind", "type_schema", "mod", "nullable", "default", "identity", "generated", "comment", "formatted"}).AddRow(1, "MiXeDColumn", "int8", "b", "pg_catalog", -1, false, nil, "", "", "Column comment", "bigint"))
	mock.ExpectQuery(`SELECT t.relname,c.conname,c.contype,.*c.condeferrable,c.condeferred,c.connoinherit`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"table", "name", "type", "definition", "valid", "deferrable", "deferred", "noinherit"}).
		AddRow("Orders", "CheckMiXeD", "c", `CHECK ("MiXeDColumn" > 0) NO INHERIT NOT VALID`, false, false, false, true).
		AddRow("Orders", "ExcludeMiXeD", "x", `EXCLUDE USING gist ("MiXeDColumn" WITH =) DEFERRABLE`, true, true, false, false))
	empty(`SELECT t.relname,i.relname FROM`)
	empty(`SELECT t.relname,g.tgname`)
	empty(`SELECT p.proname,oidvectortypes`)
	empty(`SELECT c.relname,pg_get_viewdef`)
	empty(`SELECT c.oid,n.nspname,c.relname`)
	snap, err := r.ReadPGOracleSnapshot(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, snap.Tables, 2)
	require.NotEmpty(t, snap.Tables[0].Unsupported)
	good := snap.Tables[1]
	require.Empty(t, good.Unsupported)
	require.Equal(t, "MiXeDColumn", good.Info.Columns[0].Name)
	require.Len(t, good.Checks, 2)
	require.Equal(t, "CheckMiXeD", good.Checks[0].Name)
	require.Empty(t, good.Checks[0].Reason)
	require.False(t, good.Checks[0].PG.Validated)
	require.True(t, good.Checks[0].PG.NoInherit)
	require.Equal(t, "constraint", good.Checks[1].Kind)
	require.Equal(t, "x", good.Checks[1].PG.Type)
	require.True(t, good.Checks[1].PG.Deferrable)
}

func pgOracleBindingRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"schema", "table", "column", "default", "identity", "owned"})
}

func TestPGOracleSequencesKeepIndependentSharedIdentityAndExcludedConsumers(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprintf("modern_%t", modern), func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			r := &PostgresReader{db: db, schema: "MiXeD", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{"pg_sequence.seqincrement": modern, "pg_attribute.attidentity": modern}}}
			mock.ExpectQuery(`SELECT c.oid,n.nspname,c.relname`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"oid", "schema", "name"}).AddRow(1, "MiXeD", "Standalone").AddRow(2, "MiXeD", "SharedSeq").AddRow(3, "MiXeD", "IdentitySeq").AddRow(4, "MiXeD", "ExcludedSeq"))
			expectState := func(oid int64, name string, last int64, called bool, increment, min, max, cache int64, cycle bool) {
				mock.ExpectQuery(`SELECT last_value,is_called FROM "MiXeD"\."` + name + `"`).WillReturnRows(sqlmock.NewRows([]string{"last", "called"}).AddRow(last, called))
				attr := sqlmock.NewRows([]string{"step", "min", "max", "cycle", "cache"}).AddRow(increment, min, max, cycle, cache)
				if modern {
					mock.ExpectQuery(`SELECT seqincrement,seqmin,seqmax,seqcycle,seqcache`).WithArgs(oid).WillReturnRows(attr)
				} else {
					mock.ExpectQuery(`SELECT increment_by,min_value,max_value,is_cycled,cache_value FROM "MiXeD"\."` + name + `"`).WillReturnRows(attr)
				}
			}
			mock.ExpectQuery(`SELECT n.nspname,t.relname,a.attname,`).WithArgs(int64(1)).WillReturnRows(pgOracleBindingRows())
			expectState(1, "Standalone", -9, true, -2, -9, -1, 4, true)
			mock.ExpectQuery(`SELECT n.nspname,t.relname,a.attname,`).WithArgs(int64(2)).WillReturnRows(pgOracleBindingRows().
				AddRow("MiXeD", "Orders", "Id", `nextval('"MiXeD"."SharedSeq"'::regclass)`, "", true).
				AddRow("MiXeD", "Excluded", "MixedID", `nextval('"MiXeD"."SharedSeq"'::regclass)`, "", false).
				AddRow("Outside", "OtherTable", "OtherID", `nextval('"MiXeD"."SharedSeq"'::regclass)+1`, "", false))
			expectState(2, "SharedSeq", 12, true, 3, 1, 9999, 20, false)
			identity := ""
			if modern {
				identity = "a"
			}
			mock.ExpectQuery(`SELECT n.nspname,t.relname,a.attname,`).WithArgs(int64(3)).WillReturnRows(pgOracleBindingRows().AddRow("MiXeD", "Orders", "AlwaysId", "", identity, true))
			expectState(3, "IdentitySeq", 10, false, 1, 10, 9999, 1, false)
			mock.ExpectQuery(`SELECT n.nspname,t.relname,a.attname,`).WithArgs(int64(4)).WillReturnRows(pgOracleBindingRows().AddRow("MiXeD", "Excluded", "Id", "", "", true))
			snap := &PGSnapshot{Tables: []PGTable{{Info: TableDDLInfo{TableName: "Orders"}}}}
			require.NoError(t, r.loadPGOracleSequences(context.Background(), snap, map[string]bool{"Orders": true}))
			require.Len(t, snap.Sequences, 3)
			require.Empty(t, snap.Objects)
			standalone, shared, owned := snap.Sequences[0], snap.Sequences[1], snap.Sequences[2]
			require.Equal(t, "Standalone", standalone.Name)
			require.True(t, standalone.Cycle)
			require.Equal(t, int64(-1), standalone.StartValue)
			require.Equal(t, int64(-9), standalone.LastValue)
			require.False(t, standalone.Shared)
			require.True(t, shared.Shared)
			require.Equal(t, int64(15), shared.StartValue)
			require.Equal(t, int64(20), shared.Cache)
			require.Len(t, shared.Bindings, 3)
			require.Equal(t, "MixedID", shared.Bindings[1].ColumnName)
			require.Equal(t, "Outside", shared.Bindings[2].Schema)
			require.Contains(t, shared.Bindings[2].Default, "+1")
			require.Equal(t, identity, owned.Identity)
			require.True(t, owned.Bindings[0].Owned)
			require.False(t, owned.IsCalled)
			require.Equal(t, int64(10), owned.StartValue)
			require.Len(t, snap.Tables[0].Sequences, 2)
			require.Equal(t, "Id", snap.Tables[0].Sequences[0].ColumnName)
			require.Equal(t, "AlwaysId", snap.Tables[0].Sequences[1].ColumnName)
		})
	}
}

func TestPGOracleSequenceNextValuePreservesStateAndDetectsExhaustion(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		last, step, min, max, want int64
		called, cycle, exhausted   bool
	}{
		{"uncalled", 41, 5, 1, 100, 41, false, false, false},
		{"ascending", 41, 5, 1, 100, 46, true, false, false},
		{"descending", -41, -5, -100, -1, -46, true, false, false},
		{"ascending_cycle", 99, 5, 1, 100, 1, true, true, false},
		{"descending_cycle", -99, -5, -100, -1, -1, true, true, false},
		{"ascending_exhausted", math.MaxInt64, 1, 1, math.MaxInt64, math.MaxInt64, true, false, true},
		{"descending_exhausted", math.MinInt64, -1, math.MinInt64, -1, math.MinInt64, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seq := PGSequence{LastValue: tc.last, IsCalled: tc.called, Increment: tc.step, Min: tc.min, Max: tc.max, Cache: 1, Cycle: tc.cycle}
			require.NoError(t, pgSequenceNextValue(&seq))
			require.Equal(t, tc.want, seq.StartValue)
			require.Equal(t, tc.exhausted, seq.Exhausted)
			require.Equal(t, tc.last, seq.LastValue)
			require.Equal(t, tc.called, seq.IsCalled)
			require.Equal(t, tc.cycle, seq.Cycle)
		})
	}
}

func TestPGOracleIndependentSequenceFailureIsObjectLocal(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "MiXeD", profile: PGProfile{Catalog: "pg_catalog"}}
	mock.ExpectQuery(`SELECT c.oid,n.nspname,c.relname`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"oid", "schema", "name"}).AddRow(1, "MiXeD", "UnreadableSeq").AddRow(2, "MiXeD", "GoodSeq"))
	mock.ExpectQuery(`SELECT n.nspname,t.relname,a.attname,`).WithArgs(int64(1)).WillReturnRows(pgOracleBindingRows())
	mock.ExpectQuery(`SELECT last_value,is_called FROM "MiXeD"\."UnreadableSeq"`).WillReturnError(fmt.Errorf("permission denied"))
	mock.ExpectQuery(`SELECT n.nspname,t.relname,a.attname,`).WithArgs(int64(2)).WillReturnRows(pgOracleBindingRows())
	mock.ExpectQuery(`SELECT last_value,is_called FROM "MiXeD"\."GoodSeq"`).WillReturnRows(sqlmock.NewRows([]string{"last", "called"}).AddRow(3, true))
	mock.ExpectQuery(`SELECT increment_by,min_value,max_value,is_cycled,cache_value`).WillReturnRows(sqlmock.NewRows([]string{"step", "min", "max", "cycle", "cache"}).AddRow(1, 1, 100, false, 1))
	snap := &PGSnapshot{}
	require.NoError(t, r.loadPGOracleSequences(context.Background(), snap, map[string]bool{}))
	require.Len(t, snap.Sequences, 2)
	require.Contains(t, snap.Sequences[0].Unsupported, "permission denied")
	require.Equal(t, int64(4), snap.Sequences[1].StartValue)
	require.Empty(t, snap.Sequences[1].Unsupported)
	require.Len(t, snap.Objects, 1)
	require.Equal(t, "UnreadableSeq", snap.Objects[0].Name)
}

func TestPGOracleSequenceTaskFailureStopsSnapshot(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "MiXeD", profile: PGProfile{Catalog: "pg_catalog"}}
	mock.ExpectQuery(`SELECT c.oid,n.nspname,c.relname`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"oid", "schema", "name"}).AddRow(1, "MiXeD", "Seq"))
	mock.ExpectQuery(`SELECT n.nspname,t.relname,a.attname,`).WithArgs(int64(1)).WillReturnRows(pgOracleBindingRows())
	mock.ExpectQuery(`SELECT last_value,is_called FROM "MiXeD"\."Seq"`).WillReturnError(context.Canceled)
	require.ErrorIs(t, r.loadPGOracleSequences(context.Background(), &PGSnapshot{}, nil), context.Canceled)
}
