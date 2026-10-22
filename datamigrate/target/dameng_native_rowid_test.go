package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// sqlmock does not implement OUT parameters. Populate the output at the
// matching INSERT while retaining the real SQL and transaction ordering.
type nativeRowIDOutArgument struct{ value string }

func (a nativeRowIDOutArgument) Match(value driver.Value) bool {
	out, ok := value.(sql.Out)
	if !ok || out.In {
		return false
	}
	dest, ok := out.Dest.(*string)
	if !ok {
		return false
	}
	*dest = a.value
	return true
}

func TestDamengNativeRowIDMappingPairsDuplicateRowsAndCommitsTogether(t *testing.T) {
	for _, mapFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "duplicate keyless rows", true: "mapping failure rolls back data"}[mapFails], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			w := &DaMengNativeWriter{&DaMengWriter{db: db, schema: "MixedOwner"}}
			table := damengnative.Table{Name: "Keyless", Schema: "MixedOwner", CaptureRowID: true, RowIDMapping: "RowMap", Columns: []damengnative.Column{{Name: "Value", DataType: "VARCHAR", Writable: true}}}
			mock.ExpectBegin()
			mapping := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO "MixedOwner"."RowMap" ("SourceRID", "TargetRID") VALUES (?, ?)`))
			insert := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO "MixedOwner"."Keyless" ("Value") VALUES (?) RETURNING ROWIDTOCHAR(ROWID) INTO ?`))
			insert.ExpectExec().WithArgs("same\x00", nativeRowIDOutArgument{"AAAAAAAAAAAAAAAAAQ"}).WillReturnResult(sqlmock.NewResult(0, 1))
			firstMap := mapping.ExpectExec().WithArgs("AAAAAAAAAAAAAAAAAB", "AAAAAAAAAAAAAAAAAQ")
			if mapFails {
				firstMap.WillReturnError(errors.New("mapping failed"))
				mock.ExpectRollback()
			} else {
				firstMap.WillReturnResult(sqlmock.NewResult(0, 1))
				insert.ExpectExec().WithArgs("same\x00", nativeRowIDOutArgument{"AAAAAAAAAAAAAAAAAR"}).WillReturnResult(sqlmock.NewResult(0, 1))
				mapping.ExpectExec().WithArgs("AAAAAAAAAAAAAAAAAC", "AAAAAAAAAAAAAAAAAR").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			}
			err = w.CopyDamengNativeBatch(context.Background(), table, [][]any{{"same\x00", "AAAAAAAAAAAAAAAAAB"}, {"same\x00", "AAAAAAAAAAAAAAAAAC"}})
			if mapFails {
				require.ErrorContains(t, err, "mapping failed")
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDamengNativeRowIDMappingRejectsInvalidTransportBeforeMutation(t *testing.T) {
	w, mock := mockNativeDM(t)
	table := damengnative.Table{Name: "Keyless", Schema: "MixedOwner", CaptureRowID: true, RowIDMapping: "RowMap", Columns: []damengnative.Column{{Name: "Value", DataType: "VARCHAR", Writable: true}}}
	for _, row := range [][]any{{"same"}, {"same", nil}, {"same", int64(1)}, {"same", "invalid'rowid"}} {
		require.Error(t, w.CopyDamengNativeBatch(context.Background(), table, [][]any{row}))
	}
	table.RowIDMapping = ""
	require.ErrorContains(t, w.CopyDamengNativeBatch(context.Background(), table, [][]any{{"same", "AAAAAAAAAAAAAAAAAB"}}), "映射计划")
	require.NoError(t, mock.ExpectationsWereMet())
}
