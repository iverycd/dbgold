package target

import (
	"context"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"testing"
)

func intervalTargetFixture(t *testing.T) *partition.Plan {
	t.Helper()
	n := func(v string) []source.PartitionValue { return []source.PartitionValue{{Kind: "number", Text: v}} }
	info := &source.TableDDLInfo{TableName: "EVENTS", Columns: []source.ColumnInfo{{Name: "ID", DataType: "NUMBER", Precision: 5}}, Partition: &source.PartitionSpec{Method: "RANGE", Keys: []source.PartitionKey{{Column: "ID", DataType: "NUMBER"}}, RowMovement: "disabled", Automatic: "10", Interval: &source.PartitionInterval{Unit: "number", Amount: "10", Transition: n("0")[0]}, Partitions: []source.Partition{{Schema: "SRC", Name: "P0", Ordinal: 1, Lower: []source.PartitionValue{{Kind: "minvalue"}}, Upper: n("0"), LowerInclusive: true}, {Schema: "SRC", Name: "P4", Ordinal: 2, Lower: n("40"), Upper: n("50"), LowerInclusive: true, IsInterval: true}}}}
	p, e := partition.Compile(info, []dialect.Statement{{SQL: `CREATE TABLE "APP"."EVENTS"("ID" INT NOT NULL)`}}, partition.Profile{Product: "dameng", Version: "DM V8", Features: map[string]bool{"inline_oracle": true, "row_movement_clause": true, partition.FeatureRange: true, partition.FeatureInterval: true}}, partition.Options{SourceType: "dameng", TargetType: "dameng", TargetSchema: "APP"})
	require.NoError(t, e)
	return p
}

func TestIntervalMaterializerInsertRollbackAndCancellationCannotSucceed(t *testing.T) {
	for _, mode := range []string{"insert", "rollback", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			db, mock := partitionInspectorDB(t)
			w := &DaMengWriter{db: db, schema: "APP", dia: dialect.NewDaMeng()}
			p := intervalTargetFixture(t)
			expectIntervalTargetMetadata(mock, "")
			mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "APP"\."EVENTS"`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
			for _, view := range []string{"ALL_TRIGGERS", "ALL_CONSTRAINTS"} {
				mock.ExpectQuery(`SELECT COUNT\(\*\) FROM `+view).WithArgs("APP", "EVENTS", "APP", "EVENTS").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
			}
			mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("EVENTS", "APP").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TABLE "APP"."EVENTS"("ID" INT NOT NULL)`))
			if mode == "cancel" {
				mock.ExpectBegin().WillReturnError(context.Canceled)
			} else {
				mock.ExpectBegin()
				insert := mock.ExpectExec(`INSERT INTO "APP"\."EVENTS"`)
				if mode == "insert" {
					insert.WillReturnError(errors.New("seed rejected"))
				} else {
					insert.WillReturnResult(sqlmock.NewResult(0, 1))
				}
				rollback := mock.ExpectRollback()
				if mode == "rollback" {
					rollback.WillReturnError(errors.New("connection lost during rollback"))
				}
			}
			var events []repair.Event
			ctx := repair.WithObserver(context.Background(), func(e repair.Event) error { events = append(events, e); return nil })
			require.Error(t, w.MaterializePartitionIntervals(ctx, p))
			require.Len(t, events, 2)
			require.Equal(t, "running", events[0].Status)
			if mode == "insert" {
				require.Equal(t, "failed", events[1].Status)
			} else {
				require.Equal(t, "unknown", events[1].Status)
			}
		})
	}
}

func expectIntervalTargetMetadata(mock sqlmock.Sqlmock, generatedName string) {
	mock.ExpectQuery(`SELECT PARTITIONING_TYPE,SUBPARTITIONING_TYPE`).WithArgs("APP", "EVENTS").WillReturnRows(sqlmock.NewRows([]string{"m", "s"}).AddRow("RANGE", "NONE"))
	mock.ExpectQuery(`SELECT ROW_MOVEMENT FROM ALL_TABLES`).WithArgs("APP", "EVENTS").WillReturnRows(sqlmock.NewRows([]string{"r"}).AddRow("DISABLED"))
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("EVENTS", "APP").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TABLE "APP"."EVENTS"("ID" INT NOT NULL) PARTITION BY RANGE("ID") INTERVAL(10) (PARTITION "P0" VALUES LESS THAN(0))`))
	mock.ExpectQuery(`SELECT k.COLUMN_NAME,c.DATA_TYPE`).WithArgs("APP", "EVENTS").WillReturnRows(sqlmock.NewRows([]string{"col", "type"}).AddRow("ID", "INT"))
	rows := sqlmock.NewRows([]string{"n", "p", "h", "s"}).AddRow("P0", 1, "0", 0)
	if generatedName != "" {
		rows.AddRow(generatedName, 2, "50", 0)
	}
	mock.ExpectQuery(`SELECT PARTITION_NAME,PARTITION_POSITION,HIGH_VALUE,SUBPARTITION_COUNT`).WithArgs("APP", "EVENTS").WillReturnRows(rows)
}

func TestIntervalMaterializerRollsBackRowAndRenamesNativeEmptyLeaf(t *testing.T) {
	db, mock := partitionInspectorDB(t)
	db.SetMaxOpenConns(1)
	w := &DaMengWriter{db: db, schema: "APP", dia: dialect.NewDaMeng()}
	plan := intervalTargetFixture(t)
	expectIntervalTargetMetadata(mock, "")
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "APP"\."EVENTS"`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	for _, view := range []string{"ALL_TRIGGERS", "ALL_CONSTRAINTS"} {
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM `+view).WithArgs("APP", "EVENTS", "APP", "EVENTS").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	}
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("EVENTS", "APP").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TABLE "APP"."EVENTS"("ID" INT NOT NULL) PARTITION BY RANGE("ID") INTERVAL(10) (PARTITION "P0" VALUES LESS THAN(0))`))
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO "APP"\."EVENTS" \("ID"\) VALUES \(40\)`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()
	expectIntervalTargetMetadata(mock, "SYS_AUTO")
	mock.ExpectExec(`ALTER TABLE "APP"\."EVENTS" RENAME PARTITION "SYS_AUTO" TO "P4"`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "APP"\."EVENTS"`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	expectIntervalTargetMetadata(mock, "P4")
	require.NoError(t, w.MaterializePartitionIntervals(context.Background(), plan))
}

