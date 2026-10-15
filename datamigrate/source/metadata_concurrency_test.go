package source

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestMetadataReadConcurrencyRespectsPoolLimit(t *testing.T) {
	for _, limit := range []int{0, 1, 2, 4, 50} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			db, _, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			db.SetMaxOpenConns(limit)
			want := 4
			if limit > 0 {
				want = min(want, limit)
			}
			mysql := &MySQLReader{db: db}
			sqlserver := &SQLServerReader{db: db}
			for _, reader := range []interface{ MetadataReadConcurrency() int }{
				mysql, &MySQLSQLServerReader{mysql}, sqlserver, &SQLServerDamengReader{sqlserver},
				&OracleReader{db: db}, &DaMengReader{db: db}, &PostgresReader{db: db},
			} {
				require.Equal(t, want, reader.MetadataReadConcurrency(), "%T", reader)
			}
		})
	}
}

func TestMySQLSnapshotMetadataRemainsSerial(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &MySQLReader{db: db, snapshotTx: &sql.Tx{}}
	require.Equal(t, 1, r.MetadataReadConcurrency())
	require.Equal(t, 1, (&MySQLSQLServerReader{r}).MetadataReadConcurrency())
	r.snapshotTx = nil
	require.Equal(t, 4, r.MetadataReadConcurrency())
}
