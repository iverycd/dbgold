package target

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestPartitionMySQLKeyRequiresOfficialServerIdentity(t *testing.T) {
	for _, tc := range []struct {
		comment   string
		supported bool
	}{{"MySQL Community Server - GPL", true}, {"MySQL Enterprise Server - Commercial", true}, {"Source distribution", true}, {"Percona Server (GPL)", false}, {"TiDB Server (Apache License 2.0) Community Edition", false}} {
		t.Run(tc.comment, func(t *testing.T) {
			db, mock := partitionInspectorDB(t)
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT VERSION(),@@SESSION.sql_mode,@@version_comment`)).WillReturnRows(sqlmock.NewRows([]string{"version", "mode", "comment"}).AddRow("8.0.36", "", tc.comment))
			p, err := (dbPartitionInspector{db: db, product: "mysql", schema: "App"}).profile(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.supported, p.Features["mysql_key_innodb"])
		})
	}
}
