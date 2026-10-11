package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	coreAutoArchiveOnce  sync.Once
	coreAutoArchive      []byte
	coreAutoArchiveError error
)

// Only a temporary executable is ever installed. Darwin gets a native binary
// so the real signing stage is also exercised, but the restart is a recorder.
func coreAutomaticArchive(t *testing.T) []byte {
	t.Helper()
	coreAutoArchiveOnce.Do(func() {
		payload := []byte("new core fixture, never executed\n")
		if runtime.GOOS == "darwin" {
			var path string
			path, coreAutoArchiveError = os.Executable()
			if coreAutoArchiveError == nil {
				payload, coreAutoArchiveError = os.ReadFile(path)
			}
		}
		if coreAutoArchiveError != nil {
			return
		}
		member := DefaultCoreUpdater.CoreBaseName()
		if runtime.GOOS == "windows" {
			member += ".exe"
		}
		coreAutoArchive = testCoreArchive(t, payload, member, runtime.GOOS == "windows")
	})
	if coreAutoArchiveError != nil {
		t.Fatal(coreAutoArchiveError)
	}
	return coreAutoArchive
}

type coreAutomaticFixture struct {
	u        *CoreUpdater
	clock    uiTestAutomaticClock
	path     string
	old      []byte
	version  atomic.Value
	requests atomic.Int64
	archives atomic.Int64
	restarts atomic.Int64
	commits  chan error
}

func newCoreAutomaticFixture(t *testing.T, current, latest string, hook func(http.ResponseWriter, *http.Request) bool) *coreAutomaticFixture {
	t.Helper()
	f := &coreAutomaticFixture{path: filepath.Join(t.TempDir(), "isolated-core"), old: []byte("old core fixture, never executed\n"), commits: make(chan error, 8)}
	f.version.Store(current)
	if err := os.WriteFile(f.path, f.old, 0o751); err != nil {
		t.Fatal(err)
	}
	// Keep the actual temporary file as the guard's anchor. Production resolves
	// symlinks, which also normalizes Windows path spelling and Darwin /var.
	fixturePath, err := filepath.EvalSymlinks(f.path)
	if err != nil {
		t.Fatal(err)
	}
	archive := coreAutomaticArchive(t)
	digest := sha256.Sum256(archive)
	target, err := coreTarget(runtime.GOOS, runtime.GOARCH, coreBuildSettings())
	if err != nil {
		t.Fatal(err)
	}
	extension := ".gz"
	if runtime.GOOS == "windows" {
		extension = ".zip"
	}
	asset := latest + "-" + target + extension
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if strings.HasSuffix(r.URL.Path, extension) {
			f.archives.Add(1)
		}
		if hook != nil && hook(w, r) {
			return
		}
		switch r.URL.Path {
		case "/releases/latest/download/version.txt":
			fmt.Fprintln(w, latest)
		case "/releases/download/" + latest + "/SHA256SUMS":
			fmt.Fprintf(w, "%x  %s\n", digest, asset)
		case "/releases/download/" + latest + "/" + asset:
			_, _ = w.Write(archive)
		default:
			t.Errorf("automatic updater used an unpinned or unexpected source: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.u = &CoreUpdater{
		releaseURL: server.URL + "/releases/", newTicker: f.clock.newTicker,
		executable:     func() (string, error) { return f.path, nil },
		currentVersion: func() string { return f.version.Load().(string) },
	}
	f.u.SetAutomaticRestartHandler(func(path string, commit func() error) (err error) {
		// Report guard failures too, instead of leaving commitResult to time out.
		defer func() { f.commits <- err }()
		expected, err := os.Stat(fixturePath)
		if err != nil {
			return fmt.Errorf("inspect temporary executable: %w", err)
		}
		actual, err := os.Stat(path)
		if err != nil || !actual.Mode().IsRegular() || !os.SameFile(expected, actual) {
			return fmt.Errorf("attempt to restart outside the temporary fixture: %q", path)
		}
		err = commit()
		if err == nil {
			if !f.u.mu.TryLock() {
				err = errors.New("restart retained the installation operation lock")
			} else {
				f.u.mu.Unlock()
			}
			if !f.u.policyMu.TryLock() {
				err = errors.New("restart retained the automatic policy lock")
			} else {
				f.u.policyMu.Unlock()
			}
			staging, readErr := filepath.Glob(filepath.Join(filepath.Dir(f.path), ".mihomo-update-*"))
			if readErr != nil || len(staging) != 0 {
				err = errors.New("successful installation left staging files before process restart")
			}
		}
		if err == nil {
			f.restarts.Add(1)
		}
		return err
	})
	t.Cleanup(func() {
		f.u.policyMu.Lock()
		job := f.u.automatic
		f.u.stopAutomaticUpdatesLocked()
		f.u.policyMu.Unlock()
		if job != nil {
			coreAutomaticDone(t, job.done)
		}
	})
	return f
}

func coreAutomaticWait(t *testing.T, condition func() bool) {
	t.Helper()
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for !condition() {
		select {
		case <-timeout.C:
			t.Fatal("core automatic update did not reach expected state")
		case <-poll.C:
		}
	}
}

func coreAutomaticDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled core task did not finish")
	}
}

