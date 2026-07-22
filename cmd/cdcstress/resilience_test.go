package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"syscall"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	gomysql "github.com/go-sql-driver/mysql"
)

func TestClassifyWorkloadError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"ephemeral ports", syscall.EADDRNOTAVAIL, failureEnvironment},
		{"connection refused", syscall.ECONNREFUSED, failureEnvironment},
		{"too many mysql connections", &gomysql.MySQLError{Number: 1040, Message: "too many connections"}, failureCapacity},
		{"lock wait timeout", &gomysql.MySQLError{Number: 1205, Message: "lock wait timeout"}, failureCapacity},
		{"deadlock", &gomysql.MySQLError{Number: 1213, Message: "deadlock"}, failureCapacity},
		{"statement error", &gomysql.MySQLError{Number: 1064, Message: "syntax"}, failureCorrectness},
		{"ordinary error", errors.New("constraint mismatch"), failureCorrectness},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyWorkloadError(test.err); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestResumeJobTreatsAlreadyRunningAsSuccess(t *testing.T) {
	var posts int
	client, closeServer := resumeTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			posts++
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"任务已在运行"}`))
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(incrementalJob{JobID: "job-1", Status: "running"})
		}
	})
	defer closeServer()
	cfg, state := resumeTestState(t)
	if err := resumeJob(context.Background(), cfg, &state, client, "job-1"); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || state.RecoveryAttempts != 1 {
		t.Fatalf("posts=%d recovery_attempts=%d", posts, state.RecoveryAttempts)
	}
}

func TestResumeJobDirectSuccess(t *testing.T) {
	client, closeServer := resumeTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "任务已恢复"})
	})
	defer closeServer()
	cfg, state := resumeTestState(t)
	if err := resumeJob(context.Background(), cfg, &state, client, "job-1"); err != nil {
		t.Fatal(err)
	}
	if state.RecoveryAttempts != 1 {
		t.Fatalf("recovery_attempts=%d", state.RecoveryAttempts)
	}
}

func TestResumeJobRetriesPauseRelease(t *testing.T) {
	var posts int
	client, closeServer := resumeTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			if posts < 3 {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"任务已在运行"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "任务已恢复"})
			return
		}
		_ = json.NewEncoder(w).Encode(incrementalJob{JobID: "job-1", Status: "paused_manual"})
	})
	defer closeServer()
	cfg, state := resumeTestState(t)
	if err := resumeJob(context.Background(), cfg, &state, client, "job-1"); err != nil {
		t.Fatal(err)
	}
	if posts != 3 || state.RecoveryAttempts != 1 {
		t.Fatalf("posts=%d recovery_attempts=%d", posts, state.RecoveryAttempts)
	}
}

func TestResumeJobPreservesUnrelatedConflict(t *testing.T) {
	client, closeServer := resumeTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"任务当前状态不能恢复"}`))
	})
	defer closeServer()
	cfg, state := resumeTestState(t)
	err := resumeJob(context.Background(), cfg, &state, client, "job-1")
	var responseErr *apiError
	if !errors.As(err, &responseErr) || responseErr.Message != "任务当前状态不能恢复" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResumeJobTimesOutWhilePauseRegistrationRemains(t *testing.T) {
	client, closeServer := resumeTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"任务已在运行"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(incrementalJob{JobID: "job-1", Status: "paused_manual"})
	})
	defer closeServer()
	client.resumeTimeout = 5 * time.Millisecond
	cfg, state := resumeTestState(t)
	if err := resumeJob(context.Background(), cfg, &state, client, "job-1"); err == nil {
		t.Fatal("expected timeout")
	}
}

