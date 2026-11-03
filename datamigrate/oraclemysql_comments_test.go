package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type oracleCommentReader struct {
	*oracleMySQLTestReader
	comments []source.CommentInfo
}

func (r *oracleCommentReader) GetComments(context.Context) ([]source.CommentInfo, error) {
	return r.comments, nil
}

type oracleCommentWriter struct {
	*oracleMySQLTestWriter
	prepared      []source.CommentInfo
	executed      []string
	prepareFailed string
	writeFailed   string
	columnType    string
	onExecute     func()
}

func (w *oracleCommentWriter) PrepareComment(_ context.Context, cm source.CommentInfo) ([]dialect.Statement, error) {
	w.prepared = append(w.prepared, cm)
	if cm.ColumnName == w.prepareFailed && w.prepareFailed != "" {
		return nil, errors.New("SHOW CREATE unavailable")
	}
	if cm.ColumnName == "" {
		return w.Dialect().CommentStatements("dst", cm), nil
	}
	columnType := w.columnType
	if columnType == "" {
		columnType = "bigint"
	}
	return []dialect.Statement{{SQL: fmt.Sprintf("ALTER TABLE %s MODIFY COLUMN %s %s NOT NULL COMMENT %s", w.Dialect().QualifyTable("dst", cm.TableName), w.Dialect().QuoteIdent(cm.ColumnName), columnType, sqlsyntax.QuoteString(cm.Comment))}}, nil
}

func (w *oracleCommentWriter) ExecuteCommentStatements(ctx context.Context, ss []dialect.Statement) error {
	for _, s := range ss {
		if err := repair.Observe(ctx, s.SQL, "running", nil); err != nil {
			return err
		}
		w.executed = append(w.executed, s.SQL)
		if w.onExecute != nil {
			w.onExecute()
		}
		if w.writeFailed != "" && strings.Contains(s.SQL, "`"+w.writeFailed+"`") {
			err := errors.New("comment denied")
			return errors.Join(err, repair.Observe(ctx, s.SQL, "failed", err))
		}
		if err := repair.Observe(ctx, s.SQL, "succeeded", nil); err != nil {
			return err
		}
	}
	return nil
}

func oracleCommentFixture(t *testing.T) (*Migrator, *oracleCommentReader, *oracleCommentWriter) {
	t.Helper()
	m, r, w := oracleMySQLFixture(t, "MixedCase")
	reader := &oracleCommentReader{oracleMySQLTestReader: r}
	writer := &oracleCommentWriter{oracleMySQLTestWriter: w}
	m.reader, m.writer = reader, writer
	return m, reader, writer
}

func TestOracleMySQLCommentsMigrationModes(t *testing.T) {
	for _, mode := range []string{"schema_only", "both", "data_only", "objects"} {
		for _, capture := range []bool{false, true} {
			for _, lower := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/capture=%v/lower=%v", mode, capture, lower), func(t *testing.T) {
					m, r, w := oracleCommentFixture(t)
					r.comments = []source.CommentInfo{{TableName: "MixedCase", Comment: "表 '注释' \\path"}, {TableName: "MixedCase", ColumnName: "ID", Comment: "列 '注释' \\path\n下一行"}, {TableName: "NotSelected", ColumnName: "ID", Comment: "outside"}}
					m.cfg.Content, m.cfg.CaptureRepair, m.cfg.LowerCaseNames = mode, capture, lower
					if mode == "objects" {
						m.cfg.Content, m.cfg.Objects, m.cfg.TableNames = "schema_only", []string{"comments"}, []string{"MixedCase"}
					}
					report := m.Run(context.Background())
					require.False(t, report.HasFailures(), "%+v", report)
					if mode == "data_only" {
						require.Empty(t, w.prepared)
						require.Empty(t, w.executed)
						require.Zero(t, report.Comments.Total)
						return
					}
					require.Equal(t, 2, report.Comments.Total)
					require.Equal(t, 2, report.Comments.Success)
					require.Len(t, w.prepared, 2, "one preparation per executed comment")
					require.Len(t, w.executed, 2)
					require.Equal(t, m.objName("MixedCase"), w.prepared[1].TableName)
					require.Equal(t, m.objName("ID"), w.prepared[1].ColumnName)
					require.Equal(t, r.comments[1].Comment, w.prepared[1].Comment)
					if capture {
						var count int
						for _, o := range report.RepairManifest.Objects {
							if o.Kind != "comment" {
								continue
							}
							require.Equal(t, "succeeded", o.Status)
							require.Len(t, o.Units, 1)
							require.Contains(t, w.executed, o.DDL)
							require.Equal(t, o.DDL, o.Units[0].SQL)
							require.Equal(t, "succeeded", o.Units[0].Status)
							count++
						}
						require.Equal(t, 2, count)
					}
				})
			}
		}
	}
}

