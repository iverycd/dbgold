package config

import (
	"os"
	"strings"
)

type Config struct {
	Port         string
	SQLitePath   string
	JWTSecret    string
	AdminUser    string
	AdminPass    string
}

func Load() *Config {
	return &Config{
		Port:       getEnv("PORT", "8080"),
		SQLitePath: getEnv("SQLITE_PATH", "dbgold.db"),
		JWTSecret:  getEnv("JWT_SECRET", "change-me-in-production"),
		AdminUser:  getEnv("ADMIN_USER", "admin"),
		AdminPass:  getEnv("ADMIN_PASS", "Admin@123"),
	}
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
