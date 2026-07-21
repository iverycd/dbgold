package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadFromFileDefaults(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg, err := LoadFromFile("")
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0", cfg.ListenHost)
	assert.Equal(t, "18089", cfg.Port)
	assert.Equal(t, "frontend/dist", cfg.StaticDir)
}

func TestLoadFromFilePrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dbgold.env")
	require.NoError(t, os.WriteFile(path, []byte("PORT=19089\nLISTEN_HOST=127.0.0.1\nTRUSTED_PROXIES=10.0.0.1, 10.0.0.0/24\n"), 0o600))
	t.Setenv("PORT", "20089")

	cfg, err := LoadFromFile(path)
	require.NoError(t, err)
	assert.Equal(t, "20089", cfg.Port)
	assert.Equal(t, "127.0.0.1", cfg.ListenHost)
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.0/24"}, cfg.TrustedProxies)
}

func TestValidate(t *testing.T) {
	cfg := &Config{ListenHost: "0.0.0.0", Port: "18089", AppEnv: "development"}
	require.NoError(t, cfg.Validate())

	cfg.Port = "80"
	assert.ErrorContains(t, cfg.Validate(), "1024")
	cfg.Port = "18089"
	cfg.ListenHost = "::"
	assert.ErrorContains(t, cfg.Validate(), "IPv4")

	cfg.ListenHost = "0.0.0.0"
	cfg.AppEnv = "production"
	cfg.JWTSecret = "change-me-in-production"
	cfg.AdminPass = "Admin@123"
	assert.ErrorContains(t, cfg.Validate(), "JWT_SECRET")
	cfg.JWTSecret = "0123456789abcdef0123456789abcdef"
	assert.ErrorContains(t, cfg.Validate(), "ADMIN_PASS")
	cfg.AdminPass = "A-secure-initial-password"
	require.NoError(t, cfg.Validate())
}