func TestOracleMySQLCommentTruncationAndDeferredInventory(t *testing.T) {
	for _, length := range []int{1023, 1024, 1025} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			m, r, w := oracleCommentFixture(t)
			original := strings.Repeat("中", length-1) + "😀"
			r.comments = []source.CommentInfo{{TableName: "MixedCase", ColumnName: "ID", Comment: original}}
			m.cfg.CaptureRepair, m.cfg.Objects, m.cfg.LowerCaseNames = true, []string{"comments"}, true
			m.beginRepair(context.Background())
			require.Empty(t, w.prepared, "no target definition reads before the execution stage")
			var comment repair.Object
			for _, o := range m.repairs.objects {
				if o.Kind == "comment" {
					comment = o
				}
			}
			require.NotNil(t, comment.Comment)
			require.Empty(t, comment.Units)
			require.Equal(t, min(length, 1024), utf8.RuneCountInString(comment.Comment.Comment))
			report := newMigrationReport()
			m.createPostDDL(context.Background(), &report, []string{"MixedCase"}, []string{"MixedCase"})
			m.finishRepair(context.Background(), &report)
			require.Equal(t, 1, report.Comments.Success)
			require.Equal(t, original, r.comments[0].Comment)
			require.True(t, utf8.ValidString(w.prepared[0].Comment))
			require.Equal(t, min(length, 1024), utf8.RuneCountInString(w.prepared[0].Comment))
			for _, o := range report.RepairManifest.Objects {
				if o.Kind == "comment" {
					require.Equal(t, w.prepared[0].Comment, o.Comment.Comment)
				}
			}
			if length <= 1024 {
				require.Empty(t, report.AutoRepairs)
			} else {
				require.Len(t, report.AutoRepairs, 1)
				a := report.AutoRepairs[0]
				require.Equal(t, "oracle_mysql_comment_truncate", a.Type)
				require.Equal(t, "MixedCase", a.Table)
				require.Equal(t, "mixedcase", a.TargetTable)
				require.Equal(t, "ID", a.Source)
				require.Equal(t, "id", a.Target)
				require.Contains(t, a.Reason, "1025 字符截断到 1024")
				require.Contains(t, strings.Join(drainLogs(m.job), "\n"), "列注释截断 [mixedcase.id]")
			}
		})
	}
	cm := source.CommentInfo{TableName: "T", Comment: strings.Repeat("中", 1025)}
	require.Equal(t, cm, limitMySQLColumnComment(cm), "the column limit does not change table comments")
}

func TestOracleMySQLCommentFailuresRemainRecoverable(t *testing.T) {
	m, r, w := oracleCommentFixture(t)
	r.comments = []source.CommentInfo{{TableName: "MixedCase", ColumnName: "ReadFail", Comment: "read"}, {TableName: "MixedCase", ColumnName: "WriteFail", Comment: strings.Repeat("中", 1025)}, {TableName: "MixedCase", ColumnName: "ID", Comment: "healthy"}}
	m.cfg.CaptureRepair, m.cfg.Objects = true, []string{"comments"}
	w.prepareFailed, w.writeFailed = "ReadFail", "WriteFail"
	report := m.Run(context.Background())
	require.Equal(t, 3, report.Comments.Total)
	require.Equal(t, 2, report.Comments.Failed)
	require.Equal(t, 1, report.Comments.Success)
	require.Len(t, w.prepared, 3)
	require.Len(t, w.executed, 2)
	require.Empty(t, report.AutoRepairs, "failed writes must not be reported as applied truncations")
	seen := map[string]repair.Object{}
	for _, o := range report.RepairManifest.Objects {
		seen[o.Name] = o
	}
	read := seen["MixedCase.ReadFail"]
	require.Equal(t, "failed", read.Status)
	require.Contains(t, read.Error, "SHOW CREATE unavailable")
	require.Empty(t, read.Units)
	write := seen["MixedCase.WriteFail"]
	require.Equal(t, "failed", write.Status)
	require.Equal(t, "failed", write.Units[0].Status)
	require.True(t, write.Units[0].Selected)
	require.Equal(t, w.executed[0], write.Units[0].SQL)
	require.Equal(t, w.executed[0], report.Comments.Items[1].DDL)
	require.Equal(t, 1024, utf8.RuneCountInString(write.Comment.Comment))
	require.Equal(t, "succeeded", seen["MixedCase.ID"].Status)
}

