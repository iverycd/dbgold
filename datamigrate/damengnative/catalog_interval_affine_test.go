package damengnative

import (
	"math/big"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestIntervalAffineParserAcceptsOnlyExactPureNumericArithmetic(t *testing.T) {
	columns := map[string]Column{"A": {Name: "A", Writable: true, DataType: "INT", SQLType: "INT"}, "B": {Name: "B", Writable: true, DataType: "DECIMAL", SQLType: "DECIMAL(10,2)", Scale: 2}, "G": {Name: "G", Writable: false, DataType: "INT", SQLType: "INT"}}
	a, _, err := parseIntervalAffine(`-("A"*2)+("B"+0.5)/2`, columns)
	require.NoError(t, err)
	require.Equal(t, "1/4", a.constant.RatString())
	require.Equal(t, "-2", a.coeff["A"].RatString())
	require.Equal(t, "1/2", a.coeff["B"].RatString())
	for _, expression := range []string{`"A"*"B"`, `ABS("A")`, `"Owner"."F"("A")`, `"A"/"B"`, `"A"/0`, `CURRENT_TIMESTAMP`, `SELECT 1`, `'1'+"A"`, `"G"*2`, `"A";DELETE FROM T`, `"A"||1`} {
		_, _, err = parseIntervalAffine(expression, columns)
		require.Error(t, err, expression)
	}
}

func TestIntervalGeneratedExpressionUsesOnlyExactColumnDeclaration(t *testing.T) {
	ddl := `CREATE TABLE "App"."T"("Fake" VARCHAR(40) DEFAULT 'AS(danger())',"A" INT,"G" INT GENERATED ALWAYS AS(("A"+1)*2) VIRTUAL,CONSTRAINT "AS" CHECK("A">0))`
	expression, err := intervalGeneratedExpression(ddl, "G")
	require.NoError(t, err)
	require.Equal(t, `("A"+1)*2`, expression)
	_, err = intervalGeneratedExpression(ddl, "Fake")
	require.Error(t, err)
}

func TestGeneratedIntervalAffineSeedsHaveNativeReadOnlyRouteProof(t *testing.T) {
	for _, tc := range []struct {
		name, expression, high, step string
		columns                      []Column
		want                         []IntervalSeedValue
	}{
		{"double", `"A"*2`, "85", "10", []Column{{Name: "A", DataType: "INT", SQLType: "INT", Writable: true}}, []IntervalSeedValue{{Column: "A", SQL: `CAST('38' AS INT)`}}},
		{"multiple", `"A"+"B"*2`, "85", "10", []Column{{Name: "A", DataType: "INT", SQLType: "INT", Writable: true}, {Name: "B", DataType: "INT", SQLType: "INT", Writable: true}}, []IntervalSeedValue{{Column: "A", SQL: `CAST('75' AS INT)`}, {Column: "B", SQL: `CAST('0' AS INT)`}}},
		{"bezout", `"A"*2+"B"*3`, "6", "1", []Column{{Name: "A", DataType: "INT", SQLType: "INT", Writable: true}, {Name: "B", DataType: "INT", SQLType: "INT", Writable: true}}, []IntervalSeedValue{{Column: "A", SQL: `CAST('-5' AS INT)`}, {Column: "B", SQL: `CAST('5' AS INT)`}}},
		{"decimal", `"A"*2+0.5`, "85", "1", []Column{{Name: "A", DataType: "DECIMAL", SQLType: "DECIMAL(10,2)", Scale: 2, Writable: true}}, []IntervalSeedValue{{Column: "A", SQL: `CAST('41.75' AS DECIMAL(10,2))`}}},
		{"canceled_variable", `"A"-"A"+10`, "15", "10", []Column{{Name: "A", DataType: "INT", SQLType: "INT", Writable: true}}, []IntervalSeedValue{{Column: "A", SQL: `CAST('0' AS INT)`}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			r := nativeMVReader(db)
			m.ExpectQuery(`SELECT CASE WHEN CAST\(.* THEN \(SELECT COUNT\(\*\) FROM "App"\."T" PARTITION FOR\(CAST\(.* ELSE -1 END FROM DUAL`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			ddl := `CREATE TABLE "App"."T"("G" INT GENERATED ALWAYS AS(` + tc.expression + `) VIRTUAL)`
			seed, values, err := r.freezeGeneratedIntervalSeed(Table{Name: "T", Columns: tc.columns}, Column{Name: "G", DataType: "INT", SQLType: "INT"}, intervalDefinition{interval: tc.step, last: "-5"}, ddl, tc.high, nil)
			require.NoError(t, err)
			require.NotEmpty(t, seed)
			require.Equal(t, tc.want, values)
			require.NoError(t, m.ExpectationsWereMet(), "只允许纯SELECT证明原有分区")
		})
	}
}

func TestIntervalRationalBoundsRoundOutwardExactly(t *testing.T) {
	for _, tc := range []struct{ value, ceil, floor string }{{"1/2", "1", "0"}, {"-1/2", "0", "-1"}, {"-2", "-2", "-2"}} {
		r, ok := new(big.Rat).SetString(tc.value)
		require.True(t, ok)
		require.Equal(t, tc.ceil, intervalCeil(r).String())
		require.Equal(t, tc.floor, intervalFloor(r).String())
	}
}

func TestConstantGeneratedIntervalUsesFrozenNullableRecipeForNonNumericInputs(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	m.ExpectQuery(`SELECT CASE WHEN CAST\(.*PARTITION FOR\(CAST\(.*`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	_, values, err := r.freezeGeneratedIntervalSeed(Table{Name: "T", Columns: []Column{{Name: "Text", DataType: "VARCHAR", SQLType: "VARCHAR(40)", Writable: true}}}, Column{Name: "G", DataType: "INT", SQLType: "INT"}, intervalDefinition{interval: "10", last: "-5"}, `CREATE TABLE "App"."T"("Text" VARCHAR(40) NOT NULL,"G" INT AS(10) VIRTUAL)`, "15", &MaterializedEmptyTouch{Columns: []MaterializedEmptyColumn{{Name: "Text", NotNull: true}}})
	require.NoError(t, err)
	require.Equal(t, []IntervalSeedValue{{Column: "Text", SQL: "NULL"}}, values)
	require.NoError(t, m.ExpectationsWereMet())
	for _, c := range []Column{{Name: "Domain", DataType: "NUMBER", SQLType: `"App"."NumberDomain"`, TypeSchema: "App"}, {Name: "Domain", DataType: "NUMBER", SQLType: `"App"."NumberDomain"`, TypeName: "NumberDomain"}} {
		_, err = intervalColumnQuantum(c)
		require.ErrorContains(t, err, "用户定义类型")
	}
}
