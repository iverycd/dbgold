package source

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"

	"dbgold/datamigrate/failure"
	"github.com/DATA-DOG/go-sqlmock"
	oranetwork "github.com/sijms/go-ora/v2/network"
	"github.com/stretchr/testify/require"
)

func expectOracleDMFailureSequenceBindings(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("SELECT TABLE_NAME,COLUMN_NAME,DATA_DEFAULT").WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"table", "column", "default"}))
	mock.ExpectQuery("SELECT TABLE_NAME,TRIGGER_NAME,TRIGGER_TYPE").WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"table", "trigger", "kind", "event", "body", "when"}).
		AddRow("Bad", "TR_BAD", "BEFORE EACH ROW", "INSERT", "BEGIN :NEW.ID := S_BAD.NEXTVAL; END;", nil).
		AddRow("Good", "TR_GOOD", "BEFORE EACH ROW", "INSERT", "BEGIN :NEW.ID := S_GOOD.NEXTVAL; END;", nil))
}

func TestOracleDMSequenceConnectionFailureRetainsCauseAndStopsRead(t *testing.T) {
	for _, cause := range []error{&net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}, context.Canceled, &oranetwork.OracleError{ErrCode: 3135}} {
		t.Run(fmt.Sprintf("%T_%s", cause, cause.Error()), func(t *testing.T) {
			r, mock := oracleDMTestReader(t, 11)
			expectOracleDMFailureSequenceBindings(mock)
			mock.ExpectQuery(`SELECT TO_CHAR\(LAST_NUMBER`).WithArgs("APP", "S_BAD").WillReturnError(cause)
			values, err := r.GetSequences(context.Background())
			require.ErrorIs(t, err, cause)
			require.True(t, failure.IsTaskError(err))
			require.Nil(t, values)
		})
	}
}

func TestOracleDMSequenceObjectFailureStillReadsHealthySequence(t *testing.T) {
	r, mock := oracleDMTestReader(t, 11)
	expectOracleDMFailureSequenceBindings(mock)
	mock.ExpectQuery(`SELECT TO_CHAR\(LAST_NUMBER`).WithArgs("APP", "S_BAD").WillReturnError(&oranetwork.OracleError{ErrCode: 1031, ErrMsg: "insufficient privileges"})
	mock.ExpectQuery(`SELECT TO_CHAR\(LAST_NUMBER`).WithArgs("APP", "S_GOOD").WillReturnRows(sqlmock.NewRows([]string{"next", "step", "min", "max", "cache", "cycle", "ordered"}).AddRow("100", "1", "1", "10000", "0", "N", "N"))
	values, err := r.GetSequences(context.Background())
	require.NoError(t, err)
	require.Len(t, values, 2)
	require.Contains(t, values[0].Unsupported, "insufficient privileges")
	require.Empty(t, values[1].Unsupported)
	require.Equal(t, "S_GOOD", values[1].Native.Name)
}
