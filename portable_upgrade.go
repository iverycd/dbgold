// The updater runs from the freshly extracted package so Windows never has to
// replace its own running executable. The target stays locked until the probe
// (an invocation of the installed executable) has closed its database.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const upgradeMarker = ".dbgold-upgrade.json"

type upgradeState struct {
	Backup         string
	Token          string
	SnapshotSHA256 string
	// Markers from releases that installed a JRE have no KeepRuntime field.
	KeepRuntime bool `json:",omitempty"`
}

func portableProgramFiles() []string {
	binary, launcher := "dbgold", "start.sh"
	if runtime.GOOS == "windows" {
		binary, launcher = "dbgold.exe", "start.cmd"
	}
	return []string{binary, launcher, "web", "lib", "VERSION", "release-manifest.json", "manifest.sha256"}
}

func runPortableUpgradeCommand(args []string) error {
	flags := flag.NewFlagSet("portable-upgrade", flag.ContinueOnError)
	target := flags.String("target", "", "existing portable installation directory")
	confirmed := flags.Bool("confirm-no-running-tasks", false, "confirm migrations and the old portable process have stopped")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *target == "" || !*confirmed || flags.NArg() != 0 {
		return errors.New("usage: dbgold portable-upgrade --target OLD_DIRECTORY --confirm-no-running-tasks")
	}
	source, err := portableExecutableDir()
	if err != nil {
		return err
	}
	return upgradePortable(source, *target, probeInstalledPortable)
}

