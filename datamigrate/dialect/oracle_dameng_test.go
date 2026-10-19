package dialect

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestOracleDamengDDLRejectsBeforeReturningDrop(t *testing.T) {
	d := NewDaMeng()
	for _, c := range []source.ColumnInfo{{Name: "N", DataType: "NUMBER", Precision: 5, Scale: -2}, {Name: "N", DataType: "NUMBER", Default: func() *string { s := "SEQ.NEXTVAL"; return &s }()}} {
		ss, e := d.CreateTableStatements("APP", &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{c}}, "oracle", TypeOpt{}, func(s string) string { return s })
		require.Error(t, e)
		require.Nil(t, ss)
	}
}

func TestOracleDamengNativeSequenceAndTriggerDDL(t *testing.T) {
	d := NewDaMeng()
	s := source.SequenceInfo{StartValueText: "100", Native: &source.NativeSequenceInfo{Name: "SEQ_MiXeD", IncrementBy: "5", MinValue: "1", MaxValue: "9223372036854775807", CacheSize: "0", Bindings: []source.SequenceBinding{{TableName: "T", ColumnName: "ID", Kind: "identity", OnlyWhenNull: true}, {TableName: "T2", ColumnName: "ID", Kind: "trigger", TriggerName: "TRG_T2", OnlyWhenNull: false}}}}
	require.NoError(t, ValidateOracleDamengSequence(s))
	ss := d.SequenceStatements("APP", s, "")
	require.Len(t, ss, 3)
	require.Contains(t, ss[0].SQL, `CREATE SEQUENCE "APP"."SEQ_MIXED" START WITH 100 INCREMENT BY 5`)
	require.Contains(t, ss[1].SQL, "IS NULL THEN")
	require.NotContains(t, ss[2].SQL, "IS NULL THEN")
	require.Equal(t, "SEQ_MIXED", SequenceName(d, s))
	require.False(t, strings.Contains(JoinSQL(ss), "IDENTITY"))
}

func TestDamengViewExplicitMappedColumns(t *testing.T) {
	ss := NewDaMeng().ViewStatements("APP", source.ViewInfo{ViewName: "V", Definition: "SELECT ID FROM T", Columns: []source.ColumnInfo{{Name: "MappedId"}}})
	require.Equal(t, `CREATE OR REPLACE VIEW "APP"."V" ("MAPPEDID") AS SELECT ID FROM T`, ss[0].SQL)
}
