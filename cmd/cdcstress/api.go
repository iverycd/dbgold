package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type apiClient struct {
	base          string
	token         string
	http          *http.Client
	poll          time.Duration
	resumeTimeout time.Duration
}

type apiError struct {
	Method     string
	Path       string
	StatusCode int
	Status     string
	Message    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s returned %s: %s", e.Method, e.Path, e.Status, e.Message)
}

type apiConnection struct {
	ID       uint   `json:"id"`
	Name     string `json:"name"`
	DBType   string `json:"db_type"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
}

type apiPosition struct {
	File     string `json:"file"`
	Position uint32 `json:"position"`
	GTID     string `json:"gtid"`
}

type preflightResponse struct {
	OK              bool          `json:"ok"`
	LogBin          bool          `json:"log_bin"`
	BinlogFormat    string        `json:"binlog_format"`
	BinlogRowImage  string        `json:"binlog_row_image"`
	GTIDMode        string        `json:"gtid_mode"`
	CurrentPosition apiPosition   `json:"current_position"`
	Tables          []interface{} `json:"tables"`
	Errors          []string      `json:"errors"`
	Warnings        []string      `json:"warnings"`
}

type incrementalRequest struct {
	SourceConnectionID uint     `json:"src_conn_id"`
	TargetConnectionID uint     `json:"dst_conn_id"`
	SourceDatabase     string   `json:"src_database"`
	TargetSchema       string   `json:"target_schema"`
	StartMode          string   `json:"start_mode"`
	PositionMode       string   `json:"position_mode,omitempty"`
	StartGTID          string   `json:"start_gtid,omitempty"`
	StartFile          string   `json:"start_file,omitempty"`
	StartPosition      uint32   `json:"start_position,omitempty"`
	MigrateMode        string   `json:"migrate_mode"`
	TableFilter        string   `json:"table_filter,omitempty"`
	ExcludedTables     []string `json:"excluded_tables,omitempty"`
	LowerCaseNames     bool     `json:"lower_case_names"`
	BootstrapPolicy    string   `json:"bootstrap_failure_policy"`
	KeylessPolicy      string   `json:"keyless_change_policy"`
}

type incrementalJob struct {
	JobID              string `json:"job_id"`
	Status             string `json:"status"`
	Phase              string `json:"phase"`
	Summary            string `json:"summary"`
	LastError          string `json:"last_error"`
	BlockingDDL        string `json:"blocking_ddl"`
	ConflictTable      string `json:"conflict_table"`
	CaughtUp           bool   `json:"caught_up"`
	LagSeconds         int64  `json:"lag_seconds"`
	CheckpointFile     string `json:"checkpoint_file"`
	CheckpointPosition uint32 `json:"checkpoint_position"`
	CheckpointGTID     string `json:"checkpoint_gtid"`
	SourceHeadFile     string `json:"source_head_file"`
	SourceHeadPosition uint32 `json:"source_head_position"`
	SourceHeadGTID     string `json:"source_head_gtid"`
	InsertCount        int64  `json:"insert_count"`
	UpdateCount        int64  `json:"update_count"`
	DeleteCount        int64  `json:"delete_count"`
	WarningCount       int64  `json:"warning_count"`
	ValidationState    string `json:"validation_state"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

func newAPIClient(ctx context.Context, cfg Config) (*apiClient, error) {
	base := strings.TrimRight(cfg.API.BaseURL, "/")
	if !strings.HasSuffix(base, "/api") {
		base += "/api"
	}
	c := &apiClient{base: base, http: &http.Client{Timeout: 60 * time.Second}, poll: cfg.Workload.PollInterval.Duration, resumeTimeout: 2 * time.Minute}
	if token := os.Getenv("CDCSTRESS_DBGOLD_TOKEN"); token != "" {
		c.token = token
		return c, nil
	}
	username, err := envRequired("CDCSTRESS_DBGOLD_USERNAME")
	if err != nil {
		return nil, err
	}
	password, err := envRequired("CDCSTRESS_DBGOLD_PASSWORD")
	if err != nil {
		return nil, err
	}
	var response struct {
		Token string `json:"token"`
	}
	if err = c.call(ctx, http.MethodPost, "/auth/login", map[string]string{"username": username, "password": password}, &response, false); err != nil {
		return nil, fmt.Errorf("dbgold login: %w", err)
	}
	if response.Token == "" {
		return nil, errors.New("dbgold login returned an empty token")
	}
	c.token = response.Token
	return c, nil
}

func (c *apiClient) call(ctx context.Context, method, path string, body, output any, auth bool) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &apiErr)
		if apiErr.Error == "" {
			apiErr.Error = strings.TrimSpace(string(data))
		}
		return &apiError{Method: method, Path: path, StatusCode: resp.StatusCode, Status: resp.Status, Message: apiErr.Error}
	}
	if output != nil && len(data) > 0 {
		if err = json.Unmarshal(data, output); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}

