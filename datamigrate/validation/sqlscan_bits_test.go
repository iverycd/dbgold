package validation

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestPGBitProjectionReadsNativeBitsInOrder(t *testing.T) {
	for _, tc := range []struct {
		name, native, tableAlias, indexAlias string
		length                               int64
	}{
		{"reversible", "bit", "__dbgold_bits", "__dbgold_bit_index", 1},
		{"Bits9", "bit", "__dbgold_bits", "__dbgold_bit_index", 9},
		{"Bits64", "bit", "__dbgold_bits", "__dbgold_bit_index", 64},
		{`Quote"d`, "bit varying", "__dbgold_bits", "__dbgold_bit_index", 0},
		{"__dbgold_bits", "varbit", "__dbgold_bits_", "__dbgold_bit_index", 0},
		{"__dbgold_bit_index", "bit", "__dbgold_bits", "__dbgold_bit_index_", 9},
		{"__DBGOLD_BITS", "bit", "__dbgold_bits", "__dbgold_bit_index", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := QuoteValidationIdent("postgres", tc.name)
			projection, err := ValidationProjection("postgres", Column{Name: tc.name, DatabaseType: tc.native, Kind: Bits, Length: tc.length})
			require.NoError(t, err)
			index := QuoteValidationIdent("postgres", tc.tableAlias) + "." + QuoteValidationIdent("postgres", tc.indexAlias)
			require.Contains(t, projection, "CASE WHEN "+q+" IS NULL THEN NULL ELSE")
			require.Contains(t, projection, "get_bit("+q+","+index+")::text")
			require.Contains(t, projection, "ORDER BY "+index)
			require.Contains(t, projection, "generate_series(0,bit_length("+q+")-1)")
			require.Contains(t, projection, "AS "+QuoteValidationIdent("postgres", tc.tableAlias)+"("+QuoteValidationIdent("postgres", tc.indexAlias)+")")
			require.Contains(t, projection, "COALESCE(string_agg(")
			require.Contains(t, projection, "),'')")
			require.NotContains(t, projection, q+"::text")
			for _, cast := range []string{"::bytea", "::bigint", "::numeric", "bit_output"} {
				require.NotContains(t, projection, cast)
			}
		})
	}
}

func TestValidatePGBitsDetectsSingleBitChanges(t *testing.T) {
	for _, scenario := range []string{"same", "changed"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.MatchExpectationsInOrder(false)
			columns := []Column{{Name: "id", DatabaseType: "bigint", Kind: Number, Precision: 64}, {Name: "reversible", DatabaseType: "bit", Kind: Bits}}
			src := &memoryScanner{meta: Table{Columns: columns, PrimaryKey: []string{"id"}}, rows: [][]any{{"1", "1"}, {"2", nil}}}
			dst := NewSQLScanner(db, "postgres", "CaseSchema").WithCatalog("pg_catalog")
			flag, descriptions, scans := "1", 3, 1
			if scenario == "changed" {
				flag, descriptions, scans = "0", 4, 2
			}
			for i := 0; i < descriptions; i++ {
				mock.ExpectQuery("SELECT c.column_name, CASE").WithArgs("CaseSchema", "Rows").WillReturnRows(
					sqlmock.NewRows([]string{"name", "type", "precision", "scale", "fsp", "length", "ordinal"}).
						AddRow("id", "bigint", 64, 0, 0, 0, 1).AddRow("reversible", "bit", 0, 0, 0, 0, 2))
				mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.conkey::text FROM "pg_catalog".pg_constraint`)).WithArgs("CaseSchema", "Rows").
					WillReturnRows(sqlmock.NewRows([]string{"conkey"}).AddRow("{1}"))
			}
			for i := 0; i < scans; i++ {
				mock.ExpectQuery(regexp.QuoteMeta(`SELECT current_setting('TimeZone'), current_setting('DateStyle')`)).
					WillReturnRows(sqlmock.NewRows([]string{"zone", "date"}).AddRow("Asia/Shanghai", "ISO, DMY"))
				mock.ExpectExec(regexp.QuoteMeta(`SELECT set_config('TimeZone','UTC',false),set_config('DateStyle','ISO, YMD',false)`)).
					WillReturnResult(sqlmock.NewResult(0, 1))
				// The old reversible::text projection cannot satisfy this query.
				mock.ExpectQuery(`SELECT .*get_bit\(.*FROM "CaseSchema"\."Rows"`).
					WillReturnRows(sqlmock.NewRows([]string{"id", "reversible"}).AddRow("1", flag).AddRow("2", nil)).RowsWillBeClosed()
				mock.ExpectExec(regexp.QuoteMeta(`SELECT set_config('TimeZone',$1,false),set_config('DateStyle',$2,false)`)).
					WithArgs("Asia/Shanghai", "ISO, DMY").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			result := validateWithOptions(context.Background(), src, dst, Plan{SourceTable: "Rows", TargetTable: "Rows"}, testOptions(t))
			require.Empty(t, result.Error)
			require.EqualValues(t, 2, result.SourceRows)
			require.EqualValues(t, 2, result.TargetRows)
			if scenario == "same" {
				require.Equal(t, StatusPassed, result.Status)
				require.Zero(t, result.ChangedRows)
			} else {
				require.Equal(t, StatusMismatch, result.Status)
				require.EqualValues(t, 1, result.ChangedRows)
				require.Len(t, result.Samples, 1)
				require.Equal(t, []string{"reversible"}, result.Samples[0].Columns)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestCanonicalBitsRemainStrict(t *testing.T) {
	c := Column{Name: "reversible", Kind: Bits, DatabaseType: "bit"}
	for _, value := range []any{"", "true", "false", `\x01`, `B'1'`, "102", []byte{0}, []byte{1}, true, false} {
		_, err := canonical(value, c, c)
		require.ErrorContains(t, err, "BIT 未以完整 0/1 位串返回", "%#v", value)
	}
	for _, bits := range []string{"0", "1", "000000101", "100000010", "1" + strings.Repeat("0", 62) + "1", strings.Repeat("1", 64)} {
		_, err := canonical(bits, c, c)
		require.NoError(t, err, bits)
	}
	null, err := canonical(nil, c, c)
	require.NoError(t, err)
	require.Equal(t, []byte{0}, null)
}
