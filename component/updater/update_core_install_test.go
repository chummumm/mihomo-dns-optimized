package updater

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCoreUnifiedVersionCheckDoesNotRequireExecutable(t *testing.T) {
	for _, current := range []string{"mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", "mihomo-optimized-v1.0.2", "development"} {
		t.Run(current, func(t *testing.T) {
			f := newCoreAutomaticFixture(t, current, "mihomo-optimized-v1.0.1", nil)
			// A Unix daemon can keep running after its executable is unlinked.
			// Checking an equal/older version does not need an installable path.
			if err := os.Remove(f.path); err != nil {
				t.Fatal(err)
			}
			reservation, err := f.u.ReserveCheckAndUpdate()
			if err != nil {
				t.Fatal(err)
			}
			defer reservation.Close()
			result, err := reservation.CheckAndUpdate(context.Background(), f.path, f.u.restartHandler, nil)
			if current == "development" {
				if !errors.Is(err, ErrCoreVersionUnavailable) {
					t.Fatalf("unknown version did not retain its comparison error: %v", err)
				}
			} else if current == "mihomo-optimized-v1.0.0" {
				if err == nil || !strings.Contains(err.Error(), "locate current executable for replacement") {
					t.Fatalf("newer version hid the missing installation path: %v", err)
				}
			} else if err != nil {
				t.Fatalf("version check depended on an executable it would not replace: %v", err)
			}
			if result.CurrentVersion != current || result.LatestVersion != "mihomo-optimized-v1.0.1" || result.Updated || f.requests.Load() != 1 || f.archives.Load() != 0 || f.restarts.Load() != 0 {
				t.Fatalf("read-only version check did not complete correctly: %+v requests=%d archives=%d restarts=%d", result, f.requests.Load(), f.archives.Load(), f.restarts.Load())
			}
		})
	}
}

func TestCoreUpdaterRestoresOwnerExecutePermission(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not use Unix execute permission bits")
	}
	for _, mode := range []os.FileMode{0o640, 0o444, 0o751} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
			if err := os.Chmod(f.path, mode); err != nil {
				t.Fatal(err)
			}
			if err := f.u.Update(f.path, ReleaseChannel, false); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(f.path)
			if err != nil || info.Mode().Perm() != mode|0o100 {
				t.Fatalf("installed core cannot execute without changing its original access policy: %v %v", info, err)
			}
			if err := validateStagedCoreContext(context.Background(), f.path, "mihomo-optimized-v1.0.1"); err != nil {
				t.Fatalf("installed temporary core cannot run after replacement: %v", err)
			}
			backup := filepath.Join(filepath.Dir(f.path), "meta-backup", filepath.Base(f.path))
			backupInfo, err := os.Stat(backup)
			if err != nil || backupInfo.Mode().Perm() != mode {
				t.Fatalf("update changed the original executable's permissions: %v %v", backupInfo, err)
			}
		})
	}
}

func TestCoreUpdaterPreflightFailurePreservesExecutableAndBackup(t *testing.T) {
	for _, behavior := range []string{"wrong-version", "wrong-platform", "failure", "hang"} {
		t.Run(behavior, func(t *testing.T) {
			t.Setenv("MIHOMO_TEST_CORE_PROBE", behavior)
			f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
			backupDir := filepath.Join(filepath.Dir(f.path), "meta-backup")
			if err := os.MkdirAll(backupDir, 0o755); err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(backupDir, filepath.Base(f.path))
			previousBackup := []byte("previous recovery core")
			if err := os.WriteFile(backup, previousBackup, 0o751); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if behavior == "hang" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 250*time.Millisecond)
				defer cancel()
			}
			reservation, err := f.u.ReserveCheckAndUpdate()
			if err != nil {
				t.Fatal(err)
			}
			defer reservation.Close()
			result, err := reservation.CheckAndUpdate(ctx, f.path, f.u.restartHandler, nil)
			if err == nil || !strings.Contains(err.Error(), "validate staged") || result.Updated || f.u.pendingRestart || !f.settled() {
				t.Fatalf("unusable release reached installation or left the updater locked: %+v %v", result, err)
			}
			if behavior == "hang" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("preflight did not retain its cancellation cause: %v", err)
			}
			f.unchanged(t)
			data, err := os.ReadFile(backup)
			if err != nil || !bytes.Equal(data, previousBackup) {
				t.Fatalf("failed preflight changed the recovery executable: %q %v", data, err)
			}
			select {
			case err := <-f.commits:
				t.Fatalf("failed preflight entered the shutdown/restart transaction: %v", err)
			default:
			}
			staging, err := filepath.Glob(filepath.Join(filepath.Dir(f.path), ".mihomo-update-*"))
			if err != nil || len(staging) != 0 {
				t.Fatalf("failed preflight left downloaded staging files: %v %v", staging, err)
			}
		})
	}
}

