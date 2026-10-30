package partition

import (
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func intervalFixture() *source.TableDDLInfo {
	number := func(s string) []source.PartitionValue { return []source.PartitionValue{{Kind: "number", Text: s}} }
	return &source.TableDDLInfo{TableName: "Events", Columns: []source.ColumnInfo{{Name: "Id", DataType: "NUMBER", Precision: 5}, {Name: "Note", DataType: "VARCHAR2"}}, Partition: &source.PartitionSpec{Method: "RANGE", Keys: []source.PartitionKey{{Column: "Id", DataType: "NUMBER"}}, RowMovement: "disabled", Automatic: " 10 ", Interval: &source.PartitionInterval{Unit: "number", Amount: "10", Transition: number("0")[0]}, Partitions: []source.Partition{
		{Schema: "Src", Name: "Before", Ordinal: 1, Lower: []source.PartitionValue{{Kind: "minvalue"}}, Upper: number("0"), LowerInclusive: true},
		{Schema: "Src", Name: "Far", Ordinal: 6, Lower: number("40"), Upper: number("50"), LowerInclusive: true, IsInterval: true},
		{Schema: "Src", Name: "Near", Ordinal: 9, Lower: number("20"), Upper: number("30"), LowerInclusive: true, IsInterval: true},
	}}}
}

func TestIntervalCompilePreservesNativeRuleSparseLeavesAndRepairRecipe(t *testing.T) {
	info := intervalFixture()
	profile := compilerProfile("dameng")
	profile.Features[FeatureInterval] = true
	plan, e := Compile(info, []dialect.Statement{{SQL: `CREATE TABLE "Dst"."Events"("Id" DECIMAL(38),"Note" VARCHAR(20))`}}, profile, Options{SourceType: "dameng", TargetType: "dameng", TargetSchema: "Dst"})
	require.NoError(t, e)
	require.Contains(t, plan.Statements[0].SQL, "INTERVAL (10)")
	require.Contains(t, plan.Statements[0].SQL, `PARTITION "Before"`)
	require.NotContains(t, plan.Statements[0].SQL, `PARTITION "Far"`)
	require.Len(t, plan.Target.Partitions, 3)
	require.Equal(t, 2, plan.Target.Partitions[1].Ordinal)
	require.Equal(t, 6, plan.Source.Partitions[1].Ordinal)
	require.Equal(t, "40", plan.Target.Partitions[1].Lower[0].Text)
	require.Len(t, plan.Interval.Seeds, 2)
	require.Equal(t, "40", plan.Interval.Seeds[0].Value.Text)
	require.Equal(t, IntervalMaterializationSQL(plan), plan.Statements[1].SQL)
	changed := *plan
	recipe := *plan.Interval
	changed.Interval = &recipe
	recipe.Table = "Other"
	require.NotEqual(t, plan.Statements[1].SQL, IntervalMaterializationSQL(&changed))
	actual := source.ClonePartitionSpec(plan.Target)
	actual.Automatic = "  1e1 "
	require.NoError(t, Equivalent(plan.Target, actual))
}

func TestIntervalCompileFailsClosedForUnprovedSemantics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*source.TableDDLInfo, *Profile)
	}{
		{"no native target", func(_ *source.TableDDLInfo, p *Profile) { p.Features[FeatureInterval] = false }},
		{"missing transition", func(i *source.TableDDLInfo, _ *Profile) { i.Partition.Interval = nil }},
		{"nullable key routes differently", func(i *source.TableDDLInfo, _ *Profile) { i.Columns[0].IsNullable = true }},
		{"unconstrained NUMBER domain", func(i *source.TableDDLInfo, _ *Profile) { i.Columns[0].Precision = 0 }},
		{"complete numeric domain exceeds target capacity", func(i *source.TableDDLInfo, _ *Profile) { i.Columns[0].Precision = 10 }},
		{"step beyond key scale", func(i *source.TableDDLInfo, _ *Profile) {
			i.Partition.Automatic = "0.5"
			i.Partition.Interval.Amount = "0.5"
			i.Partition.Partitions = i.Partition.Partitions[:1]
		}},
		{"wrong automatic lower", func(i *source.TableDDLInfo, _ *Profile) { i.Partition.Partitions[1].Lower[0].Text = "0" }},
		{"wrong grid", func(i *source.TableDDLInfo, _ *Profile) { i.Partition.Partitions[1].Upper[0].Text = "51" }},
		{"generated side effect", func(i *source.TableDDLInfo, _ *Profile) { i.Columns[1].Generated = "user_function(Id)" }},
		{"unhandled mandatory type", func(i *source.TableDDLInfo, _ *Profile) { i.Columns[1].DataType = "XMLTYPE" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := intervalFixture()
			profile := compilerProfile("dameng")
			profile.Features[FeatureInterval] = true
			tc.change(info, &profile)
			src := "dameng"
			if strings.Contains(tc.name, "nullable") || strings.Contains(tc.name, "NUMBER") || strings.Contains(tc.name, "complete numeric") || strings.Contains(tc.name, "step beyond") {
				src = "oracle"
			}
			_, e := Compile(info, []dialect.Statement{{SQL: `CREATE TABLE "Dst"."Events"("Id" INT)`}}, profile, Options{SourceType: src, TargetType: "dameng", TargetSchema: "Dst"})
			require.Error(t, e)
		})
	}
	for _, bad := range []source.PartitionValue{{Kind: "number", Text: "1;CALL bad()"}, {Kind: "maxvalue"}, {Kind: "string", Text: "ok"}} {
		sql, e := IntervalLiteral(bad)
		if bad.Kind == "string" {
			require.NoError(t, e)
			require.True(t, strings.HasPrefix(sql, "'"))
		} else {
			require.Error(t, e)
		}
	}
}