func (c *apiClient) connections(ctx context.Context) ([]apiConnection, error) {
	var out []apiConnection
	err := c.call(ctx, http.MethodGet, "/connections", nil, &out, true)
	return out, err
}

func resolveConnections(ctx context.Context, client *apiClient, cfg Config) (apiConnection, apiConnection, error) {
	connections, err := client.connections(ctx)
	if err != nil {
		return apiConnection{}, apiConnection{}, err
	}
	resolve := func(id uint, name, dbType string) (apiConnection, error) {
		var matches []apiConnection
		for _, connection := range connections {
			if (id != 0 && connection.ID == id) || (name != "" && connection.Name == name) {
				matches = append(matches, connection)
			}
		}
		if len(matches) != 1 {
			return apiConnection{}, fmt.Errorf("expected exactly one %s connection for id=%d name=%q, found %d", dbType, id, name, len(matches))
		}
		if matches[0].DBType != dbType {
			return apiConnection{}, fmt.Errorf("connection %q has type %s, expected %s", matches[0].Name, matches[0].DBType, dbType)
		}
		return matches[0], nil
	}
	source, err := resolve(cfg.API.SourceConnectionID, cfg.API.SourceConnection, "mysql")
	if err != nil {
		return apiConnection{}, apiConnection{}, err
	}
	target, err := resolve(cfg.API.TargetConnectionID, cfg.API.TargetConnection, "gaussdb")
	return source, target, err
}

func (c *apiClient) preflight(ctx context.Context, request incrementalRequest) (preflightResponse, error) {
	var out preflightResponse
	err := c.call(ctx, http.MethodPost, "/migration/incremental/preflight", request, &out, true)
	return out, err
}

func (c *apiClient) start(ctx context.Context, request incrementalRequest) (string, preflightResponse, error) {
	var out struct {
		JobID     string            `json:"job_id"`
		Preflight preflightResponse `json:"preflight"`
	}
	err := c.call(ctx, http.MethodPost, "/migration/incremental/jobs", request, &out, true)
	return out.JobID, out.Preflight, err
}

func (c *apiClient) job(ctx context.Context, jobID string) (incrementalJob, error) {
	var out incrementalJob
	err := c.call(ctx, http.MethodGet, "/migration/incremental/jobs/"+jobID, nil, &out, true)
	return out, err
}

func (c *apiClient) action(ctx context.Context, jobID, action string, body any) error {
	return c.call(ctx, http.MethodPost, "/migration/incremental/jobs/"+jobID+"/"+action, body, nil, true)
}

func incrementalRequestFor(cfg Config, state RunState, mode string, position apiPosition) incrementalRequest {
	request := incrementalRequest{SourceConnectionID: state.SourceID, TargetConnectionID: state.TargetID,
		SourceDatabase: cfg.MySQL.Database, TargetSchema: cfg.GaussDB.Schema, StartMode: mode,
		MigrateMode: "include", TableFilter: objectPrefix + shortToken(state.RunID) + "_*", LowerCaseNames: cfg.Profile.LowerCaseNames,
		BootstrapPolicy: "fail_all", KeylessPolicy: "full_row_match"}
	if mode == "incremental_only" {
		if position.GTID != "" {
			request.PositionMode, request.StartGTID = "gtid", position.GTID
		} else {
			request.PositionMode, request.StartFile, request.StartPosition = "file", position.File, position.Position
		}
	}
	return request
}