func (f *coreAutomaticFixture) settled() bool {
	if !f.u.mu.TryLock() {
		return false
	}
	f.u.mu.Unlock()
	return true
}

func (f *coreAutomaticFixture) unchanged(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(data, f.old) {
		t.Fatalf("canceled or invalid update changed the executable: %v", err)
	}
	if f.restarts.Load() != 0 {
		t.Fatal("canceled or invalid update restarted the process")
	}
}

func (f *coreAutomaticFixture) job() *coreAutomaticUpdate {
	f.u.policyMu.Lock()
	defer f.u.policyMu.Unlock()
	return f.u.automatic
}

func (f *coreAutomaticFixture) commitResult(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.commits:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("automatic core never reached commit")
		return nil
	}
}

func TestCoreUpdaterAutomaticFixtureAcceptsPathAliases(t *testing.T) {
	for _, alias := range []string{"dot-component", "directory-symlink", "windows-case"} {
		t.Run(alias, func(t *testing.T) {
			if alias == "windows-case" && runtime.GOOS != "windows" {
				t.Skip("Windows path spelling regression")
			}
			f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
			switch alias {
			case "dot-component":
				// Do not use Join: this spelling must reach production unchanged.
				f.path = filepath.Dir(f.path) + string(os.PathSeparator) + "." + string(os.PathSeparator) + filepath.Base(f.path)
			case "directory-symlink":
				link := filepath.Join(t.TempDir(), "fixture-alias")
				if err := os.Symlink(filepath.Dir(f.path), link); err != nil {
					t.Skipf("directory symlinks unavailable: %v", err)
				}
				f.path = filepath.Join(link, filepath.Base(f.path))
			case "windows-case":
				f.path = strings.ToUpper(f.path)
				if _, err := os.Stat(f.path); err != nil {
					t.Skipf("case-sensitive temporary filesystem: %v", err)
				}
			}
			resolved, err := filepath.EvalSymlinks(f.path)
			if err != nil {
				t.Fatal(err)
			}
			if resolved == f.path {
				t.Fatal("fixture did not create a distinct path spelling")
			}
			f.u.ConfigureAutoUpdate(true, 1)
			f.clock.ticker(t, 0).fire(t, time.Now())
			if err := f.commitResult(t); err != nil {
				t.Fatal("temporary executable alias was rejected", err)
			}
			if f.restarts.Load() != 1 {
				t.Fatal("temporary executable alias did not complete one recorded restart")
			}
		})
	}
}

func TestCoreUpdaterAutomaticFixtureRejectsOtherExecutables(t *testing.T) {
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
	host, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "unrelated-core")
	if err := os.WriteFile(other, f.old, 0o751); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{host, other} {
		committed := false
		err := f.u.restartHandler(path, func() error { committed = true; return nil })
		if err == nil || committed {
			t.Fatalf("fixture allowed a non-fixture executable to reach installation: %q", path)
		}
		if reported := f.commitResult(t); reported != err {
			t.Fatal("fixture did not report its rejected restart immediately")
		}
	}
	f.unchanged(t)
}

