package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultNamespace = "dbgold_cdc_stress"
	objectPrefix     = "cs_"
)

type Config struct {
	API       APIConfig      `json:"api"`
	MySQL     DatabaseConfig `json:"mysql"`
	GaussDB   DatabaseConfig `json:"gaussdb"`
	Profile   ProfileConfig  `json:"profile"`
	Workload  WorkloadConfig `json:"workload"`
	Pool      *PoolConfig    `json:"database_pool,omitempty"`
	OutputDir string         `json:"output_dir"`
}

type PoolConfig struct {
	MaxOpenConns    int      `json:"max_open_conns,omitempty"`
	MaxIdleConns    int      `json:"max_idle_conns,omitempty"`
	ConnMaxLifetime Duration `json:"conn_max_lifetime,omitempty"`
	ConnMaxIdleTime Duration `json:"conn_max_idle_time,omitempty"`
}

type resolvedPoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

type APIConfig struct {
	BaseURL            string `json:"base_url"`
	SourceConnectionID uint   `json:"source_connection_id,omitempty"`
	SourceConnection   string `json:"source_connection_name,omitempty"`
	TargetConnectionID uint   `json:"target_connection_id,omitempty"`
	TargetConnection   string `json:"target_connection_name,omitempty"`
}

type DatabaseConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Database string `json:"database"`
	Schema   string `json:"schema,omitempty"`
	SSLMode  string `json:"sslmode,omitempty"`
}

type ProfileConfig struct {
	Name            string `json:"name,omitempty"`
	TableCount      int    `json:"table_count"`
	TotalRows       int64  `json:"total_rows"`
	MaxRowsPerTable int64  `json:"max_rows_per_table,omitempty"`
	BatchSize       int    `json:"batch_size,omitempty"`
	Workers         int    `json:"workers,omitempty"`
	Seed            int64  `json:"seed,omitempty"`
	LowerCaseNames  bool   `json:"lower_case_names,omitempty"`
}

type WorkloadConfig struct {
	Steps              []int    `json:"tps_steps,omitempty"`
	StepDuration       Duration `json:"step_duration,omitempty"`
	PauseWriteDuration Duration `json:"pause_write_duration,omitempty"`
	IdleDuration       Duration `json:"idle_duration,omitempty"`
	CatchUpTimeout     Duration `json:"catch_up_timeout,omitempty"`
	PollInterval       Duration `json:"poll_interval,omitempty"`
	Workers            int      `json:"workers,omitempty"`
	EnablePauseResume  *bool    `json:"enable_pause_resume,omitempty"`
	EnableDDL          *bool    `json:"enable_ddl,omitempty"`
	EnableConflict     *bool    `json:"enable_conflict,omitempty"`
	EnableBinlogRotate *bool    `json:"enable_binlog_rotate,omitempty"`
}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	v, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err = json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err = cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	c.API.BaseURL = strings.TrimRight(c.API.BaseURL, "/")
	if c.MySQL.Database == "" {
		c.MySQL.Database = defaultNamespace
	}
	if c.GaussDB.Schema == "" {
		c.GaussDB.Schema = defaultNamespace
	}
	if c.GaussDB.SSLMode == "" {
		c.GaussDB.SSLMode = "disable"
	}
	if c.Profile.Name == "" {
		c.Profile.Name = "small"
	}
	if c.Profile.TableCount == 0 && c.Profile.TotalRows == 0 {
		switch c.Profile.Name {
		case "small":
			c.Profile.TableCount, c.Profile.TotalRows = 100, 1_000_000
		case "medium":
			c.Profile.TableCount, c.Profile.TotalRows = 1000, 10_000_000
		case "large":
			c.Profile.TableCount, c.Profile.TotalRows = 3000, 30_000_000
		}
	}
	if c.Profile.BatchSize == 0 {
		c.Profile.BatchSize = 500
	}
	if c.Profile.Workers == 0 {
		c.Profile.Workers = 4
	}
	if c.Profile.Seed == 0 {
		c.Profile.Seed = 20260722
	}
	if len(c.Workload.Steps) == 0 {
		c.Workload.Steps = []int{50, 100, 250, 500, 1000}
	}
	if c.Workload.StepDuration.Duration == 0 {
		c.Workload.StepDuration.Duration = 2 * time.Minute
	}
	if c.Workload.PauseWriteDuration.Duration == 0 {
		c.Workload.PauseWriteDuration.Duration = 10 * time.Second
	}
	if c.Workload.IdleDuration.Duration == 0 {
		c.Workload.IdleDuration.Duration = 15 * time.Second
	}
	if c.Workload.CatchUpTimeout.Duration == 0 {
		c.Workload.CatchUpTimeout.Duration = 30 * time.Minute
	}
	if c.Workload.PollInterval.Duration == 0 {
		c.Workload.PollInterval.Duration = time.Second
	}
	if c.Workload.Workers == 0 {
		c.Workload.Workers = 8
	}
	setBoolDefault(&c.Workload.EnablePauseResume, true)
	setBoolDefault(&c.Workload.EnableDDL, true)
	setBoolDefault(&c.Workload.EnableConflict, true)
	setBoolDefault(&c.Workload.EnableBinlogRotate, true)
	if c.OutputDir == "" {
		c.OutputDir = "cdcstress-results"
	}
}