func TestOracleMySQLCommentDependencyAndCancellation(t *testing.T) {
	m, r, w := oracleCommentFixture(t)
	r.comments = []source.CommentInfo{{TableName: "MixedCase", ColumnName: "ID", Comment: "blocked"}, {TableName: "Excluded", ColumnName: "ID", Comment: "excluded"}}
	m.cfg.Objects = []string{"comments"}
	m.dmBlocked = map[string]bool{"MixedCase": true}
	report := newMigrationReport()
	m.createPostDDL(context.Background(), &report, []string{"MixedCase"}, []string{"MixedCase", "Excluded"})
	require.Equal(t, 1, report.Comments.Total)
	require.Equal(t, 1, report.Comments.Failed)
	require.Empty(t, w.prepared)
	m.dmBlocked = nil
	r.comments = append(r.comments[:1], source.CommentInfo{TableName: "MixedCase", ColumnName: "Second", Comment: "later"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.onExecute = cancel
	report = newMigrationReport()
	m.createPostDDL(ctx, &report, []string{"MixedCase"}, []string{"MixedCase"})
	require.Len(t, w.prepared, 1)
	require.Len(t, w.executed, 1)
	require.Equal(t, 1, report.Comments.Total)
}

func TestOracleMySQLRecoveryCommentRegeneration(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprint(custom), func(t *testing.T) {
			r, _, cfg, table, ops, _ := recoveryFixture(t)
			reader := &oracleCommentReader{oracleMySQLTestReader: &oracleMySQLTestReader{mockReader: r.mockReader}}
			_, _, w := oracleCommentFixture(t)
			cm := source.CommentInfo{TableName: table.Table, ColumnName: "old", Comment: strings.Repeat("中", 1025)}
			o := repair.Object{Kind: "comment", Schema: "dst", Table: table.Table, TargetName: table.Table + ".old", Comment: &cm, ColumnMapping: map[string]string{"ID": "old"}, Units: []repair.Unit{{SQL: "old SQL", OriginalSQL: "old SQL"}}}
			if custom {
				o.Units[0].SQL = "manually edited SQL"
			}
			p, err := previewRecovery(context.Background(), reader, w, cfg, table, []repair.Object{o}, nil, ops)
			require.NoError(t, err)
			require.Len(t, p.Related, 1)
			got := p.Related[0]
			require.Equal(t, "renamed", got.Comment.ColumnName)
			require.Equal(t, 1024, utf8.RuneCountInString(got.Comment.Comment))
			require.Contains(t, got.SuggestedDDL, "`renamed` bigint NOT NULL")
			require.True(t, got.RequiresReview)
			if custom {
				require.Equal(t, "manually edited SQL", got.Units[0].SQL)
			} else {
				require.Equal(t, got.SuggestedDDL, got.Units[0].SQL)
			}
			require.Empty(t, w.executed, "recovery preview must not execute comment DDL")
			w.prepareFailed = "renamed"
			fallback, err := previewRecovery(context.Background(), reader, w, cfg, table, []repair.Object{o}, nil, ops)
			if custom {
				require.NoError(t, err)
				require.Equal(t, "manually edited SQL", fallback.Related[0].Units[0].SQL)
				require.Empty(t, fallback.Related[0].SuggestedDDL)
			} else {
				require.ErrorContains(t, err, "SHOW CREATE unavailable")
			}
		})
	}
}

func TestOracleMySQLRecoveryCommentReviewTracksSQLChanges(t *testing.T) {
	_, _, w := oracleCommentFixture(t)
	cm := source.CommentInfo{TableName: "MixedCase", ColumnName: "ID", Comment: "new"}
	o := repair.Object{Kind: "comment", Comment: &cm}
	first, err := prepareOracleMySQLRecoveryComment(context.Background(), w, o, o)
	require.NoError(t, err)
	require.True(t, first.RequiresReview, "a newly generated candidate must be reviewed")
	first.RequiresReview = false // Saved by the existing draft review workflow.
	second, err := prepareOracleMySQLRecoveryComment(context.Background(), w, first, first)
	require.NoError(t, err)
	require.Equal(t, first, second, "unchanged reviewed candidates must keep the same fingerprint")
	w.columnType = "varchar(80)"
	changed, err := prepareOracleMySQLRecoveryComment(context.Background(), w, second, second)
	require.NoError(t, err)
	require.True(t, changed.RequiresReview, "target definition drift changes the SQL and needs review")
	require.Contains(t, changed.Units[0].SQL, "varchar(80)")
}

// Confirm the optional target capability is implemented by the real writer.
var _ preparedCommentWriter = (*target.MySQLWriter)(nil)