func upgradePortable(source, target string, probe func(string, upgradeState) error) error {
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{{source, target}, {target, source}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err == nil && filepath.IsLocal(rel) {
			return errors.New("extract the new package into a separate directory outside the existing installation")
		}
	}
	unlock, err := lockPortableRoot(target)
	if err != nil {
		return fmt.Errorf("stop the old portable instance before upgrading: %w", err)
	}
	defer unlock()
	state, err := readUpgradeState(target)
	if err == nil {
		if err := restorePortable(target, state); err != nil {
			return fmt.Errorf("recover interrupted upgrade: %w", err)
		}
		return fmt.Errorf("restored interrupted upgrade from %s; rerun the upgrade when ready", filepath.Join(target, "backups", state.Backup))
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := checkPortableEdition(target); err != nil {
		return fmt.Errorf("invalid target portable package: %w", err)
	}
	if info, err := os.Lstat(filepath.Join(target, "config", "dbgold.env")); err != nil || !info.Mode().IsRegular() {
		return errors.New("target must be an initialized portable installation with config/dbgold.env")
	}
	backups := filepath.Join(target, "backups")
	if err := os.Mkdir(backups, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	if info, err := os.Lstat(backups); err != nil || !info.IsDir() {
		return fmt.Errorf("expected an ordinary backup directory at %s", backups)
	}
	stage, err := os.MkdirTemp(backups, ".candidate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	program := portableProgramFiles()
	for _, name := range program {
		if err := copyUpgradeTree(filepath.Join(source, name), filepath.Join(stage, name)); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
	}
	if err := verifyPortablePackage(stage); err != nil {
		return fmt.Errorf("invalid new portable package: %w", err)
	}
	snapshot, err := takePortableSnapshot(target)
	if err != nil {
		return fmt.Errorf("cold backup: %w", err)
	}
	state, err = sealPortableSnapshot(snapshot)
	if err != nil {
		_ = os.RemoveAll(snapshot)
		return fmt.Errorf("verify cold backup: %w", err)
	}
	if err := writeUpgradeState(target, state); err != nil {
		return err
	}
	fmt.Printf("Cold backup: %s\n", snapshot)
	install := func() error {
		for _, name := range program {
			dest := filepath.Join(target, name)
			if err := os.RemoveAll(dest); err != nil {
				return fmt.Errorf("replace %s: %w", name, err)
			}
			if err := os.Rename(filepath.Join(stage, name), dest); err != nil {
				return fmt.Errorf("install %s: %w", name, err)
			}
		}
		return probe(target, state)
	}
	if err := install(); err != nil {
		if restoreErr := restorePortable(target, state); restoreErr != nil {
			return fmt.Errorf("upgrade failed (%v); rollback failed, cold backup retained at %s: %w", err, snapshot, restoreErr)
		}
		return fmt.Errorf("upgrade failed; previous program and data restored from %s: %w", snapshot, err)
	}
	if err := os.Remove(filepath.Join(target, upgradeMarker)); err != nil {
		return err
	}
	fmt.Printf("Upgrade verified. Start dbgold from %s.\n", target)
	return nil
}

func checkPortableEdition(root string) error {
	data, err := os.ReadFile(filepath.Join(root, "release-manifest.json"))
	if err != nil {
		return err
	}
	var pkg struct{ Edition, OS, Architecture string }
	if err := json.Unmarshal(data, &pkg); err != nil {
		return err
	}
	if pkg.Edition != "portable" || pkg.OS != runtime.GOOS || pkg.Architecture != runtime.GOARCH {
		return fmt.Errorf("expected a portable package for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

// The updater stages only program files. Verify that every staged file is in
// the published manifest and that no listed file is missing.
func verifyPortablePackage(stage string) error {
	if err := checkPortableEdition(stage); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(stage, "manifest.sha256"))
	if err != nil {
		return err
	}
	hashes := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		hash, path, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "  ")
		_, err := hex.DecodeString(hash)
		if !ok || err != nil || len(hash) != sha256.Size*2 || !fs.ValidPath(path) || hashes[path] != "" {
			return fmt.Errorf("invalid checksum entry %q", line)
		}
		hashes[path] = hash
	}
	err = fs.WalkDir(os.DirFS(stage), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || name == "manifest.sha256" {
			return err
		}
		file, err := os.Open(filepath.Join(stage, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		defer file.Close()
		hash := sha256.New()
		if _, err = io.Copy(hash, file); err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != hashes[name] {
			return fmt.Errorf("checksum mismatch: %s", name)
		}
		delete(hashes, name)
		return nil
	})
	if err == nil && len(hashes) > 0 {
		return errors.New("checksum manifest lists a file missing from the package")
	}
	return err
}

func takePortableSnapshot(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	dest, err := os.MkdirTemp(filepath.Join(root, "backups"), "upgrade-"+time.Now().UTC().Format("20060102T150405Z")+"-")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		name := entry.Name()
		// On Windows even a differently cased "Backups" is the directory
		// containing this snapshot. Never copy the snapshot into itself.
		switch strings.ToLower(name) {
		case "backups", "tmp", ".dbgold.lock", upgradeMarker:
			continue
		case "runtime":
			bundled, err := snapshotBundledRuntime(root, name)
			if err != nil {
				os.RemoveAll(dest)
				return "", err
			}
			if !bundled {
				continue
			}
		}
		if err := copyUpgradeTree(filepath.Join(root, name), filepath.Join(dest, name)); err != nil {
			os.RemoveAll(dest)
			return "", fmt.Errorf("copy %s: %w", name, err)
		}
	}
	return dest, nil
}

// Previous releases listed the bundled, link-free JRE in their file manifest.
// Leave independently installed runtimes (including links to a system JDK)
// in place without walking into or copying them.
func snapshotBundledRuntime(root, name string) (bool, error) {
	manifest, err := os.ReadFile(filepath.Join(root, "manifest.sha256"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(manifest), "\n") {
		_, path, ok := strings.Cut(line, "  ")
		if !ok || !strings.HasPrefix(path, "runtime/") {
			continue
		}
		regularTree := true
		err := filepath.WalkDir(filepath.Join(root, name), func(_ string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && !entry.Type().IsRegular() {
				regularTree = false
				return fs.SkipAll
			}
			return nil
		})
		return regularTree, err
	}
	return false, nil
}

func sealPortableSnapshot(snapshot string) (upgradeState, error) {
	for _, name := range append(portableProgramFiles(), "config/dbgold.env", "web/index.html") {
		info, err := os.Lstat(filepath.Join(snapshot, name))
		if err != nil {
			return upgradeState{}, err
		}
		wantDir := name == "web" || name == "lib"
		if wantDir != info.IsDir() || !wantDir && !info.Mode().IsRegular() {
			return upgradeState{}, fmt.Errorf("invalid portable backup entry: %s", name)
		}
	}
	checksum, err := portableSnapshotChecksum(snapshot)
	if err != nil {
		return upgradeState{}, err
	}
	return upgradeState{Backup: filepath.Base(snapshot), Token: rand.Text(), SnapshotSHA256: checksum, KeepRuntime: true}, nil
}

// Record the complete cold tree, including empty directories and file lengths.
// A missing file can then be distinguished from one absent before the upgrade.
func portableSnapshotChecksum(snapshot string) (string, error) {
	hash := sha256.New()
	err := fs.WalkDir(os.DirFS(snapshot), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || name == ".dbgold.lock" {
			return err
		}
		if entry.IsDir() {
			fmt.Fprintf(hash, "dir\x00%s\x00", name)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported snapshot entry: %s", name)
		}
		fmt.Fprintf(hash, "file\x00%s\x00%d\x00%o\x00", name, info.Size(), info.Mode().Perm())
		file, err := os.Open(filepath.Join(snapshot, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		n, readErr := io.Copy(hash, file)
		if err := errors.Join(readErr, file.Close()); err != nil {
			return err
		}
		if n != info.Size() {
			return fmt.Errorf("snapshot file changed during verification: %s", name)
		}
		return nil
	})
	return hex.EncodeToString(hash.Sum(nil)), err
}

func copyUpgradeTree(src, dest string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dest, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Mkdir(to, 0o700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported non-regular file %s", path)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		return errors.Join(err, out.Close())
	})
}

func checkPortableUpgradeState(root string) error {
	if _, err := os.Lstat(filepath.Join(root, upgradeMarker)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("interrupted portable upgrade: run portable-upgrade again from the new package to recover the cold backup")
}

func readUpgradeState(root string) (upgradeState, error) {
	var state upgradeState
	raw, err := os.ReadFile(filepath.Join(root, upgradeMarker))
	if err != nil {
		return state, err
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	if !strings.HasPrefix(state.Backup, "upgrade-") || filepath.Base(state.Backup) != state.Backup || state.Token == "" || len(state.SnapshotSHA256) != sha256.Size*2 {
		return state, errors.New("invalid upgrade recovery record")
	}
	return state, nil
}

func writeUpgradeState(root string, state upgradeState) error {
	tmp, err := os.CreateTemp(filepath.Join(root, "backups"), ".upgrade-marker-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = json.NewEncoder(tmp).Encode(state)
	if err = errors.Join(err, tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(root, upgradeMarker))
}

func restorePortable(root string, state upgradeState) error {
	snapshot := filepath.Join(root, "backups", state.Backup)
	// An orphaned probe can still be closing SQLite if its updater was killed.
	unlock, err := lockPortableRoot(snapshot)
	if err != nil {
		return fmt.Errorf("wait for the upgrade probe to exit before recovery: %w", err)
	}
	defer unlock()
	// The target may be partially updated. Its own release manifest cannot
	// identify the program being restored.
	if err := checkPortableEdition(snapshot); err != nil {
		return fmt.Errorf("incompatible cold backup: %w", err)
	}
	checksum, err := portableSnapshotChecksum(snapshot)
	if err != nil {
		return err
	}
	if checksum != state.SnapshotSHA256 {
		return fmt.Errorf("cold backup is incomplete or has changed: %s", snapshot)
	}
	restore := append(portableProgramFiles(), "config", "data", "uploads", "logs")
	if !state.KeepRuntime {
		// An interrupted old updater may have already replaced or removed the JRE.
		restore = append(restore, "runtime")
	}
	for _, name := range restore {
		src, dst := filepath.Join(snapshot, name), filepath.Join(root, name)
		_, err := os.Lstat(src)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if errRemove := os.RemoveAll(dst); errRemove != nil {
			return errRemove
		}
		if err == nil {
			if err := copyUpgradeTree(src, dst); err != nil {
				return err
			}
		}
	}
	if state.KeepRuntime {
		if err := recoverMissingRuntime(root, snapshot); err != nil {
			return err
		}
	}
	return os.Remove(filepath.Join(root, upgradeMarker))
}

// A current updater leaves an existing local runtime untouched. If the whole
// legacy JRE disappeared, restore the cold copy without leaving a partial JRE
// that a later recovery might mistake for the operator's installation.
func recoverMissingRuntime(root, snapshot string) error {
	dest := filepath.Join(root, "runtime")
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		return err
	}
	src := filepath.Join(snapshot, "runtime")
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	stage, err := os.MkdirTemp(filepath.Join(root, "backups"), ".restore-runtime-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	staged := filepath.Join(stage, "runtime")
	if err := copyUpgradeTree(src, staged); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		return err
	}
	return os.Rename(staged, dest)
}

func probeInstalledPortable(root string, state upgradeState) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(root, portableProgramFiles()[0]), "portable-upgrade-check")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(state.Token + "\n")
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("installed executable readiness check: %w", ctx.Err())
		}
		return fmt.Errorf("installed executable readiness check: %w", err)
	}
	if !strings.Contains(stdout.String(), "\ndbgold-ready "+state.Token+"\n") {
		return errors.New("installed executable did not confirm readiness")
	}
	return nil
}

func runPortableUpgradeCheck(args []string) error {
	if len(args) != 0 {
		return errors.New("portable-upgrade-check is an internal command")
	}
	root, err := portableExecutableDir()
	if err != nil {
		return err
	}
	state, err := readUpgradeState(root)
	if err != nil {
		return err
	}
	token, err := io.ReadAll(io.LimitReader(os.Stdin, 128))
	if err != nil || strings.TrimSpace(string(token)) != state.Token {
		return errors.New("upgrade check requires the active updater")
	}
	release, err := lockPortableRoot(filepath.Join(root, "backups", state.Backup))
	if err != nil {
		return err
	}
	defer release()
	// Recovery may have run between locating the snapshot and taking its lock.
	if current, err := readUpgradeState(root); err != nil || state != current {
		return errors.New("upgrade is no longer active")
	}
	if err := preparePortableRuntime(root); err != nil {
		return err
	}
	cfg, err := loadPortableConfig(root)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := false
	if err := runServerWithReady(ctx, cfg, func() { ready = true; cancel() }); err != nil {
		return err
	}
	if !ready {
		return errors.New("installed server stopped before it was ready")
	}
	fmt.Printf("\ndbgold-ready %s\n", state.Token)
	return nil
}