func TestIntervalMaterializerRefusesBusinessDataAndTriggers(t *testing.T) {
	for _, tc := range []struct {
		name           string
		rows, triggers int
	}{{"data", 1, 0}, {"trigger", 0, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := partitionInspectorDB(t)
			w := &DaMengWriter{db: db, schema: "APP", dia: dialect.NewDaMeng()}
			p := intervalTargetFixture(t)
			expectIntervalTargetMetadata(mock, "")
			mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "APP"\."EVENTS"`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(tc.rows))
			if tc.rows == 0 {
				mock.ExpectQuery(`SELECT COUNT\(\*\) FROM ALL_TRIGGERS`).WithArgs("APP", "EVENTS", "APP", "EVENTS").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(tc.triggers))
			}
			require.Error(t, w.MaterializePartitionIntervals(context.Background(), p))
		})
	}
}

func TestIntervalMaterializerCompletedRetryIsReadOnlyAndRejectsTamperedRecipe(t *testing.T) {
	db, mock := partitionInspectorDB(t)
	w := &DaMengWriter{db: db, schema: "APP", dia: dialect.NewDaMeng()}
	p := intervalTargetFixture(t)
	expectIntervalTargetMetadata(mock, "P4")
	require.NoError(t, w.MaterializePartitionIntervals(context.Background(), p))
	p.Interval.Seeds[0].Value.Text = "0"
	require.Error(t, w.MaterializePartitionIntervals(context.Background(), p))
	for _, ddl := range []string{`CREATE TABLE "T"("ID" INT IDENTITY)`, `CREATE TABLE "T"("ID" INT AS(user_fn()))`, `CREATE TABLE "T"("ID" INT DEFAULT ON NULL user_fn())`, `CREATE TABLE "T"("ID" TIMESTAMP ON UPDATE user_fn())`, `CREATE TABLE "T"("OTHER" INT)`} {
		require.Error(t, intervalSafeDefinition(ddl, []string{"ID"}))
	}
}

func TestIntervalRepairMarkerIsTypedAndReleasesSingleConnection(t *testing.T) {
	db, mock := partitionInspectorDB(t)
	db.SetMaxOpenConns(1)
	w := &DaMengWriter{db: db, schema: "APP", dia: dialect.NewDaMeng()}
	p := intervalTargetFixture(t)
	marker := partition.IntervalMaterializationSQL(p)
	units := []repair.Unit{{SQL: p.Statements[0].SQL, Status: "succeeded"}, {SQL: marker, OriginalSQL: marker, Selected: true, Status: "failed"}}
	expectIntervalTargetMetadata(mock, "P4")
	expectIntervalTargetMetadata(mock, "P4")
	result, e := ExecuteRepair(context.Background(), w, repair.Object{Kind: "table", Schema: "APP", TargetName: "EVENTS", PartitionPlan: p}, units, nil)
	require.NoError(t, e)
	require.Equal(t, "succeeded", result[1].Status)
}

func TestIntervalProfileRequiresObservedStandaloneRuntime(t *testing.T) {
	for _, mode := range []string{"0", "1", "missing", "denied"} {
		t.Run(mode, func(t *testing.T) {
			db, mock := partitionInspectorDB(t)
			mock.ExpectQuery(`SELECT SVR_VERSION FROM V\$INSTANCE`).WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("DM Database Server V8"))
			mock.ExpectQuery(`SELECT PARA_VALUE FROM V\$DM_INI WHERE PARA_NAME='COMPATIBLE_MODE'`).WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("2"))
			mock.ExpectQuery(`SELECT PARTITION_NAME,HIGH_VALUE,PARTITION_POSITION`).WillReturnRows(sqlmock.NewRows([]string{"n", "h", "p"}))
			probe := mock.ExpectQuery(`SELECT PARA_VALUE FROM V\$DM_INI WHERE PARA_NAME='MPP_INI'`)
			if mode == "denied" {
				probe.WillReturnError(errors.New("denied"))
			} else {
				rows := sqlmock.NewRows([]string{"v"})
				if mode != "missing" {
					rows.AddRow(mode)
				}
				probe.WillReturnRows(rows)
			}
			profile, e := (dbPartitionInspector{db: db, product: "dameng", schema: "APP"}).profile(context.Background())
			require.NoError(t, e)
			require.Equal(t, mode == "0", profile.Features[partition.FeatureInterval])
		})
	}
}