func TestLegacyResumeCreatesScenarioCheckpoint(t *testing.T) {
	cfg := baseTestConfig()
	cfg.OutputDir = t.TempDir()
	cfg.applyDefaults()
	state := RunState{RunID: "run_legacy", ConfigHash: cfg.hash(), Prepared: true, ActiveJobID: "job-1", ActiveMode: "full_then_cdc"}
	if err := applyLegacyResume(cfg, &state, "both", "pause-resume"); err != nil {
		t.Fatal(err)
	}
	if state.Version != currentStateVersion || state.ActiveScenario != "full_then_cdc/pause-resume" || state.ActivePhase != "resuming" {
		t.Fatalf("unexpected state: %+v", state)
	}
	if !scenarioCompleted(state, "full_then_cdc/steady-1000tps") || len(state.SkippedLegacy) == 0 {
		t.Fatalf("legacy stages were not checkpointed: %+v", state)
	}
}

func TestRecordWorkloadReplacesSameStage(t *testing.T) {
	cfg := baseTestConfig()
	cfg.OutputDir = t.TempDir()
	cfg.applyDefaults()
	state := newRunState(cfg, "run_state")
	first := WorkloadResult{Name: "full_then_cdc/steady-100tps", Committed: 10}
	second := WorkloadResult{Name: first.Name, Committed: 20}
	if err := recordWorkload(cfg, &state, first); err != nil {
		t.Fatal(err)
	}
	if err := recordWorkload(cfg, &state, second); err != nil {
		t.Fatal(err)
	}
	if len(state.Workloads) != 1 || state.Workloads[0].Committed != 20 {
		t.Fatalf("unexpected workloads: %+v", state.Workloads)
	}
}

func TestDeferredStageReturnsWithoutTargetVisibility(t *testing.T) {
	source, sourceMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, targetMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	cfg := baseTestConfig()
	cfg.OutputDir = t.TempDir()
	cfg.Profile.Seed = 1
	cfg.Workload.Workers = 1
	cfg.Workload.CatchUpTimeout = Duration{2 * time.Second}
	cfg.applyDefaults()
	state := newRunState(cfg, "run_deferred")
	state.Tables = []TableSpec{{Name: "cs_test", Kind: kindPrimary}}
	if err = saveState(cfg, &state); err != nil {
		t.Fatal(err)
	}
	ledger, err := newLedgerWriter(cfg, state.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.close()
	engine := &workloadEngine{cfg: cfg, state: &state, source: source, target: target, ledger: ledger}
	engine.seq.Store(99)
	sourceMock.ExpectBegin()
	sourceMock.ExpectExec("INSERT INTO `cs_test`").WillReturnResult(sqlmock.NewResult(100, 1))
	sourceMock.ExpectCommit()
	started := time.Now()
	execution := engine.runStageWithOptions(context.Background(), "full_then_cdc/writes-while-paused", 1, 30*time.Millisecond, stageOptions{DeferLatency: true})
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("deferred stage blocked for target visibility: %s", elapsed)
	}
	if execution.Result.Committed != 1 || len(execution.Markers) != 1 || execution.Markers[0].RowID != 100 {
		t.Fatalf("unexpected execution: %+v", execution)
	}
	if err = sourceMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err = targetMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMeasureDeferredLatencyAfterTargetVisible(t *testing.T) {
	target, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	cfg := baseTestConfig()
	cfg.applyDefaults()
	marker := LatencyMarker{Table: "cs_test", RowID: 42, CommittedAt: time.Now().Add(-2 * time.Second)}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM "dbgold_cdc_stress"."cs_test" WHERE id=$1`)).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	engine := &workloadEngine{cfg: cfg, target: target}
	latencies, err := engine.measureDeferredLatencies(context.Background(), []LatencyMarker{marker}, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(latencies) != 1 || latencies[0] < 2000 {
		t.Fatalf("unexpected latencies: %v", latencies)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeferredLatencyVisibilityTimeout(t *testing.T) {
	target, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	cfg := baseTestConfig()
	cfg.applyDefaults()
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM "dbgold_cdc_stress"."cs_test" WHERE id=$1`)).WithArgs(int64(42)).WillReturnError(errors.New("not visible"))
	engine := &workloadEngine{cfg: cfg, target: target}
	_, err = engine.measureDeferredLatencies(context.Background(), []LatencyMarker{{Table: "cs_test", RowID: 42, CommittedAt: time.Now()}}, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected visibility timeout")
	}
}

