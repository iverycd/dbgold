package datamigrate

import (
	"context"
	"slices"
	"testing"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

type pgOracleContentReader struct{ *contentTestReader }

func (*pgOracleContentReader) DBType() string { return "postgres" }

type pgOracleContentWriter struct{ *contentTestWriter }

func (*pgOracleContentWriter) DBType() string { return "oracle" }
func (w *pgOracleContentWriter) OpenValidationScan(_ context.Context, table string, columns []validation.Column) (validation.RowStream, error) {
	var rows [][]any
	for _, row := range w.rows[table] {
		projected := make([]any, len(columns))
		for i, c := range columns {
			for j, native := range w.metadata.Columns {
				if c.Name == native.Name {
					projected[i] = row[j]
				}
			}
		}
		rows = append(rows, projected)
	}
	return &contentTestStream{rows: rows}, nil
}

func pgOracleContentFixture() (*pgOracleContentReader, *pgOracleContentWriter, *Migrator) {
	r0, w0, m := contentFixture()
	r := &pgOracleContentReader{r0}
	w := &pgOracleContentWriter{w0}
	r.metadata = validation.Table{Columns: []validation.Column{
		{Name: "id", DatabaseType: "integer", Kind: validation.Number},
		{Name: "v", DatabaseType: "character varying", Kind: validation.Text},
		{Name: "lob", DatabaseType: "text", Kind: validation.Text},
		{Name: "bits", DatabaseType: "bit varying", Kind: validation.Bits},
	}, PrimaryKey: []string{"id"}}
	w.metadata = validation.Table{Columns: []validation.Column{
		{Name: "ID", DatabaseType: "NUMBER", Kind: validation.Number},
		{Name: "V", DatabaseType: "VARCHAR2", Kind: validation.Text},
		{Name: "LOB", DatabaseType: "CLOB", Kind: validation.Text},
		{Name: "BITS", DatabaseType: "VARCHAR2", Kind: validation.Text},
	}, PrimaryKey: []string{"ID"}}
	r.rows = map[string][][]any{"Users": {{int64(1), "", "", "001"}}}
	w.rows = map[string][][]any{"USERS": {{int64(1), nil, "", "001"}}}
	m.reader, m.writer = r, w
	m.cfg.LowerCaseNames = false
	m.cfg.TargetDBType = "oracle"
	return r, w, m
}

func runPGOracleContentValidation(m *Migrator) validation.TableResult {
	m.markDataValidationCopied("Users")
	report := MigrationReport{DataValidation: &validation.Report{}}
	m.validateDataContent(context.Background(), &report)
	return report.DataValidation.Tables[0]
}

func TestPGOracleContentAcceptedEmptyStringsPreserveLOBAndBitWidth(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bit, lob any
		status   string
	}{
		{"accepted", "001", "", validation.StatusPassed},
		{"bit-width-change", "1", "", validation.StatusMismatch},
		{"lost-empty-LOB", "001", nil, validation.StatusMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, w, m := pgOracleContentFixture()
			w.rows["USERS"][0][2], w.rows["USERS"][0][3] = tc.lob, tc.bit
			m.freezeDataValidation(context.Background(), []string{"Users"})
			result := runPGOracleContentValidation(m)
			require.Equal(t, tc.status, result.Status, result.Error)
			require.Contains(t, result.Note, "位串保留全部位")
		})
	}
	t.Run("empty-varbit", func(t *testing.T) {
		r, w, m := pgOracleContentFixture()
		r.rows["Users"][0][3], w.rows["USERS"][0][3] = "", nil
		m.freezeDataValidation(context.Background(), []string{"Users"})
		result := runPGOracleContentValidation(m)
		require.Equal(t, validation.StatusPassed, result.Status, result.Error)
	})
}

func TestPGOracleAppendValidationPreservesOldRowsAndAllowsExtraTargetColumn(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserved", true: "old-row-mutated"}[mutate], func(t *testing.T) {
			_, w, m := pgOracleContentFixture()
			m.cfg.Content = "data_only"
			w.metadata.Columns = append(w.metadata.Columns, validation.Column{Name: "EXTRA", DatabaseType: "VARCHAR2", Kind: validation.Text})
			w.rows["USERS"] = [][]any{{int64(99), "old", "oldLOB", "0001", "default"}}
			m.freezeDataValidation(context.Background(), []string{"Users"})
			defer m.closeValidationBaselines()
			require.NoError(t, m.pgOracleValidationCopyReady("Users"))
			w.rows["USERS"] = append(w.rows["USERS"], []any{int64(1), nil, "", "001", "default"})
			if mutate {
				w.rows["USERS"][0][1] = "changed"
			}
			result := runPGOracleContentValidation(m)
			want := validation.StatusPassed
			if mutate {
				want = validation.StatusMismatch
			}
			require.Equal(t, want, result.Status, result.Error)
			require.EqualValues(t, 1, result.SourceRows)
			require.EqualValues(t, 1, result.TargetRows)
		})
	}
}

func TestPGOracleAppendMetadataDriftAndFailedBaselineFailClosed(t *testing.T) {
	t.Run("extra-column-drift", func(t *testing.T) {
		_, w, m := pgOracleContentFixture()
		m.cfg.Content = "data_only"
		w.rows["USERS"] = nil
		m.freezeDataValidation(context.Background(), []string{"Users"})
		defer m.closeValidationBaselines()
		w.metadata.Columns = append(slices.Clone(w.metadata.Columns), validation.Column{Name: "LATER", DatabaseType: "NUMBER", Kind: validation.Number})
		result := runPGOracleContentValidation(m)
		require.Equal(t, validation.StatusError, result.Status)
		require.Contains(t, result.Error, "完整列数")
	})
	t.Run("baseline-unsupported", func(t *testing.T) {
		_, w, m := pgOracleContentFixture()
		m.cfg.Content = "data_only"
		w.metadata.Columns[1].Kind = validation.Unsupported
		m.freezeDataValidation(context.Background(), []string{"Users"})
		defer m.closeValidationBaselines()
		require.ErrorContains(t, m.pgOracleValidationCopyReady("Users"), "追加前原值基线冻结失败")
	})
}

func TestPGOracleOrdinaryRecoveryValidatesAcceptedMapping(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "bit-width-corruption"}[corrupt], func(t *testing.T) {
			r, w, m := pgOracleContentFixture()
			preview := repair.RecoveryPreview{SourceTable: "Users", TargetTable: "USERS", ColumnMapping: map[string]string{"id": "ID", "v": "V", "lob": "LOB", "bits": "BITS"}}
			for _, c := range r.metadata.Columns {
				preview.SourceColumns = append(preview.SourceColumns, source.ColumnInfo{Name: c.Name, DataType: c.DatabaseType})
			}
			check, err := freezePartitionRecoveryValidation(context.Background(), r, w, m.cfg, preview)
			require.NoError(t, err)
			require.NotNil(t, check, "ordinary PG recovery must honor ValidateData")
			if corrupt {
				w.rows["USERS"][0][3] = "1"
			}
			err = check.validate(context.Background(), r, w, preview)
			if corrupt {
				require.ErrorContains(t, err, "mismatch")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
