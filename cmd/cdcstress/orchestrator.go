package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"dbgold/datamigrate/cdc"
)

const deferredLatencyVisibilityTimeout = 30 * time.Second

type RunReport struct {
	RunID            string             `json:"run_id"`
	StartedAt        time.Time          `json:"started_at"`
	FinishedAt       time.Time          `json:"finished_at"`
	ConfigHash       string             `json:"config_hash"`
	TableCount       int                `json:"table_count"`
	TotalRows        int64              `json:"initial_rows"`
	Environment      EnvironmentSummary `json:"environment"`
	ResourcePeaks    ResourcePeaks      `json:"resource_peaks"`
	Jobs             []JobResult        `json:"jobs"`
	Workloads        []WorkloadResult   `json:"workloads"`
	MaxLagSecs       int64              `json:"max_lag_seconds"`
	FailureClass     string             `json:"failure_class,omitempty"`
	FailedScenario   string             `json:"failed_scenario,omitempty"`
	RecoveryAttempts int                `json:"recovery_attempts,omitempty"`
	SkippedLegacy    []string           `json:"skipped_legacy_scenarios,omitempty"`
	Verification     Verification       `json:"verification"`
	Passed           bool               `json:"passed"`
	Errors           []string           `json:"errors,omitempty"`
}

type EnvironmentSummary struct {
	GoVersion      string `json:"go_version"`
	OS             string `json:"os"`
	Architecture   string `json:"architecture"`
	MySQLVersion   string `json:"mysql_version,omitempty"`
	GaussDBVersion string `json:"gaussdb_version,omitempty"`
	Source         string `json:"source"`
	Target         string `json:"target"`
}

type ResourcePeaks struct {
	MySQLThreadsConnected int64 `json:"mysql_threads_connected"`
	MySQLThreadsRunning   int64 `json:"mysql_threads_running"`
	GaussDBSessions       int64 `json:"gaussdb_sessions"`
}