func TestCoreUpdaterRemovesArchiveBeforeBackup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the move-based backup transaction")
	}
	for _, fallback := range []bool{false, true} {
		name := "hard-link"
		if fallback {
			name = "copy-fallback"
		}
		t.Run(name, func(t *testing.T) {
			f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
			called := false
			f.u.linkBackup = func(oldPath, backupPath string) error {
				called = true
				staging, err := filepath.Glob(filepath.Join(filepath.Dir(f.path), ".mihomo-update-*"))
				if err != nil || len(staging) != 1 {
					t.Fatalf("expected one staged update before backup: %v %v", staging, err)
				}
				entries, err := os.ReadDir(staging[0])
				if err != nil || len(entries) != 1 || entries[0].Name() != DefaultCoreUpdater.CoreBaseName() {
					t.Fatalf("backup overlaps the downloaded archive on disk: %v %v", entries, err)
				}
				if fallback {
					return errors.New("hard links are unavailable on this fixture filesystem")
				}
				return os.Link(oldPath, backupPath)
			}
			oldInfo, err := os.Stat(f.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.u.Update(f.path, ReleaseChannel, false); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("installation did not prepare a backup")
			}
			backup := filepath.Join(filepath.Dir(f.path), "meta-backup", filepath.Base(f.path))
			data, err := os.ReadFile(backup)
			if err != nil || !bytes.Equal(data, f.old) {
				t.Fatalf("backup fallback lost the original: %q %v", data, err)
			}
			backupInfo, err := os.Stat(backup)
			if err != nil || os.SameFile(oldInfo, backupInfo) == fallback {
				t.Fatalf("wrong backup behavior for copy-fallback=%t: %v", fallback, err)
			}
		})
	}
}

func TestCoreUpdaterBackupRechecksCancellationAndExecutableIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows moves the original after its identity check")
	}
	for _, fault := range []string{"canceled-hard-link", "canceled-copy", "changed-hard-link", "changed-copy"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			current, staged := filepath.Join(dir, "mihomo"), filepath.Join(dir, "staged")
			old, replacement, previousBackup := []byte("original core"), []byte("external core"), []byte("previous backup")
			if err := os.WriteFile(current, old, 0o751); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(staged, []byte("candidate core"), 0o751); err != nil {
				t.Fatal(err)
			}
			backupDir := filepath.Join(dir, "meta-backup")
			if err := os.MkdirAll(backupDir, 0o755); err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(backupDir, "mihomo")
			if err := os.WriteFile(backup, previousBackup, 0o751); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			identity, err := snapshotCoreExecutable(ctx, current)
			if err != nil {
				t.Fatal(err)
			}
			canceled := strings.HasPrefix(fault, "canceled")
			u := CoreUpdater{linkBackup: func(oldPath, backupPath string) error {
				if canceled {
					cancel()
				} else if err := os.WriteFile(current, replacement, 0o751); err != nil {
					return err
				}
				if strings.HasSuffix(fault, "copy") {
					return errors.New("hard links are unavailable on this fixture filesystem")
				}
				return os.Link(oldPath, backupPath)
			}}
			err = u.installContext(ctx, current, staged, &identity)
			if canceled {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("backup ignored cancellation: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "executable changed") {
				t.Fatalf("backup ignored an external executable change: %v", err)
			}
			want := old
			if !canceled {
				want = replacement
			}
			data, err := os.ReadFile(current)
			if err != nil || !bytes.Equal(data, want) {
				t.Fatalf("failed backup transaction changed the current core: %q %v", data, err)
			}
			data, err = os.ReadFile(backup)
			if err != nil || !bytes.Equal(data, previousBackup) {
				t.Fatalf("failed backup transaction changed the previous recovery core: %q %v", data, err)
			}
			entries, err := os.ReadDir(backupDir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "mihomo" {
				t.Fatalf("backup transaction leaked temporary files: %v %v", entries, err)
			}
		})
	}
}

func TestCoreUpdaterPreservesExecutableSymlink(t *testing.T) {
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
	alias := f.path + "-alias"
	if err := os.Symlink(f.path, alias); err != nil {
		t.Skipf("executable symlinks are unavailable: %v", err)
	}
	if err := f.u.Update(alias, ReleaseChannel, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(alias)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("update replaced the user's executable symlink: %v %v", info, err)
	}
	backup := filepath.Join(filepath.Dir(f.path), "meta-backup", filepath.Base(f.path))
	data, err := os.ReadFile(backup)
	if err != nil || !bytes.Equal(data, f.old) {
		t.Fatalf("symlink target's original executable was not backed up: %q %v", data, err)
	}
}

