package datamigrate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type spatialMigrationWriter struct {
	*partialMigrationWriter
	cap                     target.PGSpatialCapabilities
	prepareErr, existingErr error
	prepares, inspections   int
	checked                 []string
}

func (w *spatialMigrationWriter) PrepareMySQLPGSpatial(context.Context) (*target.PGSpatialCapabilities, error) {
	w.prepares++
	return &w.cap, w.prepareErr
}
func (w *spatialMigrationWriter) InspectMySQLPGSpatial(context.Context) (*target.PGSpatialCapabilities, error) {
	w.inspections++
	return &w.cap, w.prepareErr
}
func (w *spatialMigrationWriter) CheckMySQLPGSpatialTable(_ context.Context, table string, info *source.TableDDLInfo, _ *target.PGSpatialCapabilities) error {
	w.checked = append(w.checked, table)
	if table == "broken" {
		return w.existingErr
	}
	return nil
}

func spatialMigrationFixture(kind string, first bool) (*partialMigrationReader, *spatialMigrationWriter, *Migrator) {
	r, base, m := partialMigrationFixture("mysql", kind, "", first)
	r.ddl["Broken"].Columns[0].DataType = "point"
	r.ddl["Broken"].Columns[0].IsNullable = true
	w := &spatialMigrationWriter{partialMigrationWriter: base, cap: target.PGSpatialCapabilities{Schema: "Geo Ext", GISTSchema: "Geo Ext", GISTOpClass: "gist_geometry_ops_2d"}}
	m.writer = w
	return r, w, m
}

func TestMySQLSpatialCapabilityFailureIsTableLocal(t *testing.T) {
	for _, kind := range []string{"postgres", "gaussdb", "highgo", "kingbase", "vastbase", "gbase", "seabox"} {
		for _, first := range []bool{true, false} {
			t.Run(kind+map[bool]string{true: "/first", false: "/middle"}[first], func(t *testing.T) {
				_, w, m := spatialMigrationFixture(kind, first)
				w.prepareErr = errors.New("postgis 扩展包未安装")
				r := m.Run(context.Background())
				require.Equal(t, 1, w.prepares)
				require.Equal(t, 2, r.Tables.Success)
				require.Equal(t, 1, r.Tables.Failed)
				require.Equal(t, 2, r.Data.Success)
				require.ElementsMatch(t, []string{"a", "z"}, w.copied)
				require.Contains(t, r.Tables.Items[0].Error, "postgis")
				for _, sql := range w.attempts {
					require.NotContains(t, sql, `"broken"`)
				}
				require.Len(t, w.fks, 1)
				require.Equal(t, "healthy_fk", w.fks[0].ConstraintName)
			})
		}
	}
}

func TestMySQLSpatialOrdinaryTablesDoNotPrepare(t *testing.T) {
	r, w, m := spatialMigrationFixture("postgres", false)
	r.ddl["Broken"].Columns[0].DataType = "int"
	rpt := m.Run(context.Background())
	require.Equal(t, 3, rpt.Tables.Success)
	require.Zero(t, w.prepares)
	require.Nil(t, rpt.SpatialCapabilities)
}

func TestMySQLSpatialDataOnlyChecksBeforeCopy(t *testing.T) {
	_, w, m := spatialMigrationFixture("postgres", false)
	m.cfg.Content = "data_only"
	w.existingErr = errors.New("目标列不是 geometry，不能写入空间数据")
	r := m.Run(context.Background())
	require.Equal(t, []string{"broken"}, w.checked)
	require.Empty(t, w.attempts)
	require.ElementsMatch(t, []string{"a", "z"}, w.copied)
	require.Equal(t, 3, r.Data.Total)
	require.Equal(t, 2, r.Data.Success)
	require.Equal(t, 1, r.Data.Failed)
	require.Equal(t, "preflight", r.Data.Items[0].Stage)
}

func TestMySQLSpatialGISTFailureDoesNotBlockData(t *testing.T) {
	r, w, m := spatialMigrationFixture("postgres", false)
	r.indexes[1].Method = "SPATIAL"
	r.indexes[1].Unsupported = source.MySQLSpatialIndexUnsupported
	w.cap.GISTError, w.cap.GISTOpClass = "GiST 未启用", ""
	rpt := m.Run(context.Background())
	require.Equal(t, 3, rpt.Tables.Success)
	require.Equal(t, 3, rpt.Data.Success)
	require.Equal(t, 1, rpt.Indexes.Failed)
	require.Contains(t, rpt.Indexes.Items[0].Error, "GiST")
	require.Equal(t, 1, w.prepares)
}

