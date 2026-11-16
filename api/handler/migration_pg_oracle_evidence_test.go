package handler

import (
	"context"
	"testing"

	"dbgold/datamigrate"
	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func pgOracleEvidenceFixture(t *testing.T) (*store.Connection, *store.Connection, *store.DataMigrationJob, *store.MigrationRepairBinding, repair.Object) {
	t.Helper()
	migrationHandlerTestDB(t)
	require.NoError(t, store.DB.AutoMigrate(&store.MigrationRepairBinding{}, &store.MigrationRepairObject{}))
	src := &store.Connection{ID: 11, OwnerID: 7, DBType: "postgres", Host: "source", Port: 5432, Database: "appdb", Username: "reader"}
	dst := &store.Connection{ID: 12, OwnerID: 7, DBType: "oracle", Host: "target", Port: 1521, Database: "ORCL", Username: "writer"}
	job := &store.DataMigrationJob{OwnerID: 7, JobID: "structure-job", Status: "done", SrcConnID: src.ID, DstConnID: dst.ID, SrcDBType: src.DBType, DstDBType: dst.DBType,
		SourceDatabase: src.Database, SourceConnectionDatabase: src.Database, SourceSchema: "MixedSchema", DstSchema: "APP",
		SrcConnHost: src.Host, SrcConnPort: src.Port, SrcConnUsername: src.Username, DstConnHost: dst.Host, DstConnPort: dst.Port, DstConnDatabase: dst.Database, DstConnUsername: dst.Username}
	binding := &store.MigrationRepairBinding{JobID: job.JobID, SrcConnID: src.ID, DstConnID: dst.ID, SourceDatabase: src.Database, SourceSchema: job.SourceSchema, TargetSchema: "APP", SourceFingerprint: connectionFingerprint(src), TargetFingerprint: connectionFingerprint(dst), Configured: true, ConfigJSON: `{}`, Version: 1}
	plan := pgoracle.SequencePlan{Schema: "APP", Name: "ORDER_SEQUENCE", Source: source.PGSequence{Schema: "MixedSchema", Name: "Order_Sequence", Increment: 1, Min: 1, Max: 1000, Cache: 1, LastValue: 1, Bindings: []source.PGSequenceBinding{{Schema: "MixedSchema", TableName: "Orders", ColumnName: "Id", Owned: true}}}, Bindings: []pgoracle.Binding{{SourceTable: "Orders", SourceColumn: "Id", Table: "ORDERS", Column: "ID", Trigger: "TRG_ORDERS_ID"}}}
	plan.Identity = &pgoracle.SequenceIdentity{ObjectID: 42, Fingerprint: plan.Fingerprint()}
	object := pgOracleEvidenceObject(t, plan)
	return src, dst, job, binding, object
}

func pgOracleEvidenceObject(t *testing.T, plan pgoracle.SequencePlan) repair.Object {
	t.Helper()
	statements, err := plan.SequenceStatements()
	require.NoError(t, err)
	return repair.Object{ID: "sequence", Kind: "sequence", SourceName: plan.Source.Name, TargetName: plan.Name, Schema: plan.Schema, Status: "succeeded", PGOracleSequence: &plan, Units: []repair.Unit{{ID: "create", SQL: statements[0].SQL, OriginalSQL: statements[0].SQL, Status: "succeeded"}}}
}

func seedPGOracleEvidence(t *testing.T, job *store.DataMigrationJob, binding *store.MigrationRepairBinding, objects ...repair.Object) {
	t.Helper()
	require.NoError(t, store.CreateDataMigrationJob(job))
	var rows []store.MigrationRepairObject
	for _, object := range objects {
		rows = append(rows, store.MigrationRepairObject{JobID: job.JobID, ObjectID: object.ID, ObjectJSON: repairJSON(object), Version: 1})
	}
	require.NoError(t, store.SeedMigrationRepairs(binding, rows))
}

func TestPGOracleEvidenceLoadsOnlySuccessfulTrustedExactScope(t *testing.T) {
	for _, mutation := range []string{"valid", "partial-job", "other-owner", "source-id", "target-id", "source-db", "source-schema-case", "target-schema", "connection-edited", "rebound-job", "legacy", "running", "failed-object", "failed-unit", "draft-sql", "missing-identity", "bad-fingerprint", "wrong-source-name"} {
		t.Run(mutation, func(t *testing.T) {
			src, dst, job, binding, object := pgOracleEvidenceFixture(t)
			switch mutation {
			case "partial-job":
				job.Status = "failed"
			case "other-owner":
				job.OwnerID++
			case "source-id":
				binding.SrcConnID++
			case "target-id":
				binding.DstConnID++
			case "source-db":
				binding.SourceDatabase = "other"
			case "source-schema-case":
				binding.SourceSchema = "mixedschema"
			case "target-schema":
				binding.TargetSchema = "OTHER"
			case "connection-edited":
				dst.Host = "new-target"
			case "rebound-job":
				job.DstConnDatabase = "OLD_SERVICE"
			case "legacy":
				binding.Legacy = true
			case "running":
				job.Status = "running"
			case "failed-object":
				object.Status = "failed"
			case "failed-unit":
				object.Units[0].Status = "failed"
			case "draft-sql":
				object.Units[0].SQL += " -- edited"
			case "missing-identity":
				object.PGOracleSequence.Identity = nil
			case "bad-fingerprint":
				object.PGOracleSequence.Identity.Fingerprint = "wrong"
			case "wrong-source-name":
				object.SourceName = "other"
			}
			seedPGOracleEvidence(t, job, binding, object)
			before, err := store.ListMigrationRepairObjects(job.JobID)
			require.NoError(t, err)
			ctx, err := withPGOracleMigrationEvidence(context.Background(), 7, src, dst, "MixedSchema", "APP")
			require.NoError(t, err)
			plans := datamigrate.PGOracleSequenceEvidence(ctx)
			if mutation == "valid" || mutation == "partial-job" {
				require.Len(t, plans, 1)
				require.EqualValues(t, 42, plans[0].Identity.ObjectID)
				plans[0].Identity.ObjectID = 999
				require.EqualValues(t, 42, datamigrate.PGOracleSequenceEvidence(ctx)[0].Identity.ObjectID)
			} else {
				require.Empty(t, plans)
			}
			after, err := store.ListMigrationRepairObjects(job.JobID)
			require.NoError(t, err)
			require.Equal(t, before, after, "loading historical evidence must remain read-only")
		})
	}
}

func TestPGOracleEvidencePreservesConflictsForObjectLevelPreflight(t *testing.T) {
	for _, mutation := range []string{"same", "watermark", "object-id", "source-binding"} {
		t.Run(mutation, func(t *testing.T) {
			src, dst, job, binding, first := pgOracleEvidenceFixture(t)
			plan := *first.PGOracleSequence
			plan.Identity = &pgoracle.SequenceIdentity{ObjectID: 42, Fingerprint: plan.Fingerprint()}
			if mutation == "watermark" {
				plan.Source.LastValue = 20
				plan.Source.IsCalled = true
			}
			if mutation == "object-id" {
				plan.Identity.ObjectID++
			}
			if mutation == "source-binding" {
				plan.Source.Bindings = []source.PGSequenceBinding{{Schema: "MixedSchema", TableName: "Orders", ColumnName: "OtherId", Owned: true}}
				plan.Bindings = []pgoracle.Binding{{SourceTable: "Orders", SourceColumn: "OtherId", Table: "ORDERS", Column: "OTHERID", Trigger: "TRG_ORDERS_OTHERID"}}
			}
			second := pgOracleEvidenceObject(t, plan)
			second.ID = "second-sequence-evidence"
			seedPGOracleEvidence(t, job, binding, first, second)
			ctx, err := withPGOracleMigrationEvidence(context.Background(), 7, src, dst, "MixedSchema", "APP")
			require.NoError(t, err)
			if mutation == "same" || mutation == "watermark" {
				require.Len(t, datamigrate.PGOracleSequenceEvidence(ctx), 1)
			} else {
				require.Len(t, datamigrate.PGOracleSequenceEvidence(ctx), 2, "preserve competing valid plans for sequence-level attribution")
			}
		})
	}
}

func TestPGOracleEvidenceStorageErrorFailsClosed(t *testing.T) {
	src, dst, _, _, _ := pgOracleEvidenceFixture(t)
	require.NoError(t, store.DB.Migrator().DropTable(&store.MigrationRepairObject{}))
	_, err := withPGOracleMigrationEvidence(context.Background(), 7, src, dst, "MixedSchema", "APP")
	require.ErrorContains(t, err, "身份账本失败")
	dst.DBType = "mysql"
	_, err = withPGOracleMigrationEvidence(context.Background(), 7, src, dst, "MixedSchema", "APP")
	require.NoError(t, err, "unrelated migration paths do not query Oracle evidence")
}