func TestCoreUpdaterRejectsBackupOfTemporarilyReplacedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not use hard-link backups")
	}
	dir := t.TempDir()
	current, staged, saved := filepath.Join(dir, "mihomo"), filepath.Join(dir, "staged"), filepath.Join(dir, "original")
	original := []byte("same bytes must still belong to the original inode")
	if err := os.WriteFile(current, original, 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new core"), 0o751); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(current)
	if err != nil {
		t.Fatal(err)
	}
	u := CoreUpdater{linkBackup: func(oldPath, backupPath string) error {
		if err := os.Rename(current, saved); err != nil {
			return err
		}
		if err := os.WriteFile(current, original, 0o751); err != nil {
			return err
		}
		// Make even the size, mode, timestamp and digest match. A real hard
		// link must still refer to the initial executable inode.
		if err := os.Chtimes(current, oldInfo.ModTime(), oldInfo.ModTime()); err != nil {
			return err
		}
		if err := os.Link(current, backupPath); err != nil {
			return err
		}
		if err := os.Remove(current); err != nil {
			return err
		}
		return os.Rename(saved, current)
	}}
	if err := u.install(current, staged); err == nil || !strings.Contains(err.Error(), "executable changed") {
		t.Fatalf("backup accepted a different inode after the current path was restored: %v", err)
	}
	currentInfo, err := os.Stat(current)
	if err != nil || !os.SameFile(currentInfo, oldInfo) {
		t.Fatalf("failed backup replaced the original core: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "meta-backup"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("mismatched backup became persistent: %v %v", entries, err)
	}
}

func TestCoreUpdaterUnixBackupSharesOriginalFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows moves the running executable before replacement")
	}
	dir := t.TempDir()
	current, staged := filepath.Join(dir, "mihomo"), filepath.Join(dir, "staged")
	original := []byte("old core must remain recoverable")
	if err := os.WriteFile(current, original, 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new core"), 0o751); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(current)
	if err != nil {
		t.Fatal(err)
	}
	u := &CoreUpdater{}
	if err := u.install(current, staged); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "meta-backup", "mihomo")
	backupInfo, err := os.Stat(backup)
	if err != nil || !os.SameFile(originalInfo, backupInfo) {
		t.Fatalf("Unix update copied the running core instead of retaining a hard link: %v", err)
	}
	data, err := os.ReadFile(backup)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("backup lost the original executable: %q %v", data, err)
	}
}

func TestCoreUpdaterBackupPreservesCompetingTemporaryPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not use hard-link backups")
	}
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			current, staged := filepath.Join(dir, "mihomo"), filepath.Join(dir, "staged")
			old, candidate, previousBackup := []byte("original core"), []byte("candidate core"), []byte("previous backup")
			if err := os.WriteFile(current, old, 0o751); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(staged, candidate, 0o751); err != nil {
				t.Fatal(err)
			}
			backupDir := filepath.Join(dir, "meta-backup")
			if err := os.MkdirAll(backupDir, 0o755); err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(backupDir, "mihomo")
			if err := os.WriteFile(backup, previousBackup, 0o751); err != nil {
				t.Fatal(err)
			}
			marker := []byte("a competing operation owns this file")
			symlinkTarget := filepath.Join(dir, "competing-target")
			if err := os.WriteFile(symlinkTarget, marker, 0o600); err != nil {
				t.Fatal(err)
			}
			var competingPath string
			var competingInfo os.FileInfo
			u := CoreUpdater{linkBackup: func(_ string, backupPath string) error {
				competingPath = backupPath
				if kind == "symlink" {
					if err := os.Symlink(symlinkTarget, backupPath); err != nil {
						t.Skipf("symlinks are unavailable: %v", err)
					}
				} else if err := os.WriteFile(backupPath, marker, 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				competingInfo, err = os.Lstat(backupPath)
				if err != nil {
					t.Fatal(err)
				}
				return fs.ErrExist
			}}
			if err := u.install(current, staged); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("backup did not retain the exclusive-create failure: %v", err)
			}
			info, err := os.Lstat(competingPath)
			if err != nil || !os.SameFile(competingInfo, info) {
				t.Fatalf("failed backup removed or replaced a competing temporary %s: %v", kind, err)
			}
			for path, want := range map[string][]byte{current: old, staged: candidate, backup: previousBackup, competingPath: marker, symlinkTarget: marker} {
				data, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(data, want) {
					t.Fatalf("failed backup changed %s: %q %v", filepath.Base(path), data, err)
				}
			}
		})
	}
}
