package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	AppEnv                  string
	ListenHost              string
	Port                    string
	StaticDir               string
	TrustedProxies          []string
	SQLitePath              string
	JWTSecret               string
	AdminUser               string
	AdminPass               string
	LogDir                  string
	LogLevel                string
	LogMaxFiles             int
	LogMaxTotalMB           int64  // 日志总量上限（MB），默认 2048（=2GB）
	UploadDir               string // 工单离线文件落盘目录
	MaxUploadBytes          int64  // 单个上传文件大小上限（字节）
	JWTExpireHours          int    // JWT 过期时间（小时），默认 240
	QueryTimeoutSeconds     int    // 查询中心单语句超时，默认 30 秒
	QueryMaxRows            int    // 查询中心最大返回行数，默认 1000
	QueryAuditRetentionDays int    // 查询中心审计保留天数，默认 90 天
}

func Load() *Config {
	cfg, _ := LoadFromFile("")
	return cfg
}

// LoadFromFile loads configuration without mutating the process environment.
// Real environment variables take precedence over values from the env file.
// An empty path means ".env" is optional; an explicit path must exist.
func LoadFromFile(path string) (*Config, error) {
	values := map[string]string{}
	if path == "" {
		if parsed, err := godotenv.Read(); err == nil {
			values = parsed
		}
	} else {
		parsed, err := godotenv.Read(path)
		if err != nil {
			return nil, fmt.Errorf("load config file %s: %w", path, err)
		}
		values = parsed
	}

	lookup := func(key, fallback string) string {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
		if value := strings.TrimSpace(values[key]); value != "" {
			return value
		}
		return fallback
	}
	lookupInt := func(key string, fallback int) int {
		value := lookup(key, "")
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			return n
		}
		return fallback
	}
	lookupInt64 := func(key string, fallback int64) int64 {
		value := lookup(key, "")
		if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
			return n
		}
		return fallback
	}
	trustedProxies := splitCSV(lookup("TRUSTED_PROXIES", ""))

	return &Config{
		AppEnv:                  lookup("APP_ENV", "development"),
		ListenHost:              lookup("LISTEN_HOST", "0.0.0.0"),
		Port:                    lookup("PORT", "18089"),
		StaticDir:               lookup("STATIC_DIR", "frontend/dist"),
		TrustedProxies:          trustedProxies,
		SQLitePath:              lookup("SQLITE_PATH", "dbgold.db"),
		JWTSecret:               lookup("JWT_SECRET", "change-me-in-production"),
		AdminUser:               lookup("ADMIN_USER", "admin"),
		AdminPass:               lookup("ADMIN_PASS", "Admin@123"),
		LogDir:                  lookup("LOG_DIR", "log"),
		LogLevel:                lookup("LOG_LEVEL", "info"),
		LogMaxFiles:             lookupInt("LOG_MAX_FILES", 7),
		LogMaxTotalMB:           lookupInt64("LOG_MAX_TOTAL_MB", 2048),
		UploadDir:               lookup("UPLOAD_DIR", "uploads"),
		MaxUploadBytes:          lookupInt64("MAX_UPLOAD_BYTES", 50<<30), // 默认 50GB
		JWTExpireHours:          lookupInt("JWT_EXPIRE_HOURS", 240),
		QueryTimeoutSeconds:     lookupInt("QUERY_TIMEOUT_SECONDS", 30),
		QueryMaxRows:            lookupInt("QUERY_MAX_ROWS", 1000),
		QueryAuditRetentionDays: lookupInt("QUERY_AUDIT_RETENTION_DAYS", 90),
	}, nil
}

func (c *Config) Validate() error {
	port, err := strconv.Atoi(c.Port)
	if err != nil || port < 1024 || port > 65535 {
		return fmt.Errorf("PORT must be an integer between 1024 and 65535")
	}
	ip := net.ParseIP(c.ListenHost)
	if strings.Contains(c.ListenHost, ":") || ip == nil || ip.To4() == nil {
		return fmt.Errorf("LISTEN_HOST must be an IPv4 address")
	}
	if strings.EqualFold(c.AppEnv, "production") {
		if c.JWTSecret == "change-me-in-production" || len(c.JWTSecret) < 32 {
			return fmt.Errorf("production JWT_SECRET must be changed and contain at least 32 characters")
		}
		if c.AdminPass == "Admin@123" || len(c.AdminPass) < 8 {
			return fmt.Errorf("production ADMIN_PASS must be changed and contain at least 8 characters")
		}
	}
	return nil
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}