func TestCoreUpdaterAutomaticDefaultsReloadAndNoBrowser(t *testing.T) {
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
	if f.u.AutoUpdate() || f.u.UpdateInterval() != 24 || f.u.NextCheck() != "" || f.job() != nil {
		t.Fatal("default core policy started automatic work")
	}
	f.u.ConfigureAutoUpdate(false, 6)
	if f.u.UpdateInterval() != 6 || len(f.clock.tickers) != 0 || f.requests.Load() != 0 {
		t.Fatal("disabled core policy created a timer or made a request")
	}
	f.u.ConfigureAutoUpdate(true, 6)
	timer := f.clock.ticker(t, 0)
	job, initialNext := f.job(), f.u.NextCheck()
	if timer.interval != 6*time.Hour || initialNext == "" || f.requests.Load() != 0 {
		t.Fatal("first core check did not wait its configured interval")
	}
	for i := 0; i < 30; i++ {
		f.u.ConfigureAutoUpdate(true, 6)
	}
	if f.job() != job || f.u.NextCheck() != initialNext || f.clock.ticker(t, 0) != timer {
		t.Fatal("same-policy reload replaced or postponed the core timer")
	}
	at := time.Now()
	timer.fire(t, at)
	if err := f.commitResult(t); err != nil {
		t.Fatal(err)
	}
	coreAutomaticWait(t, f.settled)
	if f.requests.Load() != 3 || f.archives.Load() != 1 || f.restarts.Load() != 1 {
		t.Fatalf("background update did not perform exactly one checked install: requests=%d archives=%d restarts=%d", f.requests.Load(), f.archives.Load(), f.restarts.Load())
	}
	if got := f.u.NextCheck(); got != at.Add(6*time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("next check=%s", got)
	}
	backup, err := os.ReadFile(filepath.Join(filepath.Dir(f.path), "meta-backup", filepath.Base(f.path)))
	if err != nil || !bytes.Equal(backup, f.old) {
		t.Fatal("automatic installation did not preserve the old executable", err)
	}
	// C.Version remains old until restart. A second tick must not repeat the
	// download/install during that window, even if the fake restart returns.
	timer.fire(t, at.Add(6*time.Hour))
	coreAutomaticWait(t, f.settled)
	if f.requests.Load() != 3 || f.restarts.Load() != 1 {
		t.Fatal("pending restart triggered a repeated install")
	}
	if err := f.u.UpdateContext(context.Background(), f.path, ReleaseChannel, true); !errors.Is(err, ErrCoreUpdateBusy) {
		t.Fatalf("a manual request overwrote an installed core awaiting restart: %v", err)
	}
	f.u.ConfigureAutoUpdate(false, 6)
	coreAutomaticDone(t, job.done)
	if f.u.NextCheck() != "" || f.u.AutoUpdate() {
		t.Fatal("disabling core updates left an active schedule")
	}
}

func TestCoreUpdaterAutomaticRequiresStrictlyNewerVersion(t *testing.T) {
	for _, current := range []string{"mihomo-optimized-v1.0.1", "mihomo-optimized-v1.1.0", "unknown-development-build"} {
		t.Run(current, func(t *testing.T) {
			f := newCoreAutomaticFixture(t, current, "mihomo-optimized-v1.0.1", nil)
			f.u.ConfigureAutoUpdate(true, 1)
			f.clock.ticker(t, 0).fire(t, time.Now())
			coreAutomaticWait(t, func() bool { return f.requests.Load() == 1 && f.settled() })
			if f.archives.Load() != 0 {
				t.Fatal("same, older, or unrecognized current version downloaded an archive")
			}
			f.unchanged(t)
		})
	}
}

func TestCoreUpdaterAutomaticCancelsNetworkOnDisableOrReschedule(t *testing.T) {
	for _, stage := range []string{"version.txt", ".gz"} {
		if runtime.GOOS == "windows" && stage == ".gz" {
			stage = ".zip"
		}
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled-%t", stage, enabled), func(t *testing.T) {
				started, canceled, unblock := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var once sync.Once
				f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", func(w http.ResponseWriter, r *http.Request) bool {
					if !strings.HasSuffix(r.URL.Path, stage) {
						return false
					}
					once.Do(func() { close(started) })
					select {
					case <-r.Context().Done():
						close(canceled)
					case <-unblock:
					}
					return true
				})
				t.Cleanup(func() { close(unblock) })
				f.u.ConfigureAutoUpdate(true, 6)
				old := f.job()
				f.clock.ticker(t, 0).fire(t, time.Now())
				coreAutomaticDone(t, started)
				applied := make(chan struct{})
				go func() { f.u.ConfigureAutoUpdate(enabled, 12); close(applied) }()
				coreAutomaticDone(t, applied)
				coreAutomaticDone(t, old.done)
				coreAutomaticDone(t, canceled)
				f.unchanged(t)
				if enabled {
					if f.clock.ticker(t, 1).interval != 12*time.Hour || f.job() == old {
						t.Fatal("new interval did not create exactly one replacement timer")
					}
				} else if f.u.NextCheck() != "" {
					t.Fatal("disabled policy retained next check")
				}
			})
		}
	}
}

