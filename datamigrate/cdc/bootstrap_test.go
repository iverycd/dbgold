package cdc

import (
	"context"
	"testing"

	"dbgold/datamigrate"
	"github.com/stretchr/testify/require"
)

func TestBuildBootstrapReviewClassifiesFailures(t *testing.T) {
	report := datamigrate.MigrationReport{
		Tables: datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "bad_schema", DDL: "CREATE TABLE bad_schema", Error: "unsupported type"}}},
		Data:   datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "bad_data", Error: "copy failed"}}},
		RowCounts: []datamigrate.TableRowCount{
			{Table: "good", Src: 10, Dst: 10, Match: true},
			{Table: "bad_data", Src: 5, Dst: 2, Match: false},
			{Table: "bad_count", Src: 7, Dst: 6, Match: false},
			{Table: "bad_compat", Src: 3, Dst: 3, Match: true},
		},
		Indexes: datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "idx_good", Error: "index failed"}}},
	}
	expected := []string{"good", "bad_schema", "bad_data", "bad_count", "bad_compat"}
	review := BuildBootstrapReview(Position{File: "mysql-bin.000001", Pos: 123}, expected, report, false, map[string]string{"bad_compat": "missing unique constraint"})

	require.Equal(t, []string{"good"}, review.EffectiveTables)
	require.Len(t, review.ExcludedTables, 4)
	require.Equal(t, "schema", review.ExcludedTables[0].Stage)
	require.Equal(t, "data", review.ExcludedTables[1].Stage)
	require.Equal(t, "row_count", review.ExcludedTables[2].Stage)
	require.Equal(t, "cdc_compatibility", review.ExcludedTables[3].Stage)
	require.Contains(t, review.Warnings[0], "idx_good")
	require.NotEmpty(t, review.ManifestHash)
}

func TestBuildBootstrapReviewUsesLowercaseTargetCountName(t *testing.T) {
	report := datamigrate.MigrationReport{RowCounts: []datamigrate.TableRowCount{{Table: "mixedcase", Src: 1, Dst: 1, Match: true}}}
	review := BuildBootstrapReview(Position{GTID: "uuid:1"}, []string{"MixedCase"}, report, true, nil)
	require.Equal(t, []string{"MixedCase"}, review.EffectiveTables)
	require.Empty(t, review.ExcludedTables)
}

func TestBuildBootstrapReviewExcludesMissingRowCount(t *testing.T) {
	review := BuildBootstrapReview(Position{File: "bin.1", Pos: 4}, []string{"users"}, datamigrate.MigrationReport{}, false, nil)
	require.Empty(t, review.EffectiveTables)
	require.Equal(t, "row_count", review.ExcludedTables[0].Stage)
}

func TestBuildBootstrapReviewIncludesPrimaryKeyFailureDetail(t *testing.T) {
	report := datamigrate.MigrationReport{
		PrimaryKeys: datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{
			Name: "mixedcase", DDL: `ALTER TABLE "mixedcase" ADD PRIMARY KEY ("id")`, Error: "duplicate key",
		}}},
		RowCounts: []datamigrate.TableRowCount{{Table: "mixedcase", Src: 2, Dst: 2, Match: true}},
	}
	review := BuildBootstrapReview(Position{File: "bin.1", Pos: 4}, []string{"MixedCase"}, report, true, nil)
	require.Len(t, review.ExcludedTables, 1)
	issue := review.ExcludedTables[0]
	require.Equal(t, "MixedCase", issue.Table)
	require.Equal(t, "cdc_compatibility", issue.Stage)
	require.Contains(t, issue.Error, "duplicate key")
	require.NotEmpty(t, issue.DDL)
}

func TestBuildBootstrapFailedObjectsCoversDDLAndValidationCategories(t *testing.T) {
	report := datamigrate.MigrationReport{
		Tables:      datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "bad_table", DDL: "DROP TABLE bad_table;\nCREATE TABLE bad_table(id badtype);", Error: "bad type"}}},
		Data:        datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "bad_data", Error: "copy failed"}}},
		PrimaryKeys: datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "pk_table", DDL: "ALTER TABLE pk_table ADD PRIMARY KEY(id)", Error: "duplicate"}}},
		Views:       datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "bad_view", DDL: "CREATE VIEW bad_view AS SELECT 1", Error: "view failed"}}},
		Indexes:     datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "bad_index", DDL: "CREATE INDEX bad_index ON t(id)", Error: "index failed"}}},
		Constraints: datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "bad_fk", DDL: "ALTER TABLE t ADD CONSTRAINT bad_fk FOREIGN KEY(id) REFERENCES p(id)", Error: "fk failed"}}},
		Sequences:   datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "bad_seq", DDL: "CREATE SEQUENCE bad_seq", Error: "sequence failed"}}},
		Comments:    datamigrate.CategoryReport{Items: []datamigrate.ObjectResult{{Name: "bad_comment", DDL: "COMMENT ON TABLE t IS 'x'", Error: "comment failed"}}},
		RowCounts: []datamigrate.TableRowCount{
			{Table: "bad_table", Src: 1, Dst: 0, Match: false},
			{Table: "bad_data", Src: 1, Dst: 1, Match: true},
			{Table: "good_compat", Src: 1, Dst: 1, Match: true},
		},
	}
	items := BuildBootstrapFailedObjects([]string{"bad_table", "bad_data", "missing_count", "good_compat"}, report, false, map[string]string{"good_compat": "missing unique constraint"})
	categories := make(map[string]int)
	for _, item := range items {
		categories[item.Category]++
	}
	for _, category := range []string{"table", "data", "primary_key", "view", "index", "foreign_key", "sequence", "comment", "row_count", "cdc_compatibility"} {
		require.Positive(t, categories[category], category)
	}
	require.Len(t, items, 10)
}

func TestHashBootstrapManifestIsOrderIndependent(t *testing.T) {
	first := BootstrapRecord{
		Position:        Position{File: "mysql-bin.000001", Pos: 88},
		EffectiveTables: []string{"b", "a"},
		ExcludedTables: []BootstrapIssue{
			{Table: "d", Stage: "data", Error: "x"},
			{Table: "c", Stage: "schema", Error: "y"},
		},
	}
	second := BootstrapRecord{
		Position:        first.Position,
		EffectiveTables: []string{"a", "b"},
		ExcludedTables: []BootstrapIssue{
			{Table: "c", Stage: "schema", Error: "y"},
			{Table: "d", Stage: "data", Error: "x"},
		},
	}
	require.Equal(t, HashBootstrapManifest(first), HashBootstrapManifest(second))
	second.Position.Pos++
	require.NotEqual(t, HashBootstrapManifest(first), HashBootstrapManifest(second))
}

func TestLoadConfiguredTablesRejectsEmptyFrozenManifest(t *testing.T) {
	_, err := LoadConfiguredTables(context.Background(), nil, Config{TableNames: []string{}})
	require.ErrorContains(t, err, "有效 CDC 表清单为空或损坏")
}