func setBoolDefault(dst **bool, value bool) {
	if *dst == nil {
		v := value
		*dst = &v
	}
}

func (c Config) validate() error {
	var missing []string
	for name, value := range map[string]string{
		"api.base_url": c.API.BaseURL, "mysql.host": c.MySQL.Host, "mysql.user": c.MySQL.User,
		"gaussdb.host": c.GaussDB.Host, "gaussdb.user": c.GaussDB.User, "gaussdb.database": c.GaussDB.Database,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	if c.MySQL.Port < 1 || c.MySQL.Port > 65535 || c.GaussDB.Port < 1 || c.GaussDB.Port > 65535 {
		return errors.New("database ports must be between 1 and 65535")
	}
	if !safeNamespace(c.MySQL.Database) || !safeNamespace(c.GaussDB.Schema) {
		return fmt.Errorf("test database/schema must start with %q and contain only letters, digits, or underscores", defaultNamespace)
	}
	if c.Profile.TableCount < 1 || c.Profile.TotalRows < 0 || c.Profile.BatchSize < 1 || c.Profile.Workers < 1 {
		return errors.New("profile table_count, batch_size and workers must be positive; total_rows cannot be negative")
	}
	if c.Pool != nil {
		if c.Pool.MaxOpenConns < 0 || c.Pool.MaxIdleConns < 0 || c.Pool.ConnMaxLifetime.Duration < 0 || c.Pool.ConnMaxIdleTime.Duration < 0 {
			return errors.New("database_pool values cannot be negative")
		}
		if c.Pool.MaxOpenConns > 0 && c.Pool.MaxIdleConns > c.Pool.MaxOpenConns {
			return errors.New("database_pool.max_idle_conns cannot exceed max_open_conns")
		}
	}
	if c.Profile.Name != "small" && c.Profile.Name != "medium" && c.Profile.Name != "large" && c.Profile.Name != "custom" {
		return errors.New("profile.name must be small, medium, large, or custom")
	}
	for _, tps := range c.Workload.Steps {
		if tps < 1 {
			return errors.New("all workload TPS steps must be positive")
		}
	}
	if (c.API.SourceConnectionID == 0) == (c.API.SourceConnection == "") {
		return errors.New("set exactly one of api.source_connection_id or api.source_connection_name")
	}
	if (c.API.TargetConnectionID == 0) == (c.API.TargetConnection == "") {
		return errors.New("set exactly one of api.target_connection_id or api.target_connection_name")
	}
	return nil
}

func (c Config) resolvedPool() resolvedPoolConfig {
	workers := max(c.Profile.Workers, c.Workload.Workers)
	result := resolvedPoolConfig{
		MaxOpenConns: workers + 4, MaxIdleConns: workers + 4,
		ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
	}
	if c.Pool == nil {
		return result
	}
	if c.Pool.MaxOpenConns > 0 {
		result.MaxOpenConns = c.Pool.MaxOpenConns
	}
	if c.Pool.MaxIdleConns > 0 {
		result.MaxIdleConns = c.Pool.MaxIdleConns
	}
	if c.Pool.ConnMaxLifetime.Duration > 0 {
		result.ConnMaxLifetime = c.Pool.ConnMaxLifetime.Duration
	}
	if c.Pool.ConnMaxIdleTime.Duration > 0 {
		result.ConnMaxIdleTime = c.Pool.ConnMaxIdleTime.Duration
	}
	if result.MaxIdleConns > result.MaxOpenConns {
		result.MaxIdleConns = result.MaxOpenConns
	}
	return result
}

func safeNamespace(name string) bool {
	if name != defaultNamespace && !strings.HasPrefix(name, defaultNamespace+"_") {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func (c Config) hash() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func (c Config) resultDir(runID string) string { return filepath.Join(c.OutputDir, runID) }

func envRequired(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("required environment variable %s is not set", name)
	}
	return v, nil
}