func TestCoreUpdaterAutomaticRechecksPolicyAndVersionAtCommit(t *testing.T) {
	for _, change := range []string{"disable", "reschedule", "current-version", "restart-handler"} {
		t.Run(change, func(t *testing.T) {
			f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
			ready, release := make(chan struct{}), make(chan struct{})
			f.u.beforeCommit = func() { close(ready); <-release }
			f.u.ConfigureAutoUpdate(true, 6)
			old := f.job()
			f.clock.ticker(t, 0).fire(t, time.Now())
			coreAutomaticDone(t, ready)
			switch change {
			case "disable":
				f.u.ConfigureAutoUpdate(false, 6)
			case "reschedule":
				f.u.ConfigureAutoUpdate(true, 12)
			case "current-version":
				f.version.Store("mihomo-optimized-v1.1.0")
			case "restart-handler":
				f.u.SetAutomaticRestartHandler(nil)
			}
			close(release)
			if err := f.commitResult(t); err == nil {
				t.Fatal("obsolete update committed")
			}
			coreAutomaticWait(t, f.settled)
			f.unchanged(t)
			if change != "current-version" {
				coreAutomaticDone(t, old.done)
			}
		})
	}
}

func TestCoreUpdaterAutomaticCannotRestartAfterWaitingForReload(t *testing.T) {
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
	ready, release := make(chan struct{}), make(chan struct{})
	f.u.SetAutomaticRestartHandler(func(path string, commit func() error) error {
		close(ready)
		<-release // model waiting for the executor's configuration transaction
		err := commit()
		if err == nil {
			f.restarts.Add(1)
		}
		f.commits <- err
		return err
	})
	f.u.ConfigureAutoUpdate(true, 1)
	old := f.job()
	f.clock.ticker(t, 0).fire(t, time.Now())
	coreAutomaticDone(t, ready)
	f.u.ConfigureAutoUpdate(false, 1)
	close(release)
	if err := f.commitResult(t); err == nil {
		t.Fatal("canceled transaction restarted after waiting for reload")
	}
	coreAutomaticDone(t, old.done)
	f.unchanged(t)
}

func TestCoreUpdaterAutomaticPreservesExternalReplacement(t *testing.T) {
	for _, replacement := range []string{"rename", "same-inode-same-time"} {
		t.Run(replacement, func(t *testing.T) {
			f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
			ready, release := make(chan struct{}), make(chan struct{})
			f.u.beforeCommit = func() { close(ready); <-release }
			f.u.ConfigureAutoUpdate(true, 1)
			f.clock.ticker(t, 0).fire(t, time.Now())
			coreAutomaticDone(t, ready)
			info, err := os.Stat(f.path)
			if err != nil {
				t.Fatal(err)
			}
			external := bytes.Clone(f.old)
			external[0] = 'X'
			if replacement == "rename" {
				other := f.path + ".external"
				if err = os.WriteFile(other, external, info.Mode()); err == nil {
					if runtime.GOOS == "windows" {
						err = os.Remove(f.path)
					}
					if err == nil {
						err = os.Rename(other, f.path)
					}
				}
			} else {
				err = os.WriteFile(f.path, external, info.Mode())
				if err == nil {
					err = os.Chtimes(f.path, info.ModTime(), info.ModTime())
				}
			}
			close(release)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.commitResult(t); err == nil || !strings.Contains(err.Error(), "executable changed") {
				t.Fatalf("external replacement was not detected: %v", err)
			}
			actual, err := os.ReadFile(f.path)
			if err != nil || !bytes.Equal(actual, external) || f.restarts.Load() != 0 {
				t.Fatal("automatic updater overwrote or restarted an externally replaced executable", err)
			}
		})
	}
}