func TestOracleNumericIntervalCannotClaimFullKeyDomainEquivalence(t *testing.T) {
	i := intervalFixture()
	p := compilerProfile("dameng")
	p.Features[FeatureInterval] = true
	_, e := Compile(i, []dialect.Statement{{SQL: `CREATE TABLE "Dst"."Events"("Id" NUMBER(5))`}}, p, Options{SourceType: "oracle", TargetType: "dameng", TargetSchema: "Dst"})
	require.ErrorContains(t, e, "列域最大值")
	require.ErrorContains(t, e, "CHECK/DOMAIN")
	require.ErrorContains(t, e, "额外自动空叶")
}

func TestIntervalTemporalKeyPrecisionAndTimezoneAreNotDiscarded(t *testing.T) {
	for _, tc := range []struct {
		typ, amount string
		valid       bool
	}{{"DATE", "0.5", false}, {"DATE", "1", true}, {"TIMESTAMP(6)", "0.000001", false}, {"TIMESTAMP(3)", "0.000001", false}, {"TIMESTAMP(6) WITH TIME ZONE", "1", false}, {"TIMESTAMP(6) WITH LOCAL TIME ZONE", "1", false}} {
		t.Run(tc.typ+tc.amount, func(t *testing.T) {
			i := intervalFixture()
			i.Columns[0].DataType = tc.typ
			i.Partition.Partitions = i.Partition.Partitions[:1]
			i.Partition.Partitions[0].Upper = []source.PartitionValue{{Kind: "timestamp", Text: "9999-12-31 12:00:00"}}
			i.Partition.Interval = &source.PartitionInterval{Unit: "second", Amount: tc.amount, Transition: i.Partition.Partitions[0].Upper[0]}
			i.Partition.Automatic = "NUMTODSINTERVAL(" + tc.amount + ",'SECOND')"
			p := compilerProfile("dameng")
			p.Features[FeatureInterval] = true
			c := compiler{info: i, profile: p, opt: Options{SourceType: "oracle", TargetType: "dameng"}}
			err := c.prepareInterval(source.ClonePartitionSpec(i.Partition), i.Partition, 0)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestIntervalCapacityCoversCompleteDomainAndAllInitialPartitions(t *testing.T) {
	s := intervalFixture().Partition
	s.Partitions = s.Partitions[:1]
	s.Interval.Amount = "1"
	key := source.ColumnInfo{DataType: "NUMBER", Precision: 5}
	s.Interval.Transition = source.PartitionValue{Kind: "number", Text: "34466"}
	require.NoError(t, oracleDamengIntervalCapacity(s, key, 1))
	require.ErrorContains(t, oracleDamengIntervalCapacity(s, key, 2), "65535")
	s.Interval.Transition.Text = "34467"
	require.NoError(t, oracleDamengIntervalCapacity(s, key, 2))
	for _, tc := range []struct {
		unit, step, anchor string
		pass               bool
	}{{"month", "1", "2024-01-01 00:00:00", false}, {"month", "1", "5000-01-01 00:00:00", true}, {"second", "86400", "2024-01-01 00:00:00", false}, {"second", "86400", "9900-01-01 00:00:00", true}} {
		s.Interval = &source.PartitionInterval{Unit: tc.unit, Amount: tc.step, Transition: source.PartitionValue{Kind: "timestamp", Text: tc.anchor}}
		err := oracleDamengIntervalCapacity(s, source.ColumnInfo{DataType: "DATE"}, 1)
		if tc.pass {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
}

func TestIntervalCanonicalAllocationOrderDoesNotChangeNamedRouting(t *testing.T) {
	want := intervalFixture().Partition
	other := source.ClonePartitionSpec(want)
	other.Partitions[1], other.Partitions[2] = other.Partitions[2], other.Partitions[1]
	for i := range other.Partitions {
		other.Partitions[i].Ordinal = i + 1
	}
	require.NoError(t, Equivalent(want, other))
	for _, change := range []func(*source.PartitionSpec){func(s *source.PartitionSpec) { s.Partitions[1].Name = "Changed" }, func(s *source.PartitionSpec) { s.Partitions[1].Upper[0].Text = "60" }, func(s *source.PartitionSpec) { s.Partitions = s.Partitions[:2] }} {
		bad := source.ClonePartitionSpec(other)
		change(bad)
		require.Error(t, Equivalent(want, bad))
	}
	ordinary := compilerRangeFixture().Partition
	reordered := source.ClonePartitionSpec(ordinary)
	reordered.Partitions[0].Ordinal = 3
	require.Error(t, Equivalent(ordinary, reordered), "ordinary range/SQLServer ordinals remain strict")
	hash := source.ClonePartitionSpec(ordinary)
	hash.Method = "HASH"
	hash.Algorithm = "native-hash"
	changed := source.ClonePartitionSpec(hash)
	changed.Partitions[0].Ordinal = 3
	require.Error(t, Equivalent(hash, changed))
}
