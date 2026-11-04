package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPortableConfigSurvivesRestartAndMove(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "dbgold 临时")
	require.NoError(t, os.Mkdir(root, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(root, "data"), 0o755))
	t.Chdir(base)
	for _, key := range []string{"TMPDIR", "TEMP", "TMP", "SQLITE_TMPDIR"} {
		t.Setenv(key, os.Getenv(key))
	}
	t.Setenv("SQLITE_PATH", filepath.Join(base, "unrelated.db"))
	t.Setenv("ADMIN_PASS", "external-password")
	t.Setenv("PORT", "22222")

	require.NoError(t, preparePortableRuntime(root))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(root, "data"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
	require.NoError(t, initPortableConfig(root))
	first, err := loadPortableConfig(root)
	require.NoError(t, err)
	require.NotEqual(t, "external-password", first.AdminPass)
	require.Equal(t, "18089", first.Port)
	require.Equal(t, filepath.Join(root, "data", "dbgold.db"), first.SQLitePath)
	require.Equal(t, "uploads", first.UploadDir)
	require.Equal(t, filepath.Join(root, "tmp"), first.OscarTempDir)
	require.Equal(t, first.OscarTempDir, os.TempDir())
	if runtime.GOOS == "windows" {
		require.Equal(t, "127.0.0.1", first.ListenHost)
	} else {
		require.Equal(t, "0.0.0.0", first.ListenHost)
	}
	require.NoError(t, os.WriteFile(filepath.Join(first.UploadDir, "报表.dmp"), []byte("upload"), 0o600))
	require.NoError(t, os.Chdir(base))
	moved := filepath.Join(base, "moved")
	require.NoError(t, os.Rename(root, moved))
	require.NoError(t, preparePortableRuntime(moved))
	require.NoError(t, initPortableConfig(moved))
	second, err := loadPortableConfig(moved)
	require.NoError(t, err)
	require.Equal(t, first.JWTSecret, second.JWTSecret)
	require.Equal(t, first.AdminPass, second.AdminPass)
	require.Equal(t, filepath.Join(moved, "web"), second.StaticDir)
	require.Equal(t, filepath.Join(moved, "logs"), second.LogDir)
	require.Equal(t, filepath.Join(moved, "lib", "oscarJDBC8.jar"), second.OscarJDBCJar)
	require.FileExists(t, filepath.Join(second.UploadDir, "报表.dmp"))
}

func TestPortableRejectsExternalDataDirectory(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	t.Chdir(root)
	if err := os.Symlink(external, filepath.Join(root, "data")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	require.Error(t, preparePortableRuntime(root))
	entries, err := os.ReadDir(external)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestPortableMissingConfigWithExistingDatabase(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"data", "config"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, name), 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "data", "dbgold.db"), []byte("old"), 0o600))
	require.ErrorContains(t, initPortableConfig(root), "从备份恢复配置")
	require.NoFileExists(t, filepath.Join(root, "config", "dbgold.env"))
}

func TestPortableRootLock(t *testing.T) {
	root := t.TempDir()
	unlock, err := lockPortableRoot(root)
	require.NoError(t, err)
	_, err = lockPortableRoot(root)
	require.Error(t, err)
	unlock()
	unlock, err = lockPortableRoot(root)
	require.NoError(t, err)
	unlock()
}
