package datamigrate

import (
	"context"
	"encoding/json"
	"testing"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type floatingRecoveryReader struct{ *recoveryTestReader }

func (*floatingRecoveryReader) DBType() string { return "dameng" }

func TestRecoveryRefreshesDamengFloatStorageAfterSerialization(t *testing.T) {
	r, w, cfg, obj, ops, _ := recoveryFixture(t)
	r.ddl[obj.SourceTable].Columns[0].DataType = "DOUBLE"
	r.ddl[obj.SourceTable].Columns[0].DamengFloatStorage = "double"
	dmReader := &floatingRecoveryReader{r}
	p, err := previewRecovery(context.Background(), dmReader, w, cfg, obj, nil, nil, ops)
	require.NoError(t, err)
	data, err := json.Marshal(p)
	require.NoError(t, err)
	var saved repair.RecoveryPreview
	require.NoError(t, json.Unmarshal(data, &saved))
	require.Empty(t, saved.SourceColumns[0].DamengFloatStorage)
	prepared := false
	ops.prepare = func(_ context.Context, _ target.Writer, _ string, columns []source.ColumnInfo) error {
		prepared = true
		require.Equal(t, "renamed", columns[0].Name)
		require.Equal(t, "double", columns[0].DamengFloatStorage)
		return nil
	}
	_, err = runRecovery(context.Background(), dmReader, w, cfg, saved, false, nil, ops)
	require.NoError(t, err)
	require.True(t, prepared)
}

func TestRecoveryAcceptsNewMySQLDeclarationInLegacySnapshot(t *testing.T) {
	r, w, cfg, obj, ops, _ := recoveryFixture(t)
	r.ddl[obj.SourceTable].Columns = []source.ColumnInfo{{Name: "ID", DataType: "double", Precision: 22, FormattedType: "double"}}
	obj.SourceColumns = []source.ColumnInfo{{Name: "ID", DataType: "double", Precision: 22}}
	p, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.NoError(t, err)
	require.Equal(t, "double", p.SourceColumns[0].FormattedType)
	obj.SourceColumns = p.SourceColumns
	r.ddl[obj.SourceTable].Columns[0].FormattedType = "double(22,0)"
	_, err = previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.ErrorContains(t, err, "偏离迁移快照")
	obj.SourceColumns[0].FormattedType = ""
	r.ddl[obj.SourceTable].Columns[0].DataType = "float"
	_, err = previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.ErrorContains(t, err, "偏离迁移快照")
}