func TestMissingDeferredMarkerFailsPauseScenario(t *testing.T) {
	target, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	cfg := baseTestConfig()
	cfg.OutputDir = t.TempDir()
	cfg.applyDefaults()
	scenario := "full_then_cdc/pause-resume"
	state := newRunState(cfg, "run_missing_marker")
	state.ActiveScenario, state.ActivePhase = scenario, "resuming"
	state.PendingLatencyWorkload = "full_then_cdc/writes-while-paused"
	state.PendingLatencyMarkers = []LatencyMarker{{Table: "cs_test", RowID: 42, CommittedAt: time.Now()}}
	state.Workloads = []WorkloadResult{{Name: state.PendingLatencyWorkload, Committed: 1}}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM "dbgold_cdc_stress"."cs_test" WHERE id=$1`)).WithArgs(int64(42)).WillReturnError(errors.New("not visible"))
	engine := &workloadEngine{cfg: cfg, target: target}
	err = finishDeferredPauseLatencyWithTimeout(context.Background(), cfg, &state, engine, scenario, 10*time.Millisecond)
	if err == nil || state.FailureClass != failureCorrectness || state.FailedScenario != scenario || len(state.PendingLatencyMarkers) != 1 || scenarioCompleted(state, scenario) {
		t.Fatalf("missing marker was not retained as correctness failure: err=%v state=%+v", err, state)
	}
}

func TestDeferredMarkersPersistAndClearAtomically(t *testing.T) {
	cfg := baseTestConfig()
	cfg.OutputDir = t.TempDir()
	cfg.applyDefaults()
	state := newRunState(cfg, "run_markers")
	scenario := "full_then_cdc/pause-resume"
	workload := WorkloadResult{Name: "full_then_cdc/writes-while-paused", Committed: 1}
	marker := LatencyMarker{Table: "cs_test", RowID: 42, CommittedAt: time.Now().UTC()}
	if err := recordDeferredWorkload(cfg, &state, workload, []LatencyMarker{marker}, scenario, "resuming"); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadState(cfg, state.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PendingLatencyWorkload != workload.Name || len(loaded.PendingLatencyMarkers) != 1 || loaded.ActivePhase != "resuming" {
		t.Fatalf("deferred markers not persisted: %+v", loaded)
	}
	loaded.FailedScenario, loaded.FailureClass = scenario, failureCorrectness
	if err = completeDeferredWorkload(cfg, &loaded, scenario, []float64{1234}); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadState(cfg, state.RunID)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := workloadByName(loaded, workload.Name)
	if !ok || len(result.LatencyMS) != 1 || result.LatencyMS[0] != 1234 || len(loaded.PendingLatencyMarkers) != 0 || !scenarioCompleted(loaded, scenario) || loaded.FailureClass != "" || loaded.FailedScenario != "" {
		t.Fatalf("deferred markers not completed atomically: %+v", loaded)
	}
	if err = completeDeferredWorkload(cfg, &loaded, scenario, []float64{9999}); err != nil {
		t.Fatal(err)
	}
	result, _ = workloadByName(loaded, workload.Name)
	if len(result.LatencyMS) != 1 || result.LatencyMS[0] != 1234 {
		t.Fatalf("repeated completion changed latency: %+v", result.LatencyMS)
	}
}

func resumeTestClient(t *testing.T, handler http.HandlerFunc) (*apiClient, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	return &apiClient{base: server.URL, http: server.Client(), poll: time.Millisecond, resumeTimeout: time.Second}, server.Close
}

func resumeTestState(t *testing.T) (Config, RunState) {
	t.Helper()
	cfg := baseTestConfig()
	cfg.OutputDir = t.TempDir()
	cfg.applyDefaults()
	return cfg, newRunState(cfg, "run_resume")
}
