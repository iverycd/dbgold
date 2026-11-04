package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeUpgradeFixtureFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o700))
}

func makeUpgradePackage(t *testing.T, root, version string) {
	t.Helper()
	program := portableProgramFiles()
	for _, name := range []string{program[0], program[1], "web/index.html", "runtime/bin/java", "lib/oscar.jar", "VERSION"} {
		writeUpgradeFixtureFile(t, root, name, version)
	}
	writeUpgradeFixtureFile(t, root, "release-manifest.json",
		fmt.Sprintf(`{"edition":"portable","os":"%s","architecture":"%s","version":"%s"}`, runtime.GOOS, runtime.GOARCH, version))
	var hashes bytes.Buffer
	require.NoError(t, fs.WalkDir(os.DirFS(root), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(filepath.Join(root, name))
		if err == nil {
			fmt.Fprintf(&hashes, "%x  %s\n", sha256.Sum256(content), name)
		}
		return err
	}))
	writeUpgradeFixtureFile(t, root, "manifest.sha256", hashes.String())
}

func setupUpgradeTest(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	source, target := filepath.Join(base, "新版 portable"), filepath.Join(base, "moved old")
	makeUpgradePackage(t, source, "new")
	makeUpgradePackage(t, target, "old")
	for _, name := range []string{"data/dbgold.db", "data/dbgold.db-wal", "config/dbgold.env", "uploads/nested/test.db", "logs/old.log", "tmp/junk", "backups/prior/file"} {
		writeUpgradeFixtureFile(t, target, name, "original")
	}
	return source, target
}

func requireUpgradeFile(t *testing.T, root, name, content string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	require.Equal(t, content, string(raw), name)
}

func TestPortableUpgrade(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("probeFailure=%v", fail), func(t *testing.T) {
			source, target := setupUpgradeTest(t)
			var backup string
			err := upgradePortable(source, target, func(root string, state upgradeState) error {
				backup = filepath.Join(root, "backups", state.Backup)
				requireUpgradeFile(t, root, "web/index.html", "new")
				requireUpgradeFile(t, backup, "web/index.html", "old")
				requireUpgradeFile(t, backup, "data/dbgold.db-wal", "original")
				requireUpgradeFile(t, backup, "uploads/nested/test.db", "original")
				require.NoDirExists(t, filepath.Join(backup, "backups"))
				require.NoDirExists(t, filepath.Join(backup, "tmp"))
				_, err := lockPortableRoot(root)
				require.Error(t, err)
				writeUpgradeFixtureFile(t, root, "data/dbgold.db", "migration")
				writeUpgradeFixtureFile(t, root, "data/new.wal", "new")
				if fail {
					return os.ErrInvalid
				}
				return nil
			})
			require.NotEmpty(t, backup)
			require.NoFileExists(t, filepath.Join(target, upgradeMarker))
			requireUpgradeFile(t, target, "config/dbgold.env", "original")
			requireUpgradeFile(t, target, "uploads/nested/test.db", "original")
			requireUpgradeFile(t, target, "backups/prior/file", "original")
			if fail {
				require.ErrorContains(t, err, "previous program and data restored")
				requireUpgradeFile(t, target, "VERSION", "old")
				requireUpgradeFile(t, target, "lib/oscar.jar", "old")
				requireUpgradeFile(t, target, "data/dbgold.db", "original")
				require.NoFileExists(t, filepath.Join(target, "data/new.wal"))
			} else {
				require.NoError(t, err)
				requireUpgradeFile(t, target, "VERSION", "new")
				requireUpgradeFile(t, target, "data/dbgold.db", "migration")
			}
		})
	}
}

func TestPortableUpgradeRejectsBusyOrBrokenPackage(t *testing.T) {
	source, target := setupUpgradeTest(t)
	release, err := lockPortableRoot(target)
	require.NoError(t, err)
	probe := func(string, upgradeState) error { t.Fatal("unexpected probe"); return nil }
	require.ErrorContains(t, upgradePortable(source, target, probe), "stop the old portable instance")
	release()
	writeUpgradeFixtureFile(t, source, "web/index.html", "modified")
	require.ErrorContains(t, upgradePortable(source, target, probe), "checksum mismatch")
	require.ErrorContains(t, upgradePortable(source, source, probe), "separate directory")
	require.NoFileExists(t, filepath.Join(target, upgradeMarker))
	requireUpgradeFile(t, target, "VERSION", "old")
	requireUpgradeFile(t, target, "data/dbgold.db", "original")
}

