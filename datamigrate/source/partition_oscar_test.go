package source

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func oscarPartitionCatalogFixture() OscarPartitionCatalog {
	return OscarPartitionCatalog{Schema: "MixedOwner", DDL: `CREATE TABLE "MixedOwner"."Root"("Id" INT,"Category" INT) PARTITION BY RANGE("Id") SUBPARTITION BY LIST("Category") (PARTITION "P1" VALUES LESS THAN(10)(SUBPARTITION "S1" VALUES(1),SUBPARTITION "Rest1" VALUES(DEFAULT)),PARTITION "P2" VALUES LESS THAN(MAXVALUE)(SUBPARTITION "S2" VALUES(2)))`, Method: 1, Submethod: 2, KeyCount: 1, SubkeyCount: 1, PartitionCount: 2, Partitions: []OscarPartitionCatalogRow{
		{Name: "P1", Position: 0, Boundary: "(10)", SubpartitionCount: 2, Subpartitions: []OscarPartitionCatalogRow{{Name: "S1", Position: 0, Boundary: "(1,NULL)"}, {Name: "Rest1", Position: 1, Boundary: "(DEFAULT)"}}},
		{Name: "P2", Position: 1, Boundary: "MAXVALUE", SubpartitionCount: 1, Subpartitions: []OscarPartitionCatalogRow{{Name: "S2", Position: 0, Boundary: "2"}}},
	}}
}

func TestOscarPartitionCatalogPreservesNativeNamesKeysAndLeaves(t *testing.T) {
	c := oscarPartitionCatalogFixture()
	s, err := DecodeOscarPartitionCatalog(c)
	require.NoError(t, err)
	require.Equal(t, "RANGE", s.Method)
	require.Equal(t, "Id", s.Keys[0].Column)
	require.Equal(t, "enabled", s.RowMovement)
	require.Equal(t, "MixedOwner", s.Partitions[0].Schema)
	require.Equal(t, 1, s.Partitions[0].Ordinal)
	require.Equal(t, "minvalue", s.Partitions[0].Lower[0].Kind)
	require.Equal(t, "10", s.Partitions[1].Lower[0].Text)
	require.Equal(t, "Category", s.Partitions[0].Subpartition.Keys[0].Column)
	require.Equal(t, "null", s.Partitions[0].Subpartition.Partitions[0].Values[1][0].Kind)
	require.True(t, s.Partitions[0].Subpartition.Partitions[1].Default)
	require.Equal(t, "enabled", s.Partitions[0].Subpartition.RowMovement)
	require.Equal(t, c.DDL, s.SourceDDL)
}

func TestOscarPartitionCatalogMultiColumnRangeAndQuotedBoundary(t *testing.T) {
	c := OscarPartitionCatalog{Schema: "Owner", Method: 1, KeyCount: 2, PartitionCount: 2, DDL: `CREATE TABLE "T"("A" INT,"B" INT) PARTITION BY RANGE(a,"B") (PARTITION p VALUES LESS THAN(-1,2), PARTITION q VALUES LESS THAN(MAXVALUE,MAXVALUE))`, Partitions: []OscarPartitionCatalogRow{{Name: "P", Position: 1, Boundary: "(-1,2)"}, {Name: "Q", Position: 2, Boundary: "(MAXVALUE,MAXVALUE)"}}}
	s, err := DecodeOscarPartitionCatalog(c)
	require.NoError(t, err)
	require.Equal(t, "A", s.Keys[0].Column)
	require.Equal(t, "-1", s.Partitions[0].Upper[0].Text)
	require.Len(t, s.Partitions[1].Lower, 2)
	c.Method, c.KeyCount, c.PartitionCount = 2, 1, 1
	c.DDL = `CREATE TABLE "T"("Key" VARCHAR(20)) PARTITION BY LIST("Key") (PARTITION "P" VALUES('a,b','O''Brien'))`
	c.Partitions = []OscarPartitionCatalogRow{{Name: "P", Boundary: "('a,b','O''Brien')"}}
	s, err = DecodeOscarPartitionCatalog(c)
	require.NoError(t, err)
	require.Equal(t, "a,b", s.Partitions[0].Values[0][0].Text)
	require.Equal(t, "O'Brien", s.Partitions[0].Values[1][0].Text)
}

func TestOscarPartitionCatalogRejectsIncompleteOrUnprovenSemantics(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*OscarPartitionCatalog)
	}{
		{"missing ddl", func(c *OscarPartitionCatalog) { c.DDL = "" }},
		{"not table ddl", func(c *OscarPartitionCatalog) {
			c.DDL = `PARTITION BY RANGE("Id") (PARTITION "P1" VALUES LESS THAN(10))`
		}},
		{"multiple statements", func(c *OscarPartitionCatalog) { c.DDL += `; SELECT 1` }},
		{"key count drift", func(c *OscarPartitionCatalog) { c.KeyCount = 2 }},
		{"method drift", func(c *OscarPartitionCatalog) { c.Method = 2 }},
		{"missing partition", func(c *OscarPartitionCatalog) { c.Partitions = c.Partitions[:1] }},
		{"missing child", func(c *OscarPartitionCatalog) { c.Partitions[0].Subpartitions = nil }},
		{"partial child list", func(c *OscarPartitionCatalog) { c.Partitions[0].Subpartitions = c.Partitions[0].Subpartitions[:1] }},
		{"excess child list", func(c *OscarPartitionCatalog) { c.Partitions[0].SubpartitionCount = 1 }},
		{"negative child count", func(c *OscarPartitionCatalog) { c.Partitions[0].SubpartitionCount = -1 }},
		{"hidden child count", func(c *OscarPartitionCatalog) { c.Partitions[0].Subpartitions[0].SubpartitionCount = 1 }},
		{"duplicate name", func(c *OscarPartitionCatalog) { c.Partitions[1].Name = "P1" }},
		{"duplicate order", func(c *OscarPartitionCatalog) { c.Partitions[1].Position = 0 }},
		{"executable bound", func(c *OscarPartitionCatalog) { c.Partitions[0].Boundary = "evil(1)" }},
		{"empty bound", func(c *OscarPartitionCatalog) { c.Partitions[0].Boundary = "" }},
		{"interval catalog", func(c *OscarPartitionCatalog) { c.Interval = "1" }},
		{"interval ddl", func(c *OscarPartitionCatalog) {
			c.DDL = `CREATE TABLE t(id INT) PARTITION BY RANGE(id) INTERVAL(1)(PARTITION p VALUES LESS THAN(1))`
		}},
		{"expression key", func(c *OscarPartitionCatalog) {
			c.DDL = `CREATE TABLE t(id INT) PARTITION BY RANGE(abs(id))(PARTITION p VALUES LESS THAN(1))`
		}},
		{"key list trailing comma", func(c *OscarPartitionCatalog) {
			c.DDL = `CREATE TABLE t(id INT) PARTITION BY RANGE(id,)(PARTITION p VALUES LESS THAN(1))`
		}},
		{"hash", func(c *OscarPartitionCatalog) {
			c.DDL = `CREATE TABLE t(id INT) PARTITION BY HASH(id)(PARTITION p)`
			c.Method = 3
		}},
		{"default not last", func(c *OscarPartitionCatalog) { c.Partitions[0].Subpartitions[0].Boundary = "DEFAULT" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := oscarPartitionCatalogFixture()
			tc.edit(&c)
			_, err := DecodeOscarPartitionCatalog(c)
			require.Error(t, err)
		})
	}
}
