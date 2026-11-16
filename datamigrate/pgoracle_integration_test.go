package datamigrate

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// Every matrix cell is opt-in. This fixture creates a fresh random PG schema
// and only random objects in an explicitly dedicated Oracle schema. It never
// removes an existing source schema or a target object found before the run.
func TestPGOracleIntegration(t *testing.T) {
	for _, product := range []struct{ name, driver string }{
		{"postgres", "postgres"}, {"gaussdb", "opengauss"}, {"highgo", "highgo"},
		{"vastbase", "postgres"}, {"gbase", "postgres"}, {"seabox", "postgres"}, {"kingbase", "postgres"},
	} {
		for _, oracle := range []struct{ name, version string }{{"11G", "11."}, {"19C", "19."}} {
			t.Run(product.name+"/"+oracle.name, func(t *testing.T) {
				if os.Getenv("DBGOLD_PG_ORACLE_ALLOW_DDL") != "1" {
					t.Skip("未实库认证：须显式设置 DBGOLD_PG_ORACLE_ALLOW_DDL=1")
				}
				sourceKey := "DBGOLD_PG_ORACLE_" + strings.ToUpper(product.name) + "_DSN"
				targetKey := "DBGOLD_PG_ORACLE_" + oracle.name + "_DSN"
				schemaKey := "DBGOLD_PG_ORACLE_" + oracle.name + "_SCHEMA"
				sourceDSN, targetDSN, targetSchema := os.Getenv(sourceKey), os.Getenv(targetKey), os.Getenv(schemaKey)
				if sourceDSN == "" || targetDSN == "" || targetSchema == "" {
					t.Skipf("未实库认证：缺少 %s、%s 或 %s；不探测其他连接", sourceKey, targetKey, schemaKey)
				}
				require.True(t, strings.HasPrefix(targetSchema, "DBGOLD_"), "target schema must be explicitly dedicated and start with DBGOLD_")
				require.NoError(t, dialect.ValidateOracleName(targetSchema))
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				pg, err := sql.Open(product.driver, sourceDSN)
				require.NoError(t, err)
				defer pg.Close()
				require.NoError(t, pg.PingContext(ctx))
				ora, err := sql.Open("oracle", targetDSN)
				require.NoError(t, err)
				defer ora.Close()
				require.NoError(t, ora.PingContext(ctx))
				writer, err := target.NewOracle(targetDSN, targetSchema, target.ConnPoolConfig{})
				require.NoError(t, err)
				defer writer.Close()
				profile, err := writer.InspectPGOracle(ctx)
				require.NoError(t, err)
				require.True(t, strings.HasPrefix(profile.Version, oracle.version), "matrix target version mismatch: %s", profile.Version)
				var entropy [8]byte
				_, err = rand.Read(entropy[:])
				require.NoError(t, err)
				suffix := hex.EncodeToString(entropy[:])
				sourceSchema, table, view := "DBGOLD_"+suffix, "Pgo_"+suffix, "Pgv_"+suffix
				qSource := source.QuotePGIdent(sourceSchema)
				qTable := qSource + "." + source.QuotePGIdent(table)
				qView := qSource + "." + source.QuotePGIdent(view)
				// CREATE without IF NOT EXISTS fails rather than adopting a collision.
				_, err = pg.ExecContext(ctx, "CREATE SCHEMA "+qSource)
				require.NoError(t, err)
				defer func() {
					cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
					defer done()
					for _, statement := range []string{"DROP VIEW IF EXISTS " + qView, "DROP TABLE IF EXISTS " + qTable, "DROP SCHEMA " + qSource} {
						if _, err := pg.ExecContext(cleanup, statement); err != nil {
							t.Errorf("source fixture cleanup failed: %v", err)
						}
					}
				}()
				exec := func(db *sql.DB, statement string) {
					t.Helper()
					_, err := db.ExecContext(ctx, statement)
					require.NoError(t, err, "fixture SQL: %s", statement)
				}
				exec(pg, `CREATE TABLE `+qTable+` ("Id" serial PRIMARY KEY, "Label" varchar(40) NOT NULL, "Enabled" boolean NOT NULL DEFAULT true, "Amount" numeric(10,2) DEFAULT 0, CONSTRAINT `+source.QuotePGIdent("CK_"+suffix)+` CHECK ("Amount" >= 0))`)
				exec(pg, `INSERT INTO `+qTable+` ("Label", "Enabled", "Amount") VALUES ('first', true, 12.34), ('second', false, 0)`)
				exec(pg, `CREATE VIEW `+qView+` AS SELECT "Id", "Label" FROM `+qTable+` WHERE "Enabled"`)
				reader, err := source.NewPostgresProductForOracle(product.name, product.driver, sourceDSN, sourceSchema, source.ConnPoolConfig{})
				require.NoError(t, err)
				defer reader.Close()
				snapshot, err := reader.ReadPGOracleSnapshot(ctx, []string{table})
				require.NoError(t, err)
				require.Len(t, snapshot.Tables, 1)
				require.Equal(t, table, snapshot.Tables[0].Info.TableName)
				require.NotEmpty(t, snapshot.Sequences)
				t.Logf("source product=%s version=%s mode=%s catalog=%s; Oracle version=%s schema=%s charset=%s ncharset=%s", snapshot.Profile.Product, snapshot.Profile.Version, snapshot.Profile.Mode, snapshot.Profile.Catalog, profile.Version, profile.Schema, profile.Charset, profile.NCharset)
				job := &Job{LogCh: make(chan string, 4096)}
				m := NewMigrator(reader, writer, job, Config{Mode: "all", Content: "both", TargetDBType: "oracle", TargetSchema: targetSchema, PageSize: 1, MaxParallel: 1, CaptureRepair: true})
				preflight := m.PreflightPGOracle(ctx)
				require.True(t, preflight.Ready, "preflight: %+v", preflight.Issues)
				require.True(t, preflight.CanStart)
				// Record absence before authorizing cleanup. The source schema is
				// new, so every planned name belongs to this fixture only.
				for _, object := range preflight.Objects {
					if object.Object.Kind == "comment" {
						continue
					}
					var count int
					require.NoError(t, ora.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=:1 AND OBJECT_NAME=:2`, targetSchema, object.Target).Scan(&count))
					require.Zero(t, count, "refusing existing target object %s", object.Target)
				}
				defer cleanupPGOracleFixture(t, ora, targetSchema, preflight.Objects)
				report := m.Run(ctx)
				require.False(t, report.HasFailures(), "report: %+v", report)
				require.Equal(t, 1, report.Tables.Success)
				require.Equal(t, 1, report.Data.Success)
				require.Equal(t, 1, report.Views.Success)
				require.Equal(t, 1, report.Sequences.Success)
				require.Equal(t, 1, report.Triggers.Success)
				require.NotNil(t, report.RepairManifest)
				identityFound := false
				for _, object := range report.RepairManifest.Objects {
					if object.Kind == "sequence" && object.PGOracleSequence != nil && object.PGOracleSequence.Identity != nil {
						identityFound = object.PGOracleSequence.Identity.ObjectID > 0
					}
				}
				require.True(t, identityFound, "sequence catalog identity must be preserved")
				qTarget := writer.Dialect().QualifyTable(targetSchema, strings.ToUpper(table))
				qTargetView := writer.Dialect().QualifyTable(targetSchema, strings.ToUpper(view))
				var count, nextID int64
				require.NoError(t, ora.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qTarget).Scan(&count))
				require.EqualValues(t, 2, count)
				require.NoError(t, ora.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qTargetView).Scan(&count))
				require.EqualValues(t, 1, count)
				exec(ora, `INSERT INTO `+qTarget+` ("LABEL") VALUES ('after')`)
				require.NoError(t, ora.QueryRowContext(ctx, `SELECT "ID" FROM `+qTarget+` WHERE "LABEL"='after'`).Scan(&nextID))
				require.Greater(t, nextID, int64(2), "serial trigger must continue beyond migrated IDs")
				require.NoError(t, ora.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qTargetView).Scan(&count))
				require.EqualValues(t, 2, count)
				t.Logf("fixture passed: %s → Oracle %s, serial next ID=%d; this result applies only to the logged versions and mode", product.name, profile.Version, nextID)
			})
		}
	}
}

func cleanupPGOracleFixture(t *testing.T, db *sql.DB, schema string, objects []PlannedObject) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, kind := range []string{"view", "table", "sequence"} {
		for _, object := range objects {
			if object.Object.Kind != kind {
				continue
			}
			var count int
			err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=:1 AND OBJECT_NAME=:2 AND OBJECT_TYPE=:3`, schema, object.Target, strings.ToUpper(kind)).Scan(&count)
			if err != nil {
				t.Errorf("fixture cleanup inspection: %v", err)
				continue
			}
			if count == 0 {
				continue
			}
			statement := fmt.Sprintf("DROP %s %s", strings.ToUpper(kind), dialect.NewOracle().QualifyTable(schema, object.Target))
			if kind == "table" {
				statement += " PURGE"
			}
			if _, err := db.ExecContext(ctx, statement); err != nil {
				t.Errorf("target fixture cleanup failed: %v", err)
			}
		}
	}
}