func TestPortableUpgradeRecoversInterruptedWork(t *testing.T) {
	source, target := setupUpgradeTest(t)
	snapshot, err := takePortableSnapshot(target)
	require.NoError(t, err)
	state, err := sealPortableSnapshot(snapshot)
	require.NoError(t, err)
	require.NoError(t, writeUpgradeState(target, state))
	writeUpgradeFixtureFile(t, target, "data/dbgold.db", "partial")
	require.ErrorContains(t, checkPortableUpgradeState(target), "interrupted")
	// A probe might still be exiting after its parent was interrupted.
	unlock, err := lockPortableRoot(snapshot)
	require.NoError(t, err)
	require.ErrorContains(t, upgradePortable(source, target, nil), "wait for the upgrade probe")
	requireUpgradeFile(t, target, "data/dbgold.db", "partial")
	unlock()
	require.ErrorContains(t, upgradePortable(source, target, nil), "restored interrupted upgrade")
	require.NoError(t, checkPortableUpgradeState(target))
	requireUpgradeFile(t, target, "data/dbgold.db", "original")
}

func TestPortableUpgradeRejectsDamagedRecoverySnapshot(t *testing.T) {
	for _, name := range []string{"config/dbgold.env", "data/dbgold.db", "web/index.html", "uploads/nested/test.db", "damaged-wal"} {
		t.Run(name, func(t *testing.T) {
			source, target := setupUpgradeTest(t)
			snapshot, err := takePortableSnapshot(target)
			require.NoError(t, err)
			state, err := sealPortableSnapshot(snapshot)
			require.NoError(t, err)
			require.NoError(t, writeUpgradeState(target, state))
			if name == "damaged-wal" {
				// Same length as the original; size-only verification would miss this.
				writeUpgradeFixtureFile(t, snapshot, "data/dbgold.db-wal", "damaged!")
			} else {
				require.NoError(t, os.Remove(filepath.Join(snapshot, name)))
			}
			writeUpgradeFixtureFile(t, target, "data/dbgold.db", "live")
			writeUpgradeFixtureFile(t, target, "VERSION", "partial")
			require.ErrorContains(t, upgradePortable(source, target, nil), "cold backup is incomplete")
			requireUpgradeFile(t, target, "data/dbgold.db", "live")
			requireUpgradeFile(t, target, "VERSION", "partial")
			require.FileExists(t, filepath.Join(target, upgradeMarker))
		})
	}
}

func TestPortableUpgradeRecoveryChecksSnapshotPlatform(t *testing.T) {
	source, target := setupUpgradeTest(t)
	snapshot, err := takePortableSnapshot(target)
	require.NoError(t, err)
	writeUpgradeFixtureFile(t, snapshot, "release-manifest.json", `{"edition":"portable","os":"other","architecture":"other"}`)
	state, err := sealPortableSnapshot(snapshot)
	require.NoError(t, err)
	require.NoError(t, writeUpgradeState(target, state))
	writeUpgradeFixtureFile(t, target, "VERSION", "partial")
	require.ErrorContains(t, upgradePortable(source, target, nil), "incompatible cold backup")
	requireUpgradeFile(t, target, "VERSION", "partial")
	require.FileExists(t, filepath.Join(target, upgradeMarker))
}

func TestPortableUpgradeRestoresOriginallyAbsentDatabase(t *testing.T) {
	source, target := setupUpgradeTest(t)
	require.NoError(t, os.RemoveAll(filepath.Join(target, "data")))
	require.ErrorContains(t, upgradePortable(source, target, func(root string, _ upgradeState) error {
		writeUpgradeFixtureFile(t, root, "data/dbgold.db", "partial")
		return os.ErrInvalid
	}), "previous program and data restored")
	require.NoDirExists(t, filepath.Join(target, "data"))
	requireUpgradeFile(t, target, "config/dbgold.env", "original")
}
