package target

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func expectKingbaseUUIDCandidatesMissing(m sqlmock.Sqlmock) {
	for _, name := range uuidCandidates("kingbase") {
		expectUUIDCandidate(m, name, "", "", nil)
	}
}

func TestKingbaseExistingUUIDGeneratorsDoNotInstallExtension(t *testing.T) {
	for _, generator := range uuidCandidates("kingbase") {
		t.Run(generator, func(t *testing.T) {
			w, m := repairPGMock(t)
			w.dbType = "kingbase"
			expectUUIDMissing(m, "app")
			for _, name := range uuidCandidates("kingbase") {
				if name == generator {
					expectUUIDCandidate(m, name, "native", "uuid", nil)
					break
				}
				expectUUIDCandidate(m, name, "", "", nil)
			}
			expectUUIDCreates(m, qualifiedFunction("native", generator), "uuid", nil)

			report, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
			require.NoError(t, err)
			require.Equal(t, 1, report.Created)
			require.Equal(t, qualifiedFunction("native", generator), report.Items[0].Generator)
			require.Empty(t, report.Extensions)
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestKingbaseExistingGetUUIDDoesNotInstallExtension(t *testing.T) {
	w, m := repairPGMock(t)
	w.dbType = "kingbase"
	expectUUIDReused(m)

	report, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Equal(t, 1, report.Reused)
	require.Empty(t, report.Extensions)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestKingbaseUUIDExtensionFallback(t *testing.T) {
	for _, state := range []string{"install", "reuse", "rollback", "denied", "missing files", "generator missing", "commit unknown"} {
		t.Run(state, func(t *testing.T) {
			w, m := repairPGMock(t)
			w.dbType = "kingbase"
			expectUUIDMissing(m, "app")
			expectKingbaseUUIDCandidatesMissing(m)
			var installErr, createErr error
			switch state {
			case "denied":
				installErr = &pq.Error{Code: "42501", Message: "permission denied to create extension"}
			case "missing files":
				installErr = errors.New("uuid-ossp.control: No such file")
			case "rollback":
				createErr = errors.New("failed function creation")
			}
			expectUUIDExtension(m, state == "reuse", installErr)
			switch {
			case installErr != nil:
				m.ExpectRollback()
			case state == "generator missing":
				expectUUIDCandidate(m, "uuid_generate_v4", "", "", nil)
				m.ExpectRollback()
			case state == "commit unknown":
				expectUUIDCandidate(m, "uuid_generate_v4", "extensions", "uuid", nil)
				m.ExpectExec(regexp.QuoteMeta(uuidFunctionDDL("app", "get_uuid", qualifiedFunction("extensions", "uuid_generate_v4"), "uuid"))).
					WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectCommit().WillReturnError(errors.New("commit connection lost"))
			default:
				expectUUIDCandidate(m, "uuid_generate_v4", "extensions", "uuid", nil)
				expectUUIDCreates(m, qualifiedFunction("extensions", "uuid_generate_v4"), "uuid", createErr)
			}

			report, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
			if state == "install" || state == "reuse" {
				require.NoError(t, err)
				require.Equal(t, 1, report.Created)
				require.Equal(t, `"extensions"."uuid_generate_v4"`, report.Items[0].Generator)
			} else {
				require.Error(t, err)
				require.Equal(t, 1, report.Failed)
				require.Zero(t, report.Created)
			}
			if installErr != nil {
				require.ErrorIs(t, err, installErr)
				require.Contains(t, err.Error(), `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`)
				require.NotContains(t, err.Error(), "HighGo")
			}
			if state == "generator missing" {
				for _, name := range uuidCandidates("kingbase") {
					require.Contains(t, err.Error(), name)
				}
				require.Contains(t, err.Error(), "启用 uuid-ossp 后 uuid_generate_v4")
			}
			require.Len(t, report.Extensions, 1)
			want := map[string]string{
				"install": "installed", "reuse": "reused", "rollback": "rolled_back",
				"denied": "failed", "missing files": "failed", "generator missing": "rolled_back", "commit unknown": "unknown",
			}[state]
			require.Equal(t, want, report.Extensions[0].Status)
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestKingbaseConcurrentUUIDExtensionCreationRetriesDiscovery(t *testing.T) {
	w, m := repairPGMock(t)
	w.dbType = "kingbase"
	expectUUIDMissing(m, "app")
	expectKingbaseUUIDCandidatesMissing(m)
	expectUUIDExtension(m, false, &pq.Error{Code: "23505", Message: "extension created concurrently"})
	m.ExpectRollback()
	// The concurrent installer is now visible. Rediscovery must reuse its
	// generator without attempting extension installation a second time.
	expectUUIDMissing(m, "app")
	for _, name := range uuidCandidates("kingbase") {
		if name == "uuid_generate_v4" {
			expectUUIDCandidate(m, name, "extensions", "uuid", nil)
			break
		}
		expectUUIDCandidate(m, name, "", "", nil)
	}
	expectUUIDCreates(m, qualifiedFunction("extensions", "uuid_generate_v4"), "uuid", nil)

	report, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Equal(t, 1, report.Created)
	require.Empty(t, report.Extensions)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestKingbaseConcurrentGetUUIDCreationRollsBackExtensionAndReuses(t *testing.T) {
	w, m := repairPGMock(t)
	w.dbType = "kingbase"
	expectUUIDMissing(m, "app")
	expectKingbaseUUIDCandidatesMissing(m)
	expectUUIDExtension(m, false, nil)
	expectUUIDCandidate(m, "uuid_generate_v4", "extensions", "uuid", nil)
	expectUUIDCreates(m, qualifiedFunction("extensions", "uuid_generate_v4"), "uuid", &pq.Error{Code: "42723", Message: "function created concurrently"})
	expectUUIDReused(m)

	report, err := ensurePGUUIDCompatibility(context.Background(), w.db, w.schema, w.DBType())
	require.NoError(t, err)
	require.Equal(t, 1, report.Reused)
	require.Zero(t, report.Created)
	require.Empty(t, report.Extensions)
	require.NoError(t, m.ExpectationsWereMet())
}