func TestMySQLSpatialIndexAndRepairPlanShareQualifiedDDL(t *testing.T) {
	r, w, m := spatialMigrationFixture("postgres", false)
	r.indexes[1].Method = "SPATIAL"
	r.indexes[1].Unsupported = source.MySQLSpatialIndexUnsupported
	m.cfg.CaptureRepair = true
	rpt := m.Run(context.Background())
	require.Equal(t, 3, rpt.Tables.Success)
	require.Equal(t, 3, rpt.Indexes.Success)
	require.Equal(t, 1, w.prepares)
	require.Contains(t, strings.Join(w.created, "\n"), `"Geo Ext"."geometry"(Point)`)
	require.Equal(t, "point", r.ddl["Broken"].Columns[0].DataType)
	require.Equal(t, "SPATIAL", r.indexes[1].Method)
	found := false
	for _, idx := range w.indexes {
		if idx.TableName == "broken" && !idx.IsPrimary {
			require.Equal(t, "gist", idx.Method)
			require.Contains(t, dialect.JoinSQL(w.Dialect().IndexStatements("", idx)), `USING gist`)
			found = true
		}
	}
	require.True(t, found)
	require.NotNil(t, rpt.RepairManifest)
	for _, obj := range rpt.RepairManifest.Objects {
		if obj.Kind == "index" && obj.SourceTable == "Broken" {
			require.Equal(t, "gist", obj.Index.Method)
			require.Equal(t, "Geo Ext", obj.Index.PGSpatialSchema)
		}
	}
}

func TestMySQLSpatialRecoveryUsesReadOnlyInspection(t *testing.T) {
	r, w, _ := spatialMigrationFixture("postgres", false)
	err := inspectMySQLPGSpatialRecovery(context.Background(), r, w, r.ddl["Broken"], "broken", map[string]string{"Id": "id"})
	require.NoError(t, err)
	require.Zero(t, w.prepares)
	require.Equal(t, 1, w.inspections)
	require.Equal(t, []string{"broken"}, w.checked)
}

func TestMySQLSpatialMetadataClonePreservesSRIDIsolation(t *testing.T) {
	srid := uint32(4326)
	info := &source.TableDDLInfo{Columns: []source.ColumnInfo{{DataType: "point", SpatialSRID: &srid}}}
	cloned := cloneSourceTableDDLInfo(info)
	*cloned.Columns[0].SpatialSRID = 0
	require.EqualValues(t, 4326, *info.Columns[0].SpatialSRID)
}

func TestMySQLSpatialDataOnlyDoesNotConvertSourceDefault(t *testing.T) {
	r, w, m := spatialMigrationFixture("postgres", false)
	def := "point(1, 2)"
	r.ddl["Broken"].Columns[0].Default = &def
	m.cfg.Content, m.cfg.CaptureRepair = "data_only", true
	rpt := m.Run(context.Background())
	require.Equal(t, 3, rpt.Data.Success)
	require.Zero(t, rpt.Data.Failed)
	require.ElementsMatch(t, []string{"a", "broken", "z"}, w.copied)
	for _, obj := range rpt.RepairManifest.Objects {
		if obj.SourceTable == "Broken" {
			require.NotContains(t, obj.Error, "默认表达式")
		}
	}
}

type spatialPartitionWriter struct {
	*partitionFlowWriter
	spy *spatialMigrationWriter
}

func (w *spatialPartitionWriter) PrepareMySQLPGSpatial(ctx context.Context) (*target.PGSpatialCapabilities, error) {
	return w.spy.PrepareMySQLPGSpatial(ctx)
}
func (w *spatialPartitionWriter) InspectMySQLPGSpatial(ctx context.Context) (*target.PGSpatialCapabilities, error) {
	return w.spy.InspectMySQLPGSpatial(ctx)
}
func (w *spatialPartitionWriter) CheckMySQLPGSpatialTable(ctx context.Context, table string, info *source.TableDDLInfo, caps *target.PGSpatialCapabilities) error {
	return w.spy.CheckMySQLPGSpatialTable(ctx, table, info, caps)
}

func TestMySQLSpatialPartitionPreviewNeverEnablesExtension(t *testing.T) {
	m, r, original := newPartitionFlow(t, "Root")
	r.ddl["Root"].Columns = append(r.ddl["Root"].Columns, source.ColumnInfo{Name: "Shape", DataType: "point", IsNullable: true})
	r.specs["Root"] = partitionFlowSpec()
	spy := &spatialMigrationWriter{cap: target.PGSpatialCapabilities{Schema: "geo"}}
	m.writer = &spatialPartitionWriter{partitionFlowWriter: original, spy: spy}
	p := m.PreflightPartitions(context.Background())
	require.True(t, p.CanStart, "%+v", p)
	require.Zero(t, spy.prepares)
	require.Equal(t, 1, spy.inspections)
	require.Empty(t, original.created)
	m.Run(context.Background())
	require.Equal(t, 1, spy.prepares, "execution must recheck and may enable an available extension")
}
