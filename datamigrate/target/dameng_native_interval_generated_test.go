package target

import (
	"context"
	"regexp"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func nativeGeneratedIntervalTestPlan() *damengnative.IntervalPartitionPlan {
	ddl := `CREATE TABLE "Dst"."Empty"("Input" INT NOT NULL,"Key" INT GENERATED ALWAYS AS ("Input"*"Input") VIRTUAL) PARTITION BY RANGE("Key") INTERVAL(10)(PARTITION "Initial" VALUES LESS THAN(0))`
	return &damengnative.IntervalPartitionPlan{Column: "Key", GeneratedKey: true, DefinitionSQL: ddl, EmptyTouch: &damengnative.MaterializedEmptyTouch{Table: "Empty", DefinitionSQL: ddl, Columns: []damengnative.MaterializedEmptyColumn{{Name: "Input", NotNull: true}}}, Partitions: []damengnative.IntervalPartition{{Name: "Initial", HighSQL: "0"}, {Name: "Old", HighSQL: "30", SeedSQL: "CAST(29 AS INT)"}, {Name: "New", HighSQL: "50", SeedSQL: "CAST(49 AS INT)"}}}
}

func TestIntervalGeneratedUnknownAcceptsOnlyFrozenNativeExpressions(t *testing.T) {
	for _, name := range []string{"original", "frozen_constant", "changed_expression", "changed_other_column"} {
		t.Run(name, func(t *testing.T) {
			conn, m := historyEmptyTestConn(t)
			p := nativeGeneratedIntervalTestPlan()
			actual := p.DefinitionSQL
			var err error
			switch name {
			case "frozen_constant":
				actual, err = damengnative.RewriteNativeGeneratedColumnConstant(actual, "Key", p.Partitions[1].SeedSQL)
				require.NoError(t, err)
				actual = regexp.MustCompile(`"Input" INT NOT NULL`).ReplaceAllString(actual, `"Input" INT`)
			case "changed_expression":
				actual, err = damengnative.RewriteNativeGeneratedColumnConstant(actual, "Key", "CAST(999 AS INT)")
				require.NoError(t, err)
			case "changed_other_column":
				actual = regexp.MustCompile(`"Input" INT NOT NULL`).ReplaceAllString(actual, `"Input" BIGINT NOT NULL`)
			}
			expectEmptyDDL(m, actual)
			valid := name == "original" || name == "frozen_constant"
			if valid {
				m.ExpectQuery(`SELECT c.OWNER,c.CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "table", "kind", "status", "validated", "column"}))
			}
			_, err = nativeIntervalGeneratedCurrent(context.Background(), conn, "Dst", p)
			if valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "已偏离冻结计划")
			}
			require.NoError(t, m.ExpectationsWereMet(), "unknown-state proof is read-only")
		})
	}
}

func TestIntervalGeneratedConstantNeverChangesNonemptyTable(t *testing.T) {
	conn, m := historyEmptyTestConn(t)
	p := nativeGeneratedIntervalTestPlan()
	m.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	require.ErrorContains(t, nativeIntervalGeneratedTouch(context.Background(), conn, "Dst", p, p.Partitions[1]), "已有数据")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestIntervalGeneratedTouchRestoresOriginalDeclarationAfterModifyUnknown(t *testing.T) {
	conn, m := historyEmptyTestConn(t)
	p := nativeGeneratedIntervalTestPlan()
	m.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	expectEmptyDDL(m, p.DefinitionSQL)
	m.ExpectQuery(`SELECT c.OWNER,c.CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "table", "kind", "status", "validated", "column"}))
	m.ExpectExec(regexp.QuoteMeta(`ALTER TABLE "Dst"."Empty" MODIFY "Key" INT GENERATED ALWAYS AS (CAST(29 AS INT)) VIRTUAL`)).WillReturnError(context.DeadlineExceeded)
	m.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	m.ExpectExec(regexp.QuoteMeta(`ALTER TABLE "Dst"."Empty" MODIFY "Key" INT GENERATED ALWAYS AS ("Input"*"Input") VIRTUAL`)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectEmptyDDL(m, p.DefinitionSQL)
	require.ErrorIs(t, nativeIntervalGeneratedTouch(context.Background(), conn, "Dst", p, p.Partitions[1]), context.DeadlineExceeded)
	require.NoError(t, m.ExpectationsWereMet())
}
