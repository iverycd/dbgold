package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const currentStateVersion = 3

type RunState struct {
	Version                int              `json:"state_version,omitempty"`
	RunID                  string           `json:"run_id"`
	ConfigHash             string           `json:"config_hash"`
	CreatedAt              time.Time        `json:"created_at"`
	UpdatedAt              time.Time        `json:"updated_at"`
	Prepared               bool             `json:"prepared"`
	SourceID               uint             `json:"source_connection_id,omitempty"`
	TargetID               uint             `json:"target_connection_id,omitempty"`
	JobIDs                 []string         `json:"job_ids,omitempty"`
	ActiveJobID            string           `json:"active_job_id,omitempty"`
	ActiveMode             string           `json:"active_mode,omitempty"`
	Tables                 []TableSpec      `json:"tables"`
	Completed              []string         `json:"completed_scenarios,omitempty"`
	ActiveScenario         string           `json:"active_scenario,omitempty"`
	ActivePhase            string           `json:"active_scenario_phase,omitempty"`
	Workloads              []WorkloadResult `json:"workloads,omitempty"`
	SkippedLegacy          []string         `json:"skipped_legacy_scenarios,omitempty"`
	FailureClass           string           `json:"failure_class,omitempty"`
	FailedScenario         string           `json:"failed_scenario,omitempty"`
	RecoveryAttempts       int              `json:"recovery_attempts,omitempty"`
	PendingLatencyWorkload string           `json:"pending_latency_workload,omitempty"`
	PendingLatencyMarkers  []LatencyMarker  `json:"pending_latency_markers,omitempty"`
	PendingConflict        *ConflictRepair  `json:"pending_conflict,omitempty"`
}

type ConflictRepair struct {
	Table   string        `json:"table"`
	Columns []string      `json:"columns"`
	Values  []StoredValue `json:"values"`
}

type StoredValue struct {
	Kind  string `json:"kind"`
	Value string `json:"value,omitempty"`
}

func newRunState(cfg Config, runID string) RunState {
	return RunState{Version: currentStateVersion, RunID: runID, ConfigHash: cfg.hash(), CreatedAt: time.Now(), UpdatedAt: time.Now()}
}

func statePath(cfg Config, runID string) string {
	return filepath.Join(cfg.resultDir(runID), "state.json")
}

func loadState(cfg Config, runID string) (RunState, error) {
	b, err := os.ReadFile(statePath(cfg, runID))
	if err != nil {
		return RunState{}, err
	}
	var state RunState
	if err = json.Unmarshal(b, &state); err != nil {
		return RunState{}, err
	}
	if state.RunID != runID || state.ConfigHash != cfg.hash() {
		return RunState{}, fmt.Errorf("state does not match run ID or config (state=%s config=%s)", state.ConfigHash, cfg.hash())
	}
	return state, nil
}

func saveState(cfg Config, state *RunState) error {
	state.UpdatedAt = time.Now()
	dir := cfg.resultDir(state.RunID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".state.json.tmp")
	if err = os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(cfg, state.RunID))
}

func startScenario(cfg Config, state *RunState, name, phase string) error {
	state.Version = currentStateVersion
	state.ActiveScenario, state.ActivePhase = name, phase
	state.FailureClass, state.FailedScenario = "", ""
	return saveState(cfg, state)
}

func updateScenarioPhase(cfg Config, state *RunState, name, phase string) error {
	state.Version = currentStateVersion
	state.ActiveScenario, state.ActivePhase = name, phase
	return saveState(cfg, state)
}

func completeScenario(cfg Config, state *RunState, name string) error {
	state.Version = currentStateVersion
	if !scenarioCompleted(*state, name) {
		state.Completed = append(state.Completed, name)
	}
	if state.ActiveScenario == name {
		state.ActiveScenario, state.ActivePhase = "", ""
	}
	return saveState(cfg, state)
}

func recordWorkload(cfg Config, state *RunState, result WorkloadResult) error {
	storeWorkload(state, result)
	return saveState(cfg, state)
}

func recordDeferredWorkload(cfg Config, state *RunState, result WorkloadResult, markers []LatencyMarker, scenario, phase string) error {
	storeWorkload(state, result)
	state.Version = currentStateVersion
	state.ActiveScenario, state.ActivePhase = scenario, phase
	state.PendingLatencyWorkload = result.Name
	state.PendingLatencyMarkers = append([]LatencyMarker(nil), markers...)
	return saveState(cfg, state)
}

func completeDeferredWorkload(cfg Config, state *RunState, scenario string, latencies []float64) error {
	if state.PendingLatencyWorkload != "" {
		if result, ok := workloadByName(*state, state.PendingLatencyWorkload); ok {
			result.LatencyMS = append([]float64(nil), latencies...)
			storeWorkload(state, result)
		}
	}
	state.Version = currentStateVersion
	state.PendingLatencyWorkload = ""
	state.PendingLatencyMarkers = nil
	if !scenarioCompleted(*state, scenario) {
		state.Completed = append(state.Completed, scenario)
	}
	if state.ActiveScenario == scenario {
		state.ActiveScenario, state.ActivePhase = "", ""
	}
	if state.FailedScenario == scenario {
		state.FailedScenario, state.FailureClass = "", ""
	}
	return saveState(cfg, state)
}

func storeWorkload(state *RunState, result WorkloadResult) {
	for i := range state.Workloads {
		if state.Workloads[i].Name == result.Name {
			state.Workloads[i] = result
			return
		}
	}
	state.Workloads = append(state.Workloads, result)
}

func recordScenarioFailure(cfg Config, state *RunState, name, class string) error {
	state.Version = currentStateVersion
	state.ActiveScenario, state.FailedScenario, state.FailureClass = name, name, class
	return saveState(cfg, state)
}

func applyLegacyResume(cfg Config, state *RunState, mode, resumeFrom string) error {
	if resumeFrom == "" {
		return nil
	}
	if resumeFrom != "pause-resume" {
		return fmt.Errorf("unsupported --resume-from %q; supported value: pause-resume", resumeFrom)
	}
	if state.Version >= 2 {
		return fmt.Errorf("--resume-from is only for legacy state files without scenario checkpoints")
	}
	if state.ActiveJobID == "" || state.ActiveMode == "" {
		return fmt.Errorf("--resume-from requires a legacy state with an active CDC job")
	}
	if mode != "both" && mode != state.ActiveMode {
		return fmt.Errorf("--resume-from mode mismatch: active=%s requested=%s", state.ActiveMode, mode)
	}
	prefix := state.ActiveMode + "/"
	stages := []string{prefix + "snapshot-concurrent-write", prefix + "transaction-boundaries"}
	for _, tps := range cfg.Workload.Steps {
		stages = append(stages, fmt.Sprintf("%ssteady-%dtps", prefix, tps))
	}
	stages = append(stages, prefix+"burst")
	for _, stage := range stages {
		if !scenarioCompleted(*state, stage) {
			state.Completed = append(state.Completed, stage)
		}
		state.SkippedLegacy = appendUnique(state.SkippedLegacy, stage)
	}
	state.Version = currentStateVersion
	state.ActiveScenario, state.ActivePhase = prefix+"pause-resume", "resuming"
	return saveState(cfg, state)
}

func appendUnique(items []string, value string) []string {
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}