func TestCoreUpdaterAutomaticSharesManualGateAndInstalledVersion(t *testing.T) {
	for _, manualFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("manual-first-%t", manualFirst), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, "version.txt") {
					once.Do(func() { close(started) })
					select {
					case <-release:
					case <-r.Context().Done():
						return true
					}
				}
				return false
			})
			f.u.ConfigureAutoUpdate(true, 1)
			timer := f.clock.ticker(t, 0)
			manualDone := make(chan error, 1)
			if manualFirst {
				go func() { manualDone <- f.u.UpdateContext(context.Background(), f.path, ReleaseChannel, false) }()
			} else {
				timer.fire(t, time.Now())
			}
			coreAutomaticDone(t, started)
			if manualFirst {
				at := time.Now()
				timer.fire(t, at)
				coreAutomaticWait(t, func() bool { return f.u.NextCheck() == at.Add(time.Hour).UTC().Format(time.RFC3339) })
			} else if err := f.u.UpdateContext(context.Background(), f.path, ReleaseChannel, false); !errors.Is(err, ErrCoreUpdateBusy) {
				t.Fatalf("manual update did not share automatic operation gate: %v", err)
			}
			close(release)
			if manualFirst {
				select {
				case err := <-manualDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(30 * time.Second):
					t.Fatal("manual install did not finish")
				}
			} else if err := f.commitResult(t); err != nil {
				t.Fatal(err)
			}
			coreAutomaticWait(t, f.settled)
			if f.requests.Load() != 3 || f.archives.Load() != 1 {
				t.Fatal("manual and automatic tasks overlapped downloads")
			}
			timer.fire(t, time.Now())
			coreAutomaticWait(t, f.settled)
			if f.requests.Load() != 3 {
				t.Fatal("already installed version was ignored while restart was pending")
			}
			if manualFirst && f.restarts.Load() != 0 {
				t.Fatal("manual compatibility update unexpectedly invoked automatic restart")
			}
		})
	}
}

func TestCoreUpdaterAutomaticChecksumFailureRecoversNextCycle(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "SHA256SUMS") || !fail.Load() {
			return false
		}
		target, _ := coreTarget(runtime.GOOS, runtime.GOARCH, coreBuildSettings())
		extension := ".gz"
		if runtime.GOOS == "windows" {
			extension = ".zip"
		}
		fmt.Fprintf(w, "%s  mihomo-optimized-v1.0.1-%s%s\n", strings.Repeat("0", 64), target, extension)
		return true
	})
	f.u.ConfigureAutoUpdate(true, 1)
	timer := f.clock.ticker(t, 0)
	timer.fire(t, time.Now())
	coreAutomaticWait(t, func() bool { return f.requests.Load() == 3 && f.settled() })
	f.unchanged(t)
	fail.Store(false)
	timer.fire(t, time.Now().Add(time.Hour))
	if err := f.commitResult(t); err != nil {
		t.Fatal(err)
	}
	if f.archives.Load() != 2 || f.restarts.Load() != 1 {
		t.Fatal("failed automatic update could not recover on its next cycle")
	}
}

func TestCoreUpdaterAutomaticRechecksAfterManualInstallWinsCommit(t *testing.T) {
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
	ready, release := make(chan struct{}), make(chan struct{})
	f.u.SetAutomaticRestartHandler(func(path string, commit func() error) error {
		close(ready)
		<-release
		err := commit()
		if err == nil {
			f.restarts.Add(1)
		}
		f.commits <- err
		return err
	})
	f.u.ConfigureAutoUpdate(true, 1)
	f.clock.ticker(t, 0).fire(t, time.Now())
	coreAutomaticDone(t, ready)
	// The automatic archive is ready but the executor's transaction is not.
	// A manual install is now allowed to win, and its version must be honored.
	manualErr := f.u.UpdateContext(context.Background(), f.path, ReleaseChannel, false)
	close(release)
	if manualErr != nil {
		t.Fatal("prepared automatic task blocked manual installer", manualErr)
	}
	if err := f.commitResult(t); err == nil {
		t.Fatal("automatic task installed again after manual update won")
	}
	if f.restarts.Load() != 0 {
		t.Fatal("obsolete automatic task restarted a manually updated process")
	}
	if f.archives.Load() != 2 {
		t.Fatal("expected one prepared archive from each serialized installer")
	}
}

func TestCoreUpdaterAutomaticRequiresRuntimeRestartHandler(t *testing.T) {
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
	f.u.SetAutomaticRestartHandler(nil)
	f.u.ConfigureAutoUpdate(true, 6)
	if !f.u.AutoUpdate() || f.job() != nil || f.u.NextCheck() != "" || f.requests.Load() != 0 {
		t.Fatal("embedded updater scheduled replacement of the host executable")
	}
}