type JobResult struct {
	Mode       string         `json:"mode"`
	JobID      string         `json:"job_id"`
	StartedAt  time.Time      `json:"started_at"`
	ReadyAt    time.Time      `json:"ready_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Final      incrementalJob `json:"final"`
}

func executeRun(ctx context.Context, cfg Config, state *RunState, mode string, manualRestart bool, startTPS int, resumeFrom string) error {
	if !state.Prepared {
		return errors.New("run is not prepared")
	}
	if err := applyLegacyResume(cfg, state, mode, resumeFrom); err != nil {
		return err
	}
	if err := precheck(ctx, cfg); err != nil {
		return err
	}
	dbs, err := openDatabases(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer dbs.close()
	client, err := newAPIClient(ctx, cfg)
	if err != nil {
		return err
	}
	source, target, err := resolveConnections(ctx, client, cfg)
	if err != nil {
		return err
	}
	state.SourceID, state.TargetID = source.ID, target.ID
	if err = saveState(cfg, state); err != nil {
		return err
	}
	engine, err := newWorkloadEngine(cfg, state, dbs.mysql, dbs.gauss)
	if err != nil {
		return err
	}
	defer engine.close()
	report := RunReport{RunID: state.RunID, StartedAt: time.Now().UTC(), ConfigHash: cfg.hash(), TableCount: len(state.Tables), TotalRows: cfg.Profile.TotalRows,
		Environment: collectEnvironment(ctx, cfg, dbs), Workloads: append([]WorkloadResult(nil), state.Workloads...),
		FailureClass: state.FailureClass, FailedScenario: state.FailedScenario, RecoveryAttempts: state.RecoveryAttempts,
		SkippedLegacy: append([]string(nil), state.SkippedLegacy...)}
	sampleCtx, stopSampling := context.WithCancel(ctx)
	resourceSamples := make(chan ResourcePeaks, 1)
	go func() { resourceSamples <- sampleResourcePeaks(sampleCtx, dbs, cfg.Workload.PollInterval.Duration) }()
	finishSampling := func() { stopSampling(); report.ResourcePeaks = <-resourceSamples }
	modes := []string{mode}
	if mode == "both" {
		modes = []string{"full_then_cdc", "incremental_only"}
	}
	resumeMode := state.ActiveMode
	for modeIndex, currentMode := range modes {
		if scenarioCompleted(*state, currentMode) {
			log.Printf("mode %s was already completed; skipping", currentMode)
			continue
		}
		modeStartTPS := 0
		if startTPS > 0 && ((resumeMode != "" && currentMode == resumeMode) || (resumeMode == "" && modeIndex == 0)) {
			modeStartTPS = startTPS
		}
		job, workloads, maxLag, runErr := executeMode(ctx, cfg, state, client, dbs, engine, currentMode, manualRestart && currentMode == modes[len(modes)-1], modeStartTPS)
		report.Jobs = append(report.Jobs, job)
		_ = workloads
		report.Workloads = append([]WorkloadResult(nil), state.Workloads...)
		report.FailureClass, report.FailedScenario = state.FailureClass, state.FailedScenario
		report.RecoveryAttempts = state.RecoveryAttempts
		if maxLag > report.MaxLagSecs {
			report.MaxLagSecs = maxLag
		}
		if runErr != nil {
			if state.FailedScenario == "" && state.ActiveScenario != "" {
				_ = recordScenarioFailure(cfg, state, state.ActiveScenario, classifyWorkloadError(runErr))
			}
			report.Errors = append(report.Errors, runErr.Error())
			report.FailureClass, report.FailedScenario = state.FailureClass, state.FailedScenario
			report.FinishedAt = time.Now().UTC()
			finishSampling()
			_ = writeRunReport(cfg, report)
			return runErr
		}
	}
	report.Verification, err = verify(ctx, cfg, state, dbs)
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
	}
	report.Passed = err == nil && report.Verification.Match && state.FailureClass == "" && state.ActiveScenario == ""
	report.Workloads = append([]WorkloadResult(nil), state.Workloads...)
	report.FailureClass, report.FailedScenario, report.RecoveryAttempts = state.FailureClass, state.FailedScenario, state.RecoveryAttempts
	report.FinishedAt = time.Now().UTC()
	finishSampling()
	if writeErr := writeRunReport(cfg, report); writeErr != nil && err == nil {
		err = writeErr
	}
	if err != nil {
		return err
	}
	if !report.Passed {
		return errors.New("CDC verification found source/target differences")
	}
	return nil
}

func collectEnvironment(ctx context.Context, cfg Config, dbs *databases) EnvironmentSummary {
	result := EnvironmentSummary{GoVersion: runtime.Version(), OS: runtime.GOOS, Architecture: runtime.GOARCH,
		Source: redactedDSN(cfg.MySQL), Target: redactedDSN(cfg.GaussDB)}
	_ = dbs.mysql.QueryRowContext(ctx, "SELECT VERSION()").Scan(&result.MySQLVersion)
	_ = dbs.gauss.QueryRowContext(ctx, "SELECT VERSION()").Scan(&result.GaussDBVersion)
	return result
}

func sampleResourcePeaks(ctx context.Context, dbs *databases, interval time.Duration) ResourcePeaks {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var peaks ResourcePeaks
	sample := func() {
		rows, err := dbs.mysql.QueryContext(ctx, "SHOW GLOBAL STATUS WHERE Variable_name IN ('Threads_connected','Threads_running')")
		if err == nil {
			for rows.Next() {
				var name, value string
				if rows.Scan(&name, &value) == nil {
					parsed, _ := strconv.ParseInt(value, 10, 64)
					if name == "Threads_connected" && parsed > peaks.MySQLThreadsConnected {
						peaks.MySQLThreadsConnected = parsed
					}
					if name == "Threads_running" && parsed > peaks.MySQLThreadsRunning {
						peaks.MySQLThreadsRunning = parsed
					}
				}
			}
			rows.Close()
		}
		var sessions int64
		if dbs.gauss.QueryRowContext(ctx, "SELECT COUNT(*) FROM pg_stat_activity").Scan(&sessions) == nil && sessions > peaks.GaussDBSessions {
			peaks.GaussDBSessions = sessions
		}
	}
	for {
		select {
		case <-ctx.Done():
			return peaks
		case <-ticker.C:
			sample()
		}
	}
}

func executeMode(ctx context.Context, cfg Config, state *RunState, client *apiClient, dbs *databases, engine *workloadEngine, mode string, manualRestart bool, startTPS int) (JobResult, []WorkloadResult, int64, error) {
	jobResult := JobResult{Mode: mode, StartedAt: time.Now().UTC()}
	pauseScenario := mode + "/pause-resume"
	recoveringPause := state.ActiveScenario == pauseScenario && state.ActivePhase == "resuming"
	conflictScenarioName := mode + "/target-conflict"
	recoveringConflict := state.ActiveScenario == conflictScenarioName && state.PendingConflict != nil
	var err error
	var position apiPosition
	if mode == "incremental_only" {
		current, err := cdc.CurrentPosition(ctx, dbs.mysql)
		if err != nil {
			return jobResult, nil, 0, err
		}
		position = apiPosition{File: current.File, Position: current.Pos, GTID: current.GTID}
	}
	request := incrementalRequestFor(cfg, *state, mode, position)
	jobID := state.ActiveJobID
	existingStatus := ""
	if jobID != "" {
		if state.ActiveMode != mode {
			return jobResult, nil, 0, fmt.Errorf("state has active %s job %s; cannot run mode %s", state.ActiveMode, jobID, mode)
		}
		current, loadErr := client.job(ctx, jobID)
		if loadErr != nil {
			return jobResult, nil, 0, loadErr
		}
		existingStatus = current.Status
		if current.Status == "stopped" || current.Status == "aborted" {
			state.ActiveJobID, state.ActiveMode = "", ""
			if current.Status == "stopped" {
				state.Completed = append(state.Completed, mode)
				_ = saveState(cfg, state)
				return JobResult{Mode: mode, JobID: jobID, Final: current, FinishedAt: time.Now().UTC()}, nil, current.LagSeconds, nil
			}
			_ = saveState(cfg, state)
			return jobResult, nil, current.LagSeconds, fmt.Errorf("active job %s was aborted", jobID)
		}
		if current.Status == "pausing" {
			if _, _, waitErr := waitForJob(ctx, client, jobID, 2*time.Minute, func(j incrementalJob) bool { return j.Status == "paused_manual" }); waitErr != nil {
				return jobResult, nil, current.LagSeconds, waitErr
			}
			if err := resumeJob(ctx, cfg, state, client, jobID); err != nil {
				return jobResult, nil, current.LagSeconds, fmt.Errorf("resume job %s after interrupted pause: %w", jobID, err)
			}
		}
		if current.Status == "paused_manual" || current.Status == "paused_restart" || current.Status == "failed" {
			if err := resumeJob(ctx, cfg, state, client, jobID); err != nil {
				return jobResult, nil, current.LagSeconds, fmt.Errorf("resume job %s: %w", jobID, err)
			}
		}
		if current.Status == "paused_ddl" {
			if err := finishPendingDDL(ctx, cfg, state, client, dbs, jobID); err != nil {
				return jobResult, nil, current.LagSeconds, err
			}
		}
		if current.Status == "paused_row_conflict" {
			if err := repairPendingConflict(ctx, cfg, state, client, dbs, jobID); err != nil {
				return jobResult, nil, current.LagSeconds, err
			}
		}
		jobResult.JobID = jobID
		log.Printf("continuing existing %s job %s from status %s", mode, jobID, current.Status)
	} else {
		preflight, err := client.preflight(ctx, request)
		if err != nil {
			return jobResult, nil, 0, err
		}
		if !preflight.OK {
			return jobResult, nil, 0, fmt.Errorf("%s preflight failed: %s", mode, strings.Join(preflight.Errors, "; "))
		}
		if len(preflight.Tables) != len(state.Tables) {
			return jobResult, nil, 0, fmt.Errorf("preflight selected %d tables, expected %d", len(preflight.Tables), len(state.Tables))
		}
		jobID, _, err = client.start(ctx, request)
		if err != nil {
			return jobResult, nil, 0, err
		}
		jobResult.JobID = jobID
		state.ActiveJobID, state.ActiveMode = jobID, mode
		state.JobIDs = append(state.JobIDs, jobID)
		if err = saveState(cfg, state); err != nil {
			return jobResult, nil, 0, err
		}
		log.Printf("started %s job %s with %d tables", mode, jobID, len(preflight.Tables))
	}
	var workloads []WorkloadResult
	var bootstrapCancel context.CancelFunc
	var bootstrapDone chan WorkloadResult
	snapshotScenario := mode + "/snapshot-concurrent-write"
	if mode == "full_then_cdc" && !scenarioCompleted(*state, snapshotScenario) {
		if err = startScenario(cfg, state, snapshotScenario, "running"); err != nil {
			return jobResult, workloads, 0, err
		}
		var bootstrapCtx context.Context
		bootstrapCtx, bootstrapCancel = context.WithCancel(ctx)
		bootstrapDone = make(chan WorkloadResult, 1)
		go func() {
			bootstrapDone <- engine.runStage(bootstrapCtx, mode+"/snapshot-concurrent-write", cfg.Workload.Steps[0], 24*time.Hour)
		}()
	}
	cutoverRecovery := state.ActiveScenario == mode+"/cutover" && (existingStatus == "cutting_over" || existingStatus == "ready_to_cutover" || existingStatus == "ready_with_warnings")
	var maxLag int64
	if !cutoverRecovery {
		_, maxLag, err = waitForJob(ctx, client, jobID, cfg.Workload.CatchUpTimeout.Duration, func(j incrementalJob) bool {
			return (j.Status == "running" || j.Status == "catching_up") && j.CaughtUp
		})
	}
	if bootstrapCancel != nil {
		bootstrapCancel()
		bootstrapResult := <-bootstrapDone
		workloads = append(workloads, bootstrapResult)
		if saveErr := recordWorkload(cfg, state, bootstrapResult); saveErr != nil {
			return jobResult, workloads, maxLag, saveErr
		}
		if bootstrapResult.Errors > 0 && bootstrapResult.FailureClass != failureCapacity {
			_ = recordScenarioFailure(cfg, state, snapshotScenario, bootstrapResult.FailureClass)
			return jobResult, workloads, maxLag, workloadFailure(bootstrapResult)
		}
		if saveErr := completeScenario(cfg, state, snapshotScenario); saveErr != nil {
			return jobResult, workloads, maxLag, saveErr
		}
	}
	if err != nil {
		return jobResult, workloads, maxLag, err
	}
	jobResult.ReadyAt = time.Now().UTC()
	if recoveringPause {
		if state.PendingLatencyWorkload != "" || len(state.PendingLatencyMarkers) > 0 {
			err = finishDeferredPauseLatency(ctx, cfg, state, engine, pauseScenario)
		} else {
			err = completeScenario(cfg, state, pauseScenario)
		}
		if err != nil {
			return jobResult, workloads, maxLag, err
		}
		log.Printf("completed interrupted %s scenario after resume and catch-up", pauseScenario)
	}
	if recoveringConflict && state.PendingConflict == nil {
		if err = completeScenario(cfg, state, conflictScenarioName); err != nil {
			return jobResult, workloads, maxLag, err
		}
	}

	transactionScenario := mode + "/transaction-boundaries"
	if !scenarioCompleted(*state, transactionScenario) {
		if err = startScenario(cfg, state, transactionScenario, "running"); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if err = transactionBoundaryScenario(ctx, engine); err != nil {
			_ = recordScenarioFailure(cfg, state, transactionScenario, classifyWorkloadError(err))
			return jobResult, workloads, maxLag, err
		}
		if err = completeScenario(cfg, state, transactionScenario); err != nil {
			return jobResult, workloads, maxLag, err
		}
	}
	saturated := false
	ranSteadyStage := false
	for _, tps := range cfg.Workload.Steps {
		if startTPS > 0 && tps < startTPS {
			continue
		}
		stageName := fmt.Sprintf("%s/steady-%dtps", mode, tps)
		if scenarioCompleted(*state, stageName) {
			log.Printf("scenario %s was already completed; skipping", stageName)
			if previous, ok := workloadByName(*state, stageName); ok && previous.Errors > 0 && previous.FailureClass == failureCapacity {
				saturated = true
				log.Printf("preserving previously recorded capacity boundary at %d TPS", tps)
				break
			}
			continue
		}
		ranSteadyStage = true
		if err = startScenario(cfg, state, stageName, "running"); err != nil {
			return jobResult, workloads, maxLag, err
		}
		result := engine.runStage(ctx, stageName, tps, cfg.Workload.StepDuration.Duration)
		workloads = append(workloads, result)
		if err = recordWorkload(cfg, state, result); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if result.Errors > 0 && result.FailureClass == failureCapacity {
			saturated = true
			log.Printf("capacity boundary reached at %d TPS: committed=%d errors=%d; stopping escalation and waiting for CDC catch-up", tps, result.Committed, result.Errors)
			for _, sample := range result.ErrorSamples {
				log.Printf("workload error sample: %s", sample)
			}
		}
		job, lag, waitErr := waitForJob(ctx, client, jobID, cfg.Workload.CatchUpTimeout.Duration, func(j incrementalJob) bool { return j.CaughtUp && j.Status == "running" })
		if lag > maxLag {
			maxLag = lag
		}
		if waitErr != nil {
			return jobResult, workloads, maxLag, waitErr
		}
		if result.Errors > 0 && result.FailureClass != failureCapacity {
			_ = recordScenarioFailure(cfg, state, stageName, result.FailureClass)
			return jobResult, workloads, maxLag, workloadFailure(result)
		}
		if err = completeScenario(cfg, state, stageName); err != nil {
			return jobResult, workloads, maxLag, err
		}
		log.Printf("stage %s actual_tps=%.1f lag=%ds events=%d", result.Name, result.ActualTPS, job.LagSeconds, job.InsertCount+job.UpdateCount+job.DeleteCount)
		if saturated {
			break
		}
	}
	if !ranSteadyStage && startTPS > 0 && !hasCompletedSteadyScenario(*state, mode, startTPS) {
		return jobResult, workloads, maxLag, fmt.Errorf("no configured TPS step is at or above --start-tps=%d", startTPS)
	}
	burstScenario := mode + "/burst"
	if !saturated && !scenarioCompleted(*state, burstScenario) {
		if err = startScenario(cfg, state, burstScenario, "running"); err != nil {
			return jobResult, workloads, maxLag, err
		}
		burst := engine.runStage(ctx, burstScenario, cfg.Workload.Steps[len(cfg.Workload.Steps)-1]*2, min(cfg.Workload.StepDuration.Duration, 30*time.Second))
		workloads = append(workloads, burst)
		if err = recordWorkload(cfg, state, burst); err != nil {
			return jobResult, workloads, maxLag, err
		}
		_, lag, err := waitForJob(ctx, client, jobID, cfg.Workload.CatchUpTimeout.Duration, func(j incrementalJob) bool { return j.CaughtUp && j.Status == "running" })
		if lag > maxLag {
			maxLag = lag
		}
		if err != nil {
			return jobResult, workloads, maxLag, err
		}
		if burst.Errors > 0 && burst.FailureClass != failureCapacity {
			_ = recordScenarioFailure(cfg, state, burstScenario, burst.FailureClass)
			return jobResult, workloads, maxLag, workloadFailure(burst)
		}
		if err = completeScenario(cfg, state, burstScenario); err != nil {
			return jobResult, workloads, maxLag, err
		}
	}

	if *cfg.Workload.EnablePauseResume && !scenarioCompleted(*state, pauseScenario) {
		if err = startScenario(cfg, state, pauseScenario, "pausing"); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if err = client.action(ctx, jobID, "pause", nil); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if _, _, err = waitForJob(ctx, client, jobID, 2*time.Minute, func(j incrementalJob) bool { return j.Status == "paused_manual" }); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if err = updateScenarioPhase(cfg, state, pauseScenario, "writing"); err != nil {
			return jobResult, workloads, maxLag, err
		}
		pausedExecution := engine.runStageWithOptions(ctx, mode+"/writes-while-paused", cfg.Workload.Steps[0], cfg.Workload.PauseWriteDuration.Duration, stageOptions{DeferLatency: true})
		paused := pausedExecution.Result
		workloads = append(workloads, paused)
		if paused.Errors > 0 && paused.FailureClass != failureCapacity {
			if err = recordWorkload(cfg, state, paused); err != nil {
				return jobResult, workloads, maxLag, err
			}
			_ = recordScenarioFailure(cfg, state, pauseScenario, paused.FailureClass)
			if resumeErr := resumeJob(ctx, cfg, state, client, jobID); resumeErr != nil {
				return jobResult, workloads, maxLag, fmt.Errorf("%w; additionally failed to resume paused CDC job: %v", workloadFailure(paused), resumeErr)
			}
			if _, _, catchUpErr := waitForJob(ctx, client, jobID, cfg.Workload.CatchUpTimeout.Duration, func(j incrementalJob) bool { return j.CaughtUp && j.Status == "running" }); catchUpErr != nil {
				return jobResult, workloads, maxLag, fmt.Errorf("%w; additionally failed waiting for CDC catch-up: %v", workloadFailure(paused), catchUpErr)
			}
			return jobResult, workloads, maxLag, workloadFailure(paused)
		}
		if err = recordDeferredWorkload(cfg, state, paused, pausedExecution.Markers, pauseScenario, "resuming"); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if err = resumeJob(ctx, cfg, state, client, jobID); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if _, _, err = waitForJob(ctx, client, jobID, cfg.Workload.CatchUpTimeout.Duration, func(j incrementalJob) bool { return j.CaughtUp && j.Status == "running" }); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if err = finishDeferredPauseLatency(ctx, cfg, state, engine, pauseScenario); err != nil {
			return jobResult, workloads, maxLag, err
		}
	}
	rotationScenario := mode + "/binlog-rotate"
	if *cfg.Workload.EnableBinlogRotate && !scenarioCompleted(*state, rotationScenario) {
		if err = startScenario(cfg, state, rotationScenario, "rotating"); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if _, err = dbs.mysql.ExecContext(ctx, "FLUSH BINARY LOGS"); err != nil {
			return jobResult, workloads, maxLag, fmt.Errorf("rotate binlog: %w", err)
		}
		time.Sleep(cfg.Workload.IdleDuration.Duration)
		rotation := engine.runStage(ctx, mode+"/after-binlog-rotate", cfg.Workload.Steps[0], min(cfg.Workload.StepDuration.Duration, 20*time.Second))
		workloads = append(workloads, rotation)
		if err = recordWorkload(cfg, state, rotation); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if _, _, err = waitForJob(ctx, client, jobID, cfg.Workload.CatchUpTimeout.Duration, func(j incrementalJob) bool { return j.CaughtUp && j.Status == "running" }); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if rotation.Errors > 0 && rotation.FailureClass != failureCapacity {
			_ = recordScenarioFailure(cfg, state, rotationScenario, rotation.FailureClass)
			return jobResult, workloads, maxLag, workloadFailure(rotation)
		}
		if err = completeScenario(cfg, state, rotationScenario); err != nil {
			return jobResult, workloads, maxLag, err
		}
	}
	if *cfg.Workload.EnableDDL && !scenarioCompleted(*state, "ddl") {
		if err = startScenario(cfg, state, "ddl", "waiting-for-ddl-pause"); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if err = ddlScenario(ctx, cfg, state, client, dbs, jobID); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if err = completeScenario(cfg, state, "ddl"); err != nil {
			return jobResult, workloads, maxLag, err
		}
	}
	conflictName := conflictScenarioName
	if *cfg.Workload.EnableConflict && !scenarioCompleted(*state, conflictName) {
		if err = startScenario(cfg, state, conflictName, "injecting"); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if err = conflictScenario(ctx, cfg, state, client, dbs, jobID); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if err = completeScenario(cfg, state, conflictName); err != nil {
			return jobResult, workloads, maxLag, err
		}
	}
	restartName := mode + "/application-restart"
	if manualRestart && !scenarioCompleted(*state, restartName) {
		if err = startScenario(cfg, state, restartName, "waiting-for-operator"); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if err = manualRestartScenario(ctx, cfg, state, client, jobID); err != nil {
			return jobResult, workloads, maxLag, err
		}
		if err = completeScenario(cfg, state, restartName); err != nil {
			return jobResult, workloads, maxLag, err
		}
	}
	cutoverName := mode + "/cutover"
	if !scenarioCompleted(*state, cutoverName) {
		if err = startScenario(cfg, state, cutoverName, "preparing"); err != nil {
			return jobResult, workloads, maxLag, err
		}
		current, loadErr := client.job(ctx, jobID)
		if loadErr != nil {
			return jobResult, workloads, maxLag, loadErr
		}
		if current.Status != "cutting_over" && current.Status != "ready_to_cutover" && current.Status != "ready_with_warnings" {
			if err = client.action(ctx, jobID, "prepare-cutover", nil); err != nil {
				return jobResult, workloads, maxLag, err
			}
		}
	}
	final, lag, err := waitForJob(ctx, client, jobID, cfg.Workload.CatchUpTimeout.Duration, func(j incrementalJob) bool {
		return j.Status == "ready_to_cutover" || j.Status == "ready_with_warnings"
	})
	if lag > maxLag {
		maxLag = lag
	}
	if err != nil {
		return jobResult, workloads, maxLag, err
	}
	if err = client.action(ctx, jobID, "stop", map[string]bool{"acknowledge_warnings": true, "acknowledge_exclusions": false}); err != nil {
		return jobResult, workloads, maxLag, err
	}
	final, _, err = waitForJob(ctx, client, jobID, time.Minute, func(j incrementalJob) bool { return j.Status == "stopped" })
	jobResult.Final, jobResult.FinishedAt = final, time.Now().UTC()
	if err != nil {
		return jobResult, workloads, maxLag, err
	}
	if err = completeScenario(cfg, state, cutoverName); err != nil {
		return jobResult, workloads, maxLag, err
	}
	state.ActiveJobID, state.ActiveMode = "", ""
	if !scenarioCompleted(*state, mode) {
		state.Completed = append(state.Completed, mode)
	}
	_ = saveState(cfg, state)
	return jobResult, workloads, maxLag, err
}

func waitForJob(ctx context.Context, client *apiClient, jobID string, timeout time.Duration, ready func(incrementalJob) bool) (incrementalJob, int64, error) {
	deadline := time.Now().Add(timeout)
	var last incrementalJob
	var maxLag int64
	for time.Now().Before(deadline) {
		job, err := client.job(ctx, jobID)
		if err != nil {
			return last, maxLag, err
		}
		last = job
		if job.LagSeconds > maxLag {
			maxLag = job.LagSeconds
		}
		if ready(job) {
			return job, maxLag, nil
		}
		if job.Status == "failed" || job.Status == "aborted" || job.Status == "cutover_blocked" || job.Status == "paused_bootstrap_review" {
			return job, maxLag, fmt.Errorf("job %s stopped in status=%s phase=%s: %s %s", jobID, job.Status, job.Phase, job.Summary, job.LastError)
		}
		interval := client.poll
		if interval <= 0 {
			interval = time.Second
		}
		select {
		case <-ctx.Done():
			return job, maxLag, ctx.Err()
		case <-time.After(interval):
		}
	}
	return last, maxLag, fmt.Errorf("timed out waiting for job %s; last status=%s phase=%s lag=%ds", jobID, last.Status, last.Phase, last.LagSeconds)
}

func resumeJob(ctx context.Context, cfg Config, state *RunState, client *apiClient, jobID string) error {
	timeout := client.resumeTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	loggedWaiting := false
	for time.Now().Before(deadline) {
		err := client.action(ctx, jobID, "resume", nil)
		if err == nil {
			state.RecoveryAttempts++
			if saveErr := saveState(cfg, state); saveErr != nil {
				return saveErr
			}
			log.Printf("job %s resume request accepted", jobID)
			return nil
		}
		lastErr = err
		var responseErr *apiError
		if !errors.As(err, &responseErr) || responseErr.StatusCode != 409 || !strings.Contains(responseErr.Message, "已在运行") {
			return err
		}
		job, loadErr := client.job(ctx, jobID)
		if loadErr != nil {
			return fmt.Errorf("inspect job after resume conflict: %w", loadErr)
		}
		switch job.Status {
		case "initializing", "running", "catching_up":
			state.RecoveryAttempts++
			if saveErr := saveState(cfg, state); saveErr != nil {
				return saveErr
			}
			log.Printf("job %s resume was already handled by the server; status=%s", jobID, job.Status)
			return nil
		case "pausing", "paused_manual", "paused_restart", "failed", "paused_row_conflict":
			if !loggedWaiting {
				log.Printf("job %s is still releasing its pause registration; waiting before retrying resume", jobID)
				loggedWaiting = true
			}
		default:
			return err
		}
		interval := client.poll
		if interval <= 0 {
			interval = time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("timed out retrying resume for job %s: %w", jobID, lastErr)
}

func transactionBoundaryScenario(ctx context.Context, engine *workloadEngine) error {
	var tables []TableSpec
	for _, table := range engine.state.Tables {
		if table.Kind == kindPrimary || table.Kind == kindWide {
			tables = append(tables, table)
			if len(tables) == 2 {
				break
			}
		}
	}
	if len(tables) < 2 {
		return nil
	}
	tx, err := engine.source.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	seq := engine.seq.Add(1)
	for _, table := range tables {
		query, args := mysqlInsertBatch(table, engine.state.RunID, seq, seq)
		if _, err = tx.ExecContext(ctx, query, args...); err != nil {
			tx.Rollback()
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO "+mysqlIdent("cdcstress_noise_outside_scope")+"(id,payload) VALUES (?,?) ON DUPLICATE KEY UPDATE payload=VALUES(payload)", seq, "same-database-noise"); err != nil {
		tx.Rollback()
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO "+mysqlIdent(engine.cfg.MySQL.Database+"_noise")+"."+mysqlIdent("cdcstress_cross_database_noise")+"(id,payload) VALUES (?,?) ON DUPLICATE KEY UPDATE payload=VALUES(payload)", seq, "cross-database-noise"); err != nil {
		tx.Rollback()
		return err
	}
	select {
	case <-ctx.Done():
		tx.Rollback()
		return ctx.Err()
	case <-time.After(min(engine.cfg.Workload.IdleDuration.Duration, 5*time.Second)):
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, table := range tables {
		_ = engine.ledger.write(LedgerEntry{Sequence: seq, Transaction: fmt.Sprintf("cross-%d", seq), Table: table.Name, Action: "insert", RowID: seq, Committed: true, CommittedAt: time.Now().UTC()})
	}
	tx, err = engine.source.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollbackID := engine.seq.Add(1)
	query, args := mysqlInsertBatch(tables[0], engine.state.RunID, rollbackID, rollbackID)
	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
		tx.Rollback()
		return err
	}
	if err = tx.Rollback(); err != nil {
		return err
	}
	if err = engine.ledger.write(LedgerEntry{Sequence: rollbackID, Transaction: fmt.Sprintf("rollback-%d", rollbackID), Table: tables[0].Name, Action: "insert", RowID: rollbackID, Committed: false}); err != nil {
		return err
	}
	for _, kind := range []TableKind{kindPrimary, kindComposite, kindUnique} {
		var table TableSpec
		for _, candidate := range engine.state.Tables {
			if candidate.Kind == kind {
				table = candidate
				break
			}
		}
		if table.Name == "" {
			continue
		}
		locatorID := engine.seq.Add(1)
		tx, err = engine.source.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		query, args = mysqlInsertBatch(table, engine.state.RunID, locatorID, locatorID)
		if _, err = tx.ExecContext(ctx, query, args...); err != nil {
			tx.Rollback()
			return err
		}
		newID := locatorID
		switch kind {
		case kindPrimary:
			newID = locatorID + 1_000_000_000
			_, err = tx.ExecContext(ctx, "UPDATE "+mysqlIdent(table.Name)+" SET id=?,event_id=? WHERE id=?", newID, locatorID, locatorID)
		case kindComposite:
			oldTenant := int(locatorID%31 + 1)
			_, err = tx.ExecContext(ctx, "UPDATE "+mysqlIdent(table.Name)+" SET tenant_id=?,event_id=? WHERE tenant_id=? AND id=?", oldTenant+100, locatorID, oldTenant, locatorID)
		case kindUnique:
			_, err = tx.ExecContext(ctx, "UPDATE "+mysqlIdent(table.Name)+" SET code=?,event_id=? WHERE id=?", fmt.Sprintf("locator-changed-%d", locatorID), locatorID, locatorID)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		if err = engine.ledger.write(LedgerEntry{Sequence: locatorID, Transaction: fmt.Sprintf("locator-%d", locatorID), Table: table.Name, Action: "locator_update", RowID: newID, Committed: true, CommittedAt: time.Now().UTC()}); err != nil {
			return err
		}
	}
	return nil
}

func ddlScenario(ctx context.Context, cfg Config, state *RunState, client *apiClient, dbs *databases, jobID string) error {
	var table TableSpec
	for _, candidate := range state.Tables {
		if candidate.Kind == kindPrimary {
			table = candidate
			break
		}
	}
	if table.Name == "" {
		return nil
	}
	if scenarioColumnExists(ctx, dbs.mysql, cfg, table, false, "cdc_extra") && scenarioColumnExists(ctx, dbs.gauss, cfg, table, true, "cdc_extra") {
		return nil
	}
	if !scenarioColumnExists(ctx, dbs.mysql, cfg, table, false, "cdc_extra") {
		if _, err := dbs.mysql.ExecContext(ctx, "ALTER TABLE "+mysqlIdent(table.Name)+" ADD COLUMN cdc_extra BIGINT NULL"); err != nil {
			return fmt.Errorf("source DDL: %w", err)
		}
	}
	if _, _, err := waitForJob(ctx, client, jobID, 2*time.Minute, func(j incrementalJob) bool { return j.Status == "paused_ddl" }); err != nil {
		return err
	}
	targetName := table.Name
	if cfg.Profile.LowerCaseNames {
		targetName = strings.ToLower(targetName)
	}
	if !scenarioColumnExists(ctx, dbs.gauss, cfg, table, true, "cdc_extra") {
		if _, err := dbs.gauss.ExecContext(ctx, "ALTER TABLE "+pgIdent(cfg.GaussDB.Schema)+"."+pgIdent(targetName)+" ADD COLUMN cdc_extra BIGINT NULL"); err != nil {
			return fmt.Errorf("target DDL: %w", err)
		}
	}
	if err := client.action(ctx, jobID, "ack-ddl", nil); err != nil {
		return err
	}
	_, _, err := waitForJob(ctx, client, jobID, cfg.Workload.CatchUpTimeout.Duration, func(j incrementalJob) bool { return j.CaughtUp && j.Status == "running" })
	return err
}

func finishPendingDDL(ctx context.Context, cfg Config, state *RunState, client *apiClient, dbs *databases, jobID string) error {
	var table TableSpec
	for _, candidate := range state.Tables {
		if candidate.Kind == kindPrimary {
			table = candidate
			break
		}
	}
	if table.Name == "" {
		return errors.New("cannot identify stress DDL table")
	}
	name := table.Name
	if cfg.Profile.LowerCaseNames {
		name = strings.ToLower(name)
	}
	if !scenarioColumnExists(ctx, dbs.gauss, cfg, table, true, "cdc_extra") {
		if _, err := dbs.gauss.ExecContext(ctx, "ALTER TABLE "+pgIdent(cfg.GaussDB.Schema)+"."+pgIdent(name)+" ADD COLUMN cdc_extra BIGINT NULL"); err != nil {
			return err
		}
	}
	if err := client.action(ctx, jobID, "ack-ddl", nil); err != nil {
		return err
	}
	if !scenarioCompleted(*state, "ddl") {
		state.Completed = append(state.Completed, "ddl")
	}
	return saveState(cfg, state)
}

func conflictScenario(ctx context.Context, cfg Config, state *RunState, client *apiClient, dbs *databases, jobID string) error {
	var table TableSpec
	for _, candidate := range state.Tables {
		if candidate.Kind == kindKeyless && candidate.Rows > 0 {
			table = candidate
			break
		}
	}
	if table.Name == "" {
		return nil
	}
	conflictID := time.Now().UnixNano()
	query, args := mysqlInsertBatch(table, state.RunID, conflictID, conflictID)
	if _, err := dbs.mysql.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("seed conflict row: %w", err)
	}
	if _, _, err := waitForJob(ctx, client, jobID, cfg.Workload.CatchUpTimeout.Duration, func(j incrementalJob) bool { return j.CaughtUp && j.Status == "running" }); err != nil {
		return err
	}
	columns := commonColumnNames(table)
	values := make([]any, len(columns))
	pointers := make([]any, len(values))
	for i := range values {
		pointers[i] = &values[i]
	}
	quotedMySQL := quoteColumns(columns, mysqlIdent)
	if err := dbs.mysql.QueryRowContext(ctx, "SELECT "+strings.Join(quotedMySQL, ",")+" FROM "+mysqlIdent(table.Name)+" WHERE id=? LIMIT 1", conflictID).Scan(pointers...); err != nil {
		return err
	}
	values = mysqlRowForTarget(columns, values)
	state.PendingConflict = encodeConflictRepair(table.Name, columns, values)
	if err := saveState(cfg, state); err != nil {
		return err
	}
	targetName := table.Name
	if cfg.Profile.LowerCaseNames {
		targetName = strings.ToLower(targetName)
	}
	qualified := pgIdent(cfg.GaussDB.Schema) + "." + pgIdent(targetName)
	if _, err := dbs.gauss.ExecContext(ctx, "DELETE FROM "+qualified+" WHERE id=$1", conflictID); err != nil {
		return err
	}
	if _, err := dbs.mysql.ExecContext(ctx, "UPDATE "+mysqlIdent(table.Name)+" SET payload=?,event_id=? WHERE id=? LIMIT 1", "forced-conflict", time.Now().UnixNano(), conflictID); err != nil {
		return err
	}
	if _, _, err := waitForJob(ctx, client, jobID, 2*time.Minute, func(j incrementalJob) bool { return j.Status == "paused_row_conflict" }); err != nil {
		return err
	}
	if err := repairPendingConflict(ctx, cfg, state, client, dbs, jobID); err != nil {
		return err
	}
	_, _, err := waitForJob(ctx, client, jobID, cfg.Workload.CatchUpTimeout.Duration, func(j incrementalJob) bool { return j.CaughtUp && j.Status == "running" })
	return err
}

func encodeConflictRepair(table string, columns []string, values []any) *ConflictRepair {
	repair := &ConflictRepair{Table: table, Columns: append([]string(nil), columns...), Values: make([]StoredValue, len(values))}
	for i, value := range values {
		switch typed := value.(type) {
		case nil:
			repair.Values[i] = StoredValue{Kind: "null"}
		case []byte:
			repair.Values[i] = StoredValue{Kind: "bytes", Value: base64.StdEncoding.EncodeToString(typed)}
		case time.Time:
			repair.Values[i] = StoredValue{Kind: "time", Value: typed.Format(time.RFC3339Nano)}
		default:
			repair.Values[i] = StoredValue{Kind: "text", Value: fmt.Sprint(value)}
		}
	}
	return repair
}

func decodeConflictRepair(repair *ConflictRepair) ([]any, error) {
	values := make([]any, len(repair.Values))
	for i, stored := range repair.Values {
		switch stored.Kind {
		case "null":
			values[i] = nil
		case "bytes":
			decoded, err := base64.StdEncoding.DecodeString(stored.Value)
			if err != nil {
				return nil, err
			}
			values[i] = decoded
		case "time":
			parsed, err := time.Parse(time.RFC3339Nano, stored.Value)
			if err != nil {
				return nil, err
			}
			values[i] = parsed
		case "text":
			values[i] = stored.Value
		default:
			return nil, fmt.Errorf("unknown stored repair value kind %q", stored.Kind)
		}
	}
	return values, nil
}

func repairPendingConflict(ctx context.Context, cfg Config, state *RunState, client *apiClient, dbs *databases, jobID string) error {
	if state.PendingConflict == nil {
		return errors.New("row conflict has no persisted repair payload; repair the target row manually before resuming")
	}
	repair := state.PendingConflict
	values, err := decodeConflictRepair(repair)
	if err != nil {
		return err
	}
	name := repair.Table
	if cfg.Profile.LowerCaseNames {
		name = strings.ToLower(name)
	}
	qualified := pgIdent(cfg.GaussDB.Schema) + "." + pgIdent(name)
	where := make([]string, len(repair.Columns))
	for i, column := range repair.Columns {
		where[i] = pgIdent(column) + fmt.Sprintf(" IS NOT DISTINCT FROM $%d", i+1)
	}
	if _, err = dbs.gauss.ExecContext(ctx, "DELETE FROM "+qualified+" WHERE "+strings.Join(where, " AND "), values...); err != nil {
		return fmt.Errorf("remove stale repair row: %w", err)
	}
	marks := make([]string, len(values))
	for i := range marks {
		marks[i] = fmt.Sprintf("$%d", i+1)
	}
	if _, err = dbs.gauss.ExecContext(ctx, "INSERT INTO "+qualified+" ("+strings.Join(quoteColumns(repair.Columns, pgIdent), ",")+") VALUES ("+strings.Join(marks, ",")+")", values...); err != nil {
		return fmt.Errorf("repair conflict row: %w", err)
	}
	if err = resumeJob(ctx, cfg, state, client, jobID); err != nil {
		return err
	}
	state.PendingConflict = nil
	return saveState(cfg, state)
}

func manualRestartScenario(ctx context.Context, cfg Config, state *RunState, client *apiClient, jobID string) error {
	fmt.Printf("\nManual restart checkpoint for job %s. Restart dbgold safely, wait for readiness, then press Enter...\n", jobID)
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	job, _, err := waitForJob(ctx, client, jobID, 10*time.Minute, func(j incrementalJob) bool { return j.Status == "paused_restart" || j.Status == "paused_manual" })
	if err != nil {
		return err
	}
	log.Printf("dbgold returned with task status %s; resuming", job.Status)
	if err = resumeJob(ctx, cfg, state, client, jobID); err != nil {
		return err
	}
	_, _, err = waitForJob(ctx, client, jobID, cfg.Workload.CatchUpTimeout.Duration, func(j incrementalJob) bool { return j.CaughtUp && j.Status == "running" })
	return err
}

func commonColumnNames(table TableSpec) []string {
	columns := []string{"id", "tenant_id", "code", "event_id", "payload", "amount", "score", "active", "created_at", "note", "blob_data"}
	if table.Kind == kindWide {
		columns = append(columns, "extra_int", "extra_text")
	}
	if table.Kind == kindMixedCase {
		columns = append(columns, "order")
	}
	return columns
}

func quoteColumns(columns []string, quote func(string) string) []string {
	out := make([]string, len(columns))
	for i, column := range columns {
		out[i] = quote(column)
	}
	return out
}

func mysqlRowForTarget(columns []string, values []any) []any {
	out := append([]any(nil), values...)
	for i, value := range out {
		bytes, ok := value.([]byte)
		if !ok || columns[i] == "blob_data" {
			continue
		}
		switch columns[i] {
		case "id", "tenant_id", "event_id", "active", "extra_int", "cdc_extra":
			var parsed int64
			if _, err := fmt.Sscan(string(bytes), &parsed); err == nil {
				out[i] = parsed
			} else {
				out[i] = string(bytes)
			}
		default:
			out[i] = string(bytes)
		}
	}
	return out
}
func scenarioCompleted(state RunState, name string) bool {
	for _, item := range state.Completed {
		if item == name {
			return true
		}
	}
	return false
}

func hasCompletedSteadyScenario(state RunState, mode string, startTPS int) bool {
	for _, item := range state.Completed {
		if !strings.HasPrefix(item, mode+"/steady-") || !strings.HasSuffix(item, "tps") {
			continue
		}
		value := strings.TrimSuffix(strings.TrimPrefix(item, mode+"/steady-"), "tps")
		tps, err := strconv.Atoi(value)
		if err == nil && tps >= startTPS {
			return true
		}
	}
	return false
}

func workloadFailure(result WorkloadResult) error {
	class := result.FailureClass
	if class == "" {
		class = failureCorrectness
	}
	detail := ""
	if len(result.ErrorSamples) > 0 {
		detail = ": " + result.ErrorSamples[0]
	}
	return fmt.Errorf("workload %s failed (%s), committed=%d errors=%d%s", result.Name, class, result.Committed, result.Errors, detail)
}

func finishDeferredPauseLatency(ctx context.Context, cfg Config, state *RunState, engine *workloadEngine, scenario string) error {
	return finishDeferredPauseLatencyWithTimeout(ctx, cfg, state, engine, scenario, deferredLatencyVisibilityTimeout)
}

func finishDeferredPauseLatencyWithTimeout(ctx context.Context, cfg Config, state *RunState, engine *workloadEngine, scenario string, timeout time.Duration) error {
	latencies, err := engine.measureDeferredLatencies(ctx, state.PendingLatencyMarkers, timeout)
	if err != nil {
		_ = recordScenarioFailure(cfg, state, scenario, failureCorrectness)
		return fmt.Errorf("measure deferred pause latency after CDC catch-up: %w", err)
	}
	return completeDeferredWorkload(cfg, state, scenario, latencies)
}

func workloadByName(state RunState, name string) (WorkloadResult, bool) {
	for _, result := range state.Workloads {
		if result.Name == name {
			return result, true
		}
	}
	return WorkloadResult{}, false
}
