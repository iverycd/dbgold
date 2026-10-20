package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type oracleDMPreparedWriter struct {
	*oracleDMWriter
	prepared bool
	useDate  bool
	badView  string
	options  dialect.TypeOpt
	dia      *dialect.DaMengDialect
}

func (w *oracleDMPreparedWriter) Dialect() dialect.Dialect {
	if w.dia == nil {
		w.dia = dialect.NewDaMeng()
	}
	return w.dia
}

func (w *oracleDMPreparedWriter) PrepareOracleDameng(_ context.Context, infos []*source.TableDDLInfo, opt dialect.TypeOpt) error {
	w.prepared, w.options = true, opt
	for _, info := range infos {
		if w.badView != "" && info.TableName == w.badView {
			return fmt.Errorf("列 %s.TS 无法保留九位精度", info.TableName)
		}
	}
	return nil
}
func (w *oracleDMPreparedWriter) OracleDamengUsesDate() bool { return w.useDate }
func (w *oracleDMPreparedWriter) ValidateOracleDamengExisting(ctx context.Context, info *source.TableDDLInfo, opt dialect.TypeOpt) error {
	if !w.prepared {
		return fmt.Errorf("能力检查顺序错误")
	}
	return w.oracleDMWriter.ValidateOracleDamengExisting(ctx, info, opt)
}

type oracleDMValidatedReader struct {
	*oracleDMReader
	err   error
	infos []*source.TableDDLInfo
}

func (r *oracleDMValidatedReader) ValidateOracleDamengSource(_ context.Context, infos []*source.TableDDLInfo) error {
	r.infos = infos
	return r.err
}

func TestOracleDamengSourceCapabilityFailureHasZeroWrites(t *testing.T) {
	r, w, m := oracleDMFixture("both")
	validated := &oracleDMValidatedReader{oracleDMReader: r, err: fmt.Errorf("XML 源序列化函数不可用")}
	m.reader = validated
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
	require.Equal(t, "preflight", report.Tables.Items[0].Stage)
	require.Equal(t, "RowId", validated.infos[0].Columns[0].Name, "源投影必须使用原始名称")
}

func TestOracleDamengPreparedDATEIsIdenticalInRepairAndExecution(t *testing.T) {
	r, w, m := oracleDMFixture("schema_only")
	r.ddl["Mixed"].Columns[1].DataType = "DATE"
	prepared := &oracleDMPreparedWriter{oracleDMWriter: w, useDate: true}
	m.writer = prepared
	m.cfg.CaptureRepair = true
	var manifest RepairManifest
	m.cfg.RepairSink = func(value RepairManifest) error { manifest = value; return nil }
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Contains(t, w.created[0], `"TXT" DATE`)
	for _, object := range manifest.Objects {
		if object.Kind == "table" && object.SourceTable == "Mixed" {
			require.Contains(t, object.DDL, `"TXT" DATE`)
			require.NotContains(t, object.DDL, `"TXT" TIMESTAMP(0)`)
		}
	}
}

func TestOracleDamengDataOnlyPreparesOptionsBeforeExistingCheck(t *testing.T) {
	_, w, m := oracleDMFixture("data_only")
	prepared := &oracleDMPreparedWriter{oracleDMWriter: w}
	m.writer = prepared
	m.cfg.CharInLength, m.cfg.UseNvarchar2 = true, true
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, dialect.TypeOpt{CharInLength: true, UseNvarchar2: true}, prepared.options)
	require.Empty(t, w.created)
	require.Equal(t, 1, w.existingChecks)
}

func TestOracleDamengOneViewCapabilityFailureDoesNotBlockOtherViews(t *testing.T) {
	r, w, m := oracleDMFixture("schema_only")
	r.views = []source.ViewInfo{
		{ViewName: "BadView", Definition: `SELECT "Txt" FROM "SRC"."Mixed"`},
		{ViewName: "GoodView", Definition: `SELECT "Txt" FROM "SRC"."Mixed"`},
	}
	m.writer = &oracleDMPreparedWriter{oracleDMWriter: w, badView: "BadView"}
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Views.Failed)
	require.Equal(t, 1, report.Views.Success)
	require.Len(t, w.views, 1)
	require.Equal(t, "GOODVIEW", w.views[0].ViewName)
	require.True(t, strings.Contains(report.Views.Items[0].Error, "精度"))
}

func TestOracleDamengViewOnlyChecksExistingDependencyTypes(t *testing.T) {
	_, w, m := oracleDMFixture("schema_only")
	w.existingErr = fmt.Errorf("目标 XMLTYPE 被改成 VARCHAR")
	results := m.MigrateViews(context.Background(), []string{"V"})
	require.Len(t, results, 1)
	require.Contains(t, results[0].Error, "XMLTYPE")
	require.Equal(t, 1, w.existingChecks)
	require.Empty(t, w.views)
}

type oracleDMRownumViewWriter struct {
	*oracleDMWriter
	validated []string
}

func (w *oracleDMRownumViewWriter) ValidateOracleDamengView(_ context.Context, sql string) error {
	w.validated = append(w.validated, sql)
	if strings.Contains(strings.ToUpper(sql), "ROWNUM") {
		return fmt.Errorf("目标库 ROWNUM 能力探测失败")
	}
	return nil
}

func TestOracleDamengRownumViewCapabilityFailureDoesNotBlockOtherViews(t *testing.T) {
	r, w, m := oracleDMModuleVisitLogFixture()
	r.views = append(r.views, source.ViewInfo{
		ViewName:   "VIEW_VISIT_CODES",
		Definition: `SELECT ModuleCode FROM Frame_VisitLog`,
		Columns:    []source.ColumnInfo{{Name: "MODULECODE"}},
	})
	validated := &oracleDMRownumViewWriter{oracleDMWriter: w}
	m.writer = validated
	results := m.MigrateViews(context.Background(), []string{"VIEW_MODULEVISITLOG", "VIEW_VISIT_CODES"})
	require.Len(t, results, 2)
	require.Equal(t, "VIEW_MODULEVISITLOG", results[0].Name)
	require.Contains(t, results[0].Error, "ROWNUM 能力探测失败")
	require.Equal(t, "VIEW_VISIT_CODES", results[1].Name)
	require.Empty(t, results[1].Error)
	require.Len(t, validated.validated, 2)
	require.Equal(t, oracleDMModuleVisitLogSQL, validated.validated[0])
	require.Len(t, w.views, 1)
	require.Equal(t, "VIEW_VISIT_CODES", w.views[0].ViewName)
	require.Contains(t, results[1].DDL, `CREATE OR REPLACE VIEW "APP"."VIEW_VISIT_CODES" ("MODULECODE") AS `)
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
}
